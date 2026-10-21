// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
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

	// Surface 3 (reviewer minor 3b): the CONTRACT-STREAM projection is the
	// third surface the tripwire must cover. Build the real agentd
	// authority with a phrase translator and assert its subscribe
	// snapshot carries the SAME join key: sessionId = phrase,
	// internalSessionId = harness (State()/statusz stay harness-keyed).
	auth, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      t16Parser{},
		Passwords:   []string{"agentd-password"},
		IDTranslator: func(id string) string {
			if id == harnessID {
				return phraseID
			}
			return ""
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = auth.Close() })
	auth.IngestForTest(&abiv1.Event{
		Type:      abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
		SessionId: harnessID,
		Status:    abiv1.SessionStatus_SESSION_STATUS_BUSY,
	})

	frames, cancel, err := auth.Stream(t.Context())
	require.NoError(t, err)
	defer cancel()
	var snapshot *abiv1.SessionSnapshot
	select {
	case frame := <-frames:
		sf, ok := frame.Frame.(*abiv1.StreamFrame_Snapshot)
		require.True(t, ok, "first contract frame must be the subscribe snapshot")
		require.NotNil(t, sf.Snapshot)
		require.NotNil(t, sf.Snapshot.Snapshot)
		for _, row := range sf.Snapshot.Snapshot.Sessions {
			if row.GetSessionId() == phraseID || row.GetInternalSessionId() == harnessID {
				snapshot = row
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no contract-stream snapshot frame")
	}
	require.NotNil(t, snapshot, "the contract snapshot must contain the session join row")
	assert.Equal(t, phraseID, snapshot.GetSessionId(),
		"contract-stream snapshot: wire id must equal the sessions-list key (the third surface)")
	assert.Equal(t, harnessID, snapshot.GetInternalSessionId(),
		"contract-stream snapshot: the harness id stays additive")
}

// t16Parser is the minimal contract event parser the T1.6 third-surface
// assertion needs ("busy <id>" / "idle <id>").
type t16Parser struct{}

func (t16Parser) Parse(raw []byte) (*abiv1.Event, bool, error) {
	var id, status string
	if n, _ := fmt.Sscanf(string(raw), "%s %s", &status, &id); n != 2 {
		return nil, false, nil
	}
	switch status {
	case "busy":
		return &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: id, Status: abiv1.SessionStatus_SESSION_STATUS_BUSY}, true, nil
	case "idle":
		return &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: id, Status: abiv1.SessionStatus_SESSION_STATUS_IDLE}, true, nil
	default:
		return nil, false, nil
	}
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
	h.translateSessionIDToPhrase(context.Background(), "ws-1", &evt)

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
	// Seed the existing phrase so the guard's Register(harness) returns the
	// phrase — confirming the frame was already translated. MINT mode models
	// production's EnsureSessionRegistered: an unknown session (the phrase)
	// would be minted a NEW row — exactly the phantom-row defect. The pin
	// asserts the phrase is never registered, so no mint happens.
	si.phrases["ws-1/"+harnessID] = phraseID
	si.mu.Lock()
	si.mint = true
	si.mu.Unlock()

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

	// THE PHANTOM-ROW GUARD: the phrase was never registered (a double
	// translation would Register(phrase) and mint a new row in mint mode).
	// Only the harness id may have been consulted (the guard's resolve).
	calls := si.registeredCalls()
	assert.Zero(t, calls["ws-1/"+phraseID],
		"the phrase must never be registered — registering it mints a phantom short-id row (the double-translation defect)")
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

