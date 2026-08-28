// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// --- V2 session.next.* translation (design 0052) ---
// Payloads below are verbatim from the live 1.18.15 capture (IDs
// swapped for synthetic equals-length values; REFRESH.md provenance).

const nextPromptedRaw = `{"id":"evt_1","type":"session.next.prompted","properties":{"sessionID":"ses_1","timestamp":"2026-08-27T23:38:02.178Z","messageID":"msg_1","prompt":{"text":"fourth queued message"},"delivery":"queue"}}`

const nextTextDeltaRaw = `{"id":"evt_2","type":"session.next.text.delta","properties":{"sessionID":"ses_1","assistantMessageID":"msg_2","timestamp":"2026-08-27T23:38:05.051Z","textID":"text-0","delta":"mock-reply-10: "}}`

const nextTextEndedRaw = `{"id":"evt_3","type":"session.next.text.ended","properties":{"sessionID":"ses_1","assistantMessageID":"msg_2","timestamp":"2026-08-27T23:38:05.796Z","textID":"text-0","text":"mock-reply-10: ok"}}`

const nextStepEndedRaw = `{"id":"evt_4","type":"session.next.step.ended","properties":{"sessionID":"ses_1","timestamp":"2026-08-27T23:38:05.805Z","assistantMessageID":"msg_2","finish":"stop","cost":0,"tokens":{"input":120,"output":30,"reasoning":0,"cache":{"read":0,"write":10}}}}`

func TestClientEventsFromNextPrompted_UserEcho(t *testing.T) {
	a := NewAdapter(nil, nil, nil)
	evs := a.ClientEventsFromEvent("session.next.prompted", nextPromptedRaw)
	require.Len(t, evs, 1)
	assert.Equal(t, session.EventPartEnd, evs[0].Type)
	assert.Equal(t, "ses_1", evs[0].SessionID)
	assert.Equal(t, "msg_1", evs[0].MessageID)
	require.NotNil(t, evs[0].Part)
	assert.Equal(t, session.PartText, evs[0].Part.Type)
	assert.Equal(t, "fourth queued message", evs[0].Part.Text,
		"the echo text must equal the admitted prompt — the frontend's queued-message strip matches on it")
}

func TestClientEventsFromNextTextDelta(t *testing.T) {
	a := NewAdapter(nil, nil, nil)
	evs := a.ClientEventsFromEvent("session.next.text.delta", nextTextDeltaRaw)
	require.Len(t, evs, 1)
	assert.Equal(t, session.EventPartDelta, evs[0].Type)
	assert.Equal(t, "msg_2", evs[0].MessageID, "assistantMessageID maps onto MessageID")
	assert.Equal(t, "text-0", evs[0].PartID, "textID maps onto PartID")
	assert.Equal(t, "mock-reply-10: ", evs[0].Delta)
}

func TestClientEventsFromNextTextEnded(t *testing.T) {
	a := NewAdapter(nil, nil, nil)
	evs := a.ClientEventsFromEvent("session.next.text.ended", nextTextEndedRaw)
	require.Len(t, evs, 1)
	assert.Equal(t, session.EventPartEnd, evs[0].Type)
	require.NotNil(t, evs[0].Part)
	assert.Equal(t, "mock-reply-10: ok", evs[0].Part.Text)
	assert.Equal(t, "text-0", evs[0].Part.ID)
}

func TestClientEventsFromNextStepEnded_ContextUsage(t *testing.T) {
	a := NewAdapter(nil, nil, nil)
	evs := a.ClientEventsFromEvent("session.next.step.ended", nextStepEndedRaw)
	require.Len(t, evs, 1)
	assert.Equal(t, session.EventSessionUpdated, evs[0].Type)
	require.NotNil(t, evs[0].Session)
	require.NotNil(t, evs[0].Session.ContextUsage)
	assert.Equal(t, int64(130), evs[0].Session.ContextUsage.Used,
		"prompt occupancy = input + cache.read + cache.write (mirrors wire.Tokens.PromptTokens)")
}

func TestClientEventsFromNextIgnoredTypes(t *testing.T) {
	a := NewAdapter(nil, nil, nil)
	for _, et := range []string{
		"session.next.prompt.admitted",
		"session.next.step.started",
	} {
		assert.Nil(t, a.ClientEventsFromEvent(et, `{"id":"e","type":"`+et+`","properties":{"sessionID":"s"}}`),
			"%s carries no client-facing signal", et)
	}
}

