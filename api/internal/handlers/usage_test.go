// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

var mockAnyCtx = mock.Anything

func setupUsageTestEnv(t *testing.T) (*UsageHandler, *mocks.MockMeteringService, *mocks.MockDatabaseService, sqlmock.Sqlmock, func()) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	meteringSvc := new(mocks.MockMeteringService)
	dbSvc := new(mocks.MockDatabaseService)
	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	handler := NewUsageHandler(meteringSvc, dbSvc)
	handler.SetDB(db)

	return handler, meteringSvc, dbSvc, mock, func() { db.Close() }
}

func TestUsageHandler_GetUsage_Success(t *testing.T) {
	handler, meteringSvc, _, _, cleanup := setupUsageTestEnv(t)
	defer cleanup()

	report := &types.UsageReport{Totals: map[string]int64{"llm_tokens": 100}}
	meteringSvc.On("GetUsage", mockAnyCtx, mock.Anything, mock.Anything, mock.Anything).Return(report, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "user-1")
	c.Request = httptest.NewRequest("GET", "/api/v1/usage", nil)

	handler.GetUsage(c)

	assert.Equal(t, http.StatusOK, w.Code)
	meteringSvc.AssertExpectations(t)
}

func TestUsageHandler_GetUsage_Unauthorized(t *testing.T) {
	handler, _, _, _, cleanup := setupUsageTestEnv(t)
	defer cleanup()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/usage", nil)

	handler.GetUsage(c)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestUsageHandler_GetQuotaStatus_Empty(t *testing.T) {
	handler, meteringSvc, _, _, cleanup := setupUsageTestEnv(t)
	defer cleanup()

	meteringSvc.On("GetQuotaStatus", mockAnyCtx, mock.Anything).Return([]types.QuotaStatus{}, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "user-1")
	c.Request = httptest.NewRequest("GET", "/api/v1/usage/quota", nil)

	handler.GetQuotaStatus(c)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp []types.QuotaStatus
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Equal(t, 0, len(resp))
}

func TestUsageHandler_GetQuotaStatus_WithLimits(t *testing.T) {
	handler, meteringSvc, _, _, cleanup := setupUsageTestEnv(t)
	defer cleanup()

	statuses := []types.QuotaStatus{
		{EventType: "llm_request", PeriodType: "monthly", Limit: 100, Used: 30, Remaining: 70},
	}
	meteringSvc.On("GetQuotaStatus", mockAnyCtx, mock.Anything).Return(statuses, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "user-1")
	c.Request = httptest.NewRequest("GET", "/api/v1/usage/quota", nil)

	handler.GetQuotaStatus(c)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp []types.QuotaStatus
	json.Unmarshal(w.Body.Bytes(), &resp)
	require.Equal(t, 1, len(resp))
	assert.Equal(t, int64(70), resp[0].Remaining)
}

func TestUsageHandler_GetWorkspaceUsage_NotOwned(t *testing.T) {
	handler, _, dbSvc, _, cleanup := setupUsageTestEnv(t)
	defer cleanup()

	dbSvc.On("CheckResourceOwnership", "user-1", "workspace", "ws-1").Return(false, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "user-1")
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	c.Request = httptest.NewRequest("GET", "/api/v1/usage/workspaces/ws-1", nil)

	handler.GetWorkspaceUsage(c)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUsageHandler_AdminGetUsage_Success(t *testing.T) {
	handler, meteringSvc, _, _, cleanup := setupUsageTestEnv(t)
	defer cleanup()

	report := &types.UsageReport{Totals: map[string]int64{"api_call": 50}}
	meteringSvc.On("GetUsage", mockAnyCtx, mock.Anything, mock.Anything, mock.Anything).Return(report, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "ownerId", Value: "user-2"}}
	c.Request = httptest.NewRequest("GET", "/api/v1/admin/usage/user-2", nil)

	handler.AdminGetUsage(c)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUsageHandler_AdminGetDLQ_Empty(t *testing.T) {
	handler, _, _, mock, cleanup := setupUsageTestEnv(t)
	defer cleanup()

	mock.ExpectQuery("SELECT id, payload, error_message").
		WillReturnRows(sqlmock.NewRows([]string{"id", "payload", "error_message", "retry_count", "first_failed_at", "last_retried_at"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/admin/billing/dlq", nil)

	handler.AdminGetDLQ(c)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUsageHandler_AdminBillingStatus(t *testing.T) {
	handler, _, _, mock, cleanup := setupUsageTestEnv(t)
	defer cleanup()

	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	mock.ExpectQuery("SELECT provider").WillReturnRows(
		sqlmock.NewRows([]string{"provider", "last_exported_id", "last_exported_at"}).AddRow("noop", int64(100), "2026-06-13T00:00:00Z"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/admin/billing/status", nil)

	handler.AdminBillingStatus(c)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageHandler_AdminBillingStatus_NoDB(t *testing.T) {
	handler := NewUsageHandler(nil, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/admin/billing/status", nil)

	handler.AdminBillingStatus(c)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestParsePeriod_Valid(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/usage?from=2026-06-01T00:00:00Z&to=2026-06-30T23:59:59Z", nil)

	from, to, err := parsePeriod(c)
	assert.NoError(t, err)
	assert.False(t, from.IsZero())
	assert.False(t, to.IsZero())
}

func TestParsePeriod_InvalidFrom(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/usage?from=invalid-date", nil)

	_, _, err := parsePeriod(c)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid from date")
}

func TestParsePeriod_DefaultPeriod(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/usage", nil)

	from, to, err := parsePeriod(c)
	assert.NoError(t, err)
	assert.False(t, from.IsZero())
	assert.False(t, to.IsZero())
}
