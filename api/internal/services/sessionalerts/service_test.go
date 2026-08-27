// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionalerts

import (
	"context"
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
	db.On("InsertSessionAlert", mock.Anything, "ws-1", "ses-x", "session_hung", 960).
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
	db.On("InsertSessionAlert", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
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