// #1817 piece 3 (r2 finding 4): the delete tail runs AFTER DeleteSession has
// removed the row. If it used the MINTING translateSessionIDToPhrase,
// Register(workspace, harness) would EnsureSessionRegister → INSERT a ghost
// row for the just-deleted session, and the `deleted` clear would carry a
// freshly-minted phrase the frontend does not recognize. The READ-ONLY twin
// (translateSessionIDToPhraseNoMint) consults only the surviving LRU —
// DeleteSession does not evict it — so the clear matches the busy flip's
// phrase key WITHOUT minting. This pin asserts: the clear is phrase-keyed
// (joins the busy flip), AND Register was never called (no ghost row).
func TestDeleteClearIsPhraseKeyedWithoutMint(t *testing.T) {
	const harnessID = "ses_delete_1817"
	const phraseID = "hotel-foxtrot-9"

	h, si := newBridgeIndexHandler(t)
	// Seed the phrase the session was ALREADY registered to. Production's
	// LRU survives DeleteSession, so the read-only seam still resolves it.
	si.phrases["ws-1/"+harnessID] = phraseID
	// MINT mode models production: if the delete tail (wrongly) called the
	// minting Register, a fresh ghost phrase would be minted. The pin asserts
	// the delete-tail clear never registers at all.
	si.mu.Lock()
	si.mint = true
	si.mu.Unlock()

	// The delete tail builds a harness-keyed session.status=deleted and runs
	// it through the READ-ONLY translation.
	evt := apitypes.WorkspaceSSEEvent{
		Type:      "session.status",
		SessionID: harnessID,
		Status:    "deleted",
	}
	h.translateSessionIDToPhraseNoMint("ws-1", &evt)

	// THE JOIN: the clear is keyed by the phrase so it clears the
	// phrase-keyed busy flip (the delete half of the stuck-busy).
	assert.Equal(t, phraseID, evt.SessionID,
		"the delete clear must be phrase-keyed to match the phrase-keyed busy flip — the LRU survives DeleteSession, so the read-only seam resolves the known phrase")
	if d, ok := evt.Data.(map[string]any); ok {
		assert.Equal(t, harnessID, d["internal_session_id"],
			"the harness id rides additively on the delete clear too (never dropped)")
	}

	// THE GHOST-ROW GUARD: the delete tail must NEVER register (a mint would
	// INSERT a row for a deleted session — the delete-tail ghost row).
	assert.Zero(t, si.registeredCalls()["ws-1/"+harnessID],
		"the delete-tail clear must not mint — it must resolve the phrase from the surviving LRU without a Register (no ghost row for a deleted session)")
}

// #1817 piece 3 (r3 finding 2): a late context-usage event on a refreshed pod
// carries the PHRASE on the wire and the harness id additively, AFTER a
// delete. The delete tombstone is recorded under the HARNESS id, so guarding
// only on the wire phrase would miss it and Register(rowID=harness) would
// re-create the just-deleted index row (the delete-tail ghost-row class,
// re-opening on the context-usage path). This pin deletes the harness, then
// fires a phrase-keyed context-usage event and asserts NO row is
// re-created (no Register, no UpsertContextUsed for the deleted session).
func TestContextUsedNoWrite_AfterDelete_TombstoneHarnessKeyed(t *testing.T) {
	const harnessID = "ses_ctxdeleted_1817"
	const phraseID = "kilo-lima-4"

	h, si := newBridgeIndexHandler(t)
	si.phrases["ws-1/"+harnessID] = phraseID
	// Record the delete tombstone under the HARNESS id (the delete path's
	// key space).
	h.MarkSessionDeletedForTest("ws-1", harnessID)
	// MINT mode: if the guard were bypassed, Register(harness) would mint /
	// re-create the deleted row. The pin asserts no register happens.
	si.mu.Lock()
	si.mint = true
	si.mu.Unlock()

	// The refreshed-pod shape: phrase on the wire, harness additive.
	(&usageBridge{h: h}).ContextUsedWithInternal("ws-1", phraseID, harnessID, 1234)

	// THE GHOST-ROW GUARD: the deleted session's row must NOT be re-created —
	// no Register on the harness id (the tombstone key).
	assert.Zero(t, si.registeredCalls()["ws-1/"+harnessID],
		"a context-usage write for a deleted session must be suppressed — the tombstone is harness-keyed, so the guard must check the harness id (not just the phrase) before the Register")
}

