// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// #828 batch 4: the session-index/parents helpers are adapter-only. The
// raw-HTTP tails are deleted — a nil adapter (dev/test wiring) is a
// no-op for fire-and-forget helpers and an error for the fetcher; and
// the backfill gate keys on the adapter, not the (retired) dialect.

func TestFetchAndPersistTitle_NilAdapter_NoPodHTTP(t *testing.T) {
	var hits atomic.Int32
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.handler.sessionIndex = newRecordingSessionIndex()
	require.Nil(t, env.handler.adapter, "precondition: no adapter")

	env.handler.fetchAndPersistTitle("ws-1", "ses_1")

	assert.Zero(t, hits.Load(), "the raw-HTTP tail is gone; a nil adapter must not reach the pod")
}

func TestFetchAndPersistTitle_AdapterPath_PersistsTitleAndParent(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		getSessionFn: func(_ context.Context, _, _, sid string) (*session.Session, error) {
			assert.Equal(t, "ses_1", sid)
			return &session.Session{ID: "ses_1", Title: "My Title", ParentID: "ses_root"}, nil
		},
	}
	si := newRecordingSessionIndex()
	env.handler.sessionIndex = si

	env.handler.fetchAndPersistTitle("ws-1", "ses_1")

	require.Eventually(t, func() bool { return si.parentCount() > 0 }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, "My Title", si.titleOf("ses_1"))
	assert.Equal(t, "ses_root", si.parentOf("ses_1"))
}

func TestFetchSessionParent_NilAdapter_ReturnsTypedError(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	require.Nil(t, env.handler.adapter, "precondition: no adapter")

	parent, err := env.handler.fetchSessionParent(context.Background(), "ws-1", "ses_1")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent adapter not configured")
	assert.Empty(t, parent)
}

func TestBackfillSessionParents_NilAdapter_NoRetryStorm(t *testing.T) {
	var hits atomic.Int32
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.sessionIndex = newRecordingSessionIndex()
	require.Nil(t, env.handler.adapter, "precondition: no adapter")

	// Repeated calls: the adapter gate returns before any goroutine or
	// HTTP fires — the raw SessionListPath tail is gone and nothing
	// spawns at all.
	for i := 0; i < 5; i++ {
		env.handler.BackfillSessionParents(context.Background(), "ws-1")
	}
	time.Sleep(150 * time.Millisecond)

	assert.Zero(t, hits.Load(), "the raw SessionListPath tail is gone")
	assert.Zero(t, env.handler.sessionIndex.(*recordingSessionIndex).parentCount(),
		"nothing written — no goroutine ever ran")
}

// The gate keys on the adapter, not the dialect (retired): with an
// adapter wired and no dialect anywhere, the backfill runs.
func TestBackfillSessionParents_AdapterPath_NoDialect_WritesParents(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	env.handler.adapter = &mockAdapter{
		listSessionsFn: func(_ context.Context, _, _ string) ([]session.Session, error) {
			return []session.Session{
				{ID: "ses_root"},
				{ID: "ses_child", ParentID: "ses_root"},
			}, nil
		},
	}
	si := newRecordingSessionIndex()
	env.handler.sessionIndex = si

	env.handler.BackfillSessionParents(context.Background(), "ws-1")

	require.Eventually(t, func() bool { return si.parentCount() == 1 }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, "ses_root", si.parentOf("ses_child"))
}

// The adapter-arm retry contract (previously pinned against the legacy
// backend): a failed ListSessions clears the backfilled gate so the next
// call retries.
func TestBackfillSessionParents_AdapterError_ClearsGateForRetry(t *testing.T) {
	env := newTestEnv(t)
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", "Active", "ws-1")
	fail := true
	env.handler.adapter = &mockAdapter{
		listSessionsFn: func(_ context.Context, _, _ string) ([]session.Session, error) {
			if fail {
				return nil, assert.AnError
			}
			return []session.Session{{ID: "ses_recovered", ParentID: "ses_root"}}, nil
		},
	}
	si := newRecordingSessionIndex()
	env.handler.sessionIndex = si

	env.handler.BackfillSessionParents(context.Background(), "ws-1")
	require.Eventually(t, func() bool {
		return !env.handler.state().GetParentBackfilled(context.Background(), "ws-1")
	}, 2*time.Second, 10*time.Millisecond, "a failed backfill must clear the gate")

	fail = false
	env.handler.BackfillSessionParents(context.Background(), "ws-1")
	require.Eventually(t, func() bool { return si.parentCount() == 1 }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, "ses_root", si.parentOf("ses_recovered"))
}