// The prompted echo leaves the frontend buffer in "user-echo" (discard
// deltas); text.started must flip it back to "text" via an empty-text
// part.end, or a V2 turn's content appears only at text.ended.
func TestClientEventsFromNextTextStarted_PrimesStreamingBuffer(t *testing.T) {
	a := NewAdapter(nil, nil, nil)
	raw := `{"id":"evt_5","type":"session.next.text.started","properties":{"sessionID":"ses_1","assistantMessageID":"msg_2","timestamp":"2026-08-27T23:38:05.048Z","textID":"text-0"}}`
	evs := a.ClientEventsFromEvent("session.next.text.started", raw)
	require.Len(t, evs, 1)
	assert.Equal(t, session.EventPartEnd, evs[0].Type)
	assert.Equal(t, "msg_2", evs[0].MessageID)
	require.NotNil(t, evs[0].Part)
	assert.Equal(t, session.PartText, evs[0].Part.Type)
	assert.Empty(t, evs[0].Part.Text, "empty text — the priming shape the part.end handler's empty branch expects")
}

// Unhappy paths: malformed input must degrade to nil, never panic or
// half-emit (the tracker dispatches every raw event through here).
func TestClientEventsFromNextTextStarted_UnhappyPaths(t *testing.T) {
	a := NewAdapter(nil, nil, nil)
	cases := []struct {
		name string
		raw  string
	}{
		{"malformed json", `{"id":"e","type":"session.next.text.started","propert`},
		{"empty properties", `{"id":"e","type":"session.next.text.started","properties":{}}`},
		{"missing sessionID", `{"id":"e","type":"session.next.text.started","properties":{"assistantMessageID":"msg_2","textID":"text-0"}}`},
		{"missing assistantMessageID", `{"id":"e","type":"session.next.text.started","properties":{"sessionID":"ses_1","textID":"text-0"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Nil(t, a.ClientEventsFromEvent("session.next.text.started", tc.raw))
		})
	}
}

// TestV2TurnSequence_LiveStreamingOrder pins the ordering property the
// fix exists for: within one V2 text block the priming part.end
// (text.started) precedes the deltas carrying the same PartID, so the
// frontend's mode routing (user-echo → discard) flips to "text" BEFORE
// any delta arrives. This is the regression shape of the #1110
// post-merge gap: prompted → delta with no priming between them.
func TestV2TurnSequence_LiveStreamingOrder(t *testing.T) {
	a := NewAdapter(nil, nil, nil)

	prompted := `{"id":"e1","type":"session.next.prompted","properties":{"sessionID":"ses_1","messageID":"msg_u","prompt":{"text":"queued hello"}}}`
	started := `{"id":"e2","type":"session.next.text.started","properties":{"sessionID":"ses_1","assistantMessageID":"msg_a","textID":"text-0"}}`
	delta1 := `{"id":"e3","type":"session.next.text.delta","properties":{"sessionID":"ses_1","assistantMessageID":"msg_a","textID":"text-0","delta":"Hello "}}`
	delta2 := `{"id":"e4","type":"session.next.text.delta","properties":{"sessionID":"ses_1","assistantMessageID":"msg_a","textID":"text-0","delta":"world"}}`
	ended := `{"id":"e5","type":"session.next.text.ended","properties":{"sessionID":"ses_1","assistantMessageID":"msg_a","textID":"text-0","text":"Hello world"}}`

	type step struct {
		typ    session.EventType
		partID string
		delta  string
	}
	var seq []step
	for _, tc := range []struct {
		eventType string
		raw       string
	}{
		{"session.next.prompted", prompted},
		{"session.next.text.started", started},
		{"session.next.text.delta", delta1},
		{"session.next.text.delta", delta2},
		{"session.next.text.ended", ended},
	} {
		for _, ev := range a.ClientEventsFromEvent(tc.eventType, tc.raw) {
			seq = append(seq, step{typ: ev.Type, partID: ev.PartID, delta: ev.Delta})
		}
	}

	require.Len(t, seq, 5)
	// 1: user echo; 2: priming part.end (empty text, assistant slot);
	// 3-4: deltas on the SAME slot; 5: final snapshot.
	assert.Equal(t, session.EventPartEnd, seq[0].typ, "the prompted user echo")
	assert.Equal(t, session.EventPartEnd, seq[1].typ, "the priming event is a part.end")
	assert.Equal(t, "text-0", seq[1].partID)
	assert.Equal(t, session.EventPartDelta, seq[2].typ)
	assert.Equal(t, "text-0", seq[2].partID, "deltas route to the primed slot")
	assert.Equal(t, "Hello ", seq[2].delta)
	assert.Equal(t, session.EventPartDelta, seq[3].typ)
	assert.Equal(t, session.EventPartEnd, seq[4].typ)
	assert.Equal(t, "text-0", seq[4].partID)
}
