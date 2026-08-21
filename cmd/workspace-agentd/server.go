// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// serverDeps bundles the runtime collaborators shared across the
// background loops and HTTP servers. agentd constructs it once at boot
// and threads it through the handlers and goroutines that need it,
// mirroring the reloadSecretsDeps pattern.
type serverDeps struct {
	client          *OpenCodeClient
	cache           *providerCache
	sseTracker      *sessionStatusTracker
	pressureMonitor *memoryPressureMonitor
	healthCache     *healthzCache
	gr              *gateRecorder
	proc            *managedProcess
	password        string
	// resolvedAdminToken is the admin-mux bearer, resolved ONCE at boot
	// (main) — the mux wrap and every consumer read this field, closing
	// the file/env re-read TOCTOU window between gate and use.
	resolvedAdminToken string
	startedAt          time.Time
	agentConfigWriter  agent.AgentConfigWriter

	// restarter, when non-nil, is the health-watchdog's restart path.
	// Single-container mode leaves it nil (the loop falls back to
	// deps.proc); sidecar mode wires the control-socket restarter.
	restarter healthWatchdogRestarter
	// vitals, when non-nil, is the watchdog corroboration probe.
	// Single-container mode leaves it nil (built from deps.proc);
	// sidecar mode wires the socket gatherer. NEVER leave both nil in
	// sidecar mode — nil vitals restores pre-#892 kill-on-timeout
	// semantics (the 2026-08-15 kill-churn class).
	vitals vitalsGatherer
	// sys supplies statusz's system metrics. Nil → local cgroup reads
	// (single-container mode). Sidecar mode wires socket-backed reads —
	// the sidecar's own cgroup is the wrong container (0050 finding).
	sys sysMetricsSource
	// controlPlanePassword is the design-0051 §D1 agentdPassword (US-3):
	// accepted on control-plane routes alongside the workspace password
	// (D6.1 mixed-generation window). Empty in single-container mode.
	controlPlanePassword string
	// reloadProc is the reload-secrets path's restarter for sidecar mode
	// (US-4a): pushes the fresh secrets-env delta over the socket, then
	// requests the credential_reload restart. Used only when proc is nil
	// (single-container mode keeps the in-process managedProcess).
	reloadProc restartableProcess
}

// sysMetricsSource is the statusz system-metrics seam: typed functions
// so sidecar mode can substitute socket-backed reads for the local
// cgroup ones without globals.
type sysMetricsSource struct {
	memory func() *agentd.MemoryUsage
	cpu    func() *agentd.CPUUsage
	disk   func() *agentd.DiskUsage
}

// defaultSysMetrics reads the local cgroupfs — the single-container
// behavior, unchanged.
func defaultSysMetrics() sysMetricsSource {
	return sysMetricsSource{memory: getMemoryUsage, cpu: getCPUUsage, disk: getDiskUsage}
}

func (s sysMetricsSource) orDefaults() sysMetricsSource {
	out := s
	if out.memory == nil {
		out.memory = getMemoryUsage
	}
	if out.cpu == nil {
		out.cpu = getCPUUsage
	}
	if out.disk == nil {
		out.disk = getDiskUsage
	}
	return out
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
	modelWarnPath string,
	sys sysMetricsSource,
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

		sys := sys.orDefaults()
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
			Disk:                sys.disk(),
			Memory:              sys.memory(),
			CPU:                 sys.cpu(),
			RelayFreeModels:     RelayFreeModelsState(),
			Context:             contextUsage,
			MemoryPressure:      pressure,
			Warnings:            modelResolutionWarnings(modelWarnPath),
		})
	})
}

