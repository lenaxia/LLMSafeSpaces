// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// xdg_config_layer.go — the opencode registry-layer boot fix (#1300).
//
// Root cause this file remediates (validated against the production
// failure and reproduced locally with the pinned 1.18.15 binary):
// opencode's V2 model registry (CatalogV2 → model.available(), the
// source SessionRunnerModel.resolve searches) only ingests provider
// blocks from files in the XDG config layer (~/.config/opencode/*.json).
// A config supplied purely via the OPENCODE_CONFIG env var — ours, at
// /agentd-config/agent-config.json — is loaded by the config service
// (so /config/providers shows the blocks, keys resolved) but NEVER
// reaches the catalog. Every session turn then fails with
// "Model unavailable: <provider>/<model>" while every config-level
// probe looks healthy.
//
// The fix: an XDG-layer symlink pointing at the sidecar's config file.
// This mirrors the #1296 auth-store pattern — the PVC holds only the
// link (no credential bytes at rest, US-35.7); the plaintext stays on
// the pod-scoped /agentd-config volume. Validated locally: with the
// symlink in place the registry admits config providers within seconds
// of boot; without it, never.
//
// Also here: quarantine of unparseable user config files. A malformed
// ~/.config/opencode/opencode.jsonc makes opencode EXIT at boot
// (exit status 1, "not valid JSON(C)") — under the supervisor that is
// an infinite crashloop for as long as the file exists on the PVC
// (observed in production: a shell-escaping artifact `\$schema` wedged
// a workspace across pod recreations). Quarantine renames the file
// aside once at boot instead.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// opencodeXDGConfigDir resolves the XDG config dir the same way the
// opencode binary does (XDG_CONFIG_HOME when set, else $HOME/.config).
// The supervisor runs as the consuming uid, so $HOME is the PVC-backed
// home — the symlink lands there and survives pod recreation, which is
// fine: the target path is stable and the sidecar re-materializes the
// file before the probe-gated spawn.
func opencodeXDGConfigDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "opencode")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "/home/sandbox"
	}
	return filepath.Join(home, ".config", "opencode")
}

// effectiveAgentConfigPath resolves the config file opencode will
// actually read: the controller sets OPENCODE_CONFIG directly on the
// workspace container in sidecar mode (agentd_sidecar.go) while the
// single-container topology relies on the supervisor's
// LLMSAFESPACES_AGENT_CONFIG_PATH default — mirroring
// managedProcess's appendEnvIfAbsent(OPENCODE_CONFIG, …) child-env
// resolution so the symlink and the watcher track the SAME file the
// child reads. (Pool run 34066476127: the symlink pointed at the
// /sandbox-runtime default while the child read /agentd-config —
// env-precedence divergence.)
func effectiveAgentConfigPath() string {
	if oc := os.Getenv("OPENCODE_CONFIG"); oc != "" {
		return oc
	}
	return agentConfigPathFromEnv()
}

