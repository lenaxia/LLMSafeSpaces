// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate_test

// authority_orphan_sweep_test.go — #1342 S12 backstop: after a harness
// (opencode) restart, in-flight tool parts that never received terminal
// state are folded into the restored projection as aborted with the
// synthetic reason "harness restart". The interrupted/force-killed turn
// renders honestly (no eternal spinner) and the agent's next turn sees
// the tool died instead of a hole.

import (
	"context"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sweepStore is the minimal StoreReader for the sweep tests: fixed
// session seeds, no pending inputs, no message evidence.
type sweepStore struct {
	seeds map[string]sessionstate.SessionSeed
}

func (s *sweepStore) SessionStates(context.Context) (map[string]sessionstate.SessionSeed, error) {
	return s.seeds, nil
}

func (s *sweepStore) MessagePresence(_ context.Context, _ string, messageIDs []string) (map[string]bool, error) {
	present := map[string]bool{}
	for _, id := range messageIDs {
		present[id] = false
	}
	return present, nil
}

func (s *sweepStore) PendingInputs(context.Context) (map[string][]*abiv1.InputRequest, error) {
	return map[string][]*abiv1.InputRequest{}, nil
}

func newSweepAuthority(t *testing.T, seeds map[string]sessionstate.SessionSeed) *sessionstate.Authority {
	t.Helper()
	a, err := sessionstate.New(sessionstate.Config{
		Parser:      &sweepParser{},
		Store:       &sweepStore{seeds: seeds},
		Passwords:   []string{"pw"},
		PlatformDir: t.TempDir(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// sweepParser accepts nothing (events arrive via IngestForTest).
type sweepParser struct{}

func (*sweepParser) Parse([]byte) (*abiv1.Event, bool, error) { return nil, false, nil }

func runningToolPart(id, callID, name string) *abiv1.Part {
	return &abiv1.Part{
		Id:   id,
		Type: abiv1.PartType_PART_TYPE_TOOL,
		Payload: &abiv1.Part_Tool{Tool: &abiv1.ToolPart{
			CallId: callID,
			Name:   name,
			State:  &abiv1.ToolState{Status: abiv1.ToolStatus_TOOL_STATUS_RUNNING},
		}},
	}
}

func completedToolPart(id string) *abiv1.Part {
	return &abiv1.Part{
		Id:   id,
		Type: abiv1.PartType_PART_TYPE_TOOL,
		Payload: &abiv1.Part_Tool{Tool: &abiv1.ToolPart{
			CallId: "call_done",
			Name:   "bash",
			State:  &abiv1.ToolState{Status: abiv1.ToolStatus_TOOL_STATUS_COMPLETED},
		}},
	}
}

func toolPartState(t *testing.T, a *sessionstate.Authority, sessionID, partID string) *abiv1.ToolState {
	t.Helper()
	snap := a.State()
	rec, ok := snap.Sessions[sessionID]
	require.True(t, ok, "session %s missing from projection", sessionID)
	for _, p := range rec.InFlightParts {
		if p.GetId() == partID {
			tool, ok := p.GetPayload().(*abiv1.Part_Tool)
			require.True(t, ok, "part %s is not a tool part", partID)
			return tool.Tool.GetState()
		}
	}
	return nil
}

// TestReseedGenerationChange_SweepsRunningToolPartsAborted: the core
// backstop — a running tool part orphaned by the harness restart is
// restored as aborted with the synthetic "harness restart" reason.
func TestReseedGenerationChange_SweepsRunningToolPartsAborted(t *testing.T) {
	seeds := map[string]sessionstate.SessionSeed{
		"ses_a": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	}
	a := newSweepAuthority(t, seeds)
	a.IngestForTest(&abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_MESSAGE_START, SessionId: "ses_a",
		Message: &abiv1.Message{Id: "msg_1", Parts: []*abiv1.Part{runningToolPart("prt_run", "call_1", "bash")}},
	})

	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange))

	st := toolPartState(t, a, "ses_a", "prt_run")
	require.NotNil(t, st, "orphaned running part must survive the reseed in the restored projection")
	assert.Equal(t, abiv1.ToolStatus_TOOL_STATUS_ERROR, st.GetStatus(),
		"orphaned running part must be terminal (error) after the sweep")
	assert.Equal(t, sessionstate.OrphanSweepReason, st.GetError(),
		"the synthetic abort reason must name the harness restart")
	assert.NotNil(t, st.GetCompletedAt(), "the synthetic terminal state must carry a completion timestamp")
}

// TestReseedGenerationChange_LeavesTerminalPartsAlone: a part that
// already reached a terminal state before the restart is never
// re-marked by the sweep. The reseed swap drops in-flight view parts
// (store truth owns history; live clients re-snapshot on the reseeded
// frame) — the sweep must not resurrect a completed part as aborted.
func TestReseedGenerationChange_LeavesTerminalPartsAlone(t *testing.T) {
	seeds := map[string]sessionstate.SessionSeed{
		"ses_a": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	}
	a := newSweepAuthority(t, seeds)
	a.IngestForTest(&abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_MESSAGE_START, SessionId: "ses_a",
		Message: &abiv1.Message{Id: "msg_1", Parts: []*abiv1.Part{completedToolPart("prt_done")}},
	})

	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange))

	snap := a.State()
	for _, p := range snap.Sessions["ses_a"].InFlightParts {
		tool, ok := p.GetPayload().(*abiv1.Part_Tool)
		if !ok {
			continue
		}
		assert.NotEqual(t, sessionstate.OrphanSweepReason, tool.Tool.GetState().GetError(),
			"a terminal part must never carry the sweep reason")
		assert.NotEqual(t, abiv1.ToolStatus_TOOL_STATUS_ERROR, tool.Tool.GetState().GetStatus(),
			"a completed part must not be re-marked by the sweep")
	}
}

