// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

// RestartReasonMarkerPath is the PVC-backed path where agentd writes a
// marker file recording WHY opencode was restarted. The file persists
// across pod restarts so the next boot can surface the reason to the
// operator. Written to /workspace (PVC subPath: workspace). The OOM-
// specific marker that previously lived alongside this file was removed
// (worklog 371 H5) — it had zero read-side consumers and the reason="oom"
// entry in this marker subsumes its useful information.
const RestartReasonMarkerPath = "/workspace/.opencode-restart-reason"

// SidecarRestartMarkerPath is defined in pkg/agentd (types.go) — the
// controller stamps the same string into both containers' env. Markers
// under it are 0640 (cross-uid, shared group).
const SidecarRestartMarkerPath = agentd.SidecarRestartMarkerPath

// markerPathFromEnv resolves the active marker path: the
// LLMSAFESPACES_RESTART_MARKER_PATH env (set on both containers by the
// controller when agentdSidecar is enabled) overrides the single-
// container default. Env unset → today's behavior, byte-identical.
func markerPathFromEnv() string {
	if p := os.Getenv("LLMSAFESPACES_RESTART_MARKER_PATH"); p != "" {
		return p
	}
	return RestartReasonMarkerPath
}

// restartReasonStaleThreshold is how old a marker may be before the
// boot-time reader treats it as "stale" (likely unrelated to this boot,
// e.g. a crash hours ago surfaced by an unrelated node drain). Stale
// markers are logged at Debug with an attribution caveat instead of Info.
const restartReasonStaleThreshold = 10 * time.Minute

// Restart reason constants. These are used as metric labels and marker
// file values, so they must be consistent across all call sites.
const (
	// RestartReasonHealthWatchdog is recorded when the health-watchdog
	// detects opencode is hung and triggers a restart.
	RestartReasonHealthWatchdog = "health_watchdog"
)

// restartReason is the on-disk JSON shape of the restart-reason marker.
// The shape is exactly {reason, timestamp, secretNames} per the US-44.7
// spec. SecretNames is omitted from the file when empty.
type restartReason struct {
	Reason      string   `json:"reason"`
	Timestamp   string   `json:"timestamp"`
	SecretNames []string `json:"secretNames,omitempty"`
}

