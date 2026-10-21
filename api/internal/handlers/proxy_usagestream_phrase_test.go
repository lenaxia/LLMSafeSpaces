// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// #1817 / #1821 piece 1 (T0.1): the ID-contract pin. The #1756/#1775/
// #1806 phrase re-keys flipped the FRONTEND's session identity to the
// phrase space (list rows, history REST) while the user-stream
// session-keyed events kept emitting the HARNESS id — the join misses
// for every minted row, killing live busy tracking and streaming
// (history-at-end kept working because REST flipped correctly).
//
// These pins are RED on main: every user-stream session-keyed emission
// must carry the PHRASE (the id space the frontend joins on) with the
// harness id preserved ADDITIVELY as internal_session_id.
func TestUserStreamSessionEventsCarryPhraseID(t *testing.T) {
	const harnessID = "ses_harness_1817"
	const phraseID = "amber-river-7"

	h, si := newBridgeIndexHandler(t)
	si.phrases["ws-1/"+harnessID] = phraseID

	broker := eventbroker.NewUserEventBroker()
	h.SetUserBrokerForTest(broker)
	broker.RecordWorkspaceOwner("ws-1", "user-1817")
	sub, err := broker.SubscribeUser("user-1817")
	require.NoError(t, err)
	defer broker.UnsubscribeUser("user-1817", sub)

	next := func(t *testing.T) (eventType, sessionID, internalID string) {
		t.Helper()
		evt := <-sub.Ch
		if d, ok := evt.Data.(map[string]any); ok {
			if v, ok := d["internal_session_id"].(string); ok {
				internalID = v
			}
		}
		if d, ok := evt.Data.(map[string]string); ok {
			internalID = d["internal_session_id"]
		}
		return evt.Type, evt.SessionID, internalID
	}

	t.Run("session.status carries the phrase", func(t *testing.T) {
		(&usageBridge{h: h}).SessionStatus("ws-1", harnessID, true)
		_, sid, iid := next(t)
		assert.Equal(t, phraseID, sid,
			"the busy join keys on the phrase — the harness id never matches a minted row (the #1817 root cause)")
		assert.Equal(t, harnessID, iid,
			"the harness id rides along ADDITIVELY as internal_session_id (never dropped, never the join key)")
	})

	t.Run("agent.question carries the phrase", func(t *testing.T) {
		(&usageBridge{h: h}).InputRequested("ws-1", &abiv1.InputRequest{
			Id: "req-1", SessionId: harnessID, Kind: abiv1.InputKind_INPUT_KIND_QUESTION, Question: "q?",
		})
		_, sid, iid := next(t)
		assert.Equal(t, phraseID, sid)
		assert.Equal(t, harnessID, iid)
	})

	t.Run("agent.question.resolved carries the phrase", func(t *testing.T) {
		(&usageBridge{h: h}).InputResolved("ws-1", harnessID, "req-1")
		_, sid, iid := next(t)
		assert.Equal(t, phraseID, sid)
		assert.Equal(t, harnessID, iid)
	})
}

// The degraded seam keeps today's behavior: an unregistered session
// (no phrase minted — the index degraded or the row never created)
// emits the harness id unchanged; internal_session_id is omitted
// rather than fabricated.
func TestUserStreamSessionEvents_DegradedIndex_PassthroughHarnessID(t *testing.T) {
	h, _ := newBridgeIndexHandler(t)

	broker := eventbroker.NewUserEventBroker()
	h.SetUserBrokerForTest(broker)
	broker.RecordWorkspaceOwner("ws-1", "user-1817")
	sub, err := broker.SubscribeUser("user-1817")
	require.NoError(t, err)
	defer broker.UnsubscribeUser("user-1817", sub)

	(&usageBridge{h: h}).SessionStatus("ws-1", "ses_never_registered", true)
	evt := <-sub.Ch
	assert.Equal(t, "ses_never_registered", evt.SessionID,
		"no phrase → harness id passthrough (never worse; these are the sporadic rows that matched today)")
}

// T1.6 (the #1817 tripwire): the CROSS-SURFACE JOIN, asserted
// mechanically — the sessions list row (the frontend's session
// identity, keyed by phrase since #1756) and the user-stream
// session.status emission (keyed by the harness id at the ABI edge)
// must carry ONE joinable key after the piece-1 translation. This is
// the test class whose absence blinded every suite: mocks injected a
// single id into both surfaces, so the join could never fail.
func TestUserStreamSessionEvents_JoinableWithSessionList(t *testing.T) {
	now := time.Now()
	const harnessID = "ses_harness_t16"
	const phraseID = "quiet-mountain-9"

	h, si := newBridgeIndexHandler(t)
	// Surface 1: the session-index row as the sessions list serves it
	// (ID = the phrase, InternalID = the harness id — post-#1756 shape).
	si.mu.Lock()
	si.rows = map[string][]types.SessionListItem{
		"ws-1": {{
			ID: phraseID, InternalID: harnessID,
			Title: "join test", LastMessageAt: &now,
		}},
	}
	si.phrases["ws-1/"+harnessID] = phraseID
	si.mu.Unlock()

	broker := eventbroker.NewUserEventBroker()
	h.SetUserBrokerForTest(broker)
	broker.RecordWorkspaceOwner("ws-1", "user-1817")
	sub, err := broker.SubscribeUser("user-1817")
	require.NoError(t, err)
	defer broker.UnsubscribeUser("user-1817", sub)

	// Surface 2: the busy emission keyed by the HARNESS id at the ABI
	// edge — the exact live shape that broke v0.40.0.
	(&usageBridge{h: h}).SessionStatus("ws-1", harnessID, true)
	evt := <-sub.Ch

	// THE JOIN: the wire key must equal the list row's key.
	assert.Equal(t, phraseID, evt.SessionID,
		"session.status must key on the same id the sessions list serves — the v0.40.0 breakage was exactly this mismatch")
	var iid string
	if d, ok := evt.Data.(map[string]any); ok {
		iid, _ = d["internal_session_id"].(string)
	}
	assert.Equal(t, harnessID, iid, "the harness id stays reachable (additive internal_session_id)")
}
