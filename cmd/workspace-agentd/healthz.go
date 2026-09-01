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
// (one json.Encode, one small ReadFile via modelResolutionWarnings, and
// a clock read); all observed latency is from json encoding and the
// OS-level HTTP layer, not in-handler logic.
//
// Response shape is agentd.HealthzResponse. Healthy is always true when
// the handler executes (a dead process can't respond, by definition).
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
func healthzHandler(startedAt time.Time, modelWarnPath string, spawnEnvSnapshot func() *agentd.SpawnEnvHealth) http.HandlerFunc {
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
			Healthy:       true,
			Version:       buildVersion,
			CommitSHA:     buildCommit,
			BuildTime:     buildTime,
			UptimeSeconds: int(time.Since(startedAt).Seconds()),
			Delivery:      agentd.DeliveryCapability,
			SpawnEnv:      spawnEnv,
			Warnings:      warnings,
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