// writeRestartReasonMarker writes a JSON marker file recording the reason
// opencode is about to be restarted. Creates the parent directory
// (MkdirAll 0750) and writes the file (0600).
//
// Callers that want real-time visibility should follow a successful write
// with logRestartReasonAtWrite; the on-disk marker is the persistent
// counterpart consumed by logRestartReason on the next pod boot.
//
// M3 (worklog 371) known limitation: the marker records "a restart was
// REQUESTED", not "a restart COMPLETED". When the session-aware restart
// defers (secrets.go makeSessionAwareRestartDecision) and the pod dies
// before the deferred restart fires (e.g. node drain, OOM kill of agentd
// itself), the next boot logs a restart-reason that did not actually
// occur on the previous run. This is accepted because:
//   - The marker is written at DECISION time, which is when the credential
//     change became relevant — operationally the right attribution.
//   - The 10-minute stale threshold (restartReasonStaleThreshold) partially
//     mitigates: a marker older than 10min at boot is logged at Debug with
//     an "may be unrelated to this boot" caveat.
//   - The real-time log (logRestartReasonAtWrite) is the primary surface;
//     the boot-time log is secondary.
//
// Moving the marker write to restart-completion time would lose it entirely
// if the pod died mid-restart (the worst time to lose attribution), so
// decision-time is the safer choice.
func writeRestartReasonMarker(path, reason string, secretNames []string) error {
	marker := restartReason{
		Reason:      reason,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		SecretNames: secretNames,
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("marshal restart-reason marker: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("create marker dir %s: %w", dir, err)
	}
	// 0640 (US-2): in sidecar mode the writers straddle uids (sidecar
	// 2000, supervisor 1000) with the pod's shared group 1000; in
	// single-container mode the group bit is inert.
	if err := os.WriteFile(path, data, 0640); err != nil { //nolint:gosec // G306: design 0051 — cross-uid marker (sidecar 2000 + supervisor 1000 writers, shared gid 1000)
		// Cross-uid rotation hole (2026-10-01): the shared marker path
		// sits in the STICKY /sandbox-runtime dir, and 0640 grants the
		// other writer group-READ, never write — so whichever uid wrote
		// the marker last owns it until the pod dies, and every write
		// from the other uid fails EACCES (the sticky bit also blocks
		// the unlink/rename that would otherwise replace it). Fall back
		// to a per-uid sibling (path + ".uid<N>"): the boot reader
		// resolves the NEWEST marker across both names, so attribution
		// survives while each writer stays inside its own file.
		fallback := fmt.Sprintf("%s.uid%d", path, os.Getuid())
		if ferr := os.WriteFile(fallback, data, 0640); ferr != nil { //nolint:gosec // G306: same cross-uid marker contract as the primary path
			return fmt.Errorf("write restart-reason marker %s (fallback %s): %w (fallback: %v)", path, fallback, err, ferr)
		}
	}
	return nil
}

// restartMarkerCandidates returns every marker file that could hold the
// freshest reason: the primary path plus any per-uid fallback siblings
// written when the primary was owned by the other container's uid.
func restartMarkerCandidates(path string) []string {
	candidates := []string{}
	if _, err := os.Stat(path); err == nil {
		candidates = append(candidates, path)
	}
	if uidFiles, err := filepath.Glob(path + ".uid*"); err == nil {
		candidates = append(candidates, uidFiles...)
	}
	return candidates
}

// newestRestartMarker picks the candidate with the most recent modtime —
// the other uid's fallback can be newer than a stale primary, and the
// freshest reason is the truthful attribution.
func newestRestartMarker(candidates []string) (string, bool) {
	newest, newestMod := "", int64(-1)
	for _, c := range candidates {
		fi, err := os.Stat(c)
		if err != nil || fi.IsDir() {
			continue
		}
		if fi.ModTime().UnixNano() > newestMod {
			newest, newestMod = c, fi.ModTime().UnixNano()
		}
	}
	return newest, newest != ""
}

// logRestartReasonAtWrite is the PRIMARY logging path: emit a real-time
// log line at the moment a restart is scheduled. This runs in-pod (in the
// supervisor/reload-handler goroutine), giving immediate visibility into
// why the restart is happening without waiting for the next pod boot.
//
// Level: Warn for oom (the most severe, action-required reason); Info for
// all other reasons (credential changes and crashes are expected, handled
// states).
//
// secretNames is included as a field only when non-nil/non-empty (so a
// crash/oom reason does not carry a meaningless empty array).
//
// The core parameter is injected (rather than using the package-global
// log) so tests can capture output via zaptest/observer.
func logRestartReasonAtWrite(reason string, secretNames []string, core zapcore.Core) {
	fields := []zap.Field{zap.String("reason", reason)}
	if len(secretNames) > 0 {
		fields = append(fields, zap.Strings("secretNames", secretNames))
	}
	logger := zap.New(core).With(fields...)
	if reason == "oom" {
		logger.Warn("opencode restart scheduled")
		return
	}
	logger.Info("opencode restart scheduled")
}

// readRestartReasonMarker reads and unmarshals the restart-reason marker.
// Returns (reason, true) on success. A missing file returns (zero, false)
// silently. A corrupt or unreadable file returns (zero, false) with a
// warning logged via the injected core — the marker must never fail the
// boot. When both the primary and per-uid fallback markers exist (the
// two-writer cross-uid reality of sidecar mode), the NEWEST wins.
func readRestartReasonMarker(path string, core zapcore.Core) (restartReason, bool) {
	logger := zap.New(core)
	// Resolve the freshest marker across the primary + fallback names;
	// remember which path won so failure logs name the real file.
	resolved := path
	if newest, ok := newestRestartMarker(restartMarkerCandidates(path)); ok {
		resolved = newest
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("restart-reason marker: failed to read",
				zap.String("path", resolved), zap.Error(err))
		}
		return restartReason{}, false
	}
	var r restartReason
	if err := json.Unmarshal(data, &r); err != nil {
		logger.Warn("restart-reason marker: corrupt JSON, ignoring",
			zap.String("path", resolved), zap.Error(err))
		return restartReason{}, false
	}
	return r, true
}

