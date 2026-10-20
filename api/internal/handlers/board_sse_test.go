// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_sse_test.go — the §4.6 publisher pins at the wiring tier: the
// flat board.event frame (the merged frontend's contract — fields are
// siblings of `type`, never nested under `data`), the both-channel fan
// out (workspace stream + owner's user stream, workspace_id only on the
// user copy), and the fail-open nil-broker no-op.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/pkg/board"
)

func boardSSEFixture() board.Event {
	return board.Event{
		ID:        "11111111-2222-3333-4444-555555555555",
		ProjectID: "proj-uuid-1",
		ItemKind:  "task",
		ItemID:    "task-uuid-3",
		Action:    "move_stage",
		Actor:     json.RawMessage(`{"kind":"user","user_id":"user-1","surface":"rest"}`),
		Payload:   json.RawMessage(`{"to_stage":"done"}`),
		CreatedAt: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC),
	}
}

// TestBoardSSE_FlatFrameContract: the marshaled frame is exactly the
// frontend's BoardEventPayload — flat fields beside type, no data
// nesting, actor and payload verbatim.
func TestBoardSSE_FlatFrameContract(t *testing.T) {
	frame, err := json.Marshal(boardSSEEvent(boardSSEFixture()))
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(frame, &got))
	assert.Equal(t, "board.event", got["type"])
	assert.Equal(t, "proj-uuid-1", got["project_id"])
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", got["id"])
	assert.Equal(t, "task", got["item_kind"])
	assert.Equal(t, "task-uuid-3", got["item_id"])
	assert.Equal(t, "move_stage", got["action"])
	assert.Equal(t, map[string]any{"kind": "user", "user_id": "user-1", "surface": "rest"}, got["actor"])
	assert.Equal(t, map[string]any{"to_stage": "done"}, got["payload"])
	assert.Contains(t, got, "created_at")
	assert.NotContains(t, got, "data", "board.event rides FLAT fields, never the data envelope")
	assert.NotContains(t, got, "session_id")
}

// TestBoardSSE_FanoutBothChannels: the workspace stream and the owner's
// user stream each receive the event; only the user copy carries
// workspace_id (the publishWorkspaceAndUserEvent shape).
func TestBoardSSE_FanoutBothChannels(t *testing.T) {
	h := &ProxyHandler{userBroker: eventbroker.NewUserEventBroker()}
	wsSub, err := h.userBroker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer h.userBroker.UnsubscribeWorkspace("ws-1", wsSub)
	userSub, err := h.userBroker.SubscribeUser("user-1")
	require.NoError(t, err)
	defer h.userBroker.UnsubscribeUser("user-1", userSub)

	h.publishBoardFanout("user-1", "ws-1", boardSSEFixture())

	wsEvt, ok := <-wsSub.Ch
	require.True(t, ok, "workspace stream received the event")
	assert.Equal(t, "board.event", wsEvt.Type)
	assert.Empty(t, wsEvt.WorkspaceID, "workspace copy carries no workspace_id (implicit for subscribers)")

	userEvt, ok := <-userSub.Ch
	require.True(t, ok, "user stream received the event")
	assert.Equal(t, "board.event", userEvt.Type)
	assert.Equal(t, "ws-1", userEvt.WorkspaceID, "user copy routes by workspace_id")
	assert.Equal(t, "move_stage", userEvt.Action)
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", userEvt.BoardEventID)
}

// TestBoardSSE_NilBrokerNoOp: a brokerless handler publishes nothing
// and errors nothing (fail-open — writes never depend on the
// projection).
func TestBoardSSE_NilBrokerNoOp(t *testing.T) {
	h := &ProxyHandler{}
	assert.Nil(t, h.BoardEventPublisher(nil, nil),
		"no broker ⇒ no publisher installed (engine keeps running dark)")
	h.publishBoardFanout("user-1", "ws-1", boardSSEFixture())
}

// TestBoardSSE_OverflowDropsNeverBlocks: the write-path hand-off is
// non-blocking — once the bounded queue is full (no drainer installed
// in this test), further events drop with a logged warning and the
// publisher returns immediately.
func TestBoardSSE_OverflowDropsNeverBlocks(t *testing.T) {
	h := &ProxyHandler{userBroker: eventbroker.NewUserEventBroker()}
	log := &recordingWarnLogger{}
	events := make(chan board.Event, 2)
	pub := h.boardEventEnqueuer(events, log)

	pub(boardSSEFixture())
	pub(boardSSEFixture())
	done := make(chan struct{})
	go func() {
		pub(boardSSEFixture()) // queue full (no drainer): must drop, not block
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher blocked on a full queue — the write path must never wait on the projection")
	}
	assert.Equal(t, 1, log.warns, "the dropped event logs exactly one warning")
	assert.Len(t, events, 2, "the queued events survive for the drainer")
}

type recordingWarnLogger struct{ warns int }

func (l *recordingWarnLogger) Warn(string, ...interface{}) { l.warns++ }

// TestBoardSSE_EmptyOwnerSkipsUserCopy: an ownerless fanout (deleted
// project mid-flight) still reaches the workspace stream; the user
// publish is skipped, not panicked.
func TestBoardSSE_EmptyOwnerSkipsUserCopy(t *testing.T) {
	h := &ProxyHandler{userBroker: eventbroker.NewUserEventBroker()}
	wsSub, err := h.userBroker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer h.userBroker.UnsubscribeWorkspace("ws-1", wsSub)

	h.publishBoardFanout("", "ws-1", boardSSEFixture())

	evt, ok := <-wsSub.Ch
	require.True(t, ok)
	assert.Equal(t, "board.event", evt.Type)
}
