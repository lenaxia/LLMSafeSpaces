// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// serverDeps bundles the runtime collaborators shared across the
// background loops and HTTP servers. agentd constructs it once at boot
// and threads it through the handlers and goroutines that need it,
// mirroring the reloadSecretsDeps pattern.
type serverDeps struct {
	client            *OpenCodeClient
	cache             *providerCache
	sseTracker        *sessionStatusTracker
	pressureMonitor   *memoryPressureMonitor
	healthCache       *healthzCache
	gr                *gateRecorder
	proc              *managedProcess
	password          string
	startedAt         time.Time
	agentConfigWriter *AgentConfigWriter
}

// buildStatuszHandler returns the /v1/statusz HTTP handler, parameterised on
// all runtime dependencies. Extracted from main() so tests can exercise the
// real handler wiring without reimplementing it.
func buildStatuszHandler(
	client *OpenCodeClient,
	cache *providerCache,
	tracker *sessionStatusTracker,
	pressureMon *memoryPressureMonitor,
	startedAt time.Time,
) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		healthy, version, _ := client.IsHealthy(r.Context())
		connected, configured, sessions := cachedState(r.Context(), client, cache, tracker)
		ready := healthy && len(connected) > 0

		activeCnt := 0
		for _, s := range sessions {
			if s.Status == "busy" {
				activeCnt++
			}
		}

		// Context usage: per-session ContextUsed from SSE prompt tokens.
		// Top-level TotalTokens = model context limit (same for all sessions).
		// UsedTokens is not meaningful as an aggregate; set to 0.
		var contextUsage *agentd.ContextUsage
		{
			var modelID string
			for i, s := range sessions {
				sessions[i].ContextUsed = tracker.getPromptTokens(s.ID)
				if modelID == "" && s.Model != "" {
					modelID = s.Model
				}
			}
			contextLimit := client.ModelContextLimit(r.Context(), modelID, "")
			contextUsage = &agentd.ContextUsage{
				UsedTokens:  0,
				TotalTokens: contextLimit,
			}
		}

		// US-44.5: surface memory pressure state.
		pressure, _, _ := pressureMon.snapshot()

		_ = json.NewEncoder(w).Encode(agentd.StatuszResponse{
			Healthy:             healthy,
			Ready:               ready,
			Connected:           connected,
			ProvidersConfigured: configured,
			Sessions:            sessions,
			SessionsActive:      activeCnt,
			SessionsError:       0,
			LastError:           "",
			AgentType:           "opencode",
			AgentVersion:        version,
			UptimeSeconds:       int(time.Since(startedAt).Seconds()),
			Disk:                getDiskUsage(),
			Memory:              getMemoryUsage(),
			CPU:                 getCPUUsage(),
			Context:             contextUsage,
			MemoryPressure:      pressure,
		})
	})
}

// buildReadyzHandler returns the /v1/readyz HTTP handler. Ready requires:
// cache initialized + opencode healthy. Provider connectivity is no
// longer a readiness gate (S18.11): it is surfaced separately via
// WorkspaceConditionProviderReady on the Workspace CRD. Provider info
// is still included in the response body for observability.
//
// S18.10: providers_connected and readyz_first_200 startup gates are
// recorded here on first observation.
func buildReadyzHandler(deps serverDeps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		snap := deps.healthCache.Snapshot()

		connected, configured, _ := cachedState(r.Context(), deps.client, deps.cache, deps.sseTracker)
		ready := snap.Initialized && snap.Healthy

		// S18.10: Record providers_connected gate on first non-empty connected list.
		if len(connected) > 0 {
			deps.gr.MaybeRecord(gateProvidersConnected)
		}

		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(agentd.ReadyzResponse{
			Ready:               ready,
			ProvidersConnected:  connected,
			ProvidersConfigured: configured,
			AgentVersion:        snap.Version,
			AgentType:           "opencode",
			// RelayInjected: true once the relay injector successfully completed.
			// Included in readyz (not statusz) because readyz is cache-based and
			// lightweight, making it safe to call on every ListModels cache miss.
			RelayInjected: deps.agentConfigWriter != nil && deps.agentConfigWriter.hasRelay(),
		})

		// S18.10: Record readyz_first_200 gate on first 200 response.
		if ready {
			deps.gr.MaybeRecord(gateReadyzFirst200)
		}
	})
}