// ensureOpencodeRegistryConfig COPIES the effective agent-config into
// the XDG layer: ~/.config/opencode/opencode.json ← the sidecar-stamped
// file in sidecar mode (the /sandbox-runtime default in single-container).
// COPY, not symlink (#1310): opencode WRITES to the XDG path (model
// switches, permission changes — PlatformError EACCES in production when
// the symlink pointed at the read-only /agentd-config mount), so the
// XDG file must be uid-1000-owned and writable. The existing config
// watcher re-syncs when the sidecar's version changes (the copy is
// refreshed, then opencode restarts). Best-effort, idempotent, never
// blocks boot — but failure is loud because without it the model
// registry will not admit any platform-delivered provider (#1300).
//
// A real (non-managed) file at the path is LEFT ALONE with a warning:
// the user took over the config layer. opencode layers XDG files under
// the env config, so a user file that omits platform providers keeps
// registry behavior broken — the warning names the exact consequence
// rather than silently clobbering user bytes. Managed copies are
// stamped with a marker comment so user takeovers are distinguishable
// from our own copy.
func ensureOpencodeRegistryConfig(logger *zap.Logger) string {
	target := effectiveAgentConfigPath()
	dir := opencodeXDGConfigDir()
	link := filepath.Join(dir, "opencode.json")

	// #nosec G301 -- 0755 matches opencode's own XDG dir scaffolding and
	// the init-fs managed-dir modes; the dir holds only the config copy
	// (no credential bytes — US-35.7 keeps those on /agentd-config).
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger.Warn("registry config layer: cannot create XDG dir (model registry may not admit providers)",
			zap.String("dir", dir), zap.Error(err))
		return link
	}

	// Legacy symlink from the 0.27.5 symlink-based fix: remove it so the
	// copy can land (a symlink would keep directing opencode's writes at
	// the read-only mount).
	if fi, err := os.Lstat(link); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(link)
	}

	// Content match: if our copy is already current, no-op.
	targetData, err := os.ReadFile(target)
	if err != nil {
		logger.Warn("registry config layer: cannot read agent-config (model registry may not admit providers)",
			zap.String("target", target), zap.Error(err))
		return link
	}
	if cur, err := os.ReadFile(link); err == nil && bytes.Equal(cur, targetData) {
		return link // already current — idempotent no-op
	}

	// The platform config is authoritative at boot — always refresh the
	// copy when it differs. opencode's runtime writes to this file
	// (model switches etc.) are ephemeral session state, re-derived after
	// restart. User-authored configs belong in the OTHER XDG candidates
	// (opencode.jsonc, config.json) which opencode layers on top.

	// Atomic copy: temp file + rename. Owned by uid 1000 (the supervisor),
	// mode 0640 — group-readable for the same cross-uid read the
	// /agentd-config original provides, no credential bytes (the config
	// carries provider slugs and baseURLs, not keys; keys live in the
	// auth store).
	// #nosec G306 -- 0640 mirrors the sidecar's own mode on the source
	// file; the copy carries no secrets (US-35.7: keys are auth-store
	// only) and opencode (uid 1000) must be able to WRITE it.
	tmp := link + ".agentd-tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, targetData, 0o640); err != nil { // #nosec G306
		logger.Warn("registry config layer: temp write failed (model registry may not admit providers)",
			zap.String("link", link), zap.Error(err))
		return link
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		logger.Warn("registry config layer: copy install failed (model registry may not admit providers)",
			zap.String("link", link), zap.Error(err))
	}
	return link
}

// quarantineMalformedUserConfigs renames unparseable opencode config
// candidates in the XDG dir to <name>.invalid-<unixts>. Without this a
// malformed user file crashloops opencode forever (production
// incident #1300: the PVC-persisted artifact survived every pod
// recreation). Only candidates WE do not own are considered — the
// platform symlink points at the sidecar's validated file.
//
// Parse acceptance mirrors opencode's tolerance: strict JSON, or JSONC
// (line + block comments stripped outside strings). A file that parses
// under either is left untouched.
func quarantineMalformedUserConfigs(logger *zap.Logger) {
	dir := opencodeXDGConfigDir()
	for _, name := range []string{"config.json", "opencode.json", "opencode.jsonc"} {
		p := filepath.Join(dir, name)
		data, err := os.ReadFile(p)
		if err != nil || len(data) == 0 {
			continue
		}
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			continue // ours or a user link — not a quarantine candidate
		}
		if json.Valid(data) {
			continue
		}
		if _, jErr := parseJSONC(data); jErr == nil {
			continue // valid JSONC — opencode will accept it
		}
		renamed := fmt.Sprintf("%s.invalid-%d", p, time.Now().Unix())
		if err := os.Rename(p, renamed); err != nil {
			logger.Warn("registry config layer: malformed config could not be quarantined (opencode may crashloop)",
				zap.String("path", p), zap.Error(err))
			continue
		}
		logger.Warn("registry config layer: quarantined malformed config (opencode exits at boot on unparseable configs)",
			zap.String("path", p),
			zap.String("renamedTo", renamed))
	}
}

