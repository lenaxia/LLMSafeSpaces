// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	"github.com/stretchr/testify/assert"
)

// #1627: archive/unarchive must reach EVERY open tab — the workspace
// stream alone leaves other tabs rendering the session as live (the
// #786 lesson, same shape as the deleted event).
func TestPublishSessionArchived_StreamsWorkspaceEvent(t *testing.T) {
	env := newTestEnv(t)
	env.handler.userBroker = eventbroker.NewUserEventBroker()

	sub, err := env.handler.userBroker.SubscribeWorkspace("ws-1")
	assert.NoError(t, err)
	defer env.handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.PublishSessionArchived("ws-1", "s1", true)

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "session.status", evt.Type)
		assert.Equal(t, "s1", evt.SessionID)
		assert.Equal(t, "archived", evt.Status)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE session.status archived event")
	}
}

func TestPublishSessionArchived_UnarchiveStreamsUnarchived(t *testing.T) {
	env := newTestEnv(t)
	env.handler.userBroker = eventbroker.NewUserEventBroker()

	sub, err := env.handler.userBroker.SubscribeWorkspace("ws-1")
	assert.NoError(t, err)
	defer env.handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.PublishSessionArchived("ws-1", "s1", false)

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "session.status", evt.Type)
		assert.Equal(t, "s1", evt.SessionID)
		assert.Equal(t, "unarchived", evt.Status)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE session.status unarchived event")
	}
}

func TestPublishSessionArchived_NoBrokerIsNoop(t *testing.T) {
	env := newTestEnv(t)
	// Deliberately no userBroker — must not panic.
	env.handler.PublishSessionArchived("ws-1", "s1", true)
}

// Compile-time pin on the event shape the frontend consumes (#1627
// PR3): statuses are "archived"/"unarchived" on the existing
// session.status event type.
func TestSessionArchivedStatusConstants(t *testing.T) {
	assert.Equal(t, apitypes.WorkspaceSSEEvent{
		Type:      "session.status",
		SessionID: "s1",
		Status:    "archived",
	}, apitypes.WorkspaceSSEEvent{Type: "session.status", SessionID: "s1", Status: sessionStatusArchived})
}
