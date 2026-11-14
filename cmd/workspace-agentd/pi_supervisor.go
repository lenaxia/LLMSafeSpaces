// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pi_supervisor.go — the `supervise-pi` subcommand: the in-pod
// supervisor driver for the pi second harness (story S2; scoping
// report §1.2-§1.4, §2.3 item 4). Where supervise-opencode spawns ONE
// multi-session HTTP server, supervise-pi owns the §1.4 process-model
// policy: zero or more `pi --mode rpc` children speaking JSONL on
// stdio, their event streams folded into the statusz parity surface.
//
// Scope for S2 (the recorded ruling, docs/operator/pi-supervisor.md):
// this process is self-contained plumbing — verify, spawn, pipes,
// health derivation, shutdown. The control socket and HTTP statusz
// wiring are opencode-shaped and land with S5's harness-selection
// plumbing; until then the parity surface is Snapshot(), consumed by
// tests here. Nothing else grows in this file.
//
// Health semantics (#1632 agentz contract, moved into the supervisor
// per scoping §1.3): the health loop polls get_state/get_session_stats
// on every live child (corroboration + the model/context facts the
// event stream does not carry) and restarts a child ONLY on mechanical
// wedge evidence (stdin not draining — the child stopped reading
// commands). Busy-age (OldestBusySeconds et al.) is SURFACED for
// upstream escalation, never auto-killed — the D6 rule: "the API
// escalates to the owner, never executes". A restart is a fresh
// generation: the busy episode dies with the dead child
// (piStateAggregator.GenerationStart), so the remedy is visible in the
// same snapshot that would have shown the wedge.
//
// Shutdown (rpc.md, commits #11752/#11812): stdin EOF disposes the
// runtime and WAITS FOR PENDING WORK (an in-flight turn) before
// exiting; "a stdin EOF or a single signal does not interrupt it; a
// second signal exits at once" — the first SIGTERM/SIGINT runs the
// graceful ladder per child (EOF → grace → SIGTERM → SIGKILL), a
// second signal collapses every ladder to immediate SIGKILL and exits.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

const (
	envPiSessionDir    = "LLMSAFESPACES_PI_SESSION_DIR"
	envPiAgentDir      = "LLMSAFESPACES_PI_AGENT_DIR"
	envPiPolicy        = "LLMSAFESPACES_PI_POLICY"
	envPiPollInterval  = "LLMSAFESPACES_PI_POLL_INTERVAL_SECONDS"
	envPiShutdownGrace = "LLMSAFESPACES_PI_SHUTDOWN_GRACE_SECONDS"
	// envPiAttachOnBoot is the bring-up/test knob: comma-separated
	// session ids to Attach at boot (S5's HTTP surface replaces it as
	// the attach driver; the knob stays for cluster bring-up and the
	// exec-level suite).
	envPiAttachOnBoot = "LLMSAFESPACES_PI_ATTACH_ON_BOOT"

	// piSessionDirDefault sits on the workspace PVC (scoping §5.3) so
	// sessions survive container restart; the supervisor creates it.
	piSessionDirDefault = "/workspace/pi-sessions"

	// piAgentDirDefault hosts PI_CODING_AGENT_DIR state (settings,
	// models.json, mcp.json — S3's config writer populates it) on the
	// writable PVC tree, readOnlyRootFilesystem-safe.
	piAgentDirDefault = "/workspace/.local/pi"

	// piPolicyPerSession / piPolicySingle are the §1.4 policy switch's
	// two legal values (the interface lives in pi_policy.go).
	piPolicyPerSession = "per-session"
	piPolicySingle     = "single"

	piDefaultPollInterval  = 5 * time.Second
	piDefaultShutdownGrace = 15 * time.Second
)

