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

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// sidecarEnvPassword is the env var the controller wires from the
// password Secret (US-2 keeps the workspace password; the distinct
// agentdPassword key lands with US-3).
const sidecarEnvPassword = "AGENTD_SIDECAR_PASSWORD"

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

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	bgCtx, bgCancel := context.WithCancel(rootCtx)
	var bgWg sync.WaitGroup

	client := &OpenCodeClient{password: password, client: &http.Client{Timeout: 5 * time.Second}}

	// #857 stamp-before-read, sidecar edition: the platform blocks
	// (built-in MCP entry — which embeds the workspace password Basic
	// header — admin prompt, allowed dirs) are stamped onto
	// agent-config.json BEFORE the muxes serve. The kubelet gates the
	// main container on this sidecar's startup probe, so opencode's
	// first config read can only happen after the stamp lands.
	agentConfigPath := envOrDefault("LLMSAFESPACES_AGENT_CONFIG_PATH", agentd.AgentConfigPath)
	agentConfigWriter := ensureBootAgentConfig(agentConfigPath, agentd.AdminPromptPath, agentd.AllowedDirsPath, password)

	deps := buildSidecarDeps(sidecarConfig{
		password:    password,
		adminToken:  adminToken,
		controlAddr: ControlSocketAddr(),
	})
	deps.client = client
	deps.agentConfigWriter = agentConfigWriter

	// Same loop set as single-container mode: SSE tracking, pressure
	// monitor, ops ticker, fillGaps, health watchdog (+reaper loop,
	// which is a no-op — the sidecar spawns no children and never
	// becomes a subreaper).
	startBackgroundLoops(bgCtx, &bgWg, deps)
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
// and the workspace container's cgroup numbers all cross it.
func buildSidecarDeps(cfg sidecarConfig) serverDeps {
	cc := newControlClient(cfg.controlAddr)
	so := &socketOps{cc: cc}
	startedAt := time.Now()
	return serverDeps{
		password:           cfg.password,
		resolvedAdminToken: cfg.adminToken,
		startedAt:          startedAt,
		cache:              &providerCache{},
		sseTracker:         newSessionStatusTracker(),
		pressureMonitor:    so.pressureMonitor(),
		healthCache:        newHealthzCache(),
		gr:                 newGateRecorder(startedAt, agentdGateDurationSeconds, log),
		restarter:          so.restarter(),
		vitals:             so.vitals(fmtAgentAddr()),
		sys:                so.sysMetrics(),
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