// buildReadyzHandler returns the /v1/readyz HTTP handler.
//
// Readiness semantics (design 0050 D4, #892): agentd up AND opencode's
// port accepting TCP connections — NOT opencode responsiveness. Under
// CPU starvation (incident 2026-08-15/16) an HTTP round-trip to
// /global/health times out while opencode is alive and progressing;
// readiness flapping on that signal dropped slow-but-alive pods for no
// benefit, and the startup probe built on it killed containers mid-boot
// (kubelet Killing events, restart churn). A TCP connect is answered by
// the kernel for a listening socket regardless of the application's
// event-loop health: refused = booting or dead, accepted = can take
// traffic. It costs microseconds and involves no event-loop work, so it
// is starvation-immune by construction.
//
// readyChecker, when non-nil, supplies that kernel-level answer. nil
// (tests, partial wiring) preserves legacy semantics.
//
// Providers are reported from the provider cache's last-known values —
// readiness never triggers a synchronous opencode fetch (the previous
// cachedState call could block for seconds under load; see the statusz
// comment below for that hazard).
//
// S18.10: providers_connected and readyz_first_200 startup gates are
// recorded here on first observation.
func buildReadyzHandler(deps serverDeps, readyChecker func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		snap := deps.healthCache.Snapshot()

		ready := snap.Initialized && snap.Healthy
		if readyChecker != nil {
			ready = snap.Initialized && readyChecker()
		}

		connected, configured := deps.cache.lastKnown()
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
			RelayInjected: deps.agentConfigWriter != nil && deps.agentConfigWriter.HasRelay(),
		})

		// S18.10: Record readyz_first_200 gate on first 200 response.
		if ready {
			deps.gr.MaybeRecord(gateReadyzFirst200)
		}
	})
}

