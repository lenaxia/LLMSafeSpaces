// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
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
		evt := <-sub.Ch
		assert.Equal(t, phraseID, evt.SessionID,
			"the busy join keys on the phrase — the harness id never matches a minted row (the #1817 root cause)")
		// #1817 piece 2: agent.question carries a STRUCT payload
		// (session.InputRequest), not a map — the clobber fix preserved it.
		// The harness id rides along ADDITIVELY as the payload's own
		// SessionID field (the contract shape); session.InputRequest has
		// no internal_session_id field, so the additive-map assertion is
		// superseded by the struct assertion.
		ir, ok := evt.Data.(session.InputRequest)
		require.True(t, ok,
			"the agent.question payload must stay the session.InputRequest struct (the clobber fix preserves it)")
		assert.Equal(t, harnessID, ir.SessionID,
			"the harness id rides along ADDITIVELY as the payload's SessionID (never dropped, never the join key)")
		assert.Equal(t, "q?", ir.Question, "the question survives the translation")
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

// #1817 piece 3 (finding 3): the LIFECYCLE CLEARS. A busy flip emitted by
// the usagestream consumer is keyed by the PHRASE; the matching clear
// (idle reconcile, delete, archive, abort, pending-input, inbox) must be
// keyed by the SAME phrase or the busy latch is never cleared (the
// lifecycle half of the stuck-busy). These sites build a harness-keyed
// session.status and must run it through the same translation. This pin:
// a harness-keyed idle clear resolves to the phrase on the wire.
func TestLifecycleClearIsPhraseKeyed(t *testing.T) {
	const harnessID = "ses_lifecycle_1817"
	const phraseID = "golf-delta-8"

	h, si := newBridgeIndexHandler(t)
	si.phrases["ws-1/"+harnessID] = phraseID

	// The idle reconcile path (proxy_events.go) builds a harness-keyed
	// session.status=idle and runs it through the same translation.
	evt := apitypes.WorkspaceSSEEvent{
		Type:      "session.status",
		SessionID: harnessID,
		Status:    "idle",
	}
	h.translateSessionIDToPhrase("ws-1", &evt)

	assert.Equal(t, phraseID, evt.SessionID,
		"the lifecycle clear must be keyed by the phrase so it clears the phrase-keyed busy flip — a harness-keyed clear would leave the latch (the stuck-busy)")
	if d, ok := evt.Data.(map[string]any); ok {
		assert.Equal(t, harnessID, d["internal_session_id"],
			"the harness id rides additively on the clear too (never dropped)")
	}
}

// #1817 piece 3 (finding 1): on refreshed pods agentd already translated
// the frame to the phrase and left the harness id ADDITIVELY as
// internal_session_id (wireEvent / podSnapshotsLocked). If the bridge
// re-translated that already-phrase wire id, the Register below it would
// treat the PHRASE as a session_id and MINT a new short-id row (the
// phantom-row defect — re-breaking the busy join). The double-translation
// guard detects the additive internal id and passes the phrase through
// without re-registering. This pin asserts the wire id stays the (single)
// phrase, the harness id stays additive, and no phantom row is minted
// (the mock Register is only ever called for the harness id, never the
// phrase).
func TestTranslateNoDoubleTranslation_OnRefreshedPod(t *testing.T) {
	const harnessID = "ses_double_1817"
	const phraseID = "echo-creek-5"

	h, si := newBridgeIndexHandler(t)
	// Seed the existing phrase so the guard's resolvePhrase(harness)
	// returns the phrase — confirming the frame was already translated.
	si.phrases["ws-1/"+harnessID] = phraseID

	broker := eventbroker.NewUserEventBroker()
	h.SetUserBrokerForTest(broker)
	broker.RecordWorkspaceOwner("ws-1", "user-double")
	sub, err := broker.SubscribeUser("user-double")
	require.NoError(t, err)
	defer broker.UnsubscribeUser("user-double", sub)

	// The refreshed-pod shape: agentd put the PHRASE on the wire and the
	// HARNESS id in internal_session_id. The consumer routes it through the
	// additive seam (emitSessionStatus) carrying exactly this.
	(&usageBridge{h: h}).SessionStatusWithInternal("ws-1", phraseID, harnessID, true)
	evt := <-sub.Ch

	// THE JOIN: the wire id stays the single phrase — a double
	// translation would register the phrase and mint a phantom third id.
	assert.Equal(t, phraseID, evt.SessionID,
		"an already-phrase-keyed frame must not be re-translated — re-registering the phrase mints a phantom short-id row and re-breaks the busy join")
	var iid string
	if d, ok := evt.Data.(map[string]any); ok {
		iid, _ = d["internal_session_id"].(string)
	}
	assert.Equal(t, harnessID, iid,
		"the additive harness id is preserved (the 'never dropped' contract)")
}

// #1817 piece 2: agent.question / agent.permission carry a STRUCT
// payload (session.InputRequest), not a map. The phrase translation's
// default branch used to replace the Data with a synthetic
// {"internal_session_id": harness}, clobbering the contract payload the
// frontend renders. The translation must key on the phrase and PRESERVE
// the struct payload intact.
func TestTranslatePreservesInputRequestPayload(t *testing.T) {
	const harnessID = "ses_clobber_1817"
	const phraseID = "delta-lake-3"

	h, si := newBridgeIndexHandler(t)
	si.phrases["ws-1/"+harnessID] = phraseID

	broker := eventbroker.NewUserEventBroker()
	h.SetUserBrokerForTest(broker)
	broker.RecordWorkspaceOwner("ws-1", "user-clobber")
	sub, err := broker.SubscribeUser("user-clobber")
	require.NoError(t, err)
	defer broker.UnsubscribeUser("user-clobber", sub)

	req := &abiv1.InputRequest{
		Id: "req-clobber", SessionId: harnessID,
		Kind: abiv1.InputKind_INPUT_KIND_QUESTION, Question: "which env?",
		Header: "Pick one", Multiple: false, Custom: true,
		Options: []*abiv1.InputOption{{Label: "prod", Description: "live"}},
		Tool:    &abiv1.ToolRef{MessageId: "msg_1", CallId: "call_1"},
	}
	(&usageBridge{h: h}).InputRequested("ws-1", req)
	evt := <-sub.Ch

	assert.Equal(t, phraseID, evt.SessionID, "the wire id is the phrase")
	ir, ok := evt.Data.(session.InputRequest)
	require.True(t, ok, "the agent.question payload must stay the session.InputRequest struct — it was clobbered by the translation's synthetic map")
	assert.Equal(t, "req-clobber", ir.ID, "the payload's request id survives the translation")
	assert.Equal(t, "which env?", ir.Question, "the question survives the translation")
	assert.True(t, ir.Custom, "the custom flag survives the translation")
	require.Len(t, ir.Options, 1)
	assert.Equal(t, "prod", ir.Options[0].Label)
	require.NotNil(t, ir.Tool)
	assert.Equal(t, "call_1", ir.Tool.CallID)
}
