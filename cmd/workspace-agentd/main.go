// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/version"
)

var (
	// agentAddrAtomic holds the current opencode agent base URL.
	// Tests mutate it via setAgentAddr; production sets it once at
	// startup. atomic.Value gives data-race-free read/write so the
	// race detector doesn't flag concurrent test access.
	agentAddrAtomic atomic.Value
	listenAddr      = agentd.AgentdAddr
)

func init() {
	agentAddrAtomic.Store(fmt.Sprintf("http://localhost:%d", agentd.AgentPort))
}

// getAgentAddr returns the current opencode agent base URL.
func getAgentAddr() string {
	return agentAddrAtomic.Load().(string)
}

var log *zap.Logger

// buildVersion is the workspace-agentd build identifier surfaced via
// /v1/healthz. It reads from pkg/version — the single source of truth —
// which the image build stamps via -ldflags "-X
// github.com/lenaxia/llmsafespaces/pkg/version.Version=$VERSION". Un-stamped
// local builds report "unknown".
//
// This is the agentd build version, NOT opencode's version. See
// HealthzResponse.Version: pre-US-22.1, this field carried opencode's
// /global/health version (which conflated agentd liveness with opencode
// availability — see worklog 0096). Post-US-22.1, the field reports the
// agentd build identifier, which is meaningful for the kubelet probe's
// purpose: "is this agentd binary alive and serving HTTP?".
var buildVersion = version.Version

func main() {
	log = newLogger()
	defer func() { _ = log.Sync() }()

	// Subcommand dispatch. The materialize subcommand reads
	// /sandbox-cfg/secrets.json and applies it via pkg/agentd/secrets, then
	// exits. This replaces the legacy bash secret-loop in
	// runtimes/base/tools/entrypoints/entrypoint-common.sh and consolidates
	// secret materialization in a single, tested code path. See worklog
	// 0078 (Epic 17 G2/G20 remediation).
	if len(os.Args) > 1 && os.Args[1] == "materialize" {
		os.Exit(runMaterializeCommand(os.Args[2:], os.Stdout, os.Stderr))
	}

	// Epic 35 US-35.2: bootstrap subcommand fetches decrypted secrets from
	// the API using a projected SA token. Runs before materialize in the
	// init container. Never blocks pod boot — degrades to empty on failure.
	if len(os.Args) > 1 && os.Args[1] == "bootstrap" {
		os.Exit(runBootstrapCommand(os.Args[2:], os.Stdout, os.Stderr))
	}

	supervise := len(os.Args) > 1 && os.Args[1] == "--supervise"

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	bgCtx, bgCancel := context.WithCancel(rootCtx)
	var bgWg sync.WaitGroup

	password := readAgentPassword()
	client := &OpenCodeClient{password: password, client: &http.Client{Timeout: 5 * time.Second}}

	// US-44.7: surface the reason for the previous opencode restart
	// (if any) and consume the one-shot marker before starting the
	// supervisor. No-op when no marker is present (clean boot).
	logRestartReason(RestartReasonMarkerPath, log.Core())

	// Stamp the platform blocks (built-in MCP server, admin prompt,
	// allowed dirs) onto agent-config.json BEFORE opencode starts, so
	// its first read sees the completed config regardless of which
	// conditional write paths later skip. See boot_config.go.
	agentConfigPath := envOrDefault("LLMSAFESPACES_AGENT_CONFIG_PATH", agentd.AgentConfigPath)
	agentConfigWriter := ensureBootAgentConfig(agentConfigPath, agentd.AdminPromptPath, agentd.AllowedDirsPath, password)

	proc := startManagedProcess(supervise)

	startedAt := time.Now()
	deps := serverDeps{
		client:            client,
		cache:             &providerCache{},
		sseTracker:        newSessionStatusTracker(),
		pressureMonitor:   newMemoryPressureMonitor(),
		healthCache:       newHealthzCache(),
		gr:                newGateRecorder(startedAt, agentdGateDurationSeconds, log),
		proc:              proc,
		password:          password,
		startedAt:         startedAt,
		agentConfigWriter: agentConfigWriter,
	}

	startBackgroundLoops(bgCtx, &bgWg, deps)
	// bgCtx (not rootCtx): the session-aware deferred restart must be
	// canceled by runShutdown's bgCancel so bgWg drains within its 5s wait —
	// same context discipline as the credential-reload path (secrets.go).
	maybeStartRelayInjector(rootCtx, bgCtx, &bgWg, deps)

	adminSrv, userSrv, srvErr := wireHTTPServers(bgCtx, &bgWg, deps)

	select {
	case <-rootCtx.Done():
		log.Info("workspace-agentd received shutdown signal")
	case err := <-srvErr:
		log.Error("workspace-agentd server error", zap.Error(err))
	}

	runShutdown(adminSrv, userSrv, bgCancel, &bgWg, proc)
	log.Info("workspace-agentd shutdown complete")
}

func newLogger() *zap.Logger {
	l, err := zap.NewProduction()
	if err != nil {
		return zap.NewNop()
	}
	return l
}

