// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// sidecar_mode.go — US-2 (design 0051 §D1/§4a): the `--sidecar` mode.
//
// agentd becomes a native sidecar (uid 2000/gid 1000) holding the policy
// half of the split: muxes, health watchdog, SSE tracking, relay
// injector, credential reload. It does NOT supervise opencode — that is
// `supervise-opencode`, PID 1 of the workspace container. Every process
// interaction crosses the Appendix-A control socket:
//
//   - watchdog restart   → socketRestarter (reason enum + parity grace)
//   - watchdog vitals    → socketVitalsGatherer (dead-listener evidence)
//   - cgroup numbers     → socket metrics (workspace container's cgroup)
//
// Credentials arrive via ENV (the sidecar's own container env, uid-2000
// space — the 0600/0400 uid-1000 files under /sandbox-cfg are unreadable
// cross-uid). Missing or empty credentials are FATAL (D5.2/D5.3
// doctrine: never boot an ungated mux).

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// sidecarEnvPassword is the env var the controller wires from the
// password Secret's `password` key: the workspace password (§D1
// carve-out secret for /v1/mcp + dev-preview, and the sidecar's CLIENT
// credential for opencode-facing calls).
const sidecarEnvPassword = "AGENTD_SIDECAR_PASSWORD"

// sidecarEnvControlPlanePassword is the §D1 agentdPassword (US-3): the
// control-plane Basic secret from the Secret's `agentdPassword` key,
// delivered env-only to the sidecar. Required at sidecar boot — the
// controller's upsert-once (ensurePasswordSecret) guarantees the key
// before any sidecar-enabled pod build (design Q3), so absence is a bug
// state and the D5.2/D5.3 fail-closed doctrine applies.
const sidecarEnvControlPlanePassword = "AGENTD_CONTROL_PLANE_PASSWORD"

// readSidecarControlPlanePasswordFromEnv resolves the §D1 control-plane
// secret. Missing/empty is an error the caller treats as fatal.
func readSidecarControlPlanePasswordFromEnv() (string, error) {
	pw := os.Getenv(sidecarEnvControlPlanePassword)
	if pw == "" {
		return "", errSidecarCredentialMissing(sidecarEnvControlPlanePassword)
	}
	return pw, nil
}

// readSidecarPasswordFromEnv resolves the user-mux Basic secret for
// sidecar mode. Empty or unset is an error the caller treats as fatal.
func readSidecarPasswordFromEnv() (string, error) {
	pw := os.Getenv(sidecarEnvPassword)
	if pw == "" {
		return "", errSidecarCredentialMissing(sidecarEnvPassword)
	}
	return pw, nil
}

// resolveSidecarAdminTokenFromEnv resolves the admin-mux bearer. The
// sidecar's container env is uid-2000 space with no child processes
// inheriting it, so env delivery of the #933 distinct token is safe
// here (the no-env rule exists because opencode passes its env to every
// tool child — the sidecar spawns nothing).
func resolveSidecarAdminTokenFromEnv() (string, error) {
	tok := os.Getenv("AGENTD_ADMIN_TOKEN")
	if tok == "" {
		return "", errSidecarCredentialMissing("AGENTD_ADMIN_TOKEN")
	}
	return tok, nil
}

type sidecarCredentialError struct{ envVar string }

func (e *sidecarCredentialError) Error() string {
	return "sidecar mode: required credential env " + e.envVar + " is empty or unset — refusing to boot an ungated mux (D5.2/D5.3)"
}

func errSidecarCredentialMissing(envVar string) error {
	return &sidecarCredentialError{envVar: envVar}
}

// sidecarConfig is the resolved boot configuration for sidecar mode.
// The §D1 control-plane credential is NOT here: buildSidecarDeps
// resolves it from the env directly (single resolution site).
type sidecarConfig struct {
	password    string
	adminToken  string
	controlAddr string
}

