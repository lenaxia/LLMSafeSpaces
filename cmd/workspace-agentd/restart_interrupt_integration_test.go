// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// restart_interrupt_integration_test.go — #1342 integration: the REAL
// secrets pipeline (applySecretsBatch) against a fake harness, a real
// SSE tracker, and the real interrupter seam. Replays the 2026-09-11
// incident shape: a credential change delivered while a long-running
// turn streams output must defer (never force-kill); once the turn
// stalls, the interrupt must land BEFORE the restart, and the pending
// surface must clear when the restart applies.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

// fakeHarnessOpencode serves the routes the restart path touches:
// GET /session (live session list for the prune lister and the store
// reader), GET /session/status (live turn registry), and POST
// /session/:id/abort (the Act interrupt). Records abort calls.
type fakeHarnessOpencode struct {
	mu       sync.Mutex
	aborts   []string
	sessions []string
	// abortStatus lets a test simulate a harness that rejects/ignores
	// interrupts (non-2xx).
	abortStatus int
	// sessionFailures makes GET /session fail this many times before
	// answering (a harness still booting after a restart).
	sessionFailures int
	srv             *httptest.Server
}

func newFakeHarnessOpencode(t *testing.T, sessions ...string) *fakeHarnessOpencode {
	t.Helper()
	f := &fakeHarnessOpencode{sessions: sessions}
	orig := agentAddrAtomic.Load().(string)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/session":
			f.mu.Lock()
			ids := append([]string{}, f.sessions...)
			failures := f.sessionFailures
			if failures > 0 {
				f.sessionFailures--
			}
			f.mu.Unlock()
			if failures > 0 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			out := make([]map[string]any, len(ids))
			for i, id := range ids {
				out[i] = map[string]any{"id": id}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && path.Base(r.URL.Path) == "abort":
			f.mu.Lock()
			f.aborts = append(f.aborts, path.Base(path.Dir(r.URL.Path)))
			status := f.abortStatus
			f.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	agentAddrAtomic.Store(f.srv.URL)
	t.Cleanup(func() {
		agentAddrAtomic.Store(orig)
		f.srv.Close()
	})
	return f
}

func (f *fakeHarnessOpencode) abortCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.aborts...)
}

func integrationApplyDeps(t *testing.T, proc restartableProcess, pending *pendingApplyTracker) (materializeConfig, applySecretsDeps) {
	t.Helper()
	dir := t.TempDir()
	cfg := materializeConfig{
		secretsBaseDir:  filepath.Join(dir, "secrets"),
		sshDir:          filepath.Join(dir, ".ssh"),
		agentConfigPath: filepath.Join(dir, "agent-config.json"),
		secretsEnvPath:  filepath.Join(dir, "env"),
		gitCredsPath:    filepath.Join(dir, ".git-credentials"),
		home:            dir,
	}
	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 2 * time.Second}}
	// Cancelable lifecycle context: a deferred-restart goroutine that
	// outlives the test body (e.g. one still riding the maintenance
	// window at assert time) MUST be torn down with the test — its poll
	// tick's lister would otherwise cross-talk into a later test's fake
	// harness (the CI race-suite catch on the actor call-recorder).
	bgCtx, bgCancel := context.WithCancel(context.Background())
	t.Cleanup(bgCancel)
	deps := applySecretsDeps{
		Proc:                    proc,
		OpencodePassword:        "pw",
		Tracker:                 newSessionStatusTracker(),
		BgCtx:                   bgCtx,
		Lister:                  func(ctx context.Context) []string { return liveIDs(client, ctx) },
		Interrupter:             newSessionInterrupter("pw"),
		PendingApply:            pending,
		RestartReasonMarkerPath: filepath.Join(dir, "restart-reason"),
	}
	return cfg, deps
}