func readAgentPassword() string {
	pw, err := readAgentPasswordFromPath(agentd.PasswordPath)
	if err != nil {
		// G46: a missing or unreadable password file leaves the
		// workspace silently non-functional — opencode starts without
		// auth and the proxy's basic-auth header comparison fails for
		// every request. Pre-fix this was a Warn and continue, which
		// made the failure invisible in logs. Error + non-zero exit
		// surfaces it as a pod-level CrashLoopBackOff, which is the
		// correct signal — the workspace cannot recover without
		// operator intervention (recreate the workspace, or fix the
		// Secret mount).
		log.Error("FATAL: failed to read password file — workspace cannot start safely",
			zap.String("path", agentd.PasswordPath), zap.Error(err))
		os.Exit(1)
	}
	return pw
}

// readAgentPasswordFromPath reads and trims the password from the given
// path. Extracted from readAgentPassword for testability — the caller
// handles the fatal exit so the test can verify the error return
// without subprocess execution.
func readAgentPasswordFromPath(path string) (string, error) {
	pw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(pw)), nil
}

// startManagedProcess builds and starts the opencode supervisor when
// agentd is invoked with --supervise; returns nil otherwise.
func startManagedProcess(supervise bool) *managedProcess {
	if !supervise {
		return nil
	}
	proc := &managedProcess{}
	proc.start()
	return proc
}

// maybeStartRelayInjector launches the Epic 42 Phase-2 relay injection
// when INFERENCE_RELAY_BASEURL is set and the opencode supervisor is
// running. After opencode is healthy, fetch the live free model list
// and rewrite the config to use the self-hosted relay fleet. Runs at
// most once per pod lifetime. Skipped if the user has a personal
// opencode API key (paying Zen subscriber).
func maybeStartRelayInjector(rootCtx, bgCtx context.Context, bgWg *sync.WaitGroup, deps serverDeps) {
	relayURL := os.Getenv("INFERENCE_RELAY_BASEURL")
	if relayURL == "" || deps.proc == nil {
		return
	}
	xdgData := os.Getenv("XDG_DATA_HOME")
	homeDir, _ := os.UserHomeDir()
	authJSONPath := filepath.Join(homeDir, ".local", "opencode", "auth.json")
	if xdgData != "" {
		authJSONPath = filepath.Join(xdgData, "opencode", "auth.json")
	}
	liveSessions := liveSessionsLister(deps)
	startRelayInjector(rootCtx, relayInjectorConfig{
		RelayURL:          relayURL,
		OpenCodeBaseURL:   getAgentAddr(),
		OpenCodePassword:  deps.password,
		AgentConfigPath:   envOrDefault("LLMSAFESPACES_AGENT_CONFIG_PATH", agentd.AgentConfigPath),
		AuthJSONPath:      authJSONPath,
		AgentConfigWriter: deps.agentConfigWriter,
		HealthCheck:       func() bool { snap := deps.healthCache.Snapshot(); return snap.Initialized && snap.Healthy },
		KillOpenCode:      relayKillFunc(bgCtx, bgWg, deps.proc, deps.sseTracker, liveSessions),
	})
}

// liveSessionsLister returns the opencode /session probe used to prune stale
// busy entries from the session tracker. Shared shape with the closure in
// wireHTTPServers (server.go); kept as a function so both sites stay in sync.
func liveSessionsLister(deps serverDeps) sessionLister {
	return func(ctx context.Context) []string {
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
}

// relayKillFunc builds the relay injector's opencode-restart trigger.
//
// Session-aware kill (US-44.2 semantics): a direct proc.restart() here killed
// any session waiting on a pending question/permission — the in-memory input
// queue dies with the process while SQLite keeps toolState:"running",
// permanently sticking the session. The restart is routed through the same
// deferral the credential-reload path uses: restart now if no session is
// busy, else poll until idle (bounded by defaultMaxDefer).
func relayKillFunc(bgCtx context.Context, bgWg *sync.WaitGroup, proc restartableProcess, tracker *sessionStatusTracker, lister sessionLister) func() {
	if proc == nil {
		return func() {}
	}
	return func() {
		makeSessionAwareRestartDecision(bgCtx, proc, tracker, restartIdleCheckInterval, defaultMaxDefer, lister, bgWg) //nolint:contextcheck // bgCtx is the agentd background lifecycle context — the deferred goroutine must outlive the relay injector and be canceled at shutdown
	}
}

// runShutdown gracefully stops both HTTP servers, cancels the
// background context, waits (up to 5s) for background goroutines to
// exit, then stops the opencode supervisor.
func runShutdown(adminSrv, userSrv *http.Server, bgCancel context.CancelFunc, bgWg *sync.WaitGroup, proc *managedProcess) {
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer shutdownCancel()

	var srvWg sync.WaitGroup
	srvWg.Add(2)
	go func() {
		defer srvWg.Done()
		if err := adminSrv.Shutdown(shutdownCtx); err != nil {
			log.Warn("workspace-agentd admin server shutdown error", zap.Error(err))
		}
	}()
	go func() {
		defer srvWg.Done()
		if err := userSrv.Shutdown(shutdownCtx); err != nil {
			log.Warn("workspace-agentd user server shutdown error", zap.Error(err))
		}
	}()
	srvWg.Wait()

	bgCancel()

	bgWaitDone := make(chan struct{})
	go func() {
		bgWg.Wait()
		close(bgWaitDone)
	}()
	select {
	case <-bgWaitDone:
	case <-time.After(5 * time.Second):
		log.Warn("workspace-agentd background goroutines did not exit within 5s")
	}

	if proc != nil {
		proc.stop()
	}
}
