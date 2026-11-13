// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pi_state_test.go — the settle-contract and statusz-parity pins
// (acceptance criterion 1) at the unit level, with an injected clock
// for exact busy-age/progress math. The wire shapes come from the
// scripted fixture file (pi_fake_test.go documents the provenance);
// the end-to-end versions against the real subprocess live in
// pi_policy_test.go.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// piTestEvent builds one raw session-event record and its decoded form.
func piTestEvent(t *testing.T, raw string) (*piRecord, []byte) {
	t.Helper()
	rec := &piRecord{}
	require.NoError(t, json.Unmarshal([]byte(raw), rec))
	rec.Raw = []byte(raw)
	return rec, []byte(raw)
}

func TestPiSettleContract_AgentEndIsNotSettle(t *testing.T) {
	agg := newPiStateAggregator()
	t0 := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	s := agg.cell("s1")

	// Prompt accepted → agent_start → busy.
	apply, _ := piTestEvent(t, `{"type":"agent_start"}`)
	s.applyEvent(apply, nil, t0)
	require.True(t, s.busy, "agent_start must mark busy")

	// The validated retry shape: THREE agent_end records (willRetry
	// true) with auto_retry between them and NO agent_settled — the
	// real binary emitted exactly this while get_state kept reporting
	// isStreaming:true. Busy must survive all of them.
	for i := 0; i < 3; i++ {
		end, _ := piTestEvent(t, `{"type":"agent_end","willRetry":true}`)
		s.applyEvent(end, nil, t0.Add(time.Duration(i+1)*time.Second))
		require.True(t, s.busy, "agent_end (willRetry) must NOT settle — run %d", i+1)
	}
	require.Equal(t, 3, s.runsEnded, "agent_end count is diagnostic")

	snap := agg.Snapshot(t0.Add(10 * time.Second))
	require.Equal(t, 1, snap.SessionsActive)
	require.Equal(t, 10, snap.OldestBusySeconds, "busy age accrues through retries")
	require.Equal(t, 10, snap.BusyAges["s1"])

	// agent_settled is the ONLY idle transition.
	settled, _ := piTestEvent(t, `{"type":"agent_settled","aborted":false}`)
	s.applyEvent(settled, nil, t0.Add(11*time.Second))
	require.False(t, s.busy)

	snap = agg.Snapshot(t0.Add(12 * time.Second))
	require.Equal(t, 0, snap.SessionsActive)
	require.Equal(t, 0, snap.OldestBusySeconds)
	require.Empty(t, snap.BusyAges, "settled sessions stop contributing busy age immediately")
	require.Equal(t, "idle", snap.Sessions[0].Status)
}