func liveIDs(client *OpenCodeClient, ctx context.Context) []string {
	ss, err := client.ListSessions(ctx)
	if err != nil || ss == nil {
		return nil
	}
	ids := make([]string, len(ss))
	for i, s := range ss {
		ids[i] = s.ID
	}
	return ids
}

// streamingTurn feeds the tracker part-update events for sessionID
// every interval until stop closes — the 40-min build shape. The
// returned stop is idempotent.
func streamingTurn(tracker *sessionStatusTracker, sessionID string, every time.Duration) (stop func()) {
	tracker.set(sessionID, "busy")
	stopped := make(chan struct{})
	var once sync.Once
	go func() {
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-stopped:
				return
			case <-tick.C:
				tracker.processEvent(`{"type":"message.part.updated","properties":{"sessionID":"` + sessionID + `","part":{"id":"prt_out","type":"text","text":"chunk"}}}`)
			}
		}
	}()
	return func() {
		once.Do(func() { close(stopped) })
	}
}

// silencePastStallBound models "the turn has been silent longer than the
// production stall bound" without wall-clock waiting: the session's
// activity clock (busy-mark AND last event) is backdated past the bound.
func silencePastStallBound(tracker *sessionStatusTracker, sessionID string) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	past := time.Now().Add(-restartStallBound - 5*time.Second)
	tracker.busySince[sessionID] = past
	tracker.lastEventAt[sessionID] = past
}

// TestIntegration1342_CredentialChangeDuringStreamingTurn_Defers is the
// incident replay: env-secret delivery (restart-worthy) while a turn
// streams → NO restart, NO interrupt, pending surfaced; when the stream
// stalls → interrupt lands → grace → restart applies → pending clears.
func TestIntegration1342_CredentialChangeDuringStreamingTurn_Defers(t *testing.T) {
	if testing.Short() {
		t.Skip("wall-clock integration row: exercises the production 5s poll + 5s grace timers (runs in the full suite)")
	}
	harness := newFakeHarnessOpencode(t, "ses_build")
	proc := &mockManagedProcess{}
	pending := newPendingApplyTracker()
	cfg, deps := integrationApplyDeps(t, proc, pending)

	stop := streamingTurn(deps.Tracker, "ses_build", 30*time.Millisecond)
	defer stop()

	batch := []secrets.Secret{{Type: "env-secret", Name: "tok", Metadata: map[string]string{"var_name": "TOK"}, Plaintext: "v"}}
	outcome, aErr := applySecretsBatch(context.Background(), cfg, deps, batch, nil)
	require.Nil(t, aErr)
	assert.False(t, outcome.restarted, "busy session — the pipeline must defer, not restart")
	assert.Equal(t, 0, proc.restartCount(), "no restart while the turn streams")

	// The deferred apply is surfaced for the operator.
	require.Eventually(t, func() bool { return pending.snapshot() != nil },
		2*time.Second, 10*time.Millisecond, "deferred credential apply must surface")

	// Sustained streaming: still deferred, no interrupt (the 40-min
	// build rule — defer is unbounded while the turn progresses).
	time.Sleep(400 * time.Millisecond)
	assert.Equal(t, 0, proc.restartCount())
	assert.Empty(t, harness.abortCalls(), "streaming turn must never be interrupted")

	// The stream stalls (wedge): model the stall bound elapsing with no
	// part activity — the force path fires, interrupt FIRST.
	stop()
	silencePastStallBound(deps.Tracker, "ses_build")
	require.Eventually(t, func() bool { return len(harness.abortCalls()) == 1 },
		20*time.Second, 10*time.Millisecond, "stalled turn must receive the Act interrupt")
	require.Equal(t, 0, proc.restartCount(), "restart must not fire before the interrupt")
	require.Eventually(t, func() bool { return proc.restartCount() == 1 },
		15*time.Second, 10*time.Millisecond, "grace expired — the restart applies the credential")
	require.Eventually(t, func() bool { return pending.snapshot() == nil },
		2*time.Second, 10*time.Millisecond, "applied restart clears the pending surface")
}

