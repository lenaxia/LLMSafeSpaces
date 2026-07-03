// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
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
func healthzHandler(startedAt time.Time, reloadCachePath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agentd.HealthzResponse{
			Healthy:          true,
			Version:          buildVersion,
			UptimeSeconds:    int(time.Since(startedAt).Seconds()),
			UserCredsPresent: hasUserCreds(reloadCachePath),
		})
	}
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
