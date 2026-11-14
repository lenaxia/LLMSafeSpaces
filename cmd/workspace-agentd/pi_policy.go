// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pi_policy.go — the §1.4 process-model ruling made executable: how
// platform sessions map onto pi RPC child processes (pi second harness
// S2; scoping §1.4).
//
// THE RECORDED RULING (docs/operator/pi-supervisor.md is the long form):
// pi RPC mode is a single-conversation process — get_state/prompt/
// events address THE active session, and a busy session blocks
// switching. The platform's session-state authority (design 0055 R4)
// and the D6 escalation inputs (OldestBusySeconds et al.) require
// per-session busy truth that option (b) single-process+switch_session
// structurally cannot observe ("other sessions' busy-state is
// unobservable" — scoping §1.4). RULING: process-per-session is the
// platform policy; single-process stays implemented behind the same
// interface as the cost-bounded alternative (measured: ~79 MiB RSS per
// idle child, real pinned binary — see the ruling doc), selected per
// workspace by S5's harness plumbing, never by accident.
//
// Spawn shapes (both validated against the pinned binary):
//
//	per-session: pi --mode rpc --session-dir <PVC> --session-id <id>
//	  (persisted; a crashed child respawns with the same id and RESUMES
//	  — the §5 restart contract)
//	single:      pi --mode rpc --no-session --session-dir <PVC>
//	  (in-memory current session; Attach drives switch_session)

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// errPiBusySwitch is the single-process policy's typed refusal: the
// active session is busy, so switch_session cannot address another
// session right now (the recorded limitation of policy (b) — callers
// surface it, the per-session policy never produces it).
var errPiBusySwitch = errors.New("pi single-process policy: active session is busy; switch_session blocked")

// piSessionHandle is one addressable platform session under a policy.
type piSessionHandle interface {
	// Call issues one RPC command against the session's process.
	Call(ctx context.Context, command map[string]any) (result []byte, err error)
	// Generation is the child epoch (bumped every respawn — the #1632
	// generation contract: a new generation must not inherit the dead
	// one's unhealthy episode).
	Generation() uint64
	// Restarts is the lifetime crash/respawn count of this session's
	// process (diagnostic parity with managedProcess).
	Restarts() int
}

// piProcessPolicy is the §1.4 internal interface. Exactly two
// production implementations exist (processPerSessionPolicy,
// singleProcessPolicy); tests may fake it.
type piProcessPolicy interface {
	// Attach returns the handle for a platform session id, spawning or
	// switching as the policy dictates.
	Attach(ctx context.Context, sessionID string) (piSessionHandle, error)
	// Release drops the policy's hold on a session (idle reaping,
	// shutdown). The session's persisted files are untouched.
	Release(sessionID string)
	// Snapshot renders the statusz parity surface across all sessions
	// the policy knows (live + remembered + session-dir scan).
	Snapshot() piStatusSnapshot
	// ReconcileDir folds the session-dir truth (persisted session
	// files) into the state surface — the no-list-sessions-RPC
	// workaround, run by the supervisor's health pass.
	ReconcileDir()
	// HealthPoll runs one #1632-semantics pass over the policy's live
	// children (get_state/get_session_stats corroboration + wedge
	// restart).
	HealthPoll(ctx context.Context)
	// Shutdown terminates every child via the rpc.md ladder.
	Shutdown(ctx context.Context) error
}

// supervisedPiChild owns ONE pi child with the managedProcess lifecycle
// model (single supervisor goroutine, sole Wait, intent flags) adapted
// for RPC mode: crash → backoff → respawn with the SAME session id
// (resume), operator stop → the rpc.md shutdown ladder. Session events
// fold into the aggregator under sessionID().
type supervisedPiChild struct {
	cfg piSpawnConfig
	log *zap.Logger
	agg *piStateAggregator
	// sessionOf resolves the session id the pump attributes events to
	// (per-session: fixed at spawn; single: the policy's current id).
	sessionOf func() string

	mu               sync.Mutex
	child            *piChild
	generation       uint64
	restarts         int
	lastRestartAt    time.Time
	stopRequested    bool
	restartRequested bool
	upCh             chan struct{}
	doneCh           chan struct{}
	started          bool
}

func newSupervisedPiChild(cfg piSpawnConfig, log *zap.Logger, agg *piStateAggregator, sessionOf func() string) *supervisedPiChild {
	return &supervisedPiChild{
		cfg:       cfg,
		log:       log.With(zap.String("component", "pi_supervised_child")),
		agg:       agg,
		sessionOf: sessionOf,
		upCh:      make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

// start spawns the supervisor goroutine (idempotent).
func (s *supervisedPiChild) start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	go s.supervise()
}

// supervise is the loop: spawn → announce → wait exit → classify →
// backoff/stop. Mirrors managedProcess.supervise's invariants (sole
// Wait, fresh upCh per generation); the health-watchdog restart path
// lives in pi_supervisor.go and calls requestRestart.
func (s *supervisedPiChild) supervise() {
	defer close(s.doneCh)
	for {
		s.mu.Lock()
		if s.stopRequested {
			s.mu.Unlock()
			return
		}
		child := newPiChild(s.cfg, s.log, s.onEvent)
		err := child.start()
		if err != nil {
			s.log.Error("pi child spawn failed", zap.Error(err))
			s.mu.Unlock()
			if !s.backoffSleep() {
				return
			}
			continue
		}
		s.child = child
		s.generation++
		s.lastRestartAt = time.Now()
		upCh := s.upCh
		s.upCh = make(chan struct{})
		gen := s.generation
		s.mu.Unlock()

		sid := s.sessionOf()
		s.agg.SetLive(sid, true)
		s.agg.GenerationStart(sid, gen)
		close(upCh)

		<-child.exited // sole Wait owner is the child's wait goroutine
		s.agg.SetLive(s.sessionOf(), false)

		s.mu.Lock()
		stop := s.stopRequested
		restart := s.restartRequested
		s.restartRequested = false
		s.child = nil
		s.mu.Unlock()
		if stop {
			return
		}
		if restart {
			continue // operator/watchdog restart: no backoff
		}
		s.restarts++
		if !s.backoffSleep() {
			return
		}
	}
}

// backoffSleep applies the managedProcess backoff curve and reports
// whether the loop should continue (false once stop was requested).
func (s *supervisedPiChild) backoffSleep() bool {
	s.mu.Lock()
	step := s.restarts
	if step > 5 {
		step = 5
	}
	if !s.lastRestartAt.IsZero() && time.Since(s.lastRestartAt) > 60*time.Second {
		s.restarts = 0
		step = 0
	}
	s.mu.Unlock()
	d := time.Duration(1<<step) * time.Second
	if d > maxBackoffSec*time.Second {
		d = maxBackoffSec * time.Second
	}
	s.log.Warn("pi child exited unexpectedly; respawning", zap.Duration("backoff", d))
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		stop := s.stopRequested
		s.mu.Unlock()
		if stop {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

// onEvent folds the child's session events into the aggregator.
func (s *supervisedPiChild) onEvent(rec *piRecord, raw []byte) {
	if sid := s.sessionOf(); sid != "" {
		s.agg.ApplyEvent(sid, rec, raw, time.Now())
	}
}

// currentChild snapshots the live *piChild (nil between generations).
func (s *supervisedPiChild) currentChild() *piChild {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.child
}

// waitLive is Attach's wait: a live child NOW, or the next
// generation's announcement while a respawn is in flight. Waiting on
// the pending upCh when a child is ALREADY up would block until that
// child's death (upCh is per-NEXT-generation by construction — the
// managedProcess swap discipline), so the live check comes first.
func (s *supervisedPiChild) waitLive() bool {
	s.mu.Lock()
	live := s.child != nil
	upCh := s.upCh
	done := s.doneCh
	s.mu.Unlock()
	if live {
		return true
	}
	select {
	case <-upCh:
		return true
	case <-done:
		return false
	}
}

// Call issues a command against the current generation's child.
func (s *supervisedPiChild) Call(ctx context.Context, command map[string]any) ([]byte, error) {
	child := s.currentChild()
	if child == nil {
		return nil, errors.New("pi child not running")
	}
	return child.Call(ctx, command)
}

// Generation reports the current generation epoch.
func (s *supervisedPiChild) Generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

// Restarts reports the lifetime respawn count.
func (s *supervisedPiChild) Restarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restarts
}

// requestRestart signals the loop to respawn without backoff (the
// watchdog path: the restart IS the remedy, not a crash). The ladder
// context is detached from any request context on purpose: the respawn
// must proceed even when the poll that noticed the wedge is long gone.
func (s *supervisedPiChild) requestRestart() {
	s.mu.Lock()
	s.restartRequested = true
	child := s.child
	s.mu.Unlock()
	if child != nil {
		_ = child.Shutdown(context.Background(), 2*time.Second, time.Second) //nolint:contextcheck // deliberate detached ladder
	}
}

// stop runs the rpc.md ladder and waits for the loop to exit.
func (s *supervisedPiChild) stop(ctx context.Context, grace time.Duration) {
	s.mu.Lock()
	if !s.started || s.stopRequested {
		s.mu.Unlock()
		return
	}
	s.stopRequested = true
	child := s.child
	s.mu.Unlock()
	if child != nil {
		_ = child.Shutdown(ctx, grace, 5*time.Second)
	}
	<-s.doneCh
}

// --- session-directory scan (no list-sessions RPC exists — scoping
// §1.1 validated fact: sessions are found by scanning the session dir;
// session-format.md File Location: <timestamp>_<session-id>.jsonl) ---

// piScanSessionDir lists session ids from the session dir's persisted
// files. The timestamp prefix never contains '_', session ids may — so
// the id is everything after the FIRST '_'. Newest file wins on
// duplicates; ordering is deterministic (sorted ids).
func piScanSessionDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir) //nolint:gosec // PVC session dir from spawn config
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	seen := make(map[string]time.Time)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".jsonl")
		i := strings.Index(name, "_")
		if i <= 0 || i == len(name)-1 {
			continue
		}
		id := name[i+1:]
		info, err := e.Info()
		if err != nil {
			continue
		}
		if prev, ok := seen[id]; !ok || info.ModTime().After(prev) {
			seen[id] = info.ModTime()
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// piSessionPath resolves the newest persisted file for a session id
// (the single-process policy's switch_session target).
func piSessionPath(dir, sessionID string) (string, error) {
	entries, err := os.ReadDir(dir) //nolint:gosec // PVC session dir from spawn config
	if err != nil {
		return "", err
	}
	var newest string
	var newestMT time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".jsonl")
		i := strings.Index(name, "_")
		if i <= 0 || name[i+1:] != sessionID {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if newest == "" || info.ModTime().After(newestMT) {
			newest = filepath.Join(dir, e.Name())
			newestMT = info.ModTime()
		}
	}
	if newest == "" {
		return "", fmt.Errorf("pi session %q has no persisted file under %s", sessionID, dir)
	}
	return newest, nil
}

