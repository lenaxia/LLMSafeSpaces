// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// sidecar_pi.go — the pi leg of the sidecar's harness branch (S5,
// pi-scoping §6 item 3). AGENTD_HARNESS=pi (wired by the controller
// only on pi workspaces) swaps THREE opencode-shaped sidecar facts for
// their pi equivalents and leaves everything else — the muxes, the
// bus, the upload staging, the control-socket status mirrors — on the
// shared, harness-neutral path:
//
//  1. CONFIG: the boot stamp + the AgentConfigWriter seam use S3's pi
//     writer over the shared pi-agent-dir volume (the sidecar writes,
//     the supervisor's children read — both RW, both containers; see
//     the controller's volume wiring for the topology ruling).
//  2. STATUS: statusz/readyz/healthz derive from the harness-status
//     poller (the supervise-pi control-socket snapshot) instead of the
//     opencode HTTP client + SSE tracker.
//  3. LOOPS: the opencode SSE subscribe / fillGaps / IsHealthy refresh
//     loops are replaced by the poller loop (which also feeds the
//     readiness cache — readyz's Initialized/Healthy contract holds).
//
// opencode sidecars never enter this file: AGENTD_HARNESS absent =
// opencode = the pre-S5 wiring, byte-identical.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/agent/pi"
)

// piAgentDirEnv is the shared-volume coordinate the controller wires
// on BOTH containers (RW emptyDir, Memory medium — config-sized). The
// supervisor's own LLMSAFESPACES_PI_AGENT_DIR names the same mount.
const piAgentDirEnv = "LLMSAFESPACES_PI_AGENT_DIR"

func sidecarPiAgentDir() string {
	if d := os.Getenv(piAgentDirEnv); d != "" {
		return d
	}
	return piAgentDirDefault
}

// ensureBootPiConfig is the pi twin of ensureBootAgentConfig: S3's
// writer over the shared agent dir, with the same unconditional empty
// Apply boot stamp (the incident-2026-08-15 lesson carried verbatim:
// every conditional write path can skip, the boot stamp cannot).
func ensureBootPiConfig(agentDir, adminPromptPath string) *pi.ConfigWriter {
	w := pi.NewConfigWriter(agentDir,
		pi.WithAdminPromptPath(adminPromptPath),
	)
	if _, err := w.Apply(agent.AgentConfigInput{}); err != nil {
		if log != nil {
			log.Warn("boot pi-config normalize failed; config may lack platform blocks until the next write",
				zap.String("dir", agentDir),
				zap.Error(err),
			)
		}
	}
	return w
}

// runPiReadinessLoop is the poller loop plus the readiness-cache leg:
// a successful harness_status poll marks the cache healthy with the
// supervisor's reported pi version; a failed poll counts toward the
// same readinessFailureThreshold the opencode path uses. No watchdog
// restarts here — the supervisor owns child lifecycle (S2's wedge
// ladder); the sidecar escalates, never executes (the D6 rule).
func runPiReadinessLoop(ctx context.Context, poller *harnessStatusPoller, cache *healthzCache, logger *zap.Logger) {
	tick := time.NewTicker(poller.every)
	defer tick.Stop()
	piPollOnce(ctx, poller, cache, logger)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			piPollOnce(ctx, poller, cache, logger)
		}
	}
}

func piPollOnce(ctx context.Context, poller *harnessStatusPoller, cache *healthzCache, logger *zap.Logger) {
	pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	snap, err := fetchHarnessStatus(pollCtx, poller.addr)
	prev := cache.Snapshot()
	if err != nil {
		next := healthzCacheSnapshot{
			Initialized:         true,
			LastRefreshedAt:     time.Now(),
			Version:             prev.Version,
			Healthy:             false,
			ConsecutiveFailures: prev.ConsecutiveFailures + 1,
			LastError:           err.Error(),
		}
		if next.ConsecutiveFailures < readinessFailureThreshold {
			next.Healthy = prev.Healthy
		}
		if !next.Healthy && next.UnhealthyEpisodeStartedAt.IsZero() {
			next.UnhealthyEpisodeStartedAt = time.Now()
		}
		cache.storeResolved(&prev, &next)
		return
	}
	poller.store.store(snap)
	cache.storeResolved(&prev, &healthzCacheSnapshot{
		Initialized:         true,
		LastRefreshedAt:     time.Now(),
		Version:             snap.AgentVersion,
		Healthy:             true,
		ConsecutiveHealthy:  prev.ConsecutiveHealthy + 1,
		ConsecutiveFailures: 0,
	})
}

// buildPiStatuszHandler renders statusz from the harness-status store.
// One consistent snapshot per render (the opencode path's ONE-truth
// rule): sessions, busy ages, and progress all come from the same
// poll's result.
func buildPiStatuszHandler(deps serverDeps) func(w http.ResponseWriter, r *http.Request) {
	sys := deps.sys.orDefaults()
	return func(w http.ResponseWriter, r *http.Request) {
		snap, ok := deps.harnessStatus.snapshot()
		resp := piStatuszResponse(snap, ok, deps.startedAt, sys.memory, sys.cpu, sys.disk)
		resp.LastError = func() string {
			if s := deps.healthCache.Snapshot(); !s.Healthy {
				return s.LastError
			}
			return ""
		}()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// agentTypeName is the harness name statusz/readyz report (the
// AgentType field the frontend and operators switch on).
func (deps serverDeps) agentTypeName() string {
	if deps.harnessName != "" {
		return deps.harnessName
	}
	return "opencode"
}

// piStatusPollerForSidecar wires the poller + store pair for a pi
// sidecar. The addr is the shared-netns control socket (4099).
func piStatusPollerForSidecar() *harnessStatusPoller {
	return newHarnessStatusPoller(ControlSocketAddr(), 5*time.Second)
}

// piSidecarLoops replaces the opencode-shaped background loops (SSE
// subscribe, fillGaps, IsHealthy refresh) with the harness-status
// readiness loop. Pressure monitor, ops metrics, short-id warm, and
// the orphan reaper are harness-neutral and run for both.
func piSidecarLoops(bgCtx context.Context, bgWg *sync.WaitGroup, deps serverDeps, poller *harnessStatusPoller) {
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		runPiReadinessLoop(bgCtx, poller, deps.healthCache, log)
	}()
}

// agentdAgentTypeForEnv resolves the harness name for statusz/readyz
// (AGENTD_HARNESS; absent = opencode — the mixed-fleet encoding).
func agentdAgentTypeForEnv(getenv func(string) string) string {
	return harnessFromEnv(getenv)
}
