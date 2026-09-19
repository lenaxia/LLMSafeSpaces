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

// TestStartStop_NilLogger_NoPanic: New(db, nil) already yields a service
// whose RecordMessage and drainer nil-guard their logger — Start/Stop
// must not be the one pair that panics on the same construction (the
// trap #1452's integration test hit verbatim). Absorbed from #1461.
func TestStartStop_NilLogger_NoPanic(t *testing.T) {
	db := &mocks.MockDatabaseService{}
	db.On("UpsertSessionMessage", mock.Anything, mock.Anything, mock.Anything, mock.AnythingOfType("time.Time")).Return(nil)

	svc := New(db, nil)
	assert.NotPanics(t, func() {
		assert.NoError(t, svc.Start())
		svc.RecordMessage("ws-1", "sess-1", "", time.Now())
		assert.NoError(t, svc.Stop())
	})
	db.AssertCalled(t, "UpsertSessionMessage", mock.Anything, "ws-1", "sess-1", mock.AnythingOfType("time.Time"))
}

// PlanReconciliation is the #1340 convergence decision: which index rows
// to delete now, which history-bearing absents are kept for operator
// review (the triage message_count=0 safety guard), and the next
// miss-counters. Pure — the orchestration lives in the proxy handler;
// this matrix IS the S5b invariant.
func TestPlanReconciliation(t *testing.T) {
	row := func(id string, count int) types.SessionListItem {
		return types.SessionListItem{ID: id, Title: id, MessageCount: count}
	}
	tests := []struct {
		name       string
		indexRows  []types.SessionListItem
		harness    map[string]bool
		harnessErr bool
		prevMiss   map[string]int
		threshold  int
		wantRemove []string
		wantKept   []string
		wantMiss   map[string]int
	}{
		{
			name:      "present rows keep, counters clear",
			indexRows: []types.SessionListItem{row("ses_a", 0), row("ses_b", 5)},
			harness:   map[string]bool{"ses_a": true, "ses_b": true},
			wantMiss:  map[string]int{},
		},
		{
			name:      "absent once increments, no removal",
			indexRows: []types.SessionListItem{row("ses_ghost", 0)},
			harness:   map[string]bool{},
			prevMiss:  map[string]int{},
			threshold: 2,
			wantMiss:  map[string]int{"ses_ghost": 1},
		},
		{
			name:       "absent twice removes count-zero rows (N=2, guard admits)",
			indexRows:  []types.SessionListItem{row("ses_ghost", 0)},
			harness:    map[string]bool{},
			prevMiss:   map[string]int{"ses_ghost": 1},
			threshold:  2,
			wantRemove: []string{"ses_ghost"},
			wantMiss:   map[string]int{},
		},
		{
			name:      "absent history-bearing rows are KEPT for operator review (triage guard)",
			indexRows: []types.SessionListItem{row("ses_vanished", 12)},
			harness:   map[string]bool{},
			prevMiss:  map[string]int{"ses_vanished": 1},
			threshold: 2,
			wantKept:  []string{"ses_vanished"},
			wantMiss:  map[string]int{},
		},
		{
			name:      "presence resets a prior miss",
			indexRows: []types.SessionListItem{row("ses_flap", 0)},
			harness:   map[string]bool{"ses_flap": true},
			prevMiss:  map[string]int{"ses_flap": 1},
			threshold: 2,
			wantMiss:  map[string]int{},
		},
		{
			name:       "harness error keeps everything, counters frozen",
			indexRows:  []types.SessionListItem{row("ses_maybe", 0)},
			harnessErr: true,
			prevMiss:   map[string]int{"ses_maybe": 1},
			threshold:  2,
			wantMiss:   map[string]int{"ses_maybe": 1},
		},
		{
			name:       "mixed: keep, reap, operator-keep, stale counters dropped",
			indexRows:  []types.SessionListItem{row("ses_keep", 3), row("ses_gone", 0), row("ses_vanished", 7)},
			harness:    map[string]bool{"ses_keep": true},
			prevMiss:   map[string]int{"ses_gone": 1, "ses_vanished": 1, "ses_stale": 5},
			threshold:  2,
			wantRemove: []string{"ses_gone"},
			wantKept:   []string{"ses_vanished"},
			wantMiss:   map[string]int{},
		},
		{
			name:       "threshold 1 removes on the first absence (documented edge)",
			indexRows:  []types.SessionListItem{row("ses_fast", 0)},
			harness:    map[string]bool{},
			threshold:  1,
			wantRemove: []string{"ses_fast"},
			wantMiss:   map[string]int{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lister := func() (map[string]bool, error) {
				if tc.harnessErr {
					return nil, assert.AnError
				}
				return tc.harness, nil
			}
			remove, kept, misses := PlanReconciliation(tc.indexRows, lister, tc.prevMiss, tc.threshold, DefaultReapGuard)
			assert.ElementsMatch(t, tc.wantRemove, remove)
			assert.ElementsMatch(t, tc.wantKept, kept)
			assert.Equal(t, tc.wantMiss, misses)
		})
	}
}