// opencodeTCPReady is the production readiness answer (design 0050 D4):
// can the kernel complete a TCP handshake with opencode's port? The
// kernel services a listening socket's backlog irrespective of the
// application's event-loop health, so this distinguishes booting/dead
// (refused) from alive-but-slow (accepted) without involving either
// event loop. Timeout is generous because nothing normal bounds it —
// localhost handshakes complete in microseconds.
//
// addr is a raw host:port — production wires
// fmt.Sprintf("127.0.0.1:%d", agentd.AgentPort), NOT getAgentAddr():
// the agent addr is a URL (http://localhost:4096) and net.Dial("tcp",
// "http://...") fails on address form regardless of listeners (review
// round 1 on #895: the URL-form dial made readyz 503 forever — a
// deterministic startup-probe kill loop). Parametrized so the production
// form is testable against an arbitrary listener.
func opencodeTCPReady(addr string) func() bool {
	return func() bool {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second) //nolint:noctx // liveness probe, not a request
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
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

	// #887 D5.1: the token resolved once at boot (main) — no re-read
	// here (TOCTOU closed, review note on #934).
	adminToken := deps.resolvedAdminToken

	adminMux.HandleFunc("/v1/healthz", healthzHandler(deps.startedAt, agentd.ReloadSecretsCachePath, agentd.ModelResolutionWarningPath))
	adminMux.Handle("/v1/readyz", requireBearerToken(adminToken,
		buildReadyzHandler(deps, opencodeTCPReady(fmt.Sprintf("127.0.0.1:%d", agentd.AgentPort)))))

	// /v1/statusz is the EXPENSIVE deep-introspection endpoint. It makes
	// multiple synchronous HTTP calls to opencode (IsHealthy,
	// ConnectedProviders, ConfiguredProviderCount, ListSessions) under a
	// mutex. Under SSE load, these calls can take seconds to complete.
	// Consumers: controller deep-status poll (60s) and API status
	// enrichment (infrequent). Performance contract: NO upper bound —
	// callers must use a generous timeout (controller uses 30s). Do NOT
	// use this endpoint for liveness or readiness probes.
	adminMux.Handle("/v1/statusz", requireBearerToken(adminToken,
		buildStatuszHandler(deps.client, deps.cache, deps.sseTracker, deps.pressureMonitor, deps.startedAt, agentd.ModelResolutionWarningPath, deps.sys)))

	// S18.10: Expose Prometheus metrics on admin port so the cluster-level
	// Prometheus scraper can collect per-pod agentd gate timings.
	adminMux.Handle("/metrics", promhttp.Handler())

	// The session lister probes opencode's /session endpoint to prune
	// stale busy entries from the tracker when opencode dies mid-busy and
	// is respawned (C2a). Used only by pruneFromLister; the (b) cold-start
	// empty-tracker probing use was removed by design 0045 Change 4 (see
	// design/0045_2026-07-06_boot-time-user-dek-delivery.md). Closes over
	// the production OpenCodeClient; tests inject a stub.
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

	// Typed-nil guard: `restartableProcess(deps.proc)` with a nil
	// *managedProcess yields a NON-nil interface (the classic trap) and
	// the reload would silently no-op in sidecar mode. Compare the
	// CONCRETE pointer, then convert.
	var reloadProc restartableProcess
	if deps.proc != nil {
		reloadProc = deps.proc
	} else {
		// Sidecar mode: files apply in the sidecar; the restart crosses
		// the socket WITH the fresh secrets delta (US-4a — before this,
		// a nil proc meant files applied but opencode never restarted).
		reloadProc = deps.reloadProc
	}
	userMux.HandleFunc("/v1/reload-secrets", reloadSecretsHandler(loadMaterializeConfig(), reloadSecretsDeps{
		Proc:                 reloadProc,
		OpencodePassword:     deps.password,
		ControlPlanePassword: deps.controlPlanePassword,
		Tracker:              deps.sseTracker,
		BgCtx:                bgCtx,
		BgWg:                 bgWg,
		Lister:               liveSessions,
		AgentConfigWriter:    deps.agentConfigWriter,
	}))
	userMux.HandleFunc("/v1/agent/reload", agentReloadHandler(log, deps.password, deps.controlPlanePassword))

	// Epic 64: Workflow node execution endpoints. These are called by
	// the API server's workflow engine to dispatch individual nodes.
	userMux.HandleFunc("/v1/workflow/node/execute", workflowExecuteHandler(deps.password, deps.controlPlanePassword))
	userMux.HandleFunc("/v1/workflow/node/cancel", workflowCancelHandler(deps.password, deps.controlPlanePassword))
	userMux.HandleFunc("/v1/workflow/session/delete", workflowDeleteSessionHandler(deps.password, deps.controlPlanePassword))
	userMux.HandleFunc("/v1/mcp", mcpHandler(deps.password))

	// Epic 66: Dev Preview — authenticated HTTP/WS tunnel to localhost dev
	// servers. The API server proxies to this endpoint, which forwards to
	// localhost:<port>. Port denylist + Host rewrite per PREVIEW-CONTRACT.md.
	userMux.HandleFunc("/v1/dev-preview/", devPreviewHandler(deps.password))

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
	// memory usage, active sessions, and context token token gauges every
	// 60s. Memory reads cross the control socket in sidecar mode (the
	// sidecar's own cgroup is the wrong container — 0050 finding).
	memCurrent := readCgroupMemoryCurrent
	if deps.sys.memory != nil {
		memCurrent = func() (int64, error) {
			if m := deps.sys.memory(); m != nil {
				return m.UsedBytes, nil
			}
			return 0, fmt.Errorf("no memory metrics available")
		}
	}
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
				if memBytes, err := memCurrent(); err == nil {
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
	//
	// The refresh loop doubles as the health-watchdog; it receives the
	// starvation-corroboration probe (watchdog_vitals.go) so would-fire
	// moments check TCP + CPU evidence before killing opencode. Incident
	// 2026-08-15: six watchdog kills of a healthy-but-throttled opencode.
	//
	// Restarter/vitals seams (US-2): sidecar mode wires the control-
	// socket implementations; single-container mode keeps the in-process
	// ones built from deps.proc.
	restarter := deps.restarter
	if restarter == nil {
		restarter = deps.proc
	}
	var vitals vitalsGatherer
	if deps.vitals != nil {
		vitals = deps.vitals
	} else if deps.proc != nil {
		vitals = buildVitalsGatherer(deps.proc)
	}
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		refreshIsHealthyLoop(bgCtx, deps.client, deps.healthCache, log, deps.gr, restarter, &busySessionChecker{tracker: deps.sseTracker}, vitals)
	}()

	// #904: orphan zombie reaper. agentd is PID 1 (and a subreaper),
	// so descendants orphaned mid-execution reparent here and would
	// otherwise accumulate as permanent zombies — the Go runtime reaps
	// only children its own os/exec waiters block on.
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		pkgOrphanReaper.run(bgCtx)
	}()
}

// buildVitalsGatherer constructs the production vitals probe for the
// health-watchdog (watchdog_vitals.go). The childStartedAt wiring is the
// respawn boot grace: a refused dial on a young pid classifies RESPAWN,
// never HUNG (review round 1 on #898 — the boot window was the surviving
// kill path of the incident's churn loop).
func buildVitalsGatherer(proc *managedProcess) *procVitalsGatherer {
	return newProcVitalsGatherer(
		fmt.Sprintf("127.0.0.1:%d", agentd.AgentPort),
		proc.pid,
		proc.childStartedAt,
	)
}
