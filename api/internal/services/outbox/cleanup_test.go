// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package outbox

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCleanupWorkspace_DeletesOnlyThatWorkspace (#1119 follow-up from
// #1211 F2): deleting a Workspace CR must clean its Valkey outbox
// residue — queues, staging, dedupe, locks — or orphaned keys inflate
// llmsafespaces_outbox_entries{error} forever and are re-scanned every
// worker tick with no agent left to verify against and no queue UI to
// dismiss from.
func TestCleanupWorkspace_DeletesOnlyThatWorkspace(t *testing.T) {
	s, mr := newTestService(t)
	defer mr.Close()
	ctx := context.Background()

	seed := func(ws, ses, id string) {
		raw := fmt.Sprintf(`{"id":%q,"clientMessageID":%q,"userID":"u1","text":"hi","acceptedAt":%q,"status":"pending"}`, id, "cm-"+id, time.Now().UTC().Format(time.RFC3339Nano))
		require.NoError(t, s.SeedEntryForTest(ctx, ws, ses, raw))
	}
	seed("ws-gone", "ses-1", "e1")
	seed("ws-gone", "ses-2", "e2")
	seed("ws-keep", "ses-1", "e3")
	// Staging + dedupe + lock residue for the deleted workspace.
	require.NoError(t, mr.Set("outboxd:ws-gone:ses-1", `{"id":"e1"}`))
	require.NoError(t, mr.Set("outboxdedupe:ws-gone:ses-1:cm-e1", "1"))
	require.NoError(t, mr.Set("outboxlock:ws-gone:ses-1", "tok"))

	n, err := s.CleanupWorkspace(ctx, "ws-gone")
	require.NoError(t, err)
	assert.Equal(t, 5, n, "two queues + staging + dedupe + lock keys removed")

	for _, k := range []string{
		"outboxq:ws-gone:ses-1", "outboxq:ws-gone:ses-2",
		"outboxd:ws-gone:ses-1", "outboxdedupe:ws-gone:ses-1:cm-e1", "outboxlock:ws-gone:ses-1",
	} {
		assert.False(t, mr.Exists(k), "%s must be gone", k)
	}
	assert.True(t, mr.Exists("outboxq:ws-keep:ses-1"), "other workspaces untouched")
}

// TestCleanupWorkspace_IdempotentAndEmpty: re-running and cleaning a
// workspace with no residue are clean no-ops (a missed sweep just leaves
// the keys for the next transition — same stance as the package).
func TestCleanupWorkspace_IdempotentAndEmpty(t *testing.T) {
	s, mr := newTestService(t)
	defer mr.Close()
	ctx := context.Background()

	n, err := s.CleanupWorkspace(ctx, "ws-none")
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	raw := `{"id":"e1","clientMessageID":"cm","userID":"u","text":"x","acceptedAt":"2026-09-01T00:00:00Z","status":"pending"}`
	require.NoError(t, s.SeedEntryForTest(ctx, "ws-x", "ses-1", raw))
	_, err = s.CleanupWorkspace(ctx, "ws-x")
	require.NoError(t, err)
	n2, err := s.CleanupWorkspace(ctx, "ws-x")
	require.NoError(t, err)
	assert.Equal(t, 0, n2)
}
