// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
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

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/redact"
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

// buildCommit and buildTime carry the rest of the build identity
// (pkg/version). A bare Version string cannot distinguish a release tag
// from a devel hash build stamped with the same VERSION arg — incident
// 2026-08-15, where identifying the deployed code required binary
// disassembly. Resolved via accessor funcs (not direct var reads) so the
// buildinfo fallback in pkg/version runs.
var buildCommit = func() string { return version.Commit() }()
var buildTime = version.BuildTime

func main() {
	log = newLogger()
	defer func() { _ = log.Sync() }()

	// Subcommand dispatch. The materialize subcommand reads
	// /sandbox-cfg/secrets.json and applies it via pkg/agentd/secrets, then
	// exits. This replaces the legacy bash secret-loop in
	// runtimes/base/tools/entrypoints/entrypoint-common.sh and consolidates
	// secret materialization in a single, tested code path. See worklog
	// 0078 (Epic 17 G2/G20 remediation).
	// Design 0051 sidecar migration step 1: `init-fs` subcommand — the
	// uid-1000 PVC filesystem prep (dirs, hardened symlink farm,
	// password/admin-token installs, free-models copy) absorbed from the
	// runtime-image init containers into this digest-pinned artifact.
	if len(os.Args) > 1 && os.Args[1] == "init-fs" {
		os.Exit(runInitFSCommand(os.Args[2:], os.Stderr))
	}

	// Design 0051 US-1: same-uid supervisor mode. Same image, new role —
	// the Phase-2 sidecar split (US-2) keeps this entry point; it becomes
	// PID 1 of the workspace container and serves the Appendix-A control
	// socket on 127.0.0.1:4099. Supervisor scope invariant (0051 D1):
	// plumbing only.
	if len(os.Args) > 1 && os.Args[1] == "supervise-opencode" {
		os.Exit(runSuperviseOpencodeCommand(os.Args[2:]))
	}

	// Design 0051 US-2: native-sidecar mode. The pod splits into the
	// sidecar (this branch: policy, credentials, muxes) and a
	// supervise-opencode PID 1 in the workspace container. Dispatch
	// order matters: --sidecar shares the flag-parsing tail below, so
	// it must exit before it.
	if len(os.Args) > 1 && os.Args[1] == "--sidecar" {
		os.Exit(runSidecarCommand(os.Args[2:]))
	}

	if len(os.Args) > 1 && os.Args[1] == "materialize" {
		os.Exit(runMaterializeCommand(os.Args[2:], os.Stdout, os.Stderr))
	}

	// Epic 35 US-35.2: bootstrap subcommand fetches decrypted secrets from
	// the API using a projected SA token. Runs before materialize in the
	// init container. Never blocks pod boot — degrades to empty on failure.
	if len(os.Args) > 1 && os.Args[1] == "bootstrap" {
		os.Exit(runBootstrapCommand(os.Args[2:], os.Stdout, os.Stderr))
	}

	// Design 0053 — platform overlay delivery: redact folds into
	// the agentd binary as a subcommand. The standalone cmd/redact
	// is deleted; the supervisor provides a /sandbox-runtime/bin/redact
	// PATH wrapper so the documented "some-command | redact" UX is
	// preserved with zero bytes of a second executable in the
	// trusted artifact.
	if len(os.Args) > 1 && os.Args[1] == "redact" {
		os.Exit(runRedactCommand(os.Args[2:]))
	}

	// Design 0053 S3: the pod execs this binary directly (the baked
	// entrypoint's verify_and_select_agentd is deleted). Same
	// fail-closed self-verify as supervise-opencode — exit 81 keeps the
	// controller's AgentdVerificationFailed detection contract. Runs
	// before ANY work; no-op when the overlay marker is unset (dev).
	if err := runSupervisorSelfVerify("/proc/self/exe"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = os.WriteFile("/dev/termination-log", []byte(err.Error()), 0o644) //nolint:gosec // best-effort, mirrors runSuperviseOpencodeCommand
		os.Exit(supervisorExitVerifyFailed)
	}

	supervise := len(os.Args) > 1 && os.Args[1] == "--supervise"

	// #904: agentd is the container's PID 1 (or a subreaper when run
	// under another init), so orphaned descendants reparent here. The
	// reaper loop is started with the other background loops; this
	// prctl only widens the reparenting set when PID 1 is not us.
	if supervise {
		_ = becomeSubreaper()
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	bgCtx, bgCancel := context.WithCancel(rootCtx)
	var bgWg sync.WaitGroup

	password := readAgentPassword()
	bootAgentPassword = password
	client := &OpenCodeClient{password: password, client: &http.Client{Timeout: 5 * time.Second}}

	// #887 D5.2: refuse to boot without an admin-mux credential (unset
	// historically DISABLED the bearer gate — fail open). D5.1 TOCTOU
	// note: resolved ONCE here; mux wrap + health probe read this value.
	bootAdminToken, bootErr := resolveAdminTokenForBoot()
	if bootErr != nil {
		log.Error("FATAL: admin token required — refusing to start with an un-gated admin mux", zap.Error(bootErr))
		os.Exit(1)
	}

	// US-44.7: surface the reason for the previous opencode restart
	// (if any) and consume the one-shot marker before starting the
	// supervisor. No-op when no marker is present (clean boot).
	logRestartReason(markerPathFromEnv(), log.Core())

	// Epic 68 US-68.1: boot scrub — uploads interrupted by a crash leave
	// .tmp residue; the atomic-or-absent contract (D3) removes them once
	// at startup. Best-effort: a failed scrub never blocks boot.
	scrubUploadsAtBoot(log, uploadsPathFromEnv())

	// Design 0053 S2: install the redact PATH wrapper before any opencode
	// spawn can inherit the child env. Best-effort (see redact_wrapper.go);
	// unreachable for the subcommands dispatched above and for --sidecar
	// (which exits earlier — the wrapper belongs to uid-1000 space, not
	// the sidecar container's mounts).
	ensureRedactWrapper(log)

	// Stamp the platform blocks (built-in MCP server, admin prompt,
	// allowed dirs) onto agent-config.json BEFORE opencode starts, so
	// its first read sees the completed config regardless of which
	// conditional write paths later skip. See boot_config.go.
	// US-4b: all three store coordinates honor the LLMSAFESPACES_*_PATH
	// env overrides (sidecar-mode relocations); unset → the consts,
	// byte-identical single-container behavior.
	bootCfgPath, bootPromptPath, bootDirsPath := bootAgentConfigPathsWithEnv()
	agentConfigWriter := ensureBootAgentConfig(bootCfgPath, bootPromptPath, bootDirsPath, password)

	// The tracker must exist before the supervisor starts: its
	// generation-change hook (design 0050 D2) clears orphaned busy flags
	// at every child start.
	sseTracker := newSessionStatusTracker()

	// Epic 69 US-69.2: the session-state authority. S1 shadow — additive
	// machinery only; a non-bootable durable cursor degrades loudly
	// (sessionstate_wiring.go). The authority receives every raw SSE
	// payload, owns seq + reseed + the authenticated ABI surface on the
	// user mux.
	controlPlanePassword, _ := readSidecarControlPlanePasswordFromEnv()
	stateAuthority := newStateAuthority(client, password, controlPlanePassword)
	if stateAuthority != nil {
		sseTracker.onRawEvent = stateAuthority.Ingest
	}

	proc := startManagedProcess(supervise, sseTracker, stateAuthority)
	if stateAuthority != nil {
		startStateAuthorityReseed(bgCtx, stateAuthority, sessionstate.ReseedReasonBoot)
		// US-69.12: the stall watchdog + metric refresh (seq stall, ledger
		// funnel, promotion stall; wake failures escalate via alerts).
		go runSessionStateWatchdog(bgCtx, os.Getenv("WORKSPACE_ID"), stateAuthority, time.Minute)
	}

	startedAt := time.Now()
	var ledgerInFlight func() int64
	if stateAuthority != nil {
		ledgerInFlight = stateAuthority.InFlightDeliveries
	}
	deps := serverDeps{
		client:             client,
		cache:              &providerCache{},
		sseTracker:         sseTracker,
		stateAuthority:     stateAuthority,
		ledgerInFlight:     ledgerInFlight,
		pressureMonitor:    newMemoryPressureMonitor(),
		healthCache:        newHealthzCache(),
		gr:                 newGateRecorder(startedAt, agentdGateDurationSeconds, log),
		proc:               proc,
		password:           password,
		resolvedAdminToken: bootAdminToken,
		startedAt:          startedAt,
		agentConfigWriter:  agentConfigWriter,
	}
	// Nil guard: bare `workspace-agentd` (server-only, no --supervise)
	// runs without a managed process — every deps.proc consumer already
	// tolerates nil (server.go typed-nil guards, maybeStartRelayInjector);
	// this assignment was the one unconditional deref and panicked.
	if proc != nil {
		proc.adminToken = bootAdminToken
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
	trimmed := strings.TrimSpace(string(pw))
	if trimmed == "" {
		// #887 D5.3: a readable-but-empty password would arm the
		// guessable Basic credential "b3BlbmNvZGU6" on every gated
		// user-mux endpoint. Fail boot instead (same class as G46).
		return "", fmt.Errorf("password file %s is empty", path)
	}
	return trimmed, nil
}

// runRedactCommand implements the `redact` subcommand (design 0053).
// Folds cmd/redact into the agentd binary; the standalone cmd/redact
// is deleted. Reads stdin, applies the redact pattern set from
// --config (default /sandbox-cfg/redact-patterns.json), writes to
// stdout. Exit 1 on any failure.
func runRedactCommand(args []string) int {
	log = newLogger()
	defer func() { _ = log.Sync() }()

	fs := flag.NewFlagSet("redact", flag.ExitOnError)
	configPath := fs.String("config", "/sandbox-cfg/redact-patterns.json", "path to extra patterns JSON file")
	if err := fs.Parse(args); err != nil {
		// ExitOnError already exits on parse failure; this is the
		// unreachable defensive return for errcheck/completeness.
		return 1
	}

	r, err := redact.NewRedactorFromFile(*configPath)
	if err != nil {
		log.Error("redact: failed to load patterns", zap.Error(err))
		return 1
	}

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		log.Error("redact: failed to read stdin", zap.Error(err))
		return 1
	}

	result, err := r.Redact(string(input))
	if err != nil {
		log.Error("redact: failed", zap.Error(err))
		return 1
	}

	if _, err := fmt.Fprint(os.Stdout, result); err != nil {
		log.Error("redact: failed to write stdout", zap.Error(err))
		return 1
	}
	return 0
}

// startManagedProcess builds and starts the opencode supervisor when
// agentd is invoked with --supervise; returns nil otherwise. The
// tracker's generation hook is wired here so every child start (first
// boot and each restart/crash recovery) clears orphaned busy flags
// (design 0050 D2). Epic 69 US-69.2: a child start is ALSO the
// authoritative generation-change signal for the session-state authority
// (agentd is opencode's parent — design 0055 A9); the reseed runs async
// so the supervisor path never blocks on store reads.
func startManagedProcess(supervise bool, sseTracker *sessionStatusTracker, authority *sessionstate.Authority) *managedProcess {
	if !supervise {
		return nil
	}
	proc := &managedProcess{}
	if sseTracker != nil {
		if authority != nil {
			a := authority
			proc.onChildStarted = func() {
				sseTracker.onOpencodeGenerationStart()
				go func() {
					if err := a.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange); err != nil {
						log.Warn("sessionstate: generation-change reseed failed (will retry on next generation)", zap.Error(err))
					}
				}()
			}
		} else {
			proc.onChildStarted = sseTracker.onOpencodeGenerationStart
		}
	}
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
		AgentConfigPath:   agentConfigPathFromEnv(),
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
