// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package metering

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lenaxia/llmsafespaces/pkg/billing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeUsageReporter captures the events it would report to a billing provider.
type fakeUsageReporter struct {
	calls [][]billing.UsageExportEvent
	err   error
}

func (f *fakeUsageReporter) ReportUsage(_ context.Context, events []billing.UsageExportEvent) ([]int64, error) {
	f.calls = append(f.calls, events)
	if f.err != nil {
		return nil, f.err
	}
	ids := make([]int64, len(events))
	for i := range events {
		ids[i] = 1
	}
	return ids, nil
}

// expectExportUsageQueries wires the cursor/max queries and (if exportRows is
// non-nil) the usage-events-for-export query that exportToStripe executes.
// lastID/maxID control the id window. exportRows may be nil when no reporter
// is set (exportToStripe is skipped entirely in that case).
func expectExportUsageQueries(t *testing.T, mock sqlmock.Sqlmock, lastID, maxID int64, exportRows *sqlmock.Rows) {
	t.Helper()

	// 1. SELECT last_exported_id (provider is a literal in the SQL, no arg).
	mock.ExpectQuery("SELECT COALESCE\\(last_exported_id, 0\\) FROM billing_export_cursor").
		WillReturnRows(sqlmock.NewRows([]string{"last_exported_id"}).AddRow(lastID))

	// 2. SELECT MAX(id) — lastID is the $1 placeholder.
	mock.ExpectQuery("SELECT COALESCE\\(MAX\\(id\\), 0\\) FROM usage_events").
		WithArgs(lastID).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(maxID))

	if exportRows != nil {
		mock.ExpectQuery("FROM usage_events ue").
			WithArgs(lastID, maxID).
			WillReturnRows(exportRows)
	}
}

func expectExportCursorUpdate(t *testing.T, mock sqlmock.Sqlmock, maxID int64) {
	t.Helper()
	mock.ExpectExec("INSERT INTO billing_export_cursor").
		WithArgs(maxID).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestExportUsage_NoNewEvents_DoesNothing(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	expectExportUsageQueries(t, mock, 100, 100, nil)
	n, err := svc.ExportUsage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	// No cursor update when there is nothing to export.
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestExportUsage_NoReporter_OnlyAdvancesCursor(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	expectExportUsageQueries(t, mock, 10, 25, nil)
	expectExportCursorUpdate(t, mock, 25)

	n, err := svc.ExportUsage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 15, n)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestExportUsage_WithReporter_AggregatesAndReports(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	reporter := &fakeUsageReporter{}
	svc.SetUsageReporter(reporter)

	expectExportUsageQueries(t, mock, 10, 25,
		sqlmock.NewRows([]string{"event_type", "sum", "max_event_time", "external_customer_id"}).
			AddRow("llm_tokens", int64(1500), "2026-06-15T10:00:00Z", "cus_abc").
			AddRow("compute_seconds", int64(60), "2026-06-15T10:00:01Z", "cus_abc"),
	)
	expectExportCursorUpdate(t, mock, 25)

	n, err := svc.ExportUsage(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 15, n)

	require.Len(t, reporter.calls, 1, "reporter must be called exactly once")
	got := reporter.calls[0]
	require.Len(t, got, 2)
	assert.Equal(t, "llm_tokens", got[0].EventType)
	assert.Equal(t, int64(1500), got[0].Quantity)
	assert.Equal(t, "cus_abc", got[0].ExternalCustomerID)
	assert.Equal(t, "meter-cus_abc-llm_tokens-10-25", got[0].IdempotencyKey,
		"idempotency key must be deterministic per customer+type+window")
	assert.Equal(t, "2026-06-15T10:00:00Z", got[0].Timestamp)
	assert.Equal(t, "compute_seconds", got[1].EventType)
	assert.Equal(t, int64(60), got[1].Quantity)
	assert.Equal(t, "meter-cus_abc-compute_seconds-10-25", got[1].IdempotencyKey)
	require.NoError(t, mock.ExpectationsWereMet())
}

// Aggregation: multiple events for the same customer+type in the window must
// be summed into a single report (regression test for the per-event GROUP BY
// bug that produced N Stripe calls instead of 1).
func TestExportUsage_WithReporter_AggregatesMultipleEventsIntoOneReport(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	reporter := &fakeUsageReporter{}
	svc.SetUsageReporter(reporter)

	// Simulate 3 llm_tokens events for the same customer summed into one row.
	expectExportUsageQueries(t, mock, 10, 13,
		sqlmock.NewRows([]string{"event_type", "sum", "max_event_time", "external_customer_id"}).
			AddRow("llm_tokens", int64(3000), "2026-06-15T10:05:00Z", "cus_abc"),
	)
	expectExportCursorUpdate(t, mock, 13)

	_, err := svc.ExportUsage(context.Background())
	require.NoError(t, err)

	require.Len(t, reporter.calls, 1, "reporter must be called once with aggregated events")
	require.Len(t, reporter.calls[0], 1, "aggregation must produce exactly one event per customer+type")
	assert.Equal(t, int64(3000), reporter.calls[0][0].Quantity)
	require.NoError(t, mock.ExpectationsWereMet())
}

// When the customer has no billing account, the row's external_customer_id is
// empty. exportToStripe must skip these rows so we don't bill a phantom.
func TestExportUsage_WithReporter_SkipsEventsWithoutCustomer(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	reporter := &fakeUsageReporter{}
	svc.SetUsageReporter(reporter)

	expectExportUsageQueries(t, mock, 10, 11,
		sqlmock.NewRows([]string{"event_type", "sum", "max_event_time", "external_customer_id"}).
			AddRow("llm_tokens", int64(5), "2026-06-15T10:00:00Z", ""),
	)
	expectExportCursorUpdate(t, mock, 11)

	_, err := svc.ExportUsage(context.Background())
	require.NoError(t, err)
	require.Empty(t, reporter.calls, "orphan events with no customer must not be reported")
	require.NoError(t, mock.ExpectationsWereMet())
}

// A Stripe failure must hold the cursor and surface the error (#758):
// the window is retried on the next export cycle with identical
// idempotency keys, so already-reported groups dedupe at Stripe and the
// failed ones consume a new record. Advancing the cursor here was
// permanent revenue loss for the failed window.
func TestExportUsage_ReporterFailure_HoldsCursorAndSurfacesError(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	reporter := &fakeUsageReporter{err: errors.New("stripe timeout")}
	svc.SetUsageReporter(reporter)

	expectExportUsageQueries(t, mock, 10, 12,
		sqlmock.NewRows([]string{"event_type", "sum", "max_event_time", "external_customer_id"}).
			AddRow("llm_tokens", int64(5), "2026-06-15T10:00:00Z", "cus_abc"),
	)

	_, err := svc.ExportUsage(context.Background())
	require.Error(t, err, "ExportUsage must surface reporter failures so operators see held exports")
	assert.Contains(t, err.Error(), "cursor", "error should carry cursor context")
	require.NoError(t, mock.ExpectationsWereMet(),
		"the cursor INSERT must never run when the reporter failed")
}

// Empty export rows (cursor advanced but reporter path returns nothing) must
// not invoke the reporter at all.
func TestExportUsage_WithReporter_EmptyRows_NoCall(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	reporter := &fakeUsageReporter{}
	svc.SetUsageReporter(reporter)

	expectExportUsageQueries(t, mock, 10, 25,
		sqlmock.NewRows([]string{"event_type", "sum", "max_event_time", "external_customer_id"}),
	)
	expectExportCursorUpdate(t, mock, 25)

	_, err := svc.ExportUsage(context.Background())
	require.NoError(t, err)
	require.Empty(t, reporter.calls, "reporter must not be called when there are no billable events")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestExportUsage_QueryError_AbortsAndDoesNotAdvanceCursor(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	reporter := &fakeUsageReporter{}
	svc.SetUsageReporter(reporter)

	mock.ExpectQuery("SELECT COALESCE\\(last_exported_id, 0\\) FROM billing_export_cursor").
		WillReturnRows(sqlmock.NewRows([]string{"last_exported_id"}).AddRow(10))
	mock.ExpectQuery("SELECT COALESCE\\(MAX\\(id\\), 0\\) FROM usage_events").
		WithArgs(int64(10)).
		WillReturnError(errors.New("connection refused"))

	_, err := svc.ExportUsage(context.Background())
	require.Error(t, err)
	require.Empty(t, reporter.calls)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSetUsageReporter_NilClears(t *testing.T) {
	svc, _, cleanup := setupTestService(t)
	defer cleanup()

	reporter := &fakeUsageReporter{}
	svc.SetUsageReporter(reporter)
	svc.SetUsageReporter(nil)

	assert.Nil(t, svc.usageRpt, "SetUsageReporter(nil) must clear the reporter")
}

// --- #758: partial-failure retry + idempotency-key window binding ---

// retryReporter captures every report attempt and fails groups according
// to failFn (evaluated per attempt).
type retryReporter struct {
	calls   [][]billing.UsageExportEvent
	failFn  func(e billing.UsageExportEvent, attempt int) bool
	attempt int
}

func (r *retryReporter) ReportUsage(_ context.Context, events []billing.UsageExportEvent) ([]int64, error) {
	r.attempt++
	r.calls = append(r.calls, events)
	ids := make([]int64, 0, len(events))
	for _, e := range events {
		if r.failFn != nil && r.failFn(e, r.attempt) {
			return ids, fmt.Errorf("report usage for %s: stripe unavailable", e.EventType)
		}
		ids = append(ids, 1)
	}
	return ids, nil
}

func (r *retryReporter) countForType(eventType string) int {
	n := 0
	for _, call := range r.calls {
		for _, e := range call {
			if e.EventType == eventType {
				n++
			}
		}
	}
	return n
}

// TestExportUsage_PartialFailure_RetriesOnlyFailedGroups pins the
// partial-success semantics within one export cycle: groups that
// reported successfully are not re-attempted (their idempotency keys
// stay stable), only the failed ones retry, and the cursor advances
// once every group has been reported.
func TestExportUsage_PartialFailure_RetriesOnlyFailedGroups(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	reporter := &retryReporter{
		// group A always succeeds; group B fails on attempts 1 and 2,
		// succeeds on attempt 3.
		failFn: func(e billing.UsageExportEvent, attempt int) bool {
			return e.EventType == "compute_seconds" && attempt < 3
		},
	}
	svc.SetUsageReporter(reporter)

	expectExportUsageQueries(t, mock, 10, 12,
		sqlmock.NewRows([]string{"event_type", "sum", "max_event_time", "external_customer_id"}).
			AddRow("llm_tokens", int64(5), "2026-06-15T10:00:00Z", "cus_abc").
			AddRow("compute_seconds", int64(900), "2026-06-15T10:00:00Z", "cus_abc"),
	)
	expectExportCursorUpdate(t, mock, 12)

	n, err := svc.ExportUsage(context.Background())
	require.NoError(t, err, "cycle succeeds once group B recovers")
	assert.Equal(t, 2, n)

	assert.Equal(t, 1, reporter.countForType("llm_tokens"),
		"successfully reported group must not be re-attempted")
	assert.Equal(t, 3, reporter.countForType("compute_seconds"),
		"failed group must be retried until success")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestExportUsage_RetryExhausted_CursorHeld: when retries are exhausted
// the cycle fails, the cursor stays put, and the group that already
// succeeded was never re-attempted.
func TestExportUsage_RetryExhausted_CursorHeld(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	reporter := &retryReporter{
		failFn: func(e billing.UsageExportEvent, attempt int) bool {
			return e.EventType == "compute_seconds"
		},
	}
	svc.SetUsageReporter(reporter)

	expectExportUsageQueries(t, mock, 10, 12,
		sqlmock.NewRows([]string{"event_type", "sum", "max_event_time", "external_customer_id"}).
			AddRow("llm_tokens", int64(5), "2026-06-15T10:00:00Z", "cus_abc").
			AddRow("compute_seconds", int64(900), "2026-06-15T10:00:00Z", "cus_abc"),
	)

	_, err := svc.ExportUsage(context.Background())
	require.Error(t, err)

	assert.Equal(t, 1, reporter.countForType("llm_tokens"),
		"succeeded group must not be re-attempted while its sibling exhausts retries")
	assert.Equal(t, exportMaxAttempts, reporter.countForType("compute_seconds"))
	require.NoError(t, mock.ExpectationsWereMet(), "cursor must never be written on failure")
}

// TestExportUsage_IdempotencyKeyBindsWindow pins the key format: both
// window bounds (lastID, maxID) so a same-window retry is airtight at
// Stripe's idempotency layer even across process restarts.
func TestExportUsage_IdempotencyKeyBindsWindow(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	reporter := &fakeUsageReporter{}
	svc.SetUsageReporter(reporter)

	expectExportUsageQueries(t, mock, 100, 200,
		sqlmock.NewRows([]string{"event_type", "sum", "max_event_time", "external_customer_id"}).
			AddRow("llm_tokens", int64(500), "2026-08-26T00:00:00Z", "cus_123"),
	)
	expectExportCursorUpdate(t, mock, 200)

	_, err := svc.ExportUsage(context.Background())
	require.NoError(t, err)

	require.Len(t, reporter.calls, 1)
	require.Len(t, reporter.calls[0], 1)
	key := reporter.calls[0][0].IdempotencyKey
	assert.True(t, strings.HasPrefix(key, "meter-cus_123-llm_tokens-"),
		"key must be scoped to customer + event type, got %q", key)
	assert.Contains(t, key, "-100-200",
		"key must bind BOTH window bounds [lastID, maxID], got %q", key)
}