// #1817 piece 1 (r4 finding 2): publishInputResolved — the single seam for
// API-originated agent.*.resolved events — used to build a HARNESS-keyed
// WorkspaceSSEEvent and publish it directly, skipping the phrase translation
// every other user-stream session-keyed event gets. The frontend removes
// resolved prompts by request_id (so the impact is bounded), but the #1821
// piece-1 contract is "all user-stream session-keyed events carry the
// phrase", and a mixed-id-space emission violates it. This pin drives the
// REAL publishInputResolved with a minted session and asserts the published
// envelope is phrase-keyed (harness id additive).
func TestPublishInputResolvedIsPhraseKeyed(t *testing.T) {
	const harnessID = "ses_resolved_1817"
	const phraseID = "mike-november-6"

	h, si := newBridgeIndexHandler(t)
	si.phrases["ws-1/"+harnessID] = phraseID

	broker := eventbroker.NewUserEventBroker()
	h.SetUserBrokerForTest(broker)
	broker.RecordWorkspaceOwner("ws-1", "user-resolved")
	sub, err := broker.SubscribeUser("user-resolved")
	require.NoError(t, err)
	defer broker.UnsubscribeUser("user-resolved", sub)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/api/v1/workspaces/ws-1/permission/per_1/reply", nil)

	// The API-originated resolved publish: harness session id, a request id.
	h.publishInputResolved(c, "ws-1", harnessID, "per_1", "permission", "answered")
	evt := <-sub.Ch

	assert.Equal(t, phraseID, evt.SessionID,
		"publishInputResolved must carry the PHRASE on the wire (the #1821 piece-1 contract) — it used to skip the translation and emit a harness-keyed envelope")
	if d, ok := evt.Data.(map[string]string); ok {
		// The payload's session_id stays the harness id (the contract payload
		// shape); the JOIN is the wire session_id above. The harness id is
		// not dropped — it rides in the payload and as the join's additive
		// source.
		assert.Equal(t, harnessID, d["session_id"],
			"the payload keeps its own harness session_id (the contract shape); the join is the wire phrase")
	}
	assert.Equal(t, "per_1", evt.RequestID)
}