// TestIntegration1342_IdleCredentialChange_AppliesImmediately: the
// maintenance-window fast path — no busy session, the restart applies
// now and nothing surfaces as pending.
func TestIntegration1342_IdleCredentialChange_AppliesImmediately(t *testing.T) {
	harness := newFakeHarnessOpencode(t)
	proc := &mockManagedProcess{}
	pending := newPendingApplyTracker()
	cfg, deps := integrationApplyDeps(t, proc, pending)
	deps.Tracker.set("ses_idle", "idle")

	batch := []secrets.Secret{{Type: "env-secret", Name: "tok", Metadata: map[string]string{"var_name": "TOK"}, Plaintext: "v"}}
	outcome, aErr := applySecretsBatch(context.Background(), cfg, deps, batch, nil)
	require.Nil(t, aErr)
	assert.True(t, outcome.restarted, "idle — apply immediately")
	assert.Equal(t, 1, proc.restartCount())
	assert.Nil(t, pending.snapshot(), "nothing deferred — nothing surfaces")
	assert.Empty(t, harness.abortCalls(), "no busy session — no interrupt")
}

// TestIntegration1342_PendingApplySurfacesOnHealthz wires the tracker
// through the real healthz handler construction.
func TestIntegration1342_PendingApplySurfacesOnHealthz(t *testing.T) {
	_ = newFakeHarnessOpencode(t, "ses_busy")
	proc := &mockManagedProcess{}
	pending := newPendingApplyTracker()
	cfg, deps := integrationApplyDeps(t, proc, pending)
	deps.Tracker.set("ses_busy", "busy")
	deps.Tracker.processEvent(`{"type":"message.part.updated","properties":{"sessionID":"ses_busy","part":{"id":"p","type":"text","text":"x"}}}`)

	batch := []secrets.Secret{{Type: "env-secret", Name: "tok", Metadata: map[string]string{"var_name": "TOK"}, Plaintext: "v"}}
	_, aErr := applySecretsBatch(context.Background(), cfg, deps, batch, nil)
	require.Nil(t, aErr)

	require.Eventually(t, func() bool { return pending.snapshot() != nil },
		2*time.Second, 10*time.Millisecond)

	handler := healthzHandler(time.Now(), "", nil, pending.snapshot, nil, nil)
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/v1/healthz", nil))
	assert.Contains(t, rec.Body.String(), "pendingApply")
}

// ---------------------------------------------------------------------------
// The S12 chain, projection-level: interrupt honored → harness-written
// terminal state (no sweep needed); interrupt ignored → kill →
// generation-change reseed → orphan sweep restores honesty.
// ---------------------------------------------------------------------------

const (
	toolCalledEvt  = `{"id":"evt_tc","type":"session.next.tool.called","properties":{"sessionID":"ses_build","assistantMessageID":"msg_1","callID":"call_1","tool":"bash","input":{"command":"make build"}}}`
	toolFailureEvt = `{"id":"evt_tf","type":"session.next.tool.failure","properties":{"sessionID":"ses_build","assistantMessageID":"msg_1","callID":"call_1","error":{"message":"aborted"}}}`
	sessionBusyEvt = `{"type":"session.status","properties":{"sessionID":"ses_build","status":{"type":"busy"}}}`
	sessionIdleEvt = `{"type":"session.status","properties":{"sessionID":"ses_build","status":{"type":"idle"}}}`
)