// logRestartReason is the boot-time reader: read the marker left by the
// previous run, log the reason, then delete the file (one-shot — the
// reason is consumed on first boot). A missing marker is a silent no-op.
// Deletion errors are ignored: a stale marker only causes a duplicate log
// line on the next boot, which is harmless.
//
// FRESH vs STALE: because main() runs once per agentd PROCESS (per pod
// boot), an in-pod supervisor respawn (crash/oom/secrets) does NOT re-run
// this function — the real-time log for those comes from
// logRestartReasonAtWrite at write time. This boot-time log is the
// SECONDARY surface. A marker older than restartReasonStaleThreshold is
// treated as stale (the pod may be booting for an unrelated reason, e.g.
// a node drain days after a crash) and logged at Debug with an attribution
// caveat instead of Info, so stale markers do not pollute the Info stream
// with misleading attribution.
//
// The core parameter is injected so tests can assert on emitted fields.
func logRestartReason(markerPath string, core zapcore.Core) {
	// Consume the NEWEST marker (primary or per-uid fallback) and sweep
	// any stale siblings from the other uid so they cannot re-surface
	// as stale attributions on later boots.
	resolved := markerPath
	if newest, ok := newestRestartMarker(restartMarkerCandidates(markerPath)); ok {
		resolved = newest
	}
	r, ok := readRestartReasonMarker(markerPath, core)
	if !ok {
		return
	}
	defer func() {
		for _, c := range restartMarkerCandidates(markerPath) {
			_ = os.Remove(c)
		}
		_ = os.Remove(resolved)
	}()

	logger := zap.New(core).With(
		zap.String("reason", r.Reason),
		zap.Strings("secretNames", r.SecretNames),
		zap.String("timestamp", r.Timestamp),
	)
	if isStaleRestartReason(r.Timestamp) {
		logger.Debug("stale restart-reason marker from previous run (may be unrelated to this boot)")
		return
	}
	logger.Info("opencode restarted")
}

// isStaleRestartReason returns true if the marker timestamp is older than
// restartReasonStaleThreshold relative to now, or if the timestamp cannot
// be parsed. The safe fallback for an unparseable timestamp is stale
// (never misattribute a fresh restart to a marker with unknown age).
func isStaleRestartReason(timestamp string) bool {
	ts, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return true
	}
	return time.Since(ts) > restartReasonStaleThreshold
}

// classifySecretRestartReason maps a credential batch to the restart
// reason that should be recorded. api-key changes take precedence over
// env-secret changes (a mixed batch is recorded as api_key_changed).
// Returns ("", nil) when the batch contains no restart-triggering secret
// type, so callers can skip both the marker write and the restart.
//
// For env-secrets the env var name (Metadata["var_name"]) is preferred
// over the secret Name in SecretNames — the var name is what appears in
// opencode's environment and is the directly actionable identifier for an
// operator diagnosing the restart. The secret Name is used as a fallback
// when var_name is absent, and for api-key entries (which have no
// var_name).
func classifySecretRestartReason(batch []secrets.Secret) (reason string, secretNames []string) {
	hasAPIKey := false
	hasEnvSecret := false
	for _, s := range batch {
		switch s.Type {
		case "api-key":
			hasAPIKey = true
			secretNames = append(secretNames, s.Name)
		case "env-secret":
			hasEnvSecret = true
			if vn := s.Metadata["var_name"]; vn != "" {
				secretNames = append(secretNames, vn)
			} else {
				secretNames = append(secretNames, s.Name)
			}
		}
	}
	switch {
	case hasAPIKey:
		return "api_key_changed", secretNames
	case hasEnvSecret:
		return "env_secrets_changed", secretNames
	default:
		return "", nil
	}
}
