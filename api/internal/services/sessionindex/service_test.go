// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionindex

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func TestRecordMessage_NonBlocking(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	svc := New(db, nil)

	// Should not block even without Start()
	svc.RecordMessage("ws-1", "sess-1", "", time.Now())
	assert.Equal(t, 1, len(svc.queue))
}

func TestRecordMessage_DropsOldestWhenFull(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	svc := &Service{
		db:     db,
		queue:  make(chan recordEvent, 2),
		closeC: make(chan struct{}),
	}

	svc.RecordMessage("ws-1", "s1", "", time.Now())
	svc.RecordMessage("ws-1", "s2", "", time.Now())
	// Queue is full (cap=2), next push should drop oldest
	svc.RecordMessage("ws-1", "s3", "", time.Now())

	assert.Equal(t, 2, len(svc.queue))
}

func TestDrain_CallsUpsert(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	db.On("UpsertSessionMessage", mock.Anything, "ws-1", "sess-1", mock.AnythingOfType("time.Time")).Return(nil)

	svc := &Service{
		db:     db,
		queue:  make(chan recordEvent, 10),
		closeC: make(chan struct{}),
	}

	now := time.Now()
	svc.queue <- recordEvent{workspaceID: "ws-1", sessionID: "sess-1", at: now}
	close(svc.closeC)
	svc.wg.Add(1)
	svc.drain()

	db.AssertCalled(t, "UpsertSessionMessage", mock.Anything, "ws-1", "sess-1", now)
}

func TestListByWorkspace_DelegatesToDB(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	expected := []types.SessionListItem{
		{ID: "s1", Title: "Chat 1", MessageCount: 5, Status: "idle"},
	}
	db.On("ListSessionIndex", mock.Anything, "ws-1").Return(expected, nil)

	svc := &Service{db: db}
	result, err := svc.ListByWorkspace(context.Background(), "ws-1")

	assert.NoError(t, err)
	assert.Equal(t, expected, result)
}

func TestDeleteByWorkspace_DelegatesToDB(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	db.On("DeleteSessionIndex", mock.Anything, "ws-1").Return(nil)

	svc := &Service{db: db}
	err := svc.DeleteByWorkspace(context.Background(), "ws-1")

	assert.NoError(t, err)
	db.AssertCalled(t, "DeleteSessionIndex", mock.Anything, "ws-1")
}

func TestUpsertTitle_DelegatesToDB(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	db.On("UpsertSessionTitle", mock.Anything, "ws-1", "s1", "New Title").Return(nil)

	svc := &Service{db: db}
	err := svc.UpsertTitle(context.Background(), "ws-1", "s1", "New Title")

	assert.NoError(t, err)
	db.AssertCalled(t, "UpsertSessionTitle", mock.Anything, "ws-1", "s1", "New Title")
}

func TestDeleteSession_DelegatesToDB(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	db.On("DeleteSessionTree", mock.Anything, "ws-1", "sess-1").Return(nil)

	svc := &Service{db: db}
	err := svc.DeleteSession(context.Background(), "ws-1", "sess-1")

	assert.NoError(t, err)
	db.AssertCalled(t, "DeleteSessionTree", mock.Anything, "ws-1", "sess-1")
}

func TestDeleteSession_DBError(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	db.On("DeleteSessionTree", mock.Anything, "ws-1", "sess-1").Return(assert.AnError)

	svc := &Service{db: db}
	err := svc.DeleteSession(context.Background(), "ws-1", "sess-1")

	assert.Error(t, err)
}

func TestUpsertContextUsed_DelegatesToDB(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	db.On("UpsertSessionContextUsed", mock.Anything, "ws-1", "ses_abc", int64(12500)).Return(nil)

	svc := &Service{db: db}
	err := svc.UpsertContextUsed(context.Background(), "ws-1", "ses_abc", 12500)

	assert.NoError(t, err)
	db.AssertCalled(t, "UpsertSessionContextUsed", mock.Anything, "ws-1", "ses_abc", int64(12500))
}

func TestUpsertContextUsed_DBError(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	db.On("UpsertSessionContextUsed", mock.Anything, "ws-1", "ses_abc", int64(5000)).Return(assert.AnError)

	svc := &Service{db: db}
	err := svc.UpsertContextUsed(context.Background(), "ws-1", "ses_abc", 5000)

	assert.Error(t, err)
}

func TestUpsertContextUsed_ZeroValue(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	db.On("UpsertSessionContextUsed", mock.Anything, "ws-1", "ses_abc", int64(0)).Return(nil)

	svc := &Service{db: db}
	err := svc.UpsertContextUsed(context.Background(), "ws-1", "ses_abc", 0)

	assert.NoError(t, err)
	db.AssertCalled(t, "UpsertSessionContextUsed", mock.Anything, "ws-1", "ses_abc", int64(0))
}