// #1817 piece 3 (reviewer minor 3a): the six lifecycle clear CALL SITES must
// actually run the translation — a unit pin on the translator alone would
// stay green if a call site were deleted. Each subtest drives a real
// lifecycle path and asserts the event that reaches the broker is keyed by
// the PHRASE (and, for the delete tail, that no ghost row is minted).
func TestLifecycleClearCallSitesArePhraseKeyed(t *testing.T) {
	const harnessID = "ses_clearsite_1817"
	const phraseID = "november-foxtrot-2"

	t.Run("idle reconcile (proxy_events)", func(t *testing.T) {
		env, srv := newStatuszTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/statusz" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"sessions":[{"id":"` + harnessID + `","status":"idle"}]}`))
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		defer srv.Close()

		si := newMockSessionIndex()
		si.phrases["ws-1/"+harnessID] = phraseID
		env.handler.SetSessionIndex(si)
		env.handler.userBroker = eventbroker.NewUserEventBroker()
		env.handler.SetActiveSessionsForTest("ws-1", []string{harnessID})

		got := make(chan apitypes.WorkspaceSSEEvent, 1)
		sub, err := env.handler.userBroker.SubscribeWorkspace("ws-1")
		require.NoError(t, err)
		defer env.handler.userBroker.UnsubscribeWorkspace("ws-1", sub)
		go func() { got <- <-sub.Ch }()

		env.handler.reconcileSessionState("ws-1", "10.0.0.1", "test-password")

		select {
		case evt := <-got:
			assert.Equal(t, "session.status", evt.Type)
			assert.Equal(t, "idle", evt.Status)
			assert.Equal(t, phraseID, evt.SessionID,
				"the idle reconcile clear must be phrase-keyed (deleting its translate call leaves the busy latch stuck)")
		case <-time.After(3 * time.Second):
			t.Fatal("idle reconcile published no clear")
		}
	})

	t.Run("delete tail (proxy_handlers)", func(t *testing.T) {
		h, si := newBridgeIndexHandler(t)
		si.phrases["ws-1/"+harnessID] = phraseID
		broker := eventbroker.NewUserEventBroker()
		h.SetUserBrokerForTest(broker)

		sub, err := broker.SubscribeWorkspace("ws-1")
		require.NoError(t, err)
		defer broker.UnsubscribeWorkspace("ws-1", sub)
		got := make(chan apitypes.WorkspaceSSEEvent, 1)
		go func() { got <- <-sub.Ch }()

		h.removeActiveSessionEvents(context.Background(), "ws-1", harnessID)

		select {
		case evt := <-got:
			assert.Equal(t, "deleted", evt.Status)
			assert.Equal(t, phraseID, evt.SessionID,
				"the delete clear must be phrase-keyed (deleting its no-mint translate call leaves the busy latch stuck)")
		case <-time.After(3 * time.Second):
			t.Fatal("delete tail published no clear")
		}
		assert.Zero(t, si.registeredCalls()["ws-1/"+harnessID],
			"the delete clear resolves read-only — it must never mint")
	})

	t.Run("archive (proxy_lifecycle)", func(t *testing.T) {
		h, si := newBridgeIndexHandler(t)
		si.phrases["ws-1/"+harnessID] = phraseID
		broker := eventbroker.NewUserEventBroker()
		h.SetUserBrokerForTest(broker)

		sub, err := broker.SubscribeWorkspace("ws-1")
		require.NoError(t, err)
		defer broker.UnsubscribeWorkspace("ws-1", sub)
		got := make(chan apitypes.WorkspaceSSEEvent, 1)
		go func() { got <- <-sub.Ch }()

		h.PublishSessionArchived("ws-1", harnessID, true)

		select {
		case evt := <-got:
			assert.Equal(t, "archived", evt.Status)
			assert.Equal(t, phraseID, evt.SessionID,
				"the archive clear must be phrase-keyed (deleting its translate call leaves the busy latch stuck)")
		case <-time.After(3 * time.Second):
			t.Fatal("archive published no clear")
		}
	})

	t.Run("admin force-abort (admin_session)", func(t *testing.T) {
		proxy := newProxyForAdminTest(t)
		si := newMockSessionIndex()
		si.phrases["ws-1/"+harnessID] = phraseID
		proxy.SetSessionIndex(si)
		proxy.SetActiveSessionsForTest("ws-1", []string{harnessID})

		h := newAdminSessionHandlerForTest(t, proxy, nil)
		r := setupAdminSessionRouter(t, h, "admin")

		sub, err := proxy.userBroker.SubscribeWorkspace("ws-1")
		require.NoError(t, err)
		defer proxy.userBroker.UnsubscribeWorkspace("ws-1", sub)
		got := make(chan apitypes.WorkspaceSSEEvent, 1)
		go func() { got <- <-sub.Ch }()

		w := doAdminForceAbort(r, "ws-1", harnessID)
		require.Equal(t, http.StatusOK, w.Code)

		select {
		case evt := <-got:
			assert.Equal(t, "aborted", evt.Status)
			assert.Equal(t, phraseID, evt.SessionID,
				"the admin abort clear must be phrase-keyed (deleting its translate call leaves the busy latch stuck)")
		case <-time.After(3 * time.Second):
			t.Fatal("force-abort published no clear")
		}
	})
}

