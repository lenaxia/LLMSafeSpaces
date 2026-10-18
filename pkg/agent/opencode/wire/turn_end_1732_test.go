// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package wire

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// turn_end_1732_test.go — the abort-classification wire contract for
// #1732. Every fixture below is captured verbatim from the real wire
// (opencode 1.18.10 integration probe, 2026-10-18, #1732):
//
//   - user abort: session.error {name: "MessageAbortedError"} arrives
//     BEFORE the idle burst (#1722's probe, both 1.18.10 and 1.18.15);
//   - mid-generation provider-stream cut (the api/relay-roll halt
//     mechanism): NO session.error at all — the assistant message
//     completes with finish "unknown" and the session settles idle
//     cleanly;
//   - clean turn: finish "stop".
//
// The asymmetry is the whole classifier: abort is an EVENT, infra loss
// is a FINISH.

// userAbort1732Fixture is the #1722 probe's session.error payload.
const userAbort1732Fixture = `{"sessionID":"ses_probe","name":"MessageAbortedError","data":{"message":"Aborted"}}`

// cutFinish1732Fixture is the #1732 probe's terminal assistant upsert
// on a mid-stream provider cut (finish "unknown", completion anchored).
const cutFinish1732Fixture = `{"sessionID":"ses_probe","info":{"id":"msg_cut","parentID":"msg_user","role":"assistant","mode":"build","cost":0,"tokens":{"input":0,"output":0},"modelID":"m","providerID":"mock","time":{"created":1792344530783,"completed":1792344531908},"finish":"unknown"}}`

// cleanFinish1732Fixture is the same probe's clean-turn terminal upsert.
const cleanFinish1732Fixture = `{"sessionID":"ses_probe","info":{"id":"msg_ok","parentID":"msg_user","role":"assistant","mode":"build","cost":0,"tokens":{"input":5,"output":1},"modelID":"m","providerID":"mock","time":{"created":1792344446740,"completed":1792344447344},"finish":"stop"}}`

// midFlight1732Fixture is the in-flight creation upsert (no completion
// anchor) — must never classify as a turn end.
const midFlight1732Fixture = `{"sessionID":"ses_probe","info":{"id":"msg_new","role":"assistant","mode":"build","time":{"created":1792344512154}}}`

func Test1732_ParseSessionError_UserAbort(t *testing.T) {
	se, ok := ParseSessionError("session.error", json.RawMessage(userAbort1732Fixture))
	require.True(t, ok)
	assert.Equal(t, "ses_probe", se.SessionID)
	assert.Equal(t, SessionErrorNameUserAbort, se.Name)
}

func Test1732_ParseSessionError_RejectsOtherShapes(t *testing.T) {
	_, ok := ParseSessionError("session.status", json.RawMessage(`{"sessionID":"s"}`))
	assert.False(t, ok)
	_, ok = ParseSessionError("session.error.1", json.RawMessage(`no-json`))
	assert.False(t, ok, "unparsable props must not classify")
	_, ok = ParseSessionError("session.error", json.RawMessage(`{"name":"X"}`))
	assert.False(t, ok, "sessionless errors carry no resume target")
}

func Test1732_ParseAssistantTurnEnd_CutFinishIsUnknown(t *testing.T) {
	te, ok := ParseAssistantTurnEnd("message.updated", json.RawMessage(cutFinish1732Fixture))
	require.True(t, ok)
	assert.Equal(t, "ses_probe", te.SessionID)
	assert.Equal(t, TurnFinishUnknown, te.Finish)
}

func Test1732_ParseAssistantTurnEnd_CleanFinishIsStop(t *testing.T) {
	te, ok := ParseAssistantTurnEnd("message.updated", json.RawMessage(cleanFinish1732Fixture))
	require.True(t, ok)
	assert.Equal(t, "stop", te.Finish)
}

func Test1732_ParseAssistantTurnEnd_RejectsNonTerminalShapes(t *testing.T) {
	_, ok := ParseAssistantTurnEnd("message.updated", json.RawMessage(midFlight1732Fixture))
	assert.False(t, ok, "mid-flight upsert (no completion anchor) is not a turn end")
	_, ok = ParseAssistantTurnEnd("session.updated", json.RawMessage(`{}`))
	assert.False(t, ok)
	// The user turn's own upsert (role user) is not an assistant end.
	_, ok = ParseAssistantTurnEnd("message.updated", json.RawMessage(`{"sessionID":"s","info":{"role":"user","time":{"completed":1792344511369}}}`))
	assert.False(t, ok)
}