// runSidecarCommand is the `workspace-agentd --sidecar` entry point.
// Mirrors main()'s lifecycle shape (servers, background loops, graceful
// shutdown) minus every child-ownership duty. Exit code communicates
// failure; it never returns normally.
func runSidecarCommand(_ []string) int {
	log = newLogger()
	defer func() { _ = log.Sync() }()

	password, err := readSidecarPasswordFromEnv()
	if err != nil {
		log.Error("FATAL", zap.Error(err))
		return 1
	}
	adminToken, err := resolveSidecarAdminTokenFromEnv()
	if err != nil {
		log.Error("FATAL", zap.Error(err))
		return 1
	}
	// §D1 control-plane credential: fail fast at boot (buildSidecarDeps
	// re-resolves it for the deps; the upsert-once controller path
	// guarantees presence — absence is a bug state, D5.2/D5.3 doctrine).
	if _, err := readSidecarControlPlanePasswordFromEnv(); err != nil {
		log.Error("FATAL", zap.Error(err))
		return 1
	}

	// Credential boot phase (step-1 migration): bootstrap+materialize
	// run HERE, before ensureBootAgentConfig and the muxes — the #857
	// stamp-before-read guarantee rides this sidecar's startup probe.
	// Fail-fast: a non-zero boot phase exits the sidecar so kubelet
	// surfaces CrashLoopBackOff with a reason, never a never-Ready
	// zombie. Env: WORKSPACE_ID + LLMSAFESPACE_API_URL are already in
	// the sidecar's container env (pod builder); the controller points
	// the LLMSAFESPACES_* materialize paths at /sandbox-runtime.
	// US-70.3: the batch file coordinate comes from the same
	// LLMSAFESPACE_BOOTSTRAP_SECRETS_OUT env the resync endpoint reads —
	// boot pull and in-process re-pulls share one coordinate.
	// Runs before any context allocation — it owns the whole process on
	// failure.
	if code := runSidecarBootSecrets(sidecarBootOpts{
		WorkspaceID: os.Getenv("WORKSPACE_ID"),
		APIURL:      os.Getenv("LLMSAFESPACE_API_URL"),
		TokenFile:   bootstrapTokenPathFromEnv(),
		SecretsOut:  bootstrapSecretsOutFromEnv(),
		Stderr:      os.Stderr,
	}); code != 0 {
		log.Error("FATAL: sidecar credential boot phase failed — refusing to serve",
			zap.Int("exit", code))
		return code
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Epic 68 US-68.1: boot scrub, sidecar edition. The sidecar's
	// /workspace mount is read-only, so removals fail per-file and are
	// skipped (best-effort by design); the scrub stays wired so a future
	// writable uploads mount needs no boot-path change.
	scrubUploadsAtBoot(log, uploadsPathFromEnv())

	bgCtx, bgCancel := context.WithCancel(rootCtx)
	var bgWg sync.WaitGroup

	client := &OpenCodeClient{password: password, client: &http.Client{Timeout: 5 * time.Second}}

	// #857 stamp-before-read, sidecar edition: the platform blocks
	// (built-in MCP entry — which embeds the workspace password Basic
	// header — admin prompt, allowed dirs) are stamped onto
	// agent-config.json BEFORE the muxes serve. The kubelet gates the
	// main container on this sidecar's startup probe, so opencode's
	// first config read can only happen after the stamp lands.
	// US-4b: the sidecar's stores live on the consumer-split volumes —
	// the controller wires the env overrides; helpers default to the
	// /sandbox-runtime consts for env-less local runs.
	bootCfgPath, bootPromptPath, bootDirsPath := bootAgentConfigPathsWithEnv()
	agentConfigWriter := ensureBootAgentConfig(bootCfgPath, bootPromptPath, bootDirsPath, password)

	deps := buildSidecarDeps(sidecarConfig{
		password:    password,
		adminToken:  adminToken,
		controlAddr: ControlSocketAddr(),
	})
	deps.client = client
	deps.agentConfigWriter = agentConfigWriter

	// Epic 69 US-69.2: the sidecar runs the session-state authority too —
	// same machinery, boot reseed only (the sidecar is not opencode's
	// parent; the generation signal crosses the control socket and lands
	// with the US-69.3/69.4 surface work).
	sidecarAuthority := newStateAuthority(client, password, deps.controlPlanePassword)
	if sidecarAuthority != nil {
		deps.stateAuthority = sidecarAuthority
		deps.sseTracker.onRawEvent = sidecarAuthority.Ingest
		deps.ledgerInFlight = sidecarAuthority.InFlightDeliveries
	}

	// Same loop set as single-container mode: SSE tracking, pressure
	// monitor, ops ticker, fillGaps, health watchdog (+reaper loop,
	// which is a no-op — the sidecar spawns no children and never
	// becomes a subreaper).
	//
	// US-70.1 (design 0057 R2): the boot-time spawn-env PUSH is GONE —
	// under native-sidecar startup gating it dialed a control socket in
	// a container that could not have started yet ("connection refused",
	// every boot). The supervisor now PULLS the delta from this sidecar's
	// user mux at every spawn (bounded wait + last-good cache, served by
	// /v1/spawn-env); a dead-sidecar boot degrades loudly via the
	// supervisor-status poller below → healthz → CRD.

	// US-70.1: mirror the supervisor's terminal-verified spawn-env state
	// (spawned_rev + degrade reason) into healthz. Pull-only, periodic —
	// healthz itself stays process-only (US-22.1) and reads the cached
	// snapshot, never the socket.
	supervisorStatus := &supervisorStatusStore{}
	deps.spawnEnvSnapshot = supervisorStatus.spawnEnvHealth

	if sidecarAuthority != nil {
		startStateAuthorityReseed(bgCtx, sidecarAuthority, sessionstate.ReseedReasonBoot)
	}
	startBackgroundLoops(bgCtx, &bgWg, deps)
	startSupervisorStatusPoller(bgCtx, &bgWg, newControlClient(ControlSocketAddr()), supervisorStatus)
	maybeStartRelayInjector(rootCtx, bgCtx, &bgWg, deps)

	adminSrv, userSrv, srvErr := wireHTTPServers(bgCtx, &bgWg, deps)

	select {
	case <-rootCtx.Done():
		log.Info("sidecar agentd received shutdown signal")
	case err := <-srvErr:
		log.Error("sidecar agentd server error", zap.Error(err))
	}

	runShutdown(adminSrv, userSrv, bgCancel, &bgWg, nil)
	log.Info("sidecar agentd shutdown complete")
	return 0
}

// buildSidecarDeps assembles the sidecar's serverDeps. The control
// socket client is the single seam to the supervisor: restarts, status,
// and the workspace container's cgroup numbers all cross it. The §D1
// control-plane credential resolves here (single site) so every
// construction path carries it.
func buildSidecarDeps(cfg sidecarConfig) serverDeps {
	cc := newControlClient(cfg.controlAddr)
	so := &socketOps{cc: cc}
	startedAt := time.Now()
	controlPlanePassword, _ := readSidecarControlPlanePasswordFromEnv()
	// US-70.1: the reload path's socket-backed restarter — restart only;
	// the restarted child's spawn pulls the fresh delta from this
	// sidecar's user mux.
	reloadProc := newSocketReloadProc(cc)

	// Boot-race heal (#1244): the supervisor's preSpawn files pull can
	// beat this sidecar's first staging publish (containers start in
	// parallel), leaving the delivered file set empty until the next
	// resync. One idempotent refresh_files shortly after boot closes the
	// window — re-applying an already-applied set is a same-rev no-op.
	go func() {
		for i := 0; i < 5; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := cc.RefreshFiles(ctx)
			cancel()
			if err == nil {
				return
			}
			time.Sleep(2 * time.Second)
		}
	}()
	return serverDeps{
		password:             cfg.password,
		controlPlanePassword: controlPlanePassword,
		resolvedAdminToken:   cfg.adminToken,
		reloadProc:           reloadProc,
		startedAt:            startedAt,
		cache:                &providerCache{},
		sseTracker:           newSessionStatusTracker(),
		pressureMonitor:      so.pressureMonitor(),
		healthCache:          newHealthzCache(),
		gr:                   newGateRecorder(startedAt, agentdGateDurationSeconds, log),
		restarter:            so.restarter(),
		vitals:               so.vitals(fmtAgentAddr()),
		sys:                  so.sysMetrics(),
	}
}