// requireBearerToken wraps an http.Handler so that requests must carry
// `Authorization: Bearer <token>` matching the configured token. When
// the token is empty (env unset), the handler runs unprotected — this
// lets development / kind clusters skip the wiring while production
// gets defense-in-depth.
//
// Closes F1.4.2 (Epic 17 Phase 1): pre-fix /v1/statusz, /v1/readyz,
// and /v1/healthz on the agentd admin port were reachable from any
// pod in the workspace namespace that could route to the workspace
// pod IP. The chart's NetPol (G16) blocks workspace-to-workspace
// ingress, but a misconfigured cluster (NetPol disabled, CNI bug,
// operator opted out) would let any tenant probe another's session
// list. Token auth is the application-layer defense.
func requireBearerToken(token string, h http.Handler) http.Handler {
	if token == "" {
		return h
	}
	expected := "Bearer " + token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != expected {
			w.Header().Set("WWW-Authenticate", `Bearer realm="agentd"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// wireHTTPServers builds the admin (health probes) and user (reload)
// muxes, creates both http.Server instances, starts them, and returns
// them along with the shared error channel. The admin endpoints carry
// agent session metadata and provider-config, so /v1/statusz and
// /v1/readyz are wrapped in requireBearerToken when AGENTD_ADMIN_TOKEN
// is set (F1.4.2). /v1/healthz stays open: it only emits {ok,
// started_at} and the kubelet liveness probe targets it without
// configured headers.
//
// US-22.8: two separate http.Server instances eliminate listener-layer
// head-of-line blocking. Admin port serves health probes (kubelet,
// controller) on a dedicated goroutine pool; user port serves
// reload-secrets and future proxy endpoints independently.
func wireHTTPServers(bgCtx context.Context, bgWg *sync.WaitGroup, deps serverDeps) (adminSrv, userSrv *http.Server, srvErr chan error) {
	adminMux := http.NewServeMux()
	userMux := http.NewServeMux()

	adminToken := os.Getenv("AGENTD_ADMIN_TOKEN")

	adminMux.HandleFunc("/v1/healthz", healthzHandler(deps.startedAt, agentd.ReloadSecretsCachePath))
	adminMux.Handle("/v1/readyz", requireBearerToken(adminToken, buildReadyzHandler(deps)))

	// /v1/statusz is the EXPENSIVE deep-introspection endpoint. It makes
	// multiple synchronous HTTP calls to opencode (IsHealthy,
	// ConnectedProviders, ConfiguredProviderCount, ListSessions) under a
	// mutex. Under SSE load, these calls can take seconds to complete.
	// Consumers: controller deep-status poll (60s) and API status
	// enrichment (infrequent). Performance contract: NO upper bound —
	// callers must use a generous timeout (controller uses 30s). Do NOT
	// use this endpoint for liveness or readiness probes.
	adminMux.Handle("/v1/statusz", requireBearerToken(adminToken,
		buildStatuszHandler(deps.client, deps.cache, deps.sseTracker, deps.pressureMonitor, deps.startedAt)))

	// S18.10: Expose Prometheus metrics on admin port so the cluster-level
	// Prometheus scraper can collect per-pod agentd gate timings.
	adminMux.Handle("/metrics", promhttp.Handler())

	// The session lister probes opencode's /session endpoint to (a) prune
	// stale busy entries from the tracker when opencode dies mid-busy and
	// is respawned (C2a), and (b) decide cold-start behavior when the
	// tracker is empty after an agentd restart (C2b). It closes over the
	// production OpenCodeClient; tests inject a stub.
	liveSessions := func(ctx context.Context) []string {
		sessions, err := deps.client.ListSessions(ctx)
		if err != nil {
			return nil
		}
		ids := make([]string, len(sessions))
		for i, s := range sessions {
			ids[i] = s.ID
		}
		return ids
	}

	userMux.HandleFunc("/v1/reload-secrets", reloadSecretsHandler(loadMaterializeConfig(), reloadSecretsDeps{
		Proc:              deps.proc,
		OpencodePassword:  deps.password,
		Tracker:           deps.sseTracker,
		BgCtx:             bgCtx,
		BgWg:              bgWg,
		Lister:            liveSessions,
		AgentConfigWriter: deps.agentConfigWriter,
	}))
	userMux.HandleFunc("/v1/agent/reload", agentReloadHandler(deps.password, log))

	// Start admin server (health probes) on dedicated port.
	adminSrv = &http.Server{
		Addr:              agentd.AgentdAdminAddr,
		Handler:           adminMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	srvErr = make(chan error, 2)
	go func() {
		log.Info("workspace-agentd admin server starting", zap.String("addr", agentd.AgentdAdminAddr))
		if err := adminSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErr <- fmt.Errorf("admin server: %w", err)
		}
	}()

	// Start user server on the original port.
	log.Info("workspace-agentd user server starting", zap.String("addr", listenAddr))
	userSrv = &http.Server{
		Addr:              listenAddr,
		Handler:           userMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := userSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			srvErr <- fmt.Errorf("user server: %w", err)
		}
	}()

	return adminSrv, userSrv, srvErr
}

// startBackgroundLoops launches the agentd background goroutines:
// SSE session-status subscriber, memory-pressure monitor, periodic ops
// metrics collector, the fillGaps prompt-token backfiller, and the
// eager-refresh health cache. All are tracked on bgWg so shutdown can
// join them.
func startBackgroundLoops(bgCtx context.Context, bgWg *sync.WaitGroup, deps serverDeps) {
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		deps.sseTracker.subscribe(bgCtx, deps.client)
	}()

	// US-44.5: memory pressure monitor checks cgroup usage against the
	// 85% threshold and surfaces the state via statusz.
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		deps.pressureMonitor.run(bgCtx, log)
	}()

	// US-44.8: periodic metrics collection for ops dashboards. Updates
	// memory usage, active sessions, and context token gauges every 60s.
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		wsID := os.Getenv("WORKSPACE_ID")
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-ticker.C:
				if memBytes, err := readCgroupMemoryCurrent(); err == nil {
					pkgOpsMetrics.SetMemoryUsage(wsID, memBytes)
				}
				pkgOpsMetrics.UpdateFromTracker(wsID, deps.sseTracker)
			}
		}
	}()

	fillState := &fillGapsState{}
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		fillGaps(bgCtx, deps.client, deps.sseTracker, func() []agentd.SessionInfo {
			deps.cache.mu.Lock()
			sessions := deps.cache.sessions
			deps.cache.mu.Unlock()
			return sessions
		}, fillState)
	}()

	// US-22.2: Eager-refresh readiness cache. Background goroutine refreshes
	// opencode's IsHealthy every 5s; /v1/readyz reads from this cache without
	// making inline opencode calls.
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		refreshIsHealthyLoop(bgCtx, deps.client, deps.healthCache, log, deps.gr)
	}()
}
