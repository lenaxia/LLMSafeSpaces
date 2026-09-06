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

// ensureOpencodeRegistryConfig installs the XDG-layer symlink
// ~/.config/opencode/opencode.json → agent-config (the sidecar-stamped
// file at LLMSAFESPACES_AGENT_CONFIG_PATH). Best-effort, idempotent,
// never blocks boot — but failure is loud because without the link the
// model registry will not admit any platform-delivered provider
// (#1300). Runs before the first opencode spawn.
//
// A real (non-symlink) file at the link path is LEFT ALONE with a
// warning: the user took over the config layer. opencode layers XDG
// files under the env config, so a user file that omits platform
// providers keeps registry behavior broken — the warning names the
// exact consequence rather than silently clobbering user bytes.
func ensureOpencodeRegistryConfig(logger *zap.Logger) string {
	target := agentConfigPathFromEnv()
	dir := opencodeXDGConfigDir()
	link := filepath.Join(dir, "opencode.json")

	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger.Warn("registry config layer: cannot create XDG dir (model registry may not admit providers)",
			zap.String("dir", dir), zap.Error(err))
		return link
	}

	if fi, err := os.Lstat(link); err == nil && fi.Mode()&os.ModeSymlink == 0 {
		logger.Warn("registry config layer: real file exists at XDG config path; leaving it (platform providers may not register)",
			zap.String("path", link),
			zap.String("expectedSymlinkTarget", target))
		return link
	}

	if cur, err := os.Readlink(link); err == nil && cur == target {
		return link // already correct — idempotent no-op
	}

	// Atomic install: temp symlink + rename over the (absent or stale)
	// link. Rename over an existing symlink is atomic; a dangling link
	// left by a previous pod is repointed, never read as a file.
	tmp := link + ".agentd-tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		logger.Warn("registry config layer: symlink create failed (model registry may not admit providers)",
			zap.String("link", link), zap.String("target", target), zap.Error(err))
		return link
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		logger.Warn("registry config layer: symlink install failed (model registry may not admit providers)",
			zap.String("link", link), zap.String("target", target), zap.Error(err))
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
	cur := os.Getuid()
	if stat, ok := fi.Sys().(*syscall.Stat_t); ok && int(stat.Uid) == cur {
		return // already consuming-uid-owned
	}
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

// restarter is the supervisor seam the watcher drives — *managedProcess
// satisfies it; tests inject a recorder.
type restarter interface {
	restartWithGrace(grace time.Duration)
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
// Poll (5s) rather than inotify: the sandbox runtime's inotify
// semantics for bind-mounted files are exactly what we do not trust
// here. Hashing ~7KiB every 5s is negligible. Cooldown (60s) bounds
// restart churn when the sidecar rewrites the file several times in
// quick succession (atomic temp+rename sequences land as distinct
// hashes only when content actually changed).
func watchAgentConfigForChanges(ctx context.Context, proc restarter, configPath string, logger *zap.Logger) {
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
			if err := writeRestartReasonMarker(markerPathFromEnv(), "credential_reload", nil); err != nil {
				logger.Warn("agent-config watcher: marker write failed", zap.Error(err))
			}
			logRestartReasonAtWrite("credential_reload", nil, logger.Core())
			logger.Info("agent-config watcher: restarting opencode to rebuild the model registry",
				zap.String("path", configPath))
			proc.restartWithGrace(5 * time.Second)
		}
	}
}
