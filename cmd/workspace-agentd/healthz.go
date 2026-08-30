// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	secretpkg "github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

// healthzHandler returns the http.HandlerFunc serving GET /v1/healthz.
//
// US-22.1 contract: process-only liveness. The handler MUST NOT make any
// HTTP calls to opencode. If this handler executes, the workspace-agentd
// process is alive and able to respond to HTTP — which is exactly the
// signal kubelet's liveness probe needs.
//
// Pre-US-22.1, the handler called client.IsHealthy() (which HTTP-GETs
// opencode's /global/health). When opencode was busy under SSE load,
// IsHealthy timed out, kubelet's liveness probe failed repeatedly, and
// after FailureThreshold=6 the kubelet killed the pod even though
// agentd itself was healthy. Worklog 0096 documented the failure mode;
// this implementation eliminates it by removing the opencode dependency
// from the liveness path entirely.
//
// Performance contract: p99 < 100ms. Implementation is allocation-light
// (one json.Encode, one os.Stat + a small ReadFile via hasUserCreds,
// and a clock read); all observed latency is from json encoding and
// the OS-level HTTP layer, not from in-handler logic. hasUserCreds
// reads a tmpfs file whose size is bounded by the user's secret count
// (typically < 10KB); on tmpfs the read is ~microseconds.
//
// Response shape is agentd.HealthzResponse. Healthy is always true when
// the handler executes (a dead process can't respond, by definition).
// UserCredsPresent (worklog 0591) reports whether agentd has user-DEK
// content materialized on disk, so the API's workspace watcher can
// decide whether to fire a background auto-push after pod recreation.
// A hasUserCreds error surfaces as UserCredsPresent=false and does NOT
// affect Healthy — the field is observability, not liveness.
//
// reloadCachePath is the path to agentd's last-reload-secrets.json.
// Production wires this to agentd.ReloadSecretsCachePath; tests pass
// a t.TempDir path for isolation.
//
// modelWarnPath is the path to the model-resolution warning marker
// (agentd.ModelResolutionWarningPath). When present, the persisted default
// model could not be resolved to a provider at materialize time; the
// warning is surfaced so the controller can relay it into the AgentHealthy
// condition message the user sees. Absent or corrupt marker → no warnings;
// liveness is never affected.
//
// spawnEnvSnapshot (US-70.1, design 0057 I10/I13), when non-nil, supplies
// the supervisor's terminal-verified spawn-env state for the spawnEnv
// field: the revision the child actually spawned with, plus a
// machine-readable degrade reason when delivery is incomplete. Cached
// state only — the handler still performs no I/O (US-22.1 contract); a
// degrade surfaces as the spawnEnv field AND a `degraded:<reason>`
// warning the controller relays into the AgentHealthy condition, never
// as Healthy=false (a secrets degrade must not cascade to pod-kill).
func healthzHandler(startedAt time.Time, reloadCachePath, modelWarnPath string, spawnEnvSnapshot func() *agentd.SpawnEnvHealth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		warnings := modelResolutionWarnings(modelWarnPath)
		var spawnEnv *agentd.SpawnEnvHealth
		if spawnEnvSnapshot != nil {
			spawnEnv = spawnEnvSnapshot()
			if w := spawnEnvWarning(spawnEnv); w != "" {
				warnings = append(warnings, w)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agentd.HealthzResponse{
			Healthy:          true,
			Version:          buildVersion,
			CommitSHA:        buildCommit,
			BuildTime:        buildTime,
			UptimeSeconds:    int(time.Since(startedAt).Seconds()),
			UserCredsPresent: hasUserCreds(reloadCachePath),
			SpawnEnv:         spawnEnv,
			Warnings:         warnings,
		})
	}
}

// modelResolutionWarnings reads the model-resolution warning marker and
// renders the user-facing warning strings. Absent or corrupt marker → nil
// (degrades to no warning; the next materialize rewrites the marker).
func modelResolutionWarnings(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var marker struct {
		DefaultModel string `json:"defaultModel"`
	}
	if json.Unmarshal(data, &marker) != nil || marker.DefaultModel == "" {
		return nil
	}
	return []string{fmt.Sprintf("default model %q unavailable — using the agent default model", marker.DefaultModel)}
}

// hasUserCreds reports whether the given last-reload-secrets.json
// cache file exists AND parses AND contains at least one entry. Every
// entry in the cache represents user-DEK content that was previously
// materialized by a successful reload push — the cache is written
// only by reloadSecretsHandler after a Materialize() succeeds. So
// non-empty cache == "agentd has user-DEK content materialized."
//
// Semantics of each result:
//   - absent file → false (fresh boot, no push yet).
//   - empty batch []  → false (last push was a user unbinding
//     everything; no user-DEK content lives here).
//   - non-empty batch → true.
//   - read error or unparseable JSON → false (fail safe: the API's
//     next push will overwrite the corrupt cache; treating corrupt
//     as true would suppress the recovery push).
//
// Intentionally does NOT distinguish user-owned vs server-owned
// llm-provider entries. The cache only ever contains push-delivered
// entries; server-KEK-only content flows through /sandbox-cfg/secrets.json
// (the "base" merge), never through this cache. So any cache entry
// implies a live push happened, which implies user-DEK content (if
// any exists) was delivered.
func hasUserCreds(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		// Absent file (os.IsNotExist) or any read error: safe-default
		// to false. The API's next push (triggered by the watcher's
		// UserCredsPresent=false observation) will re-materialize and
		// re-cache. Treating "can't read" as true would silently
		// suppress the recovery push.
		return false
	}
	var batch []secretpkg.Secret
	if err := json.Unmarshal(data, &batch); err != nil {
		// Corrupt cache: same reasoning as read error. Return false so
		// the API's push overwrites the corrupt file.
		return false
	}
	return len(batch) > 0
}