// TestReseedGenerationChange_PendingToolPartIsSwept: PENDING (never
// started running) is also non-terminal — swept.
func TestReseedGenerationChange_PendingToolPartIsSwept(t *testing.T) {
	seeds := map[string]sessionstate.SessionSeed{
		"ses_a": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	}
	a := newSweepAuthority(t, seeds)
	a.IngestForTest(&abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_MESSAGE_START, SessionId: "ses_a",
		Message: &abiv1.Message{Id: "msg_1", Parts: []*abiv1.Part{{
			Id:   "prt_pending",
			Type: abiv1.PartType_PART_TYPE_TOOL,
			Payload: &abiv1.Part_Tool{Tool: &abiv1.ToolPart{
				CallId: "call_p", Name: "bash",
				State: &abiv1.ToolState{Status: abiv1.ToolStatus_TOOL_STATUS_PENDING},
			}},
		}}},
	})

	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange))

	st := toolPartState(t, a, "ses_a", "prt_pending")
	require.NotNil(t, st)
	assert.Equal(t, abiv1.ToolStatus_TOOL_STATUS_ERROR, st.GetStatus())
	assert.Equal(t, sessionstate.OrphanSweepReason, st.GetError())
}

// TestReseedGenerationChange_NoStateToolPartIsSwept: a tool part with
// NO state at all (nil) is non-terminal — swept.
func TestReseedGenerationChange_NoStateToolPartIsSwept(t *testing.T) {
	seeds := map[string]sessionstate.SessionSeed{
		"ses_a": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	}
	a := newSweepAuthority(t, seeds)
	a.IngestForTest(&abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_MESSAGE_START, SessionId: "ses_a",
		Message: &abiv1.Message{Id: "msg_1", Parts: []*abiv1.Part{{
			Id:      "prt_nostate",
			Type:    abiv1.PartType_PART_TYPE_TOOL,
			Payload: &abiv1.Part_Tool{Tool: &abiv1.ToolPart{CallId: "call_n", Name: "bash"}},
		}}},
	})

	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange))

	st := toolPartState(t, a, "ses_a", "prt_nostate")
	require.NotNil(t, st)
	assert.Equal(t, abiv1.ToolStatus_TOOL_STATUS_ERROR, st.GetStatus())
	assert.Equal(t, sessionstate.OrphanSweepReason, st.GetError())
}

// TestReseedGenerationChange_SessionAbsentFromStoreNotResurrected: a
// session the store no longer holds is not resurrected by the sweep —
// its parts died with the generation and there is no projection to
// restore into.
func TestReseedGenerationChange_SessionAbsentFromStoreNotResurrected(t *testing.T) {
	seeds := map[string]sessionstate.SessionSeed{
		"ses_live": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	}
	a := newSweepAuthority(t, seeds)
	a.IngestForTest(&abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_MESSAGE_START, SessionId: "ses_dead",
		Message: &abiv1.Message{Id: "msg_1", Parts: []*abiv1.Part{runningToolPart("prt_dead", "call_d", "bash")}},
	})

	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange))

	snap := a.State()
	assert.NotContains(t, snap.Sessions, "ses_dead",
		"the sweep must not resurrect a session the store no longer holds")
	assert.Empty(t, toolPartState(t, a, "ses_live", "prt_dead"),
		"the dead session's part must not land in another session")
}