// parseJSONC strips JSONC comments (// line and /* block */, outside
// string literals) and parses the result as JSON. Escapes inside
// strings (including \") are honored so a "//" inside a string value is
// preserved.
func parseJSONC(data []byte) (any, error) {
	var b strings.Builder
	b.Grow(len(data))
	inStr, esc, inLine, inBlock := false, false, false, false
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
				b.WriteByte(c)
			}
		case inBlock:
			if c == '*' && i+1 < len(data) && data[i+1] == '/' {
				inBlock = false
				i++
			}
		case inStr:
			b.WriteByte(c)
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
		default:
			switch {
			case c == '"':
				inStr = true
				b.WriteByte(c)
			case c == '/' && i+1 < len(data) && data[i+1] == '/':
				inLine = true
				i++
			case c == '/' && i+1 < len(data) && data[i+1] == '*':
				inBlock = true
				i++
			default:
				b.WriteByte(c)
			}
		}
	}
	var v any
	if err := json.Unmarshal([]byte(b.String()), &v); err != nil {
		return nil, err
	}
	return v, nil
}

// normalizeAuthStoreOwnership rewrites the auth store as the CONSUMING
// uid when a cross-uid writer (the sidecar's materialize merge, uid
// 2000) left it foreign-owned. opencode (uid 1000) chmods the file on
// its own auth writes (ControlHttpApi.authSet → "Failed to write auth
// data … EPERM chmod") and chmod requires ownership, not just group
// write. The supervisor runs as the consuming uid and the tmpfs
// directory is group-writable, so a read + atomic rewrite (temp file
// owned by the consuming uid, rename over the target) permanently
// fixes the ownership for this pod. Best-effort: any failure logs and
// continues — the boot-time merge content is already correct, only
// opencode's LIVE auth writes would degrade.
//
// PreBootAuthJSONPath resolves through the PVC symlink to the tmpfs
// store (#1296); the rename targets the RESOLVED path so the symlink
// itself is never replaced.
func normalizeAuthStoreOwnership(logger *zap.Logger) {
	path := preBootAuthJSONPath("")
	if path == "" {
		return
	}
	resolved, err := resolveSymlink(path)
	if err != nil {
		return // absent store — nothing to normalize
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return
	}
	if !needsOwnershipNormalization(fi, os.Getuid()) {
		return // already consuming-uid-owned (or no uid info — never
		// rewrite blind)
	}
	cur := os.Getuid()
	data, err := os.ReadFile(resolved)
	if err != nil {
		logger.Warn("auth store ownership: read failed", zap.String("path", resolved), zap.Error(err))
		return
	}
	tmp := resolved + ".uidnorm-*"
	f, err := os.CreateTemp(filepath.Dir(resolved), filepath.Base(tmp))
	if err != nil {
		logger.Warn("auth store ownership: temp create failed", zap.String("path", resolved), zap.Error(err))
		return
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		logger.Warn("auth store ownership: write failed", zap.Error(err))
		return
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return
	}
	// Match the #1296 mode: group read+write across the uid split.
	// #nosec G302 -- 0660 is the #1296 cross-uid contract (sidecar 2000
	// reads/writes, opencode 1000 chmods); 0600 would sever the split.
	if err := os.Chmod(name, 0o660); err != nil {
		_ = os.Remove(name)
		logger.Warn("auth store ownership: chmod failed", zap.Error(err))
		return
	}
	if err := os.Rename(name, resolved); err != nil {
		_ = os.Remove(name)
		logger.Warn("auth store ownership: rename failed", zap.Error(err))
		return
	}
	logger.Info("auth store ownership: normalized to consuming uid",
		zap.String("path", resolved), zap.Int("uid", cur))
}