// #1817 piece 3 (reviewer minor 3c / #1821 degrade rule): when the seam
// CANNOT resolve a phrase, the harness-keyed passthrough is flagged with
// session_id_deprecated so a consumer can tell a degraded id from a healthy
// one. The struct-shaped InputRequest payload is the documented exception —
// it must never be clobbered. A successful translation carries no flag.
func TestTranslationMissFlagsSessionIDDeprecated(t *testing.T) {
	const harnessID = "ses_miss_1817"
	const phraseID = "oscar-tango-4"

	t.Run("session.status miss is flagged", func(t *testing.T) {
		h, _ := newBridgeIndexHandler(t)
		broker := eventbroker.NewUserEventBroker()
		h.SetUserBrokerForTest(broker)
		broker.RecordWorkspaceOwner("ws-1", "user-miss")
		sub, err := broker.SubscribeUser("user-miss")
		require.NoError(t, err)
		defer broker.UnsubscribeUser("user-miss", sub)

		(&usageBridge{h: h}).SessionStatusWithInternal("ws-1", harnessID, "", true)
		evt := <-sub.Ch

		assert.Equal(t, harnessID, evt.SessionID, "a miss keeps the harness id (never worse)")
		data, ok := evt.Data.(map[string]any)
		require.True(t, ok, "a miss must materialize map Data to carry the deprecation flag")
		assert.Equal(t, "true", data["session_id_deprecated"],
			"a harness passthrough must be flagged (the #1821 degrade rule)")
		assert.NotContains(t, data, "internal_session_id",
			"a miss must not fabricate the additive harness id")
	})

	t.Run("agent.*.resolved miss is flagged", func(t *testing.T) {
		h, _ := newBridgeIndexHandler(t)
		broker := eventbroker.NewUserEventBroker()
		h.SetUserBrokerForTest(broker)
		broker.RecordWorkspaceOwner("ws-1", "user-miss")
		sub, err := broker.SubscribeUser("user-miss")
		require.NoError(t, err)
		defer broker.UnsubscribeUser("user-miss", sub)

		(&usageBridge{h: h}).InputResolvedWithInternal("ws-1", harnessID, "", "req-miss")
		evt := <-sub.Ch

		assert.Equal(t, harnessID, evt.SessionID)
		data, ok := evt.Data.(map[string]string)
		require.True(t, ok)
		assert.Equal(t, "true", data["session_id_deprecated"],
			"a resolved-event passthrough must be flagged (the #1821 degrade rule)")
	})

	t.Run("successful translation is not flagged", func(t *testing.T) {
		h, si := newBridgeIndexHandler(t)
		si.phrases["ws-1/"+harnessID] = phraseID
		broker := eventbroker.NewUserEventBroker()
		h.SetUserBrokerForTest(broker)
		broker.RecordWorkspaceOwner("ws-1", "user-miss")
		sub, err := broker.SubscribeUser("user-miss")
		require.NoError(t, err)
		defer broker.UnsubscribeUser("user-miss", sub)

		(&usageBridge{h: h}).SessionStatusWithInternal("ws-1", harnessID, "", true)
		evt := <-sub.Ch

		assert.Equal(t, phraseID, evt.SessionID)
		data, ok := evt.Data.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, harnessID, data["internal_session_id"])
		assert.NotContains(t, data, "session_id_deprecated",
			"a translated event is healthy — no deprecation flag")
	})

	t.Run("struct InputRequest payload is never clobbered or flagged", func(t *testing.T) {
		h, _ := newBridgeIndexHandler(t)
		broker := eventbroker.NewUserEventBroker()
		h.SetUserBrokerForTest(broker)
		broker.RecordWorkspaceOwner("ws-1", "user-miss")
		sub, err := broker.SubscribeUser("user-miss")
		require.NoError(t, err)
		defer broker.UnsubscribeUser("user-miss", sub)

		(&usageBridge{h: h}).InputRequested("ws-1", &abiv1.InputRequest{
			Id: "req-struct", SessionId: harnessID,
			Kind: abiv1.InputKind_INPUT_KIND_QUESTION, Question: "keep me",
		})
		evt := <-sub.Ch

		assert.Equal(t, harnessID, evt.SessionID)
		ir, ok := evt.Data.(session.InputRequest)
		require.True(t, ok, "the struct payload must survive a translation miss (documented exception)")
		assert.Equal(t, "keep me", ir.Question)
		assert.Equal(t, harnessID, ir.SessionID)
	})
}