// TestReseedStallWake_DoesNotSweepLiveParts: a stall-wake reseed is NOT
// a harness restart — in-flight parts are live turn state and must be
// left alone (the store refresh will converge them).
func TestReseedStallWake_DoesNotSweepLiveParts(t *testing.T) {
	seeds := map[string]sessionstate.SessionSeed{
		"ses_a": {Status: abiv1.SessionStatus_SESSION_STATUS_BUSY},
	}
	a := newSweepAuthority(t, seeds)
	a.IngestForTest(&abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_MESSAGE_START, SessionId: "ses_a",
		Message: &abiv1.Message{Id: "msg_1", Parts: []*abiv1.Part{runningToolPart("prt_live", "call_1", "bash")}},
	})

	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonStallWake))

	// The stall-wake reseed swaps the projection from store truth — the
	// live part is NOT re-folded as aborted. Whatever the swap left, no
	// synthetic "harness restart" abort may exist.
	snap := a.State()
	for sid, rec := range snap.Sessions {
		for _, p := range rec.InFlightParts {
			if tool, ok := p.GetPayload().(*abiv1.Part_Tool); ok {
				assert.NotEqual(t, sessionstate.OrphanSweepReason, tool.Tool.GetState().GetError(),
					"session %s part %s must not carry the sweep reason after a stall-wake reseed", sid, p.GetId())
			}
		}
	}
}

// TestReseedGenerationChange_StreamCarriesAbortedPartEnd: subscribers
// observe the reseed notice followed by a PART_END frame carrying the
// aborted state — the UI's honest signal.
func TestReseedGenerationChange_StreamCarriesAbortedPartEnd(t *testing.T) {
	seeds := map[string]sessionstate.SessionSeed{
		"ses_a": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	}
	a := newSweepAuthority(t, seeds)
	a.IngestForTest(&abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_MESSAGE_START, SessionId: "ses_a",
		Message: &abiv1.Message{Id: "msg_1", Parts: []*abiv1.Part{runningToolPart("prt_run", "call_1", "bash")}},
	})

	frames, cancel, err := a.Stream(context.Background())
	require.NoError(t, err)
	defer cancel()

	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange))

	// Snapshot frame, then reseeded, then the aborted part end.
	deadline := time.After(3 * time.Second)
	var sawReseed, sawAbortedEnd bool
	for !sawReseed || !sawAbortedEnd {
		select {
		case f := <-frames:
			switch fr := f.GetFrame().(type) {
			case *abiv1.StreamFrame_Snapshot:
			case *abiv1.StreamFrame_Reseeded:
				sawReseed = true
			case *abiv1.StreamFrame_Event:
				evt := fr.Event.GetEvent()
				if evt.GetType() == abiv1.EventType_EVENT_TYPE_PART_END {
					tool, ok := evt.GetPart().GetPayload().(*abiv1.Part_Tool)
					if ok && tool.Tool.GetState().GetError() == sessionstate.OrphanSweepReason {
						sawAbortedEnd = true
					}
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for stream frames (reseed=%v abortedEnd=%v)", sawReseed, sawAbortedEnd)
		}
	}
}

// TestReseedGenerationChange_TextPartsNotResurrected: the sweep is tool
// parts only (the phantom-spinner class); a dangling partial text part
// is not re-folded as aborted.
func TestReseedGenerationChange_TextPartsNotResurrected(t *testing.T) {
	seeds := map[string]sessionstate.SessionSeed{
		"ses_a": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	}
	a := newSweepAuthority(t, seeds)
	a.IngestForTest(&abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_PART_START, SessionId: "ses_a",
		Part: &abiv1.Part{Id: "prt_text", Type: abiv1.PartType_PART_TYPE_TEXT,
			Payload: &abiv1.Part_Text{Text: "partial..."}},
	})

	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange))

	snap := a.State()
	for _, p := range snap.Sessions["ses_a"].InFlightParts {
		assert.NotEqual(t, abiv1.PartType_PART_TYPE_TOOL, p.GetType(),
			"text parts must not be re-folded by the tool-part sweep")
	}
}

// TestReseedGenerationChange_SweepCountsInMetrics: the cumulative
// orphan-sweep counter is observable (ops surface for the incident
// class).
func TestReseedGenerationChange_SweepCountsInMetrics(t *testing.T) {
	seeds := map[string]sessionstate.SessionSeed{
		"ses_a": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	}
	a := newSweepAuthority(t, seeds)
	a.IngestForTest(&abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_MESSAGE_START, SessionId: "ses_a",
		Message: &abiv1.Message{Id: "msg_1", Parts: []*abiv1.Part{
			runningToolPart("prt_run", "call_1", "bash"),
			completedToolPart("prt_done"),
		}},
	})

	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange))

	m := a.Metrics()
	assert.Equal(t, int64(1), m.OrphanPartsAborted,
		"exactly the one non-terminal part is counted (the completed one is not)")
}