// processPerSessionPolicy — THE ruling's policy: one persisted child
// per platform session, all state observable, sessions independent.
type processPerSessionPolicy struct {
	base       piSpawnConfig
	log        *zap.Logger
	agg        *piStateAggregator
	sessionDir string

	mu       sync.Mutex
	children map[string]*supervisedPiChild
	// remembered tracks released sessions (last-known idle state stays
	// renderable until the session file disappears).
	remembered map[string]bool
}

func newProcessPerSessionPolicy(base piSpawnConfig, log *zap.Logger, agg *piStateAggregator) *processPerSessionPolicy {
	return &processPerSessionPolicy{
		base:       base,
		log:        log.With(zap.String("component", "pi_policy_per_session")),
		agg:        agg,
		sessionDir: base.SessionDir,
		children:   make(map[string]*supervisedPiChild),
		remembered: make(map[string]bool),
	}
}

// Attach returns (spawning if needed) the session's supervised child.
// The spawn pins --session-id, so a respawn after crash RESUMES the
// persisted session (validated: messageCount and entries survive
// kill -9 + respawn).
func (p *processPerSessionPolicy) Attach(_ context.Context, sessionID string) (piSessionHandle, error) {
	if sessionID == "" {
		return nil, errors.New("pi per-session policy: empty session id")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.children[sessionID]
	if !ok {
		cfg := p.base
		cfg.SessionID = sessionID
		cfg.Ephemeral = false
		s = newSupervisedPiChild(cfg, p.log, p.agg, func() string { return sessionID })
		p.children[sessionID] = s
		s.start()
	}
	if !s.waitLive() {
		return nil, fmt.Errorf("pi per-session policy: session %q supervisor exited", sessionID)
	}
	return s, nil
}

// Release stops the session's child; the aggregator cell stays as the
// last-known (idle) truth until Forget.
func (p *processPerSessionPolicy) Release(sessionID string) {
	p.mu.Lock()
	s, ok := p.children[sessionID]
	if ok {
		delete(p.children, sessionID)
		p.remembered[sessionID] = true
	}
	p.mu.Unlock()
	if ok {
		s.stop(context.Background(), 10*time.Second)
	}
}

// Snapshot renders parity over live + remembered sessions (the dir
// scan is ReconcileDir's job — a snapshot must not do I/O; the health
// pass reconciles before anyone snapshots).
func (p *processPerSessionPolicy) Snapshot() piStatusSnapshot {
	return p.agg.Snapshot(time.Now())
}

// Shutdown stops every child (rpc.md ladder per child, parallel).
func (p *processPerSessionPolicy) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	kids := make([]*supervisedPiChild, 0, len(p.children))
	for _, s := range p.children {
		kids = append(kids, s)
	}
	p.children = make(map[string]*supervisedPiChild)
	p.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range kids {
		wg.Add(1)
		go func(s *supervisedPiChild) {
			defer wg.Done()
			s.stop(ctx, 15*time.Second)
		}(s)
	}
	wg.Wait()
	return nil
}