// integrationAuthority builds the REAL authority (real ABITranslator,
// real opencode store reader against the fake harness) and wires the
// production SSE ingestion path: the tracker forwards every raw event
// to the authority before dialect parsing (main.go's wiring).
func integrationAuthority(t *testing.T, tracker *sessionStatusTracker) *sessionstate.Authority {
	t.Helper()
	t.Setenv("LLMSAFESPACES_PLATFORM_DIR", t.TempDir())
	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: 2 * time.Second}}
	a := newStateAuthority(client, "pw", "")
	require.NotNil(t, a)
	tracker.onRawEvent = a.Ingest
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func projectionToolState(a *sessionstate.Authority, sessionID string) *abiv1.ToolState {
	snap := a.State()
	rec, ok := snap.Sessions[sessionID]
	if !ok {
		return nil
	}
	for _, p := range rec.InFlightParts {
		if tool, ok := p.GetPayload().(*abiv1.Part_Tool); ok {
			return tool.Tool.GetState()
		}
	}
	return nil
}

// TestIntegration1342_HarnessHonorsInterrupt_TurnEndsTerminalNoSweep: the
// S12 primary path end-to-end — the force path's interrupt lands, the
// harness (as a healthy one does) writes terminal part state and goes
// idle, the restart applies, and the orphan sweep has NOTHING to do.
func TestIntegration1342_HarnessHonorsInterrupt_TurnEndsTerminalNoSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("wall-clock integration row: exercises the production 5s poll + 5s grace timers (runs in the full suite)")
	}
	harness := newFakeHarnessOpencode(t, "ses_build")
	proc := &mockManagedProcess{}
	pending := newPendingApplyTracker()
	cfg, deps := integrationApplyDeps(t, proc, pending)
	authority := integrationAuthority(t, deps.Tracker)

	// A live turn: busy + a running tool part in the projection (through
	// the REAL SSE ingestion path — tracker → authority).
	deps.Tracker.processEvent(sessionBusyEvt)
	deps.Tracker.processEvent(toolCalledEvt)
	require.Eventually(t, func() bool {
		st := projectionToolState(authority, "ses_build")
		return st != nil && st.GetStatus() == abiv1.ToolStatus_TOOL_STATUS_RUNNING
	}, 2*time.Second, 10*time.Millisecond, "the running tool part must reach the projection")

	// A restart-worthy credential change arrives while the turn runs.
	batch := []secrets.Secret{{Type: "env-secret", Name: "tok", Metadata: map[string]string{"var_name": "TOK"}, Plaintext: "v"}}
	_, aErr := applySecretsBatch(context.Background(), cfg, deps, batch, nil)
	require.Nil(t, aErr)

	// The turn goes silent past the stall bound → force path → interrupt.
	silencePastStallBound(deps.Tracker, "ses_build")
	require.Eventually(t, func() bool { return len(harness.abortCalls()) == 1 },
		20*time.Second, 10*time.Millisecond, "the interrupt must land")

	// The harness HONORS it: terminal part state + idle (its own events,
	// not synthetic). Assert the projection holds the harness-written
	// terminal state — S12's "terminal part state before death".
	deps.Tracker.processEvent(toolFailureEvt)
	require.Eventually(t, func() bool {
		st := projectionToolState(authority, "ses_build")
		return st != nil && st.GetStatus() == abiv1.ToolStatus_TOOL_STATUS_ERROR
	}, 2*time.Second, 10*time.Millisecond, "the honored interrupt must leave TERMINAL part state in the projection")
	deps.Tracker.processEvent(sessionIdleEvt)

	// Grace expires, the restart applies, and the generation-change
	// reseed (what onChildStarted fires) sweeps nothing — the turn ended
	// with harness-written terminal state.
	require.Eventually(t, func() bool { return proc.restartCount() == 1 },
		15*time.Second, 20*time.Millisecond, "grace expired — restart applies")
	require.NoError(t, authority.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange))
	assert.Equal(t, int64(0), authority.Metrics().OrphanPartsAborted,
		"a turn that ended with harness-written terminal state must need NO sweep")
}

