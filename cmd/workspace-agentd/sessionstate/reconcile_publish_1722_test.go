// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
	"context"
	"testing"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reconcile_publish_1722_test.go — #1722 (session shows busy after turn
// end; repro: long tool call aborted mid-turn). The abort wire evidence
// (real opencode 1.18.15 capture, probe in the issue worklog): the
// harness DOES emit the clear burst on abort — session.error,
// session.status idle, session.idle, the aborted tool part's terminal
// update — all within ~30ms. The burst rides agentd's RAW /event
// subscription, which is replay-less and recycled every 5 minutes
// (sseConnectionTimeout) plus 2s-backoff reconnects; a burst landing in
// a recycle gap is lost to the tracker AND to this authority (it
// ingests via the tracker's onRawEvent). The reconcile pass then heals
// the projection from store evidence — but SILENTLY:
// clearBusyFromEvidence mutated the record with no seq, no fold, no
// fanout, so every downstream bridge (the API consumer that publishes
// session.status SSE to the user stream) never hears the settle and the
// busy badge sticks forever. These pins make the evidence-clear an
// ORDERED PUBLICATION: the settle signal for the abort case is the
// store-evidence turn-end (the harness's own session registry holds no
// live turn), and it must cross the fanout seam like every other
// status transition.

// wedgeBusyWithRunningTool marks a session busy with a live RUNNING
// tool part — the projection state at the moment an abort's clear burst
// is lost (the aborted tool never folds terminal state).
func wedgeBusyWithRunningTool(t *testing.T, a *Authority, sid string) {
	t.Helper()
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: sid, Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_PART_START, SessionId: sid, PartId: "prt_run",
		Part: &abiv1.Part{Id: "prt_run", Type: abiv1.PartType_PART_TYPE_TOOL, Payload: &abiv1.Part_Tool{Tool: &abiv1.ToolPart{
			CallId: "call_run", Name: "bash", Input: []byte(`{"command":"sleep 2880"}`),
			State: &abiv1.ToolState{Status: abiv1.ToolStatus_TOOL_STATUS_RUNNING},
		}}}})
}

// drainFrames consumes n frames from the subscription (the wedge's own
// ingested events are fanned out like every event — the settle pin must
// read past them).
func drainFrames(t *testing.T, frames <-chan *abiv1.StreamFrame, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-frames:
		case <-time.After(2 * time.Second):
			t.Fatalf("frame %d/%d never arrived", i+1, n)
		}
	}
}

