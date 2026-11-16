// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionalerts

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func TestRecordAlert_NonBlocking(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	svc := New(db, nil)

	// Must not block even without Start().
	svc.RecordAlert("ws-1", "ses-1", "session_hung", 960)
	assert.Equal(t, 1, len(svc.queue))
}

func TestRecordAlert_DrainsToInsert(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	var inserted atomic.Bool
	db.On("InsertSessionAlert", mock.Anything, "ws-1", "ses-x", "session_hung", 960, map[string]string(nil)).
		Run(func(mock.Arguments) { inserted.Store(true) }).
		Return(nil).Once()
	svc := New(db, nil)
	require.NoError(t, svc.Start())
	t.Cleanup(func() { _ = svc.Stop() })

	svc.RecordAlert("ws-1", "ses-x", "session_hung", 960)

	require.Eventually(t, inserted.Load, 2*time.Second, 10*time.Millisecond)
	db.AssertExpectations(t)
}

func TestStop_FlushesQueuedAlerts(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	db.On("InsertSessionAlert", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Times(3)
	svc := New(db, nil)
	require.NoError(t, svc.Start())

	svc.RecordAlert("ws-1", "ses-1", "session_hung", 960)
	svc.RecordAlert("ws-1", "ses-2", "session_hung", 1200)
	svc.RecordAlert("ws-2", "ses-3", "session_hung", 1500)

	// Stop drains synchronously: all three must land before it returns.
	require.NoError(t, svc.Stop())
	db.AssertExpectations(t)
}

func TestListByWorkspace_FiltersRetention(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	fresh := time.Now().UTC().Add(-time.Hour)
	stale := time.Now().UTC().Add(-2 * AlertRetention)
	db.On("ResolveStaleSessionAlerts", mock.Anything, "ws-1", mock.AnythingOfType("time.Time")).
		Return(int64(0), nil).Once()
	db.On("ListSessionAlerts", mock.Anything, "ws-1", 50).
		Return([]types.SessionAlert{
			{ID: "1", WorkspaceID: "ws-1", SessionID: "ses-x", CreatedAt: fresh},
			{ID: "2", WorkspaceID: "ws-1", SessionID: "ses-y", CreatedAt: stale},
		}, nil).Once()
	svc := New(db, nil)

	alerts, err := svc.ListByWorkspace(context.Background(), "ws-1", 50)
	require.NoError(t, err)
	require.Len(t, alerts, 1, "alerts older than retention are hidden")
	assert.Equal(t, "ses-x", alerts[0].SessionID)
	db.AssertExpectations(t)
}

func TestResolveWorkspace_DrainsToResolve(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	var resolved atomic.Bool
	db.On("ResolveSessionAlerts", mock.Anything, "ws-1").
		Run(func(mock.Arguments) { resolved.Store(true) }).
		Return(int64(2), nil).Once()
	svc := New(db, nil)
	require.NoError(t, svc.Start())
	t.Cleanup(func() { _ = svc.Stop() })

	svc.ResolveWorkspace("ws-1")

	require.Eventually(t, resolved.Load, 2*time.Second, 10*time.Millisecond)
	db.AssertExpectations(t)
}

func TestResolveWorkspace_ResolveFailureLoggedNotFatal(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	var resolved, inserted atomic.Bool
	db.On("ResolveSessionAlerts", mock.Anything, "ws-1").
		Run(func(mock.Arguments) { resolved.Store(true) }).
		Return(int64(0), errors.New("db down")).Once()
	db.On("InsertSessionAlert", mock.Anything, "ws-1", "ses-x", "session_hung", 960, map[string]string(nil)).
		Run(func(mock.Arguments) { inserted.Store(true) }).
		Return(nil).Once()
	svc := New(db, nil)
	require.NoError(t, svc.Start())
	t.Cleanup(func() { _ = svc.Stop() })

	svc.ResolveWorkspace("ws-1")
	require.Eventually(t, resolved.Load, 2*time.Second, 10*time.Millisecond)

	// The drainer must survive the failure (best-effort durability).
	svc.RecordAlert("ws-1", "ses-x", "session_hung", 960)
	require.Eventually(t, inserted.Load, 2*time.Second, 10*time.Millisecond)
	db.AssertExpectations(t)
}

// TestRecordTaskAlert_DrainsToInsertWithFacts (#2047): the
// worker_silent arm's task facts ride the same bounded-queue drainer
// and land in the insert call verbatim.
func TestRecordTaskAlert_DrainsToInsertWithFacts(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	var inserted atomic.Bool
	facts := map[string]string{"task_short_id": "tsk-quiet-one", "project_id": "prj-x", "lease_state": "lapsed"}
	db.On("InsertSessionAlert", mock.Anything, "ws-1", "ses-silent", "worker_silent", 2701, facts).
		Run(func(mock.Arguments) { inserted.Store(true) }).
		Return(nil).Once()
	svc := New(db, nil)
	require.NoError(t, svc.Start())
	t.Cleanup(func() { _ = svc.Stop() })

	svc.RecordTaskAlert("ws-1", "ses-silent", "worker_silent", 2701, facts)

	require.Eventually(t, inserted.Load, 2*time.Second, 10*time.Millisecond)
	db.AssertExpectations(t)
}

// TestResolveSession_DrainsToSessionResolve (#2047): the session-scoped
// resolution resolves ONLY the named session's rows — a resumed worker
// clears their own worker_silent, never a sibling's.
func TestResolveSession_DrainsToSessionResolve(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	var resolved atomic.Bool
	db.On("ResolveSessionAlertsForSession", mock.Anything, "ws-1", "ses-recovered").
		Run(func(mock.Arguments) { resolved.Store(true) }).
		Return(int64(1), nil).Once()
	// The workspace-wide resolve must NEVER fire on this path.
	db.On("ResolveSessionAlerts", mock.Anything, mock.Anything).Return(int64(0), nil).Maybe()
	svc := New(db, nil)
	require.NoError(t, svc.Start())
	t.Cleanup(func() { _ = svc.Stop() })

	svc.ResolveSession("ws-1", "ses-recovered")

	require.Eventually(t, resolved.Load, 2*time.Second, 10*time.Millisecond)
	db.AssertExpectations(t)
}

// TestListByWorkspace_HealsStaleUnresolvedOnRead pins the restart-orphan
// path: the sweep's resolution authority can be lost (API restart
// spanning recovery, workspace leaving Active, resolve-flush failure).
// The read is where every latch funnels, so the read heals.
func TestListByWorkspace_HealsStaleUnresolvedOnRead(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	healedAt := time.Now().Add(-UnresolvedStaleAfter)
	db.On("ResolveStaleSessionAlerts", mock.Anything, "ws-1", mock.AnythingOfType("time.Time")).
		Run(func(args mock.Arguments) {
			before := args.Get(2).(time.Time)
			assert.WithinDuration(t, healedAt, before, time.Minute,
				"the heal cutoff must track UnresolvedStaleAfter")
		}).
		Return(int64(2), nil).Once()
	db.On("ListSessionAlerts", mock.Anything, "ws-1", 50).
		Return([]types.SessionAlert{}, nil).Once()
	svc := New(db, nil)

	_, err := svc.ListByWorkspace(context.Background(), "ws-1", 50)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

// TestListByWorkspace_HealFailureServesReadUnhealed: the heal is
// best-effort — a failure must not take the alerts read down with it.
func TestListByWorkspace_HealFailureServesReadUnhealed(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	db.On("ResolveStaleSessionAlerts", mock.Anything, "ws-1", mock.AnythingOfType("time.Time")).
		Return(int64(0), errors.New("db blip")).Once()
	db.On("ListSessionAlerts", mock.Anything, "ws-1", 50).
		Return([]types.SessionAlert{}, nil).Once()
	svc := New(db, nil)

	_, err := svc.ListByWorkspace(context.Background(), "ws-1", 50)
	require.NoError(t, err, "the read must survive a failed heal")
	db.AssertExpectations(t)
}
