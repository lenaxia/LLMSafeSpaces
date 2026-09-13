// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// recordingSessionIndex captures UpsertParent calls so backfill behavior
// can be asserted without spinning up a real PostgreSQL.
type recordingSessionIndex struct {
	mu      sync.Mutex
	parents map[string]string // sessionID → parentID
	titles  map[string]string
}

func newRecordingSessionIndex() *recordingSessionIndex {
	return &recordingSessionIndex{
		parents: make(map[string]string),
		titles:  make(map[string]string),
	}
}

func (r *recordingSessionIndex) RecordMessage(_, _, _ string, _ time.Time) {}
func (r *recordingSessionIndex) ListByWorkspace(_ context.Context, _ string) ([]types.SessionListItem, error) {
	return nil, nil
}
func (r *recordingSessionIndex) DeleteByWorkspace(_ context.Context, _ string) error { return nil }
func (r *recordingSessionIndex) DeleteSession(_ context.Context, _, _ string) error  { return nil }
func (r *recordingSessionIndex) UpsertTitle(_ context.Context, _, sessionID, title string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.titles[sessionID] = title
	return nil
}
func (r *recordingSessionIndex) UpsertParent(_ context.Context, _, sessionID, parentID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.parents[sessionID] = parentID
	return nil
}
func (r *recordingSessionIndex) UpdateLastSeen(_ context.Context, _, _ string) error { return nil }
func (r *recordingSessionIndex) UpsertContextUsed(_ context.Context, _, _ string, _ int64) error {
	return nil
}
func (r *recordingSessionIndex) Start() error { return nil }
func (r *recordingSessionIndex) Stop() error  { return nil }

func (r *recordingSessionIndex) parentOf(sessionID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.parents[sessionID]
}

func (r *recordingSessionIndex) titleOf(sessionID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.titles[sessionID]
}

func (r *recordingSessionIndex) parentCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.parents)
}

func TestBackfillSessionParents_HappyPath(t *testing.T) {
	env := newInputTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")

	si := newRecordingSessionIndex()
	env.handler.sessionIndex = si
	env.handler.adapter = &mockAdapter{
		listSessionsFn: func(_ context.Context, _, _ string) ([]session.Session, error) {
			return []session.Session{
				{ID: "ses_root"},
				{ID: "ses_child", ParentID: "ses_root"},
				{ID: "ses_grandchild", ParentID: "ses_child"},
			}, nil
		},
	}
	env.handler.BackfillSessionParents(context.Background(), "ws-1")

	require.Eventually(t, func() bool {
		return si.parentCount() == 2
	}, 2*time.Second, 10*time.Millisecond, "expected parent records for child + grandchild")

	assert.Equal(t, "ses_root", si.parentOf("ses_child"))
	assert.Equal(t, "ses_child", si.parentOf("ses_grandchild"))
	assert.Equal(t, "", si.parentOf("ses_root"), "top-level session must NOT be written")
}

// TestBackfillSessionParents_IsIdempotent verifies the once-per-workspace
// gate: subsequent calls within the same process lifetime are no-ops, so
// the steady-state cost of opening the sidebar is a single map lookup —
// not an HTTP round-trip per request.
func TestBackfillSessionParents_IsIdempotent(t *testing.T) {
	// Ported to the adapter (#828 batch 4): the once-per-workspace gate
	// — only ONE ListSessions despite 5 calls.
	var listCalls atomic.Int32
	env := newInputTestEnv(t)
	env.handler.sessionIndex = newRecordingSessionIndex()
	env.handler.adapter = &mockAdapter{
		listSessionsFn: func(_ context.Context, _, _ string) ([]session.Session, error) {
			listCalls.Add(1)
			return []session.Session{{ID: "ses_root"}}, nil
		},
	}

	for i := 0; i < 5; i++ {
		env.handler.BackfillSessionParents(context.Background(), "ws-1")
	}

	require.Eventually(t, func() bool { return listCalls.Load() >= 1 }, time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond) // window for any spurious extra calls

	assert.Equal(t, int32(1), listCalls.Load(), "backfill must only list once per workspace")
}

// TestBackfillSessionParents_RetriesAfterFailure was ported to the
// adapter as TestBackfillSessionParents_AdapterError_ClearsGateForRetry
// (proxy_batch4_migration_test.go): a failed ListSessions clears the
// gate and the retry repopulates — same contract, adapter mock instead
// of the failing backend.

// TestBackfillSessionParents_SkipsWhenWorkspaceNotActive was ported to
// the adapter seam (#828 batch 4): workspace-phase enforcement is the
// adapter resolve's concern (pinned in pkg/agent/opencode tests); the
// handler-level no-write-on-failure contract is pinned by
// AdapterError_ClearsGateForRetry and the nil-adapter NoRetryStorm row.

// TestBackfillSessionParents_InvalidateCachesAllowsRetry verifies that
// invalidateCaches (called on workspace suspend/restart) clears the
// backfill marker so the next call after the workspace becomes Active
// runs a fresh backfill against the new pod.
func TestBackfillSessionParents_InvalidateCachesAllowsRetry(t *testing.T) {
	// Ported to the adapter (#828 batch 4): suspend/restart clears the
	// backfill marker; the next call re-lists.
	var listCalls atomic.Int32
	env := newInputTestEnv(t)
	env.handler.sessionIndex = newRecordingSessionIndex()
	env.handler.adapter = &mockAdapter{
		listSessionsFn: func(_ context.Context, _, _ string) ([]session.Session, error) {
			listCalls.Add(1)
			return []session.Session{{ID: "ses_root"}}, nil
		},
	}

	env.handler.BackfillSessionParents(context.Background(), "ws-1")
	require.Eventually(t, func() bool { return listCalls.Load() == 1 }, time.Second, 10*time.Millisecond)

	// Simulate suspend/restart cycle.
	env.handler.invalidateCaches(context.Background(), "ws-1")

	env.handler.BackfillSessionParents(context.Background(), "ws-1")
	require.Eventually(t, func() bool { return listCalls.Load() == 2 }, time.Second, 10*time.Millisecond,
		"backfill must rerun after invalidateCaches")
}