// runSupervisePiCommand is the `workspace-agentd supervise-pi` entry
// point. It never returns normally; the exit code communicates failure.
func runSupervisePiCommand(_ []string) int {
	log := newLogger()
	defer func() { _ = log.Sync() }()

	// Fail-closed self-verify first — this is the same agentd binary
	// serving as PID 1; exit 81 keeps the controller's
	// AgentdVerificationFailed detection contract identical to
	// supervise-opencode's.
	if err := runSupervisorSelfVerify("/proc/self/exe"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = writeTerminationLog(err.Error())
		return verifyExitCode(err)
	}

	// The pi overlay verify (exit 85/86 on tamper — pi_overlay.go).
	// No-op unless PI_IMAGE_VOLUME=1; the binary resolution below then
	// fails loudly for an unmounted tree (pi has no PATH-lookup legacy:
	// pins are mandatory, scoping §2.3).
	verifyPiOverlayAtBoot()

	sup, err := newPiSupervisorFromEnv(log)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		_ = writeTerminationLog(err.Error())
		return 1
	}

	// PID 1 duties, same as supervise-opencode (#904/#908): reparent
	// orphans to this process and run the reap loop, or crashed
	// harness grandchildren accumulate as <defunct> forever (the
	// 2026-10-08 incident).
	if err := becomeSubreaper(); err != nil {
		log.Warn("supervise-pi: subreaper setup failed (continuing)", zap.Error(err))
	}
	rootCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()
	go pkgOrphanReaper.run(rootCtx)

	// The repeat-signal channel rides BESIDE NotifyContext: ctx feeds
	// the health loop, while sigCh — consumed exactly once per signal —
	// drives the shutdown ladder. The FIRST receive leaves the loop;
	// shutdown() then watches sigCh for a genuine SECOND signal (the
	// buffered first is consumed here, never double-counted).
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// S5: the design-0051 control socket (the sidecar's poller contract)
	// and the harness-port HTTP bridge (the S4 Transport's in-pod end).
	// Both bind before run() so a fast sidecar poll never races a
	// missing listener; both close on every exit path.
	ctrl, err := newControlSocketServer(ControlSocketAddr(), &piSupervisorProc{sup: sup})
	if err != nil {
		log.Error("supervise-pi: control socket bind failed", zap.Error(err))
		return 1
	}
	ctrl.harnessStatus = sup
	go ctrl.serve()

	bridge := newPiBridge(sup, readAgentPassword())
	harnessLn, err := net.Listen("tcp", agentd.AgentHarnessAddr)
	if err != nil {
		_ = ctrl.close()
		log.Error("supervise-pi: harness-port bind failed", zap.Error(err))
		return 1
	}
	go func() {
		if err := bridge.serve(harnessLn); err != nil && err != http.ErrServerClosed {
			log.Error("supervise-pi: bridge serve failed", zap.Error(err))
		}
	}()
	defer func() {
		bridge.close()
		_ = ctrl.close()
	}()

	runErr := sup.run(rootCtx, sigCh)
	// #1808 belt: flush dirty PVC pages (pi's session JSONL appends
	// included) while the RW mount is still ours.
	flushWorkspaceWritesLogged()
	if runErr != nil {
		_ = writeTerminationLog(runErr.Error())
		return 1
	}
	return 0
}

// piSupervisor is the driver core: the policy, its aggregator, and the
// health loop. Constructed from env by the subcommand; constructed
// directly by tests.
type piSupervisor struct {
	log       *zap.Logger
	policy    piProcessPolicy
	agg       *piStateAggregator
	// cfg is the spawn contract (binary/dir/env) — S5's bridge reads
	// it for the ephemeral control child; the policies keep their own
	// copies for the session children.
	cfg       piSpawnConfig
	pollEvery time.Duration
	// pollTimeout bounds ONE health-poll command against ONE child. pi
	// queues RPC responses behind in-flight agent work, so a busy
	// child's get_state legitimately outruns a short timeout — the
	// event pump stays primary; a timed-out poll is a no-op, never a
	// wedge verdict.
	pollTimeout   time.Duration
	shutdownGrace time.Duration
	// agentVersion is the pinned binary's --version (probed once at
	// construction, non-fatal on failure) — surfaced through
	// HarnessStatus into statusz's AgentVersion (the RC-A fleet-
	// currency field; empty = unreported).
	agentVersion string
}