func TestPiSnapshotParity_AllFields(t *testing.T) {
	agg := newPiStateAggregator()
	now := time.Date(2026, 11, 13, 10, 30, 0, 0, time.UTC)

	// s1: busy for 90s, last completed message 15s ago (#1685 stamp).
	s1 := agg.cell("s1")
	start, _ := piTestEvent(t, `{"type":"agent_start"}`)
	s1.applyEvent(start, nil, now.Add(-90*time.Second))
	userEnd := `{"type":"message_end","message":{"role":"user","text":"go"}}`
	u, rawU := piTestEvent(t, userEnd)
	s1.applyEvent(u, rawU, now.Add(-90*time.Second))
	assistantEnd := `{"type":"message_end","message":{"role":"assistant","usage":{"input":120,"output":80,"cacheRead":40,"cacheWrite":10}}}`
	a, rawA := piTestEvent(t, assistantEnd)
	s1.applyEvent(a, rawA, now.Add(-15*time.Second))

	// s2: completed turn (settled) with model + context from polls.
	s2 := agg.cell("s2")
	s2Start, _ := piTestEvent(t, `{"type":"agent_start"}`)
	s2.applyEvent(s2Start, nil, now.Add(-5*time.Minute))
	s2Settled, _ := piTestEvent(t, `{"type":"agent_settled","aborted":false}`)
	s2.applyEvent(s2Settled, nil, now.Add(-4*time.Minute))
	s2.applyStatePoll(json.RawMessage(`{
		"sessionId":"s2","sessionName":"Refactor","isStreaming":false,
		"model":{"id":"glm","provider":"relay","contextWindow":200000}}`), now)
	s2.applyStatsPoll(json.RawMessage(`{
		"tokens":{"input":1000,"output":500,"cacheRead":50,"cacheWrite":25},
		"contextUsage":{"tokens":1550,"contextWindow":200000}}`))

	// s3: zero-cell (persisted, never attached) — renders idle.
	agg.cell("s3")

	snap := agg.Snapshot(now)
	require.Len(t, snap.Sessions, 3)
	require.Equal(t, "s1", snap.Sessions[0].ID)
	require.Equal(t, "busy", snap.Sessions[0].Status)
	require.Equal(t, "idle", snap.Sessions[1].Status)
	require.Equal(t, "idle", snap.Sessions[2].Status)

	// Parity targets: []agentd.SessionInfo{ID,Title,Status,Tokens,
	// Model,ContextUsed} + OldestBusySeconds/BusyAges/
	// LastProgressSeconds (types.go #391-398 / #466-477).
	require.Equal(t, 90, snap.OldestBusySeconds)
	require.Equal(t, map[string]int{"s1": 90}, snap.BusyAges)
	require.Equal(t, 15, snap.LastProgressSeconds, "age of the last completed message on the longest-quiet busy session")

	s1Info := snap.Sessions[0]
	require.NotNil(t, s1Info.Tokens)
	require.Equal(t, int64(120), s1Info.Tokens.Input)
	require.Equal(t, int64(80), s1Info.Tokens.Output)
	require.Equal(t, int64(40), s1Info.Tokens.CacheRead)
	require.Equal(t, int64(10), s1Info.Tokens.CacheWrite)

	s2Info := snap.Sessions[1]
	require.Equal(t, "Refactor", s2Info.Title)
	require.Equal(t, "relay/glm", s2Info.Model)
	require.Equal(t, int64(1550), s2Info.ContextUsed)
	require.NotNil(t, s2Info.Tokens)
	require.Equal(t, int64(1000), s2Info.Tokens.Input)

	require.NotNil(t, snap.Context, "aggregate context window renders")
	require.Equal(t, int64(200000), snap.Context.TotalTokens)
}

func TestPiLastProgress_MessageLevelStamps(t *testing.T) {
	agg := newPiStateAggregator()
	now := time.Date(2026, 11, 13, 11, 0, 0, 0, time.UTC)
	s := agg.cell("s1")
	start, _ := piTestEvent(t, `{"type":"agent_start"}`)
	s.applyEvent(start, nil, now.Add(-100*time.Second))

	// message_end user at -60s.
	ue, ueRaw := piTestEvent(t, `{"type":"message_end","message":{"role":"user"}}`)
	s.applyEvent(ue, ueRaw, now.Add(-60*time.Second))
	// Stream chunks and tool events do NOT stamp (#1685: message-level
	// only). agent_end does not stamp either.
	upd, _ := piTestEvent(t, `{"type":"message_update","assistantMessageEvent":{"type":"delta"}}`)
	s.applyEvent(upd, nil, now.Add(-30*time.Second))
	tool, _ := piTestEvent(t, `{"type":"tool_execution_start","toolCallId":"t1"}`)
	s.applyEvent(tool, nil, now.Add(-20*time.Second))
	end, _ := piTestEvent(t, `{"type":"agent_end","willRetry":true}`)
	s.applyEvent(end, nil, now.Add(-10*time.Second))
	require.Equal(t, 60, agg.Snapshot(now).LastProgressSeconds)

	// A completed assistant message re-stamps.
	ae, aeRaw := piTestEvent(t, `{"type":"message_end","message":{"role":"assistant","usage":{"input":1,"output":1}}}`)
	s.applyEvent(ae, aeRaw, now.Add(-5*time.Second))
	require.Equal(t, 5, agg.Snapshot(now).LastProgressSeconds)

	// No busy session → 0 even with history.
	settled, _ := piTestEvent(t, `{"type":"agent_settled"}`)
	s.applyEvent(settled, nil, now)
	require.Equal(t, 0, agg.Snapshot(now).LastProgressSeconds)
}

func TestPiUnknownEventsTolerated_TopLevelPayloadsDecoded(t *testing.T) {
	agg := newPiStateAggregator()
	now := time.Now()
	s := agg.cell("s1")

	// Unknown event family: a no-op, never an error.
	unk, _ := piTestEvent(t, `{"type":"future_event_shape","payload":{"x":1}}`)
	s.applyEvent(unk, nil, now)

	// session_info_changed carries name at the envelope TOP level.
	nameEv, nameRaw := piTestEvent(t, `{"type":"session_info_changed","name":"Renamed by user"}`)
	s.applyEvent(nameEv, nameRaw, now)
	require.Equal(t, "Renamed by user", s.title)

	// The response-family guard: a record of type "response" never
	// reaches the pump (piChild.dispatch routes by family), but the
	// state decode of a stray "response"-typed line must not corrupt
	// state either.
	respLike, _ := piTestEvent(t, `{"type":"response","id":"x","success":true}`)
	s.applyEvent(respLike, nil, now) // no relevant handler → no-op
	require.False(t, s.busy)
}