// TestReconcileEvidenceClear_PublishesIdle: THE #1722 pin — the store
// evidence that re-derives a wedged session idle must PUBLISH the
// transition to subscribers, not just mutate the record. RED on the
// pre-fix tree: the subscriber sees nothing (the mutation is silent).
func TestReconcileEvidenceClear_PublishesIdle(t *testing.T) {
	store := &evidenceStore{states: map[string]abiv1.SessionStatus{"ses_1722": abiv1.SessionStatus_SESSION_STATUS_IDLE}}
	a := newReconcileAuthority(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, stop, err := a.Stream(ctx)
	require.NoError(t, err)
	defer stop()

	// Drain the connection's snapshot frame (the mandatory first).
	select {
	case <-frames:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot frame never arrived")
	}

	wedgeBusyWithRunningTool(t, a, "ses_1722")
	drainFrames(t, frames, 2) // the wedge's own BUSY + PART_START publications
	require.True(t, a.State().Sessions["ses_1722"].Busy, "precondition: wedged busy")

	stats := a.Reconcile(context.Background())
	require.Equal(t, ReconcileStats{BusyCleared: 1}, stats)
	assert.False(t, a.State().Sessions["ses_1722"].Busy, "projection healed from evidence")

	// THE PIN: the settle crossed the fanout seam as an ordered,
	// seq-carrying SESSION_STATUS frame.
	select {
	case f := <-frames:
		seq := f.GetEvent()
		require.NotNil(t, seq, "expected a sequenced event frame carrying the evidence settle")
		evt := seq.GetEvent()
		require.NotNil(t, evt)
		assert.Equal(t, abiv1.EventType_EVENT_TYPE_SESSION_STATUS, evt.GetType())
		assert.Equal(t, "ses_1722", evt.GetSessionId())
		assert.Equal(t, abiv1.SessionStatus_SESSION_STATUS_IDLE, evt.GetStatus())
	case <-time.After(2 * time.Second):
		t.Fatal("#1722: the evidence busy-clear healed the projection but was never PUBLISHED — downstream bridges cannot converge (stuck-busy badge)")
	}
}

// TestReconcileEvidenceClear_PublishesErrorStatus: an ERROR verdict
// publishes as SESSION_STATUS ERROR (the errored-turn settle — the
// bridge leg must be able to clear busy for it too).
func TestReconcileEvidenceClear_PublishesErrorStatus(t *testing.T) {
	store := &evidenceStore{states: map[string]abiv1.SessionStatus{"ses_1722": abiv1.SessionStatus_SESSION_STATUS_ERROR}}
	a := newReconcileAuthority(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, stop, err := a.Stream(ctx)
	require.NoError(t, err)
	defer stop()
	select {
	case <-frames:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot frame never arrived")
	}

	wedgeBusyWithRunningTool(t, a, "ses_1722")
	drainFrames(t, frames, 2)
	stats := a.Reconcile(context.Background())
	require.Equal(t, ReconcileStats{BusyCleared: 1}, stats)

	select {
	case f := <-frames:
		seq := f.GetEvent()
		require.NotNil(t, seq)
		evt := seq.GetEvent()
		require.NotNil(t, evt)
		assert.Equal(t, abiv1.SessionStatus_SESSION_STATUS_ERROR, evt.GetStatus(),
			"the evidence's own status rides the publication")
	case <-time.After(2 * time.Second):
		t.Fatal("#1722: ERROR-evidence clear was never published")
	}
}

// TestReconcileEvidenceClear_LiveRegistryGatesSettle: the settle
// signal is EXPLICIT (#1722 acceptance): store evidence says the turn
// ended AND the harness's live registry holds no live turn. While
// LiveBusy is set, the evidence neither clears nor publishes — a
// silent long tool run (no events for its duration, list status
// idle-biased) must not have its busy view settled mid-turn.
func TestReconcileEvidenceClear_LiveRegistryGatesSettle(t *testing.T) {
	store := &settleStore{evidenceStore: &evidenceStore{states: map[string]abiv1.SessionStatus{"ses_live": abiv1.SessionStatus_SESSION_STATUS_IDLE}}, live: map[string]bool{"ses_live": true}}
	a := newReconcileAuthority(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, stop, err := a.Stream(ctx)
	require.NoError(t, err)
	defer stop()
	select {
	case <-frames:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot frame never arrived")
	}

	wedgeBusyWithRunningTool(t, a, "ses_live")
	drainFrames(t, frames, 2)
	require.True(t, a.State().Sessions["ses_live"].Busy)

	// The mid-turn pass: the LIST says idle (idle-biased records) but
	// the live registry says the turn runs — nothing may settle.
	stats := a.Reconcile(context.Background())
	assert.Equal(t, ReconcileStats{}, stats, "LiveBusy holds: no clear, no publication")
	assert.True(t, a.State().Sessions["ses_live"].Busy, "the busy view survives the mid-turn pass")
	select {
	case f := <-frames:
		t.Fatalf("mid-turn pass must publish nothing, got %+v", f)
	case <-time.After(300 * time.Millisecond):
	}
}

// settleStore is the evidence store with the #1722 live-registry leg.
type settleStore struct {
	*evidenceStore
	live map[string]bool
}

func (s *settleStore) SessionStates(ctx context.Context) (map[string]SessionSeed, error) {
	states, err := s.evidenceStore.SessionStates(ctx)
	if err != nil {
		return nil, err
	}
	for sid, seed := range states {
		if s.live[sid] {
			seed.LiveBusy = true
			states[sid] = seed
		}
	}
	return states, nil
}

// TestReconcileEvidenceClear_LiveRegistryGateEndToEnd: the gate row
// through the full seed shape — LiveBusy=true blocks; LiveBusy=false
// (the registry cleared, the post-abort state) settles AND publishes.
func TestReconcileEvidenceClear_LiveRegistryGateEndToEnd(t *testing.T) {
	store := &settleStore{evidenceStore: &evidenceStore{states: map[string]abiv1.SessionStatus{"ses_g": abiv1.SessionStatus_SESSION_STATUS_IDLE}}, live: map[string]bool{"ses_g": true}}
	a := newReconcileAuthority(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, stop, err := a.Stream(ctx)
	require.NoError(t, err)
	defer stop()
	select {
	case <-frames:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot frame never arrived")
	}

	wedgeBusyWithRunningTool(t, a, "ses_g")
	drainFrames(t, frames, 2)

	// Pass 1 — live turn: nothing.
	require.Equal(t, ReconcileStats{}, a.Reconcile(context.Background()))
	require.True(t, a.State().Sessions["ses_g"].Busy)

	// Pass 2 — the registry cleared (the abort landed): settle + publish.
	store.mu.Lock()
	store.live["ses_g"] = false
	store.mu.Unlock()
	require.Equal(t, ReconcileStats{BusyCleared: 1}, a.Reconcile(context.Background()))
	require.False(t, a.State().Sessions["ses_g"].Busy)
	select {
	case f := <-frames:
		seq := f.GetEvent()
		require.NotNil(t, seq, "the settle publication")
		assert.Equal(t, abiv1.EventType_EVENT_TYPE_SESSION_STATUS, seq.GetEvent().GetType())
	case <-time.After(2 * time.Second):
		t.Fatal("#1722: registry-cleared settle was never published")
	}
}

// TestReconcileEvidenceClear_PublicationIdempotent: a converged
// projection publishes nothing on later passes — the settle emission is
// exactly-once per wedge, keeping the user stream quiet in steady state.
func TestReconcileEvidenceClear_PublicationIdempotent(t *testing.T) {
	store := &evidenceStore{states: map[string]abiv1.SessionStatus{"ses_a": abiv1.SessionStatus_SESSION_STATUS_IDLE}}
	a := newReconcileAuthority(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, stop, err := a.Stream(ctx)
	require.NoError(t, err)
	defer stop()
	select {
	case <-frames:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot frame never arrived")
	}

	wedgeBusyWithRunningTool(t, a, "ses_a")
	drainFrames(t, frames, 2)
	require.Equal(t, ReconcileStats{BusyCleared: 1}, a.Reconcile(context.Background()))
	// consume the settle publication
	select {
	case <-frames:
	case <-time.After(2 * time.Second):
		t.Fatal("first settle publication missing")
	}
	// second pass over converged state: no stat, no frame
	assert.Equal(t, ReconcileStats{}, a.Reconcile(context.Background()))
	select {
	case f := <-frames:
		t.Fatalf("converged pass must publish nothing, got %+v", f)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestRederiveSettle_LiveRegistryGatesSiblingLeg (#1727 r1 F1): the
// settle-signal invariant must hold on BOTH legs of the reconcile pass.
// rederiveStatuses (lease.go) runs right after the sweep and its
// IDLE/ERROR arm publishes SESSION_STATUS through applyLocked for any
// busy session whose busySince exceeds the lease bound — the silent
// 50-minute tool run class is past that bound after its first
// half-minute. With the live registry reporting the turn LIVE
// (LiveBusy=true), the sibling leg must neither clear nor publish —
// exactly the guarantee the sweep leg already carries. RED pre-fix:
// probe-verified the ungated sibling publishes IDLE mid-turn.
func TestRederiveSettle_LiveRegistryGatesSiblingLeg(t *testing.T) {
	store := &settleStore{
		evidenceStore: &evidenceStore{states: map[string]abiv1.SessionStatus{"ses_sib": abiv1.SessionStatus_SESSION_STATUS_IDLE}},
		live:          map[string]bool{"ses_sib": true},
	}
	a := newReconcileAuthority(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, stop, err := a.Stream(ctx)
	require.NoError(t, err)
	defer stop()
	select {
	case <-frames:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot frame never arrived")
	}

	wedgeBusyWithRunningTool(t, a, "ses_sib")
	drainFrames(t, frames, 2)

	// Age the busy mark past the lease bound (30s) — the silent long
	// turn's shape: no events, no busy re-marks, busySince stale.
	a.mu.Lock()
	a.sessions["ses_sib"].busySince = time.Now().Add(-(a.leaseBound() + time.Minute))
	a.mu.Unlock()

	// The pass: the sweep leg holds (LiveBusy), and the sibling
	// rederive leg must hold too.
	stats := a.Reconcile(context.Background())
	assert.Equal(t, ReconcileStats{}, stats, "live turn: neither leg settles")

	st := a.State()
	require.NotNil(t, st.Sessions["ses_sib"])
	assert.True(t, st.Sessions["ses_sib"].Busy,
		"the live-registry gate must hold the busy view on BOTH settle legs — the sibling rederive leg publishing mid-turn is the ungated mis-fire")

	select {
	case f := <-frames:
		t.Fatalf("mid-turn pass must publish nothing (the sibling leg fired): %+v", f)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestRederiveSettle_RegistryClearPublishesOnSiblingLeg (#1727 r1 F1):
// the gated sibling still settles once the registry clears — the
// aged-mark class converges through the rederive leg like the sweep's.
func TestRederiveSettle_RegistryClearPublishesOnSiblingLeg(t *testing.T) {
	store := &settleStore{
		evidenceStore: &evidenceStore{states: map[string]abiv1.SessionStatus{"ses_sib": abiv1.SessionStatus_SESSION_STATUS_IDLE}},
		live:          map[string]bool{"ses_sib": false},
	}
	a := newReconcileAuthority(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	frames, stop, err := a.Stream(ctx)
	require.NoError(t, err)
	defer stop()
	select {
	case <-frames:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot frame never arrived")
	}

	wedgeBusyWithRunningTool(t, a, "ses_sib")
	drainFrames(t, frames, 2)
	a.mu.Lock()
	a.sessions["ses_sib"].busySince = time.Now().Add(-(a.leaseBound() + time.Minute))
	a.mu.Unlock()

	// Registry clear + aged mark: both legs may run; the settle
	// publishes and the projection converges.
	stats := a.Reconcile(context.Background())
	require.Equal(t, ReconcileStats{BusyCleared: 1}, stats)
	require.False(t, a.State().Sessions["ses_sib"].Busy)
	select {
	case f := <-frames:
		seq := f.GetEvent()
		require.NotNil(t, seq, "the settle publication")
		assert.Equal(t, abiv1.EventType_EVENT_TYPE_SESSION_STATUS, seq.GetEvent().GetType())
	case <-time.After(2 * time.Second):
		t.Fatal("registry-cleared settle was never published")
	}
}
