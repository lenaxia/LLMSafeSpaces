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

	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

// fakeHarnessOpencode serves the two routes the restart path touches:
// GET /session (live session list for the prune lister) and POST
// /session/:id/abort (the Act interrupt). Records abort calls.
type fakeHarnessOpencode struct {
	mu       sync.Mutex
	aborts   []string
	sessions []string
	// abortStatus lets a test simulate a harness that rejects/ignores
	// interrupts (non-2xx).
	abortStatus int
	srv         *httptest.Server
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
			f.mu.Unlock()
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
	deps := applySecretsDeps{
		Proc:                    proc,
		OpencodePassword:        "pw",
		Tracker:                 newSessionStatusTracker(),
		BgCtx:                   context.Background(),
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

// TestIntegration1342_HarnessIgnoresInterrupt_GraceExpires: a harness
// that 500s the interrupt still gets the restart after grace — the
// orphan sweep (authority-side) is the honesty backstop.
func TestIntegration1342_HarnessIgnoresInterrupt_GraceExpires(t *testing.T) {
	harness := newFakeHarnessOpencode(t, "ses_wedged")
	harness.mu.Lock()
	harness.abortStatus = http.StatusInternalServerError
	harness.mu.Unlock()

	proc := &mockManagedProcess{}
	pending := newPendingApplyTracker()
	cfg, deps := integrationApplyDeps(t, proc, pending)
	deps.Tracker.set("ses_wedged", "busy")
	silencePastStallBound(deps.Tracker, "ses_wedged")

	batch := []secrets.Secret{{Type: "env-secret", Name: "tok", Metadata: map[string]string{"var_name": "TOK"}, Plaintext: "v"}}
	_, aErr := applySecretsBatch(context.Background(), cfg, deps, batch, nil)
	require.Nil(t, aErr)

	require.Eventually(t, func() bool { return len(harness.abortCalls()) == 1 },
		20*time.Second, 10*time.Millisecond, "the force path must still ATTEMPT the interrupt")
	require.Eventually(t, func() bool { return proc.restartCount() == 1 },
		15*time.Second, 20*time.Millisecond,
		"a harness that ignores the interrupt must not wedge the credential apply — grace expires, restart fires")
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

	handler := healthzHandler(time.Now(), "", nil, pending.snapshot)
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/v1/healthz", nil))
	assert.Contains(t, rec.Body.String(), "pendingApply")
}