// newPiSupervisorFromEnv resolves the spawn contract + policy from the
// environment. The binary path comes from the overlay resolution
// (LLMSAFESPACES_PI_BINARY or /pi/pi); a missing binary with no marker
// is a config error (the overlay did not land), not tampering.
func newPiSupervisorFromEnv(log *zap.Logger) (*piSupervisor, error) {
	bin := piOverlayBinaryPath(os.Getenv(envPiBinary))
	if !piBinaryPresent(bin) {
		return nil, fmt.Errorf("supervise-pi: pi binary %q not found — is the pi overlay mounted? (PI_IMAGE_VOLUME=%q)",
			bin, os.Getenv(envPiImageVolume))
	}

	sessionDir := piEnvDefault(envPiSessionDir, piSessionDirDefault)
	if err := os.MkdirAll(sessionDir, 0o750); err != nil {
		return nil, fmt.Errorf("supervise-pi: session dir %s: %w", sessionDir, err)
	}
	agentDir := piEnvDefault(envPiAgentDir, piAgentDirDefault)
	if err := os.MkdirAll(agentDir, 0o750); err != nil {
		return nil, fmt.Errorf("supervise-pi: agent dir %s: %w", agentDir, err)
	}

	cfg := piSpawnConfig{
		Binary:     bin,
		AgentDir:   agentDir,
		SessionDir: sessionDir,
		// Offline mirrors PI_OFFLINE=1 semantics explicitly (scoping
		// §8 R4: egress-restricted pods must not do startup catalog
		// refreshes; the flag is wired, not inherited-by-accident).
		Offline: os.Getenv("PI_OFFLINE") == "1",
	}

	agg := newPiStateAggregator()
	policyName := piEnvDefault(envPiPolicy, piPolicyPerSession)
	var policy piProcessPolicy
	switch policyName {
	case piPolicyPerSession:
		policy = newProcessPerSessionPolicy(cfg, log, agg)
	case piPolicySingle:
		policy = newSingleProcessPolicy(cfg, log, agg)
	default:
		return nil, fmt.Errorf("supervise-pi: unknown %s=%q (legal: %s | %s)",
			envPiPolicy, policyName, piPolicyPerSession, piPolicySingle)
	}

	pollEvery := piEnvDuration(log, envPiPollInterval, piDefaultPollInterval)
	grace := piEnvDuration(log, envPiShutdownGrace, piDefaultShutdownGrace)
	return &piSupervisor{
		log:           log.With(zap.String("component", "pi_supervisor")),
		policy:        policy,
		agg:           agg,
		cfg:           cfg,
		pollEvery:     pollEvery,
		pollTimeout:   pollEvery,
		shutdownGrace: grace,
		agentVersion:  probePiVersion(bin, log),
	}, nil
}

// probePiVersion runs `<bin> --version` once, bounded, non-fatal: a
// version probe failure must never block the supervisor (statusz then
// reports an empty AgentVersion — unreported, not wrong).
func probePiVersion(bin string, log *zap.Logger) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		if log != nil {
			log.Warn("supervise-pi: pi --version probe failed (AgentVersion stays unreported)", zap.Error(err))
		}
		return ""
	}
	return strings.TrimSpace(string(out))
}

// HarnessStatus is the in-process harness-driver implementation (S5,
// harness_driver.go): the aggregator's parity snapshot with the boot
// version. Served on the control socket (method harness_status) for
// the sidecar's poller and read directly by the bridge's statusz.
func (sup *piSupervisor) HarnessStatus() harnessStatusSnapshot {
	snap := sup.agg.Snapshot(time.Now())
	snap.AgentVersion = sup.agentVersion
	return snap
}

// piSupervisorProc adapts the pi supervisor onto the control socket's
// supervised-process surface (design 0051 Appendix A) so the SIDECAR's
// existing status poller keeps its contract on a pi pod. Every method
// maps honestly: pi is process-per-session (no single child pid), and
// the pi credential flow is FILE-delivered (models.json via the S3
// ConfigWriter — the opencode spawn-env push/pull contract does not
// apply). Nothing here claims a delivery that did not happen.
type piSupervisorProc struct{ sup *piSupervisor }