// TestIntegration1342_HarnessIgnoresInterrupt_KillThenSweepRestoresHonesty:
// the S12 backstop chain — interrupt fails, grace expires, the restart
// kills the turn, and the generation-change reseed (the retrying driver
// the child-started hook fires) folds the orphaned running part as
// aborted in the restored projection.
func TestIntegration1342_HarnessIgnoresInterrupt_KillThenSweepRestoresHonesty(t *testing.T) {
	if testing.Short() {
		t.Skip("wall-clock integration row: exercises the production 5s poll + 5s grace timers (runs in the full suite)")
	}
	harness := newFakeHarnessOpencode(t, "ses_build")
	harness.mu.Lock()
	harness.abortStatus = http.StatusInternalServerError
	harness.mu.Unlock()

	proc := &mockManagedProcess{}
	pending := newPendingApplyTracker()
	cfg, deps := integrationApplyDeps(t, proc, pending)
	authority := integrationAuthority(t, deps.Tracker)

	deps.Tracker.processEvent(sessionBusyEvt)
	deps.Tracker.processEvent(toolCalledEvt)
	require.Eventually(t, func() bool {
		return projectionToolState(authority, "ses_build") != nil
	}, 2*time.Second, 10*time.Millisecond)

	// A restart-worthy credential change arrives while the turn runs.
	batch := []secrets.Secret{{Type: "env-secret", Name: "tok", Metadata: map[string]string{"var_name": "TOK"}, Plaintext: "v"}}
	_, aErr := applySecretsBatch(context.Background(), cfg, deps, batch, nil)
	require.Nil(t, aErr)

	// Stall → force path attempts the interrupt (fails) → grace → kill.
	silencePastStallBound(deps.Tracker, "ses_build")
	require.Eventually(t, func() bool { return proc.restartCount() == 1 },
		20*time.Second, 20*time.Millisecond, "grace expires despite the ignored interrupt")

	// The new generation's child-started hook fires the RETRYING reseed
	// driver (startManagedProcess wiring) against the store — the sweep
	// folds the orphan as aborted.
	go startStateAuthorityReseed(context.Background(), authority, sessionstate.ReseedReasonGenerationChange)
	require.Eventually(t, func() bool {
		return authority.Metrics().OrphanPartsAborted == 1
	}, 8*time.Second, 20*time.Millisecond, "the generation-change reseed must sweep the orphaned running part")

	st := projectionToolState(authority, "ses_build")
	require.NotNil(t, st)
	assert.Equal(t, abiv1.ToolStatus_TOOL_STATUS_ERROR, st.GetStatus())
	assert.Equal(t, sessionstate.OrphanSweepReason, st.GetError())
}

// TestIntegration1342_GenerationReseedRetriesUntilStoreAnswers: the
// generation-change reseed rides the retrying driver — a harness still
// booting after the restart (failing /session twice) must not leave the
// backstop unfired.
func TestIntegration1342_GenerationReseedRetriesUntilStoreAnswers(t *testing.T) {
	harness := newFakeHarnessOpencode(t, "ses_build")
	harness.mu.Lock()
	harness.sessionFailures = 2 // /session 500s twice, then answers
	harness.mu.Unlock()

	proc := &mockManagedProcess{}
	pending := newPendingApplyTracker()
	_, deps := integrationApplyDeps(t, proc, pending)
	authority := integrationAuthority(t, deps.Tracker)

	deps.Tracker.processEvent(sessionBusyEvt)
	deps.Tracker.processEvent(toolCalledEvt)
	require.Eventually(t, func() bool {
		return projectionToolState(authority, "ses_build") != nil
	}, 2*time.Second, 10*time.Millisecond)

	// The store is unreachable twice (a freshly restarted harness); the
	// driver retries with backoff until the sweep lands.
	go startStateAuthorityReseed(context.Background(), authority, sessionstate.ReseedReasonGenerationChange)
	require.Eventually(t, func() bool {
		return authority.Metrics().OrphanPartsAborted == 1
	}, 15*time.Second, 50*time.Millisecond,
		"the retrying driver must reseed (and sweep) once the harness answers")
}
