// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

// reconcile_declared_timeout_test.go — #1576 layer 2 (TDD: authored
// before the implementation).
//
// The wedge of 2026-09-25: a bash tool carrying timeout=700000ms sat
// RUNNING 30+ minutes after its executor died — no event ever ends a
// part whose process vanished without a trace. Layer 2's rule is the
// model's own contract: a tool part past ITS OWN DECLARED timeout is
// dead — terminate it and let the turn fail visibly. Zero heuristics:
// no declared timeout → no action (layer 3's process liveness owns
// that class).

import (
	"context"
	"testing"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toolPartWithTimeout builds a RUNNING tool part whose INPUT declares
// a timeout in milliseconds (the harness bash contract).
func toolPartWithTimeout(id, callID string, timeoutMs int) *abiv1.Part {
	var input []byte
	if timeoutMs > 0 {
		input = []byte(`{"command":"go test ./...","timeout":` + itoa(timeoutMs) + `}`)
	} else {
		input = []byte(`{"command":"ls"}`)
	}
	return &abiv1.Part{
		Id:   id,
		Type: abiv1.PartType_PART_TYPE_TOOL,
		Payload: &abiv1.Part_Tool{Tool: &abiv1.ToolPart{
			CallId: callID,
			Name:   "bash",
			Input:  input,
			State:  &abiv1.ToolState{Status: abiv1.ToolStatus_TOOL_STATUS_RUNNING},
		}},
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestDeclaredTimeout_RunningPartPastDeadlineTerminated: a RUNNING
// tool part past its own declared deadline folds terminal (ERROR, the
// declared-timeout reason, a completed stamp) — the turn fails
// visibly and the busy derivation loses the part leg.
func TestDeclaredTimeout_RunningPartPastDeadlineTerminated(t *testing.T) {
	a := newBareAuthority(t)
	rec := a.sessions["s1"]
	require.NotNil(t, rec)

	started := time.Now().Add(-10 * time.Minute)
	p := toolPartWithTimeout("p1", "c1", 60000) // declared: 60s
	p.GetPayload().(*abiv1.Part_Tool).Tool.State.StartedAt = tsp(started)
	a.upsertPartLocked(rec, p)

	// The deadline registered at upsert.
	dl, ok := rec.toolDeadlines["p1"]
	require.True(t, ok, "a declared timeout must register at upsert")
	assert.WithinDuration(t, started.Add(60*time.Second), dl, 2*time.Second)

	stats := a.enforceDeclaredTimeoutsLocked(started.Add(11 * time.Minute))
	assert.Equal(t, int64(1), stats, "one past-deadline part terminated")

	tool := rec.inFly[0].GetPayload().(*abiv1.Part_Tool).Tool
	assert.Equal(t, abiv1.ToolStatus_TOOL_STATUS_ERROR, tool.GetState().GetStatus())
	assert.Equal(t, DeclaredTimeoutReason, tool.GetState().GetError())
	assert.NotNil(t, tool.GetState().GetCompletedAt(), "terminal fold stamps completion")
	assert.NotContains(t, rec.toolDeadlines, "p1", "terminal part's deadline clears")
}

// TestDeclaredTimeout_NoDeclarationNoAction: no declared timeout → no
// deadline, no action — however long the part runs (zero heuristics).
func TestDeclaredTimeout_NoDeclarationNoAction(t *testing.T) {
	a := newBareAuthority(t)
	rec := a.sessions["s1"]

	started := time.Now().Add(-24 * time.Hour)
	p := toolPartWithTimeout("p1", "c1", 0) // no timeout in the input
	p.GetPayload().(*abiv1.Part_Tool).Tool.State.StartedAt = tsp(started)
	a.upsertPartLocked(rec, p)

	assert.NotContains(t, rec.toolDeadlines, "p1")
	stats := a.enforceDeclaredTimeoutsLocked(time.Now())
	assert.Equal(t, int64(0), stats)
	assert.Equal(t, abiv1.ToolStatus_TOOL_STATUS_RUNNING,
		rec.inFly[0].GetPayload().(*abiv1.Part_Tool).Tool.GetState().GetStatus())
}

// TestDeclaredTimeout_NotYetPastDeadlineHolds: a part still inside its
// declared window is untouched (layer 2 never acts inside the model's
// own contract).
func TestDeclaredTimeout_NotYetPastDeadlineHolds(t *testing.T) {
	a := newBareAuthority(t)
	rec := a.sessions["s1"]

	started := time.Now()
	p := toolPartWithTimeout("p1", "c1", 600000) // 10 minutes
	p.GetPayload().(*abiv1.Part_Tool).Tool.State.StartedAt = tsp(started)
	a.upsertPartLocked(rec, p)

	stats := a.enforceDeclaredTimeoutsLocked(started.Add(1 * time.Minute))
	assert.Equal(t, int64(0), stats)
	assert.Equal(t, abiv1.ToolStatus_TOOL_STATUS_RUNNING,
		rec.inFly[0].GetPayload().(*abiv1.Part_Tool).Tool.GetState().GetStatus())
}

// TestDeclaredTimeout_CompletedPartUntouched: a terminal part keeps
// its own state (only RUNNING parts are layer-2's business).
func TestDeclaredTimeout_CompletedPartUntouched(t *testing.T) {
	a := newBareAuthority(t)
	rec := a.sessions["s1"]

	p := toolPartWithTimeout("p1", "c1", 60000)
	tool := p.GetPayload().(*abiv1.Part_Tool).Tool
	tool.State.Status = abiv1.ToolStatus_TOOL_STATUS_COMPLETED
	tool.State.StartedAt = tsp(time.Now().Add(-10 * time.Minute))
	a.upsertPartLocked(rec, p)

	stats := a.enforceDeclaredTimeoutsLocked(time.Now().Add(time.Hour))
	assert.Equal(t, int64(0), stats)
	assert.Equal(t, abiv1.ToolStatus_TOOL_STATUS_COMPLETED,
		rec.inFly[0].GetPayload().(*abiv1.Part_Tool).Tool.GetState().GetStatus())
}

// newBareAuthority builds the minimal Authority the layer-2 unit calls
// need (sessions map only — no store, no parser; the enforcement step
// is store-independent by design).
func newBareAuthority(t *testing.T) *Authority {
	t.Helper()
	return &Authority{
		sessions: map[string]*sessionRecord{"s1": newSessionRecord(abiv1.SessionStatus_SESSION_STATUS_IDLE)},
	}
}

// TestDeclaredTimeout_ReconcileLevel_StoreBackedAuthority (r1's
// missing level): the fold must fire through the PUBLIC Reconcile on a
// store-backed authority — the production shape. This is the test that
// catches gate-order regressions (the fold once sat below the
// Store==nil return, making its store-independence comments false) and
// pins the fold's STATED busy effect end to end: a session observed
// busy with a wedged past-deadline part reconciles to NOT busy with
// the part folded terminal and still renderable.
func TestDeclaredTimeout_ReconcileLevel_StoreBackedAuthority(t *testing.T) {
	store := &evidenceStore{
		states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_BUSY},
		msgs:   map[string]map[string]bool{},
	}
	a := newReconcileAuthority(t, store)
	// Seed s1 directly into the sessions map (the store-wired
	// authority's map is otherwise populated from unresolved ledger
	// rows — the wedge scenario deliberately has none); the STORE
	// stays wired, which is the point: the fold must run on the
	// public pass BEFORE the store gate on a production-shaped
	// authority.
	a.mu.Lock()
	a.sessions["s1"] = newSessionRecord(abiv1.SessionStatus_SESSION_STATUS_BUSY)
	rec := a.sessions["s1"]
	rec.busy = true // the observed wedge: busy with a dead tool leg
	started := time.Now().Add(-30 * time.Minute)
	p := toolPartWithTimeout("p1", "c1", 60000)
	p.GetPayload().(*abiv1.Part_Tool).Tool.State.StartedAt = tsp(started)
	a.upsertPartLocked(rec, p)
	a.mu.Unlock()

	// BUSY before: the dead leg is the only activity.
	pre := a.State()
	require.True(t, pre.Sessions["s1"].Busy, "pre-reconcile: the wedged part holds the session busy")

	stats := a.Reconcile(context.Background())
	assert.Equal(t, 1, stats.DeclaredTimeouts, "the fold ran through the public pass and counted")

	post := a.State()
	sv := post.Sessions["s1"]
	assert.False(t, sv.Busy, "post-reconcile: the terminal part no longer holds busy (the fold's stated effect)")
	// The part is STILL RENDERABLE — the fold's whole point is visible
	// failure, not disappearance.
	var toolStatus abiv1.ToolStatus
	for _, part := range sv.InFlightParts {
		if part.GetId() == "p1" {
			toolStatus = part.GetPayload().(*abiv1.Part_Tool).Tool.GetState().GetStatus()
		}
	}
	assert.Equal(t, abiv1.ToolStatus_TOOL_STATUS_ERROR, toolStatus,
		"the folded part renders its ERROR state")
}

// The hold's OTHER direction (pinned both ways): a REAL harness event
// after the fold clears foldedDead — the store's BUSY is live truth
// again for the recovered session (a new turn re-marks busy normally).
func TestDeclaredTimeout_FoldHoldClearsOnRealEvent(t *testing.T) {
	store := &evidenceStore{
		states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_BUSY},
		msgs:   map[string]map[string]bool{},
	}
	a := newReconcileAuthority(t, store)
	a.mu.Lock()
	a.sessions["s1"] = newSessionRecord(abiv1.SessionStatus_SESSION_STATUS_BUSY)
	rec := a.sessions["s1"]
	rec.busy = true
	p := toolPartWithTimeout("p1", "c1", 60000)
	p.GetPayload().(*abiv1.Part_Tool).Tool.State.StartedAt = tsp(time.Now().Add(-30 * time.Minute))
	a.upsertPartLocked(rec, p)
	a.mu.Unlock()

	_ = a.Reconcile(context.Background()) // folds; foldedDead holds
	require.True(t, rec.foldedDead, "the fold marked the session dead")
	post := a.State().Sessions["s1"]
	require.False(t, post.Busy, "the stale store BUSY does not re-latch")

	// The harness speaks again (a real status event through the
	// contract path — exactly what a recovered/new turn produces).
	a.mu.Lock()
	a.applyLocked(&abiv1.Event{SessionId: "s1", Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
		Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	a.mu.Unlock()
	require.False(t, rec.foldedDead, "a real event clears the fold-dead hold")
	assert.True(t, a.State().Sessions["s1"].Busy, "live harness truth re-marks busy through the normal path")
}

// N1 regression (r2's crash finding, reviewer-reproduced): the
// evidence-busy-clear empties toolDeadlines; the record SURVIVES; a
// new turn in the same session upserts a declared-timeout tool part —
// the deadline write must not panic on the cleared map (the sidecar's
// Ingest recover wall covers the parser only, so a panic here killed
// the process).
func TestDeclaredTimeout_DeadlineWriteAfterEvidenceClear_NoPanic(t *testing.T) {
	store := &evidenceStore{
		states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE},
		msgs:   map[string]map[string]bool{},
	}
	a := newReconcileAuthority(t, store)
	a.mu.Lock()
	a.sessions["s1"] = newSessionRecord(abiv1.SessionStatus_SESSION_STATUS_BUSY)
	rec := a.sessions["s1"]
	rec.busy = true
	p := toolPartWithTimeout("p1", "c1", 0) // no deadline needed for the clear path
	a.upsertPartLocked(rec, p)
	a.mu.Unlock()

	// The evidence sweep's busy-clear on the record (direct call — it
	// takes a.mu internally; the sweep requires ledger rows, the clear
	// itself is the crash site's caller) empties inFly +
	// toolDeadlines; the record survives.
	require.Equal(t, 1, a.clearBusyFromEvidence("s1", abiv1.SessionStatus_SESSION_STATUS_IDLE, 0))
	require.NotNil(t, a.sessions["s1"])

	// The new turn's declared-timeout upsert on the SURVIVING record:
	// must register cleanly, not panic.
	a.mu.Lock()
	p2 := toolPartWithTimeout("p2", "c2", 60000)
	p2.GetPayload().(*abiv1.Part_Tool).Tool.State.StartedAt = tsp(time.Now())
	a.upsertPartLocked(a.sessions["s1"], p2)
	deadlines := len(a.sessions["s1"].toolDeadlines)
	a.mu.Unlock()
	assert.Equal(t, 1, deadlines, "the new deadline registered on the cleared record")
}

// tsp is the test timestamp helper.
func tsp(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }
