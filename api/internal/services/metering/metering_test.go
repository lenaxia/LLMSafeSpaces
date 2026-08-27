// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package metering

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	"github.com/stretchr/testify/assert"
	testifymock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func setupTestService(t *testing.T) (*Service, sqlmock.Sqlmock, func()) {
	t.Helper()

	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	require.NoError(t, err, "failed to create mock database")

	log, err := logger.New(true, "debug", "console")
	require.NoError(t, err, "failed to create test logger")

	cfg := &config.Config{}

	svc := &Service{
		logger:  log.With("component", "metering-test"),
		config:  cfg,
		db:      db,
		billCtx: context.Background(),
		ch:      make(chan UsageEvent, 4096),
		done:    make(chan struct{}),
		stopCtx: context.Background(),
	}

	return svc, mock, func() { db.Close() }
}

func newStartedService(t *testing.T) (*Service, sqlmock.Sqlmock, func()) {
	t.Helper()
	svc, mock, cleanup := setupTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	svc.stopCtx = ctx
	svc.stopFn = cancel
	go svc.run()
	return svc, mock, cleanup
}

func makeEvent(eventType string, quantity int64) types.UsageEvent {
	return types.UsageEvent{
		IdempotencyKey: fmt.Sprintf("key-%d", time.Now().UnixNano()),
		Owner:          types.BillingOwner{ID: "user-1", Type: types.OwnerTypeUser},
		ActorID:        "user-1",
		WorkspaceID:    "ws-1",
		EventType:      eventType,
		Quantity:       quantity,
		Source:         "api",
		EventTime:      time.Now(),
	}
}

func TestRecord_NonBlocking(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	svc.ch = make(chan UsageEvent, 2)

	// Buffer-full events take the synchronous direct-write fallback, so
	// Record still must not block — the DB call has its own timeout.
	mock.ExpectExec("INSERT INTO usage_events").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	done := make(chan struct{})
	go func() {
		svc.Record(makeEvent("llm_request", 1))
		svc.Record(makeEvent("llm_request", 1))
		svc.Record(makeEvent("llm_request", 1))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record should not block even when channel is full")
	}

	assert.Equal(t, uint64(0), svc.metrics.dropped.Load(),
		"buffer-full events must be direct-written, not dropped (#766)")
}

