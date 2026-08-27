// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package metering

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

var errDBDown = errors.New("db down")

// --- #768(c): atomic quota reservation (ReserveQuota) ---

// expectLimitRow wires the usage_limits read shared by CheckQuota and
// ReserveQuota.
func expectLimitRow(mock sqlmock.Sqlmock, limit int64, periodType string) {
	mock.ExpectQuery("SELECT max_quantity, period_type FROM usage_limits").
		WillReturnRows(sqlmock.NewRows([]string{"max_quantity", "period_type"}).AddRow(limit, periodType))
}

func TestReserveQuota_Allowed_UsesAdvisoryLockAndInserts(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	expectLimitRow(mock, 5, "lifetime")
	mock.ExpectBegin()
	// Advisory lock serializes concurrent reservations per owner+type.
	mock.ExpectExec("pg_advisory_xact_lock").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Committed usage sum.
	mock.ExpectQuery("SELECT COALESCE\\(SUM\\(quantity\\), 0\\) FROM usage_events").
		WillReturnRows(sqlmock.NewRows([]string{"used"}).AddRow(int64(2)))
	// Active-reservation sum.
	mock.ExpectQuery("SELECT COALESCE\\(SUM\\(quantity\\), 0\\) FROM usage_quota_reservations").
		WillReturnRows(sqlmock.NewRows([]string{"reserved"}).AddRow(int64(1)))
	mock.ExpectExec("INSERT INTO usage_quota_reservations").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	allowed, remaining, err := svc.ReserveQuota(context.Background(),
		types.BillingOwner{ID: "user-1", Type: types.OwnerTypeUser}, "llm_request", 1)
	require.NoError(t, err)
	assert.True(t, allowed, "3 used + 1 reserved + 1 new = 5 <= limit 5")
	assert.Equal(t, int64(1), remaining)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestReserveQuota_Denied_CountsActiveReservations(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	expectLimitRow(mock, 5, "lifetime")
	mock.ExpectBegin()
	mock.ExpectExec("pg_advisory_xact_lock").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT COALESCE\\(SUM\\(quantity\\), 0\\) FROM usage_events").
		WillReturnRows(sqlmock.NewRows([]string{"used"}).AddRow(int64(2)))
	// In-flight reservation from a concurrent request is the whole point:
	// used(2) + reserved(3) + 1 > 5 → deny.
	mock.ExpectQuery("SELECT COALESCE\\(SUM\\(quantity\\), 0\\) FROM usage_quota_reservations").
		WillReturnRows(sqlmock.NewRows([]string{"reserved"}).AddRow(int64(3)))
	mock.ExpectRollback()

	allowed, remaining, err := svc.ReserveQuota(context.Background(),
		types.BillingOwner{ID: "user-1", Type: types.OwnerTypeUser}, "llm_request", 1)
	require.NoError(t, err)
	assert.False(t, allowed, "active reservations must count against the limit")
	assert.Equal(t, int64(0), remaining)
	assert.NoError(t, mock.ExpectationsWereMet(),
		"denied reservation must not insert a row")
}

func TestReserveQuota_NoLimitRow_AllowsWithoutReservation(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	// No usage_limits row for this owner+type: unlimited — nothing to
	// reserve against, no advisory lock/tx work needed.
	mock.ExpectQuery("SELECT max_quantity, period_type FROM usage_limits").
		WillReturnRows(sqlmock.NewRows([]string{"max_quantity", "period_type"}))

	allowed, _, err := svc.ReserveQuota(context.Background(),
		types.BillingOwner{ID: "user-1", Type: types.OwnerTypeUser}, "llm_request", 1)
	require.NoError(t, err)
	assert.True(t, allowed, "no-limit owner must be allowed without touching the reservation machinery")
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestReserveQuota_NonPositiveQuantity_NoOp(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	allowed, _, err := svc.ReserveQuota(context.Background(),
		types.BillingOwner{ID: "user-1", Type: types.OwnerTypeUser}, "llm_request", 0)
	require.NoError(t, err)
	assert.True(t, allowed, "zero-quantity reservation is a no-op")
	assert.NoError(t, mock.ExpectationsWereMet(),
		"zero quantity must not touch the DB at all")
}

func TestReserveQuota_LimitQueryError_Fails(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	mock.ExpectQuery("SELECT max_quantity, period_type FROM usage_limits").
		WillReturnError(errDBDown)

	_, _, err := svc.ReserveQuota(context.Background(),
		types.BillingOwner{ID: "user-1", Type: types.OwnerTypeUser}, "llm_request", 1)
	require.Error(t, err, "DB errors must surface — callers fail closed (#768b)")
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestReserveQuota_ExpiryBounded inserts a reservation with a finite
// expiry; the reaper then deletes it once past.
func TestReserveQuota_ExpiryBounded(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	expectLimitRow(mock, 5, "lifetime")
	mock.ExpectBegin()
	mock.ExpectExec("pg_advisory_xact_lock").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT COALESCE\\(SUM\\(quantity\\), 0\\) FROM usage_events").
		WillReturnRows(sqlmock.NewRows([]string{"used"}).AddRow(int64(0)))
	mock.ExpectQuery("SELECT COALESCE\\(SUM\\(quantity\\), 0\\) FROM usage_quota_reservations").
		WillReturnRows(sqlmock.NewRows([]string{"reserved"}).AddRow(int64(0)))
	mock.ExpectExec("INSERT INTO usage_quota_reservations").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	_, _, err := svc.ReserveQuota(context.Background(),
		types.BillingOwner{ID: "user-1", Type: types.OwnerTypeUser}, "llm_request", 1)
	require.NoError(t, err)
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestReapExpiredReservations_DeletesExpired pins the reaper contract.
func TestReapExpiredReservations_DeletesExpired(t *testing.T) {
	svc, mock, cleanup := setupTestService(t)
	defer cleanup()

	mock.ExpectExec("DELETE FROM usage_quota_reservations").
		WillReturnResult(sqlmock.NewResult(0, 3))

	require.NoError(t, svc.reapExpiredReservations(context.Background()))
	assert.NoError(t, mock.ExpectationsWereMet())
}

// TestReserveQuota_TTLWindow documents the expiry bound: reservations
// live for quotaReservationTTL.
func TestReserveQuota_TTLWindow(t *testing.T) {
	assert.Equal(t, 2*time.Minute, quotaReservationTTL,
		"TTL must cover a plausible in-flight request; changing it is a quota-semantics decision")
}