// socketOps bundles the socket-backed implementations.
type socketOps struct {
	cc *controlClient
}

func (o *socketOps) restarter() *socketRestarter { return &socketRestarter{cc: o.cc} }

func (o *socketOps) vitals(agentAddr string) *socketVitalsGatherer {
	return newSocketVitalsGatherer(agentAddr, o.cc)
}

// pressureMonitor returns a monitor whose reads cross the socket: the
// sidecar's own cgroup is the wrong container (0050 finding).
func (o *socketOps) pressureMonitor() *memoryPressureMonitor {
	m := newMemoryPressureMonitor()
	m.readCurrent = o.memCurrent
	m.readMax = o.memMax
	return m
}

// sysMetrics returns statusz memory/cpu sourced from the workspace
// container's cgroup via the socket. Disk stays local (statfs on the
// RO workspace mount is container-independent).
func (o *socketOps) sysMetrics() sysMetricsSource {
	return sysMetricsSource{
		memory: func() *agentd.MemoryUsage {
			m, err := o.cc.Metrics(context.Background())
			if err != nil || m == nil {
				return nil
			}
			if m.MemoryMaxBytes == 0 {
				return nil // unlimited/unreadable — same contract as getMemoryUsage
			}
			return &agentd.MemoryUsage{
				UsedBytes:  m.MemoryCurrentBytes,
				TotalBytes: m.MemoryMaxBytes,
			}
		},
		cpu: func() *agentd.CPUUsage {
			m, err := o.cc.Metrics(context.Background())
			if err != nil || m == nil || m.CPUUsageUsec == 0 {
				return nil
			}
			return &agentd.CPUUsage{
				UsageMicros: m.CPUUsageUsec,
			}
		},
		disk: getDiskUsage,
	}
}

func (o *socketOps) memCurrent() (int64, error) {
	m, err := o.cc.Metrics(context.Background())
	if err != nil {
		return 0, err
	}
	return m.MemoryCurrentBytes, nil
}

func (o *socketOps) memMax() (int64, error) {
	m, err := o.cc.Metrics(context.Background())
	if err != nil {
		return 0, err
	}
	if m.MemoryMaxBytes == 0 {
		// Unlimited: the monitor treats max==0 as no-limit (pressure
		// never fires) — same as readCgroupMemoryMax on "max".
		return 0, nil
	}
	return m.MemoryMaxBytes, nil
}

// socketRestarter adapts the watchdog's narrow restart interface to the
// control socket, carrying the reason enum (A.4 invariant 2) and the
// same 5s grace the in-process path uses.
type socketRestarter struct {
	cc *controlClient
}

func (s *socketRestarter) restart() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = s.cc.Restart(ctx, RestartReasonHealthWatchdog, int(defaultRestartGrace/time.Second))
}

func fmtAgentAddr() string {
	return fmt.Sprintf("127.0.0.1:%d", agentd.AgentPort)
}
