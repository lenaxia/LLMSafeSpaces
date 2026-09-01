// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package outbox

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedQueueEntry(t *testing.T, s *Service, ws, ses, id, status string) Entry {
	t.Helper()
	e := Entry{ID: id, ClientMessageID: "cmid-" + id, UserID: "u1", Text: "hi",
		AcceptedAt: time.Now().UTC(), Status: status, NextAttemptAt: time.Now().UTC()}
	raw, err := json.Marshal(e)
	require.NoError(t, err)
	require.NoError(t, s.client.RPush(context.Background(), qKey(ws, ses), string(raw)).Err())
	return e
}

func readQueueEntries(t *testing.T, s *Service, ws, ses string) []Entry {
	t.Helper()
	vals, err := s.client.LRange(context.Background(), qKey(ws, ses), 0, -1).Result()
	require.NoError(t, err)
	out := make([]Entry, 0, len(vals))
	for _, v := range vals {
		var e Entry
		require.NoError(t, json.Unmarshal([]byte(v), &e))
		out = append(out, e)
	}
	return out
}

// TestParkWorkspace_ModeTransition (US-69.13 / flipgate_park_with_reason):
// the flip gate parks a workspace's in-flight outbox entries with an
// explicit mode_transition reason — parked entries are inert to the
// delivery loop (never auto-retried) and re-armable only by unpark.
func TestParkWorkspace_ModeTransition(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()

	seedQueueEntry(t, s, "ws-1", "ses-1", "e1", StatusPending)
	seedQueueEntry(t, s, "ws-1", "ses-2", "e2", StatusDelivering)
	seedQueueEntry(t, s, "ws-1", "ses-2", "e3", StatusVerifying)
	seedQueueEntry(t, s, "ws-1", "ses-1", "e4", StatusError)   // already terminal
	seedQueueEntry(t, s, "ws-2", "ses-1", "e5", StatusPending) // other workspace

	n, err := s.ParkWorkspace(ctx, "ws-1", "authority flip")
	require.NoError(t, err)
	assert.Equal(t, 3, n, "pending+delivering+verifying park; error stays")

	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	byID := map[string]Entry{}
	for _, e := range append(entries, readQueueEntries(t, s, "ws-1", "ses-2")...) {
		byID[e.ID] = e
	}
	for _, id := range []string{"e1", "e2", "e3"} {
		assert.Equal(t, StatusParked, byID[id].Status, "%s parked", id)
		assert.Equal(t, "mode_transition: authority flip", byID[id].LastError, "%s carries the explicit reason", id)
		assert.True(t, byID[id].NextAttemptAt.IsZero(), "%s never auto-retries", id)
	}
	assert.Equal(t, StatusError, byID["e4"].Status, "terminal error untouched")
	other := readQueueEntries(t, s, "ws-2", "ses-1")
	require.Len(t, other, 1)
	assert.Equal(t, StatusPending, other[0].Status, "other workspaces untouched")
}

// TestUnparkWorkspace_ModeTransitionOnly: rollback re-arms exactly the
// mode_transition park — never the genuine error parks.
func TestUnparkWorkspace_ModeTransitionOnly(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()

	_, err := s.ParkWorkspace(ctx, "ws-1", "authority flip")
	require.NoError(t, err)
	seedQueueEntry(t, s, "ws-1", "ses-1", "g1", StatusError)
	err2 := s.client.LRange(ctx, qKey("ws-1", "ses-1"), 0, -1).Err()
	require.NoError(t, err2)

	// A genuine error entry must survive unpark as an error.
	n, err := s.UnparkWorkspace(ctx, "ws-1")
	require.NoError(t, err)
	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusError, entries[0].Status)
	assert.Equal(t, 0, n, "no mode_transition entries to re-arm in this queue")

	// Park then unpark round-trips to pending.
	seedQueueEntry(t, s, "ws-1", "ses-2", "p1", StatusPending)
	np, err := s.ParkWorkspace(ctx, "ws-1", "rollback drain")
	require.NoError(t, err)
	require.Equal(t, 1, np)
	nu, err := s.UnparkWorkspace(ctx, "ws-1")
	require.NoError(t, err)
	assert.Equal(t, 1, nu)
	ses2 := readQueueEntries(t, s, "ws-1", "ses-2")
	require.Len(t, ses2, 1)
	assert.Equal(t, StatusPending, ses2[0].Status, "unpark re-arms for the 0052 drain path")
}

// TestParkWorkspace_Empty: parking an idle workspace is a clean no-op.
func TestParkWorkspace_Empty(t *testing.T) {
	s, _ := newTestService(t)
	n, err := s.ParkWorkspace(context.Background(), "ws-none", "authority flip")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}