func (p *piSupervisorProc) State() (pid int, state string, restarts int, lastRestartAt time.Time) {
	// Multi-child supervisor: no single child pid to report. State is
	// the supervisor loop itself (serving until shutdown); per-child
	// restart attribution rides S7's validation-epic scope, recorded
	// here so status consumers read 0 as "not attributed", matching
	// the mixed-fleet semantics of a pre-#1776 supervisor.
	return 0, "running", 0, time.Time{}
}

func (p *piSupervisorProc) Restart(reason string, graceSeconds int) (restarted, inProgress bool) {
	// The opencode single-process restart path has no pi analogue: a
	// credential change applies through the ConfigWriter seam
	// (restartRequired per source — pi hot-reloads !command/env-keyed
	// models.json at request time); a blanket child restart would cut
	// in-flight turns for nothing. (false,false) reports honestly:
	// nothing restarted.
	_ = reason
	_ = graceSeconds
	return false, false
}

func (p *piSupervisorProc) SetSpawnEnv(env map[string]string) {
	// Accepted for protocol compatibility, deliberately NOT injected:
	// pi children take their credential surface from the agent-dir
	// files (S3), never from a spawn env delta. See SpawnEnvState.
	_ = env
}

func (p *piSupervisorProc) SpawnEnvState() spawnEnvStateReport {
	// Zero report = no spawn-env contract claimed. The sidecar's
	// healthz mirror reads this as "unreported" (the pre-US-70.1
	// mixed-fleet posture), never as a delivered revision.
	return spawnEnvStateReport{}
}

func (p *piSupervisorProc) RefreshFiles() (filesRev string, filesReason string) {
	// The file-class manifest consumer on a pi pod is the CONFIG
	// WRITER's apply path (agentd sidecar), not the supervisor.
	return "", ""
}

// piEnvDefault reads env with a default; empty means the default.
func piEnvDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// piEnvDuration parses a seconds-valued env knob; malformed values log
// and fall back (a supervisor must boot on bad env, not crash-loop).
func piEnvDuration(log *zap.Logger, name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	secs, err := strconv.ParseFloat(v, 64)
	if err != nil || secs <= 0 {
		if log != nil {
			log.Warn("supervise-pi: ignoring malformed env knob", zap.String("name", name), zap.String("value", v))
		}
		return def
	}
	return time.Duration(secs * float64(time.Second))
}

// run drives the supervisor until ctx ends (first signal) or a second
// signal arrives (immediate exit). It reconciles the session dir into
// the aggregator on every pass so persisted-but-unattached sessions
// keep rendering (idle, no live truth — by design, the recorded
// ruling), runs the health pass, and snapshots transitions for the
// log. Returns an error only for unrecoverable setup states.
func (sup *piSupervisor) run(ctx context.Context, sigCh <-chan os.Signal) error {
	sup.log.Info("supervise-pi: driver up",
		zap.Duration("poll_every", sup.pollEvery),
		zap.Duration("shutdown_grace", sup.shutdownGrace))

	// Boot attach (env knob): bring-up/tests drive children without
	// the S5 HTTP surface. Failures are loud but not fatal — a bad
	// session id must never wedge the supervisor loop.
	for _, sid := range strings.Split(os.Getenv(envPiAttachOnBoot), ",") {
		if sid = strings.TrimSpace(sid); sid == "" {
			continue
		}
		if _, err := sup.policy.Attach(ctx, sid); err != nil {
			sup.log.Error("supervise-pi: boot attach failed",
				zap.String("session", sid), zap.Error(err))
		}
	}

	ticker := time.NewTicker(sup.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-sigCh:
			// The triggering signal is consumed HERE — shutdown's
			// repeat watch starts empty, so only a genuine second
			// signal collapses the ladders. A FRESH context on
			// purpose: rootCtx is already canceled by the signal,
			// and the ladder must outlive it (contextcheck's
			// inherit-the-cancel would abort every rung instantly).
			//nolint:contextcheck // deliberate fresh ladder context
			return sup.shutdown(context.Background(), sup.shutdownGrace, sigCh)
		case <-ticker.C:
			sup.healthPass(ctx)
		}
	}
}