// TestRecord_BufferFull_WritesDirectlyToDB pins the #766 core fix: when
// the async buffer is full, the event is written to the DB synchronously
// instead of being silently dropped.
func TestRecord_BufferFull_WritesDirectlyToDB(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	svc.ch = make(chan UsageEvent, 1)

	overflow := makeEvent("llm_tokens", 50)
	overflow.IdempotencyKey = "overflow-key"

	mock.ExpectExec("INSERT INTO usage_events").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	svc.Record(makeEvent("llm_tokens", 1)) // fills the buffer
	svc.Record(overflow)                   // buffer full → direct write

	assert.Equal(t, uint64(0), svc.metrics.dropped.Load(),
		"buffer-full event must not be dropped")
	assert.Equal(t, uint64(1), svc.metrics.written.Load(),
		"buffer-full event must be written via the direct path")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestRecord_BufferFull_DirectWriteFails_Drops: if the DB itself rejects
// the fallback write, the drop is counted and surfaced — never silent.
func TestRecord_BufferFull_DirectWriteFails_Drops(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	svc.ch = make(chan UsageEvent, 1)

	mock.ExpectExec("INSERT INTO usage_events").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(fmt.Errorf("db down"))

	svc.Record(makeEvent("llm_tokens", 1)) // fills the buffer
	svc.Record(makeEvent("llm_tokens", 2)) // direct write fails

	assert.Equal(t, uint64(1), svc.metrics.dropped.Load(),
		"failed direct write must be counted as dropped")
	assert.Equal(t, uint64(0), svc.metrics.written.Load())
}

// TestRecord_AfterStop_WritesDirectly: events arriving after Stop are
// written synchronously rather than dropped — the shutdown window is
// small but billing events must not vanish in it (#766 TOCTOU).
func TestRecord_AfterStop_WritesDirectly(t *testing.T) {
	svc, mock, cleanup := newStartedService(t)
	defer cleanup()

	svc.Stop()

	late := makeEvent("llm_tokens", 3)
	late.IdempotencyKey = "late-key"
	mock.ExpectExec("INSERT INTO usage_events").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	svc.Record(late)

	assert.Equal(t, uint64(0), svc.metrics.dropped.Load(),
		"post-stop event must be direct-written, not dropped")
	assert.Equal(t, uint64(1), svc.metrics.written.Load())
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestRecord_SendOnClosedChannel_RecoversViaDirectWrite pins the TOCTOU
// half of #766: a Record racing Stop's close(s.ch) panics on send; the
// recover handler must salvage the event via the direct write instead of
// dropping it. Simulated deterministically by closing the channel out
// from under Record without setting the closed flag.
func TestRecord_SendOnClosedChannel_RecoversViaDirectWrite(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	svc.ch = make(chan UsageEvent, 1)
	close(svc.ch) // exactly what Stop does; closed flag stays false

	raced := makeEvent("llm_tokens", 7)
	raced.IdempotencyKey = "race-key"
	mock.ExpectExec("INSERT INTO usage_events").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	svc.Record(raced)

	assert.Equal(t, uint64(0), svc.metrics.dropped.Load(),
		"stop-race event must be salvaged by the direct write")
	assert.Equal(t, uint64(1), svc.metrics.written.Load())
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestRecord_AfterStop(t *testing.T) {
	svc, _, cleanup := newStartedService(t)
	defer cleanup()

	svc.Stop()

	svc.Record(makeEvent("llm_request", 1))

	assert.Equal(t, uint64(1), svc.metrics.dropped.Load(),
		"Record after Stop with a failing direct write increments dropped")
}

func TestRecord_SkipsEmptyEventType(t *testing.T) {
	svc, _, cleanup := setupTestService(t)
	defer cleanup()
	svc.ch = make(chan UsageEvent, 10)

	evt := makeEvent("", 1)
	svc.Record(evt)

	select {
	case <-svc.ch:
		t.Fatal("empty event_type events should not be enqueued")
	default:
	}
}

func TestBatchFlush_WritesToDB(t *testing.T) {
	svc, mock, cleanup := newStartedService(t)
	defer cleanup()
	defer svc.Stop()

	events := make([]UsageEvent, 5)
	for i := range events {
		events[i] = makeEvent("llm_tokens", 100)
		events[i].IdempotencyKey = fmt.Sprintf("batch-key-%d-%d", time.Now().UnixNano(), i)
		events[i].EventSubtype = "input"
	}

	mock.ExpectBegin()
	for i := range events {
		mock.ExpectExec("INSERT INTO usage_events").
			WillReturnResult(sqlmock.NewResult(int64(i+1), 1))
	}
	mock.ExpectCommit()

	for _, evt := range events {
		svc.Record(evt)
	}

	assert.Eventually(t, func() bool {
		return svc.metrics.written.Load() >= 5
	}, 3*time.Second, 50*time.Millisecond, "all events should be written to DB")

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestBatchFlush_DLQOnDBFailure(t *testing.T) {
	svc, mock, cleanup := newStartedService(t)
	defer cleanup()
	defer svc.Stop()

	evt := makeEvent("llm_request", 1)
	evt.IdempotencyKey = "dlq-test-key"

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO usage_events").
		WillReturnError(fmt.Errorf("db connection lost"))
	mock.ExpectRollback()

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO usage_events_dlq").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	svc.Record(evt)

	assert.Eventually(t, func() bool {
		return svc.metrics.failed.Load() >= 1
	}, 3*time.Second, 50*time.Millisecond, "failed events should be tracked")

	// Wait for the async DLQ write to complete before checking mock expectations.
	assert.Eventually(t, func() bool {
		return mock.ExpectationsWereMet() == nil
	}, 3*time.Second, 50*time.Millisecond, "all mock expectations should be met (including DLQ INSERT)")

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestBatchFlush_DLQAlsoFails(t *testing.T) {
	svc, mock, cleanup := newStartedService(t)
	defer cleanup()
	defer svc.Stop()

	evt := makeEvent("llm_request", 1)
	evt.IdempotencyKey = "dlq-double-fail-key"

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO usage_events").
		WillReturnError(fmt.Errorf("db down"))
	mock.ExpectRollback()

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO usage_events_dlq").
		WillReturnError(fmt.Errorf("db still down"))
	mock.ExpectRollback()

	svc.Record(evt)

	assert.Eventually(t, func() bool {
		return svc.metrics.dropped.Load() >= 1
	}, 3*time.Second, 50*time.Millisecond, "should count as dropped when both writes fail")

	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestStop_DrainsBuffer(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	svc.stopCtx = ctx
	svc.stopFn = cancel

	go svc.run()

	for i := 0; i < 10; i++ {
		evt := makeEvent("api_call", 1)
		evt.IdempotencyKey = fmt.Sprintf("drain-key-%d", i)
		svc.Record(evt)
	}

	mock.ExpectBegin()
	for i := 0; i < 10; i++ {
		mock.ExpectExec("INSERT INTO usage_events").
			WillReturnResult(sqlmock.NewResult(int64(i+1), 1))
	}
	mock.ExpectCommit()

	start := time.Now()
	svc.Stop()
	elapsed := time.Since(start)

	assert.True(t, svc.closed.Load(), "service should be marked closed")
	assert.Less(t, elapsed, 5*time.Second, "Stop should complete within timeout")

	assert.Eventually(t, func() bool {
		return svc.metrics.written.Load() >= 10
	}, 2*time.Second, 50*time.Millisecond)
}

func TestStop_Idempotent(t *testing.T) {
	svc, _, cleanup := setupTestService(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	svc.stopCtx = ctx
	svc.stopFn = cancel

	go svc.run()

	svc.Stop()
	svc.Stop()
	svc.Stop()
}

func TestDLQReaper_RetriesSuccessfully(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	evt := makeEvent("llm_request", 1)
	evt.IdempotencyKey = "dlq-retry-key"
	payload, _ := json.Marshal(evt)

	rows := sqlmock.NewRows([]string{"id", "payload", "error_message", "retry_count", "first_failed_at", "last_retried_at", "resolved_at", "resolution"}).
		AddRow(int64(1), payload, "db error", 0, time.Now(), nil, nil, nil)

	mock.ExpectQuery("SELECT (.+) FROM usage_events_dlq").
		WillReturnRows(rows)

	mock.ExpectExec("INSERT INTO usage_events").
		WithArgs(
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mock.ExpectExec("UPDATE usage_events_dlq").
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mock.ExpectQuery("SELECT COUNT").WillReturnRows(
		sqlmock.NewRows([]string{"count"}).AddRow(0))

	err := svc.reapDLQ(context.Background())
	assert.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestDLQReaper_MarksDeadAfterMaxRetries(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	evt := makeEvent("llm_request", 1)
	evt.IdempotencyKey = "dlq-dead-key"
	payload, _ := json.Marshal(evt)

	rows := sqlmock.NewRows([]string{"id", "payload", "error_message", "retry_count", "first_failed_at", "last_retried_at", "resolved_at", "resolution"}).
		AddRow(int64(1), payload, "db error", 4, time.Now(), time.Now(), nil, nil)

	mock.ExpectQuery("SELECT (.+) FROM usage_events_dlq").
		WillReturnRows(rows)

	mock.ExpectExec("INSERT INTO usage_events").
		WillReturnError(fmt.Errorf("still failing"))

	mock.ExpectExec("UPDATE usage_events_dlq").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mock.ExpectQuery("SELECT COUNT").WillReturnRows(
		sqlmock.NewRows([]string{"count"}).AddRow(0))

	err := svc.reapDLQ(context.Background())
	assert.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestRecord_Concurrency(t *testing.T) {
	svc, mock, cleanup := newStartedService(t)
	defer cleanup()
	defer svc.Stop()

	mock.ExpectBegin()
	for i := 0; i < 100; i++ {
		mock.ExpectExec("INSERT INTO usage_events").
			WillReturnResult(sqlmock.NewResult(int64(i+1), 1))
	}
	mock.ExpectCommit()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			evt := makeEvent("api_call", 1)
			evt.IdempotencyKey = fmt.Sprintf("concurrent-key-%d", idx)
			svc.Record(evt)
		}(i)
	}
	wg.Wait()

	assert.Eventually(t, func() bool {
		return svc.metrics.written.Load()+svc.metrics.failed.Load()+svc.metrics.dropped.Load() >= 100
	}, 5*time.Second, 50*time.Millisecond)
}

func TestNew_CreatesService(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	log, err := logger.New(true, "debug", "console")
	require.NoError(t, err)

	cfg := &config.Config{}

	svc, err := New(cfg, log, db)
	require.NoError(t, err)
	require.NotNil(t, svc)
	assert.Equal(t, false, svc.closed.Load())
	assert.NotNil(t, svc.ch)
	assert.Equal(t, 4096, cap(svc.ch))
}

func TestStartStop(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	log, err := logger.New(true, "debug", "console")
	require.NoError(t, err)

	cfg := &config.Config{}
	svc, err := New(cfg, log, db)
	require.NoError(t, err)

	require.NoError(t, svc.Start())

	mock.ExpectQuery("SELECT COUNT").WillReturnRows(
		sqlmock.NewRows([]string{"count"}).AddRow(0))

	time.Sleep(100 * time.Millisecond)
	require.NoError(t, svc.Stop())
}

func TestRecordLifecycleEvent_Success(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	mock.ExpectExec("INSERT INTO workspace_lifecycle_events").
		WithArgs("ws-1", "user-1", "user", "Creating", "Active", "standard", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := svc.RecordLifecycleEvent(
		context.Background(),
		"ws-1", "user-1", OwnerTypeUser,
		"Creating", "Active", "standard",
		time.Now(),
	)
	assert.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestRecordLifecycleEvent_DBFailure(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	mock.ExpectExec("INSERT INTO workspace_lifecycle_events").
		WillReturnError(fmt.Errorf("db unavailable"))

	err := svc.RecordLifecycleEvent(
		context.Background(),
		"ws-1", "user-1", OwnerTypeUser,
		"", "Pending", "",
		time.Now(),
	)
	assert.Error(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestRecordLifecycleEvent_NullFromPhase(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	mock.ExpectExec("INSERT INTO workspace_lifecycle_events").
		WithArgs("ws-2", "user-2", "user", "", "Pending", "", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := svc.RecordLifecycleEvent(
		context.Background(),
		"ws-2", "user-2", OwnerTypeUser,
		"", "Pending", "",
		time.Now(),
	)
	assert.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestReconcileComputeTime_FillsGap(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	dbSvc := new(mocks.MockDatabaseService)
	svc.SetDatabaseService(dbSvc)

	activePhases := map[string]string{"ws-1": "Active"}
	svc.SetActivePhasesChecker(func() map[string]string { return activePhases })

	dbSvc.On("ListAllWorkspaceOwners", testifymock.Anything).Return(map[string]string{"ws-1": "user-1"}, nil)

	enteredActive := time.Now().Add(-5 * time.Minute)
	mock.ExpectQuery("SELECT event_time FROM workspace_lifecycle_events").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"event_time"}).AddRow(enteredActive))

	mock.ExpectQuery("SELECT event_time FROM usage_events").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"event_time"}).AddRow(enteredActive))

	err := svc.reconcileComputeTime(context.Background())
	assert.NoError(t, err)

	chLen := len(svc.ch)
	assert.True(t, chLen >= 19, "should emit compute buckets for the gap, got %d events in channel", chLen)

	dbSvc.AssertExpectations(t)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestReconcileComputeTime_SkipsNonActive(t *testing.T) {
	svc, _, cleanup := setupTestService(t)
	defer cleanup()

	dbSvc := new(mocks.MockDatabaseService)
	svc.SetDatabaseService(dbSvc)
	svc.SetActivePhasesChecker(func() map[string]string {
		return map[string]string{"ws-1": "Suspended"}
	})

	dbSvc.On("ListAllWorkspaceOwners", testifymock.Anything).Return(map[string]string{"ws-1": "user-1"}, nil)

	err := svc.reconcileComputeTime(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, uint64(0), svc.metrics.written.Load())
}

func TestReconcileComputeTime_NoPhasesChecker(t *testing.T) {
	svc, _, cleanup := setupTestService(t)
	defer cleanup()

	err := svc.reconcileComputeTime(context.Background())
	assert.NoError(t, err)
}