func TestPiStatePollReconcilesBothDirections(t *testing.T) {
	agg := newPiStateAggregator()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	s := agg.cell("s1")

	// Missed agent_start (restart gap swallowed it): isStreaming=true
	// while the pump is idle → the poll marks busy.
	s.applyStatePoll(json.RawMessage(`{"isStreaming":true,"sessionId":"s1"}`), now)
	require.True(t, s.busy)

	// Swallowed agent_settled: pump busy, poll says idle → idle.
	s.applyStatePoll(json.RawMessage(`{"isStreaming":false,"sessionId":"s1"}`), now.Add(2*time.Second))
	require.False(t, s.busy)
}

func TestPiGenerationStartClearsEpisode_KeepsPersistedTruth(t *testing.T) {
	agg := newPiStateAggregator()
	now := time.Date(2026, 11, 13, 12, 30, 0, 0, time.UTC)
	s := agg.cell("s1")
	start, _ := piTestEvent(t, `{"type":"agent_start"}`)
	s.applyEvent(start, nil, now.Add(-100*time.Second))
	end, _ := piTestEvent(t, `{"type":"agent_end","willRetry":true}`)
	s.applyEvent(end, nil, now.Add(-50*time.Second))
	s.title = "Persisted title"
	s.model = "relay/glm"
	s.contextUsed = 5000
	ae, aeRaw := piTestEvent(t, `{"type":"message_end","message":{"role":"assistant","usage":{"input":10,"output":5}}}`)
	s.applyEvent(ae, aeRaw, now.Add(-40*time.Second))

	// The #1632 generation contract: the respawned child cannot be
	// mid-turn; the dead generation's busy episode is void.
	agg.GenerationStart("s1", 2)

	require.False(t, s.busy)
	require.Zero(t, s.busySince)
	require.Equal(t, 0, s.runsEnded)
	snap := agg.Snapshot(now)
	require.Equal(t, 0, snap.OldestBusySeconds, "busy age must not survive a restart")

	// Persisted facts survive (the new child resumes them from disk).
	require.Equal(t, "Persisted title", snap.Sessions[0].Title)
	require.Equal(t, "relay/glm", snap.Sessions[0].Model)
	require.Equal(t, int64(5000), snap.Sessions[0].ContextUsed)
	require.NotNil(t, snap.Sessions[0].Tokens)
	require.Equal(t, int64(10), snap.Sessions[0].Tokens.Input)
}

func TestPiMessageEndMalformedTolerated(t *testing.T) {
	s := &piSessionState{ID: "s1"}
	now := time.Now()
	// message_end without a message object, with malformed inner JSON,
	// and with a system role (no stamp, no usage).
	for _, raw := range []string{
		`{"type":"message_end"}`,
		`{"type":"message_end","message":"not-an-object"}`,
		`{"type":"message_end","message":{"role":"system"}}`,
	} {
		rec, recRaw := piTestEvent(t, raw)
		s.applyMessageEnd(rec, now)
		s.applyEvent(rec, recRaw, now)
	}
	require.Zero(t, s.lastMessageAt)
	require.Zero(t, s.tokens.Output)
}

func TestPiSnapshotSessionInfoShapes(t *testing.T) {
	// The parity target is agentd's own struct: the snapshot must be
	// assignable without conversion. Compiled-shape pin.
	info := agentd.SessionInfo{
		ID: "s", Title: "t", Status: "busy", Model: "m", ContextUsed: 1,
		Tokens: &agentd.SessionTokens{Input: 1, Output: 2, Reasoning: 0, CacheRead: 3, CacheWrite: 4},
	}
	require.Equal(t, "busy", info.Status)
	snap := piStatusSnapshot{
		Sessions: []agentd.SessionInfo{info}, OldestBusySeconds: 1,
		BusyAges: map[string]int{"s": 1}, LastProgressSeconds: 2,
	}
	require.Equal(t, 1, snap.OldestBusySeconds)
}