// singleProcessPolicy — the cost-bounded alternative: ONE child, the
// active session switched via switch_session. Busy-state for other
// sessions is unobservable (the recorded limitation; it is why the
// ruling did not choose it).
type singleProcessPolicy struct {
	base       piSpawnConfig
	log        *zap.Logger
	agg        *piStateAggregator
	sessionDir string

	mu      sync.Mutex
	child   *supervisedPiChild
	current string
}

func newSingleProcessPolicy(base piSpawnConfig, log *zap.Logger, agg *piStateAggregator) *singleProcessPolicy {
	cfg := base
	cfg.Ephemeral = true // in-memory current session; Attach switches
	cfg.SessionID = ""
	return &singleProcessPolicy{
		base:       cfg,
		log:        log.With(zap.String("component", "pi_policy_single")),
		agg:        agg,
		sessionDir: base.SessionDir,
	}
}

// Attach switches the one child onto the requested session.
func (p *singleProcessPolicy) Attach(ctx context.Context, sessionID string) (piSessionHandle, error) {
	if sessionID == "" {
		return nil, errors.New("pi single-process policy: empty session id")
	}
	p.mu.Lock()
	p.ensureStartedLocked()
	p.mu.Unlock()
	if !p.child.waitLive() {
		return nil, errors.New("pi single-process policy: supervisor exited")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current != sessionID {
		if st := p.agg.Snapshot(time.Now()); st.SessionsActive > 0 {
			return nil, errPiBusySwitch
		}
		path, err := piSessionPath(p.sessionDir, sessionID)
		if err != nil {
			return nil, err
		}
		if _, err := p.child.Call(ctx, map[string]any{
			"type": "switch_session", "sessionPath": path,
		}); err != nil {
			return nil, fmt.Errorf("pi switch_session: %w", err)
		}
		p.current = sessionID
		p.agg.GenerationStart(sessionID, p.child.Generation())
	}
	return p.child, nil
}

// ensureStartedLocked spawns the singleton child on first use.
func (p *singleProcessPolicy) ensureStartedLocked() {
	if p.child != nil {
		return
	}
	p.child = newSupervisedPiChild(p.base, p.log, p.agg, func() string {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.current
	})
	p.child.start()
}

// Release is a no-op: the singleton keeps running (the policy's cost
// argument IS one process; per-session Release semantics do not apply).
func (p *singleProcessPolicy) Release(string) {}

// Snapshot renders the ACTIVE session's truth (other sessions have
// none here — the recorded limitation, stated honestly).
func (p *singleProcessPolicy) Snapshot() piStatusSnapshot {
	return p.agg.Snapshot(time.Now())
}

// Shutdown stops the singleton via the rpc.md ladder.
func (p *singleProcessPolicy) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	child := p.child
	p.child = nil
	p.mu.Unlock()
	if child != nil {
		child.stop(ctx, 15*time.Second)
	}
	return nil
}
