// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_sse_bus_test.go — the S6 render-parity pin: ONE committed
// board event renders to byte-identical board.event frames whether
// the v1 point-to-point leg or the bus join carried it. The v1 leg's
// render is boardSSEEvent + publishBoardFanout; the join's render is
// deliverBoardBusMessage; this file pins them equal over the REAL
// broker (same fan-out, same envelope).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/eventbus"
)

type silentWarnLogger struct{}

func (silentWarnLogger) Warn(string, ...interface{}) {}

// parityEvent is the committed row both legs render.
func parityEvent() board.Event {
	return board.Event{
		ID: "evt-parity", ProjectID: "prj-1", ItemKind: "task", ItemID: "task-1",
		Action: "move_stage",
		Actor:  []byte(`{"kind":"user","user_id":"user-1"}`),
		Payload: []byte(`{"task_id":"task-1","from_stage":"backlog","resulting_stage":"in_progress",
			"reclaimed":false}`),
		CreatedAt: time.Date(2026, 10, 31, 1, 2, 3, 400000000, time.UTC),
	}
}

// TestBoardBusRenderParityWithV1 — the pin: the bus leg's frame and
// the v1 leg's frame are the SAME bytes on both channels.
func TestBoardBusRenderParityWithV1(t *testing.T) {
	broker := eventbroker.NewUserEventBroker()
	usr, err := broker.SubscribeUser("user-1")
	require.NoError(t, err)
	defer broker.UnsubscribeUser("user-1", usr)
	wsSub, err := broker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	defer broker.UnsubscribeWorkspace("ws-1", wsSub)

	evt := parityEvent()
	// The v1 leg's frame: render + dual-channel fan-out.
	wantUser := boardSSEEvent(evt)
	wantUser.WorkspaceID = "ws-1"
	wantWs := boardSSEEvent(evt)

	h := &ProxyHandler{userBroker: broker}
	h.publishBoardFanout("user-1", "ws-1", evt)
	v1User := <-usr.Ch
	v1Ws := <-wsSub.Ch

	// Drain for the bus leg: same event, carried as a bus message.
	busUsr, err := broker.SubscribeUser("user-2")
	require.NoError(t, err)
	defer broker.UnsubscribeUser("user-2", busUsr)
	_ = busUsr
	msg := eventbus.Message{
		ID: "1-1",
		Event: eventbus.Event{
			Topic: eventbus.BoardEventTopic("prj-1"),
			At:    evt.CreatedAt,
			Payload: eventbus.BoardEventPayload(evt.ID, evt.ItemKind, evt.ItemID, evt.Action,
				string(evt.Actor), string(evt.Payload), evt.CreatedAt.UTC().Format(time.RFC3339Nano),
				"user-1", "ws-1", "tsk-one"),
		},
	}
	BoardEventBusHandler(broker, nil, silentWarnLogger{})(context.Background(), msg)
	busUser := <-usr.Ch
	busWs := <-wsSub.Ch

	assertJSONEqual(t, wantUser, v1User, "v1 user frame")
	assertJSONEqual(t, wantWs, v1Ws, "v1 workspace frame")
	assertJSONEqual(t, wantUser, busUser, "bus user frame == v1 user frame (THE PARITY PIN)")
	assertJSONEqual(t, wantWs, busWs, "bus workspace frame == v1 workspace frame (THE PARITY PIN)")
}

// TestBoardBusRenderUnknownTopicDropped — the vocabulary is a
// convention: an unknown shape logs and ACKs (never a frame, never a
// panic, never a wedged group).
func TestBoardBusRenderUnknownTopicDropped(t *testing.T) {
	broker := eventbroker.NewUserEventBroker()
	usr, err := broker.SubscribeUser("user-1")
	require.NoError(t, err)
	defer broker.UnsubscribeUser("user-1", usr)

	handler := BoardEventBusHandler(broker, nil, silentWarnLogger{})
	require.NoError(t, handler(context.Background(), eventbus.Message{
		Event: eventbus.Event{Topic: "lease.task-1.nudged"},
	}))
	require.NoError(t, handler(context.Background(), eventbus.Message{
		Event: eventbus.Event{Topic: "task.task-1.unknown_arm"},
	}))
	select {
	case evt := <-usr.Ch:
		t.Fatalf("unknown shapes must not produce frames: %+v", evt)
	default:
	}
}

// assertJSONEqual marshals both sides — the byte-parity witness. The
// broker's own EventID (a per-publish monotonic sequence) is zeroed
// first: it identifies the PUBLISH, not the frame's content.
func assertJSONEqual(t *testing.T, want, got apitypes.WorkspaceSSEEvent, msg string) {
	t.Helper()
	want.EventID, got.EventID = 0, 0
	w, err := json.Marshal(want)
	require.NoError(t, err)
	g, err := json.Marshal(got)
	require.NoError(t, err)
	assert.Equal(t, string(w), string(g), msg)
}
