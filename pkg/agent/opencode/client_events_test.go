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
		"session.next.text.started",
	} {
		assert.Nil(t, a.ClientEventsFromEvent(et, `{"id":"e","type":"`+et+`","properties":{"sessionID":"s"}}`),
			"%s carries no client-facing signal", et)
	}
}
