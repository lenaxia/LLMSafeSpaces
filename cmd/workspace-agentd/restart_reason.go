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
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write restart-reason marker %s: %w", path, err)
	}
	return nil
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
// boot.
func readRestartReasonMarker(path string, core zapcore.Core) (restartReason, bool) {
	logger := zap.New(core)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("restart-reason marker: failed to read",
				zap.String("path", path), zap.Error(err))
		}
		return restartReason{}, false
	}
	var r restartReason
	if err := json.Unmarshal(data, &r); err != nil {
		logger.Warn("restart-reason marker: corrupt JSON, ignoring",
			zap.String("path", path), zap.Error(err))
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
	r, ok := readRestartReasonMarker(markerPath, core)
	if !ok {
		return
	}
	defer func() { _ = os.Remove(markerPath) }()

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