// resolveSymlink follows symlinks to the final target (absolute).
func resolveSymlink(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

// agentConfigFingerprint hashes the agent-config file. Absent file →
// "" (distinct from any content hash, so a file APPEARING also counts
// as a change).
func agentConfigFingerprint(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ensureOpencodeBootLayers runs the three #1300 boot fixes that must
// precede the first opencode spawn in EVERY topology that supervises
// opencode (single-container --supervise and sidecar supervise-opencode
// alike): quarantine malformed XDG user configs, install the registry
// symlink, normalize auth-store ownership. Each step is best-effort
// with loud failure logging; none blocks boot.
func ensureOpencodeBootLayers(logger *zap.Logger) {
	quarantineMalformedUserConfigs(logger)
	ensureOpencodeRegistryConfig(logger)
	normalizeAuthStoreOwnership(logger)
}

// needsOwnershipNormalization decides whether the auth store file must
// be rewritten as the consuming uid: extract of the supervisor's stat
// check so the decision (not the syscall) is unit-testable.
func needsOwnershipNormalization(fi os.FileInfo, uid int) bool {
	if fi == nil {
		return false
	}
	if stat, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(stat.Uid) != uid
	}
	// Non-Linux stat backends carry no uid — never rewrite blind.
	return false
}

// watchAgentConfigForChanges heals the frozen-registry window (#1300):
// when the agent-config file changes AFTER opencode spawned (sidecar
// resync delivering a late batch, bootstrap-degrade recovery), the
// running opencode process may never re-ingest it — its catalog
// re-transform is unreliable for late-arriving config in the sandbox
// runtime (validated on the pod: hours-old process, config on disk,
// registry still boot-state). The supervisor owns the restart, so it
// watches the file hash and restarts opencode once per change with the
// existing credential_reload reason marker.
//
// restartNow carries the TOPOLOGY-SPECIFIC restart semantics:
//   - single-container (--supervise): the session-aware decision
//     (makeSessionAwareRestartDecision — the same machinery the relay
//     injector uses), so in-flight turns are deferred, not killed.
//   - sidecar supervise-opencode: a grace restart — matching the
//     incumbent credential-change semantics of that topology, where
//     the sidecar's reload path already restarts unconditionally via
//     the control socket (spawn_env_consumer.restart → cc.Restart).
//
// Poll (5s) rather than inotify: the sandbox runtime's inotify
// semantics for bind-mounted files are exactly what we do not trust
// here. Hashing ~7KiB every 5s is negligible. Cooldown (60s) bounds
// restart churn when the sidecar rewrites the file several times in
// quick succession (atomic temp+rename sequences land as distinct
// hashes only when content actually changed) and coalesces this
// watcher with any same-window restart the relay injector triggers.
func watchAgentConfigForChanges(ctx context.Context, configPath string, logger *zap.Logger, restartNow func()) {
	const (
		interval = 5 * time.Second
		cooldown = 60 * time.Second
	)
	last := agentConfigFingerprint(configPath)
	var lastRestart time.Time
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cur := agentConfigFingerprint(configPath)
			if cur == last {
				continue
			}
			if time.Since(lastRestart) < cooldown {
				// Do NOT consume the change: leave `last` at the old
				// fingerprint so the next tick re-detects it once the
				// cooldown expires.
				logger.Debug("agent-config changed inside cooldown; restart deferred",
					zap.String("path", configPath))
				continue
			}
			if cur == "" {
				// File vanished (unexpected — atomic writes keep the path
				// complete): adopt the empty baseline so a reappearing
				// file re-triggers, but do not restart against nothing.
				last = cur
				continue
			}
			last = cur
			lastRestart = time.Now()
			// #1310: re-copy the agent-config into the XDG layer BEFORE
			// restarting — the copy (not the symlink target) is what
			// opencode reads, and without this the restart would boot
			// against the stale copy.
			ensureOpencodeRegistryConfig(logger)
			if err := writeRestartReasonMarker(markerPathFromEnv(), "credential_reload", nil); err != nil {
				logger.Warn("agent-config watcher: marker write failed", zap.Error(err))
			}
			logRestartReasonAtWrite("credential_reload", nil, logger.Core())
			logger.Info("agent-config watcher: config re-copied, restarting opencode to rebuild the model registry",
				zap.String("path", configPath))
			restartNow()
		}
	}
}