// shutdown runs the graceful ladder across the policy's children.
// A second signal (sigCh receives while ladders are in flight)
// collapses to immediate SIGKILL — rpc.md's "a second signal exits at
// once".
func (sup *piSupervisor) shutdown(ctx context.Context, grace time.Duration, sigCh <-chan os.Signal) error {
	sup.log.Info("supervise-pi: shutdown signal; running rpc.md ladder",
		zap.Duration("grace", grace))
	//nolint:contextcheck // the ladder context is fresh on purpose (see run)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.policy.Shutdown(ctx) }()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	case <-sigCh:
		sup.log.Warn("supervise-pi: second signal — immediate exit (pending work abandoned)")
		cancel()
		<-done // ladders collapse: EOF→TERM→KILL with no waits
		return nil
	}
}

// healthPass is one #1632-semantics pass: reconcile the session-dir
// truth into the aggregator, then poll every live child. Restart is
// reserved for mechanical wedge (stdin not draining); busy-age is
// surfaced, never executed upon.
func (sup *piSupervisor) healthPass(ctx context.Context) {
	sup.policy.ReconcileDir()
	pollCtx, cancel := context.WithTimeout(ctx, sup.pollEvery+sup.pollTimeout)
	defer cancel()
	sup.policy.HealthPoll(pollCtx)
}

// HealthPoll implementation shared by both policies: one get_state +
// one get_session_stats against the child attached to sessionID,
// folded as corroboration; wedge → restart (the remedy, not a crash —
// no backoff penalty, fresh generation).
func (s *supervisedPiChild) healthPoll(ctx context.Context, log *zap.Logger) {
	child := s.currentChild()
	if child == nil {
		return
	}
	if child.Wedged() {
		log.Warn("pi child wedged (stdin not draining); restarting",
			zap.String("session", s.sessionOf()),
			zap.Int("pid", child.pid()),
			zap.Duration("stdout_quiet", child.LastReadAge()))
		s.requestRestart() //nolint:contextcheck // the respawn ladder is detached by design
		return
	}
	sid := s.sessionOf()
	if sid == "" {
		return
	}
	if data, err := child.Call(ctx, map[string]any{"type": "get_state"}); err == nil {
		s.agg.ApplyStatePoll(sid, data, time.Now())
	} else {
		log.Debug("pi get_state poll missed", zap.String("session", sid), zap.Error(err))
	}
	if data, err := child.Call(ctx, map[string]any{"type": "get_session_stats"}); err == nil {
		s.agg.ApplyStatsPoll(sid, data)
	} else {
		log.Debug("pi get_session_stats poll missed", zap.String("session", sid), zap.Error(err))
	}
}

// --- policy plumbing: HealthPoll + ReconcileDir on both policies ---

// HealthPoll runs one pass over the policy's live children.
func (p *processPerSessionPolicy) HealthPoll(ctx context.Context) {
	p.mu.Lock()
	kids := make([]*supervisedPiChild, 0, len(p.children))
	for _, s := range p.children {
		kids = append(kids, s)
	}
	p.mu.Unlock()
	for _, s := range kids {
		s.healthPoll(ctx, p.log)
	}
}

// ReconcileDir folds the session-dir scan into the aggregator so
// persisted-but-unattached sessions keep rendering idle rows (parity:
// opencode's server lists every stored session; pi has no list-sessions
// RPC — the dir IS the list, scoping §1.4 validated fact).
func (p *processPerSessionPolicy) ReconcileDir() {
	if ids, err := piScanSessionDir(p.sessionDir); err == nil {
		for _, id := range ids {
			p.agg.cell(id)
		}
	}
}

// HealthPoll polls the singleton's active session (other sessions have
// no live truth under this policy — the recorded limitation).
func (p *singleProcessPolicy) HealthPoll(ctx context.Context) {
	p.mu.Lock()
	child := p.child
	p.mu.Unlock()
	if child != nil {
		child.healthPoll(ctx, p.log)
	}
}

// ReconcileDir is a no-op for the single-process policy: the singleton
// holds one in-memory active session; dir rows would render idle state
// this policy cannot know (never fabricate truth — design 0055).
func (p *singleProcessPolicy) ReconcileDir() {}
