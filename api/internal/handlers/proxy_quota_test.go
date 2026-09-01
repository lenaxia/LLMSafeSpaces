// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func TestCheckProxyQuota_NilMetering_PassThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &ProxyHandler{logger: &testLogger{}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)

	assert.True(t, h.checkProxyQuota(c, &v1.Workspace{}), "nil metering should pass through")
}

func TestCheckProxyQuota_NoUserID_PassThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ms := new(mocks.MockMeteringService)
	h := &ProxyHandler{logger: &testLogger{}, meteringSvc: ms}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)

	assert.True(t, h.checkProxyQuota(c, &v1.Workspace{}), "no userID should pass through")
}

func TestCheckProxyQuota_CanaryWorkspace_Bypassed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ms := new(mocks.MockMeteringService)
	h := &ProxyHandler{logger: &testLogger{}, meteringSvc: ms}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Set("userID", "user-123")

	ws := &v1.Workspace{}
	ws.Labels = map[string]string{"llmsafespaces.dev/canary": "true"}

	assert.True(t, h.checkProxyQuota(c, ws), "canary workspace should bypass quota")
	ms.AssertNotCalled(t, "CheckQuota")
}

func TestCheckProxyQuota_QuotaAllowed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ms := new(mocks.MockMeteringService)
	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(true, int64(9), nil)
	h := &ProxyHandler{logger: &testLogger{}, meteringSvc: ms}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Set("userID", "user-123")

	assert.True(t, h.checkProxyQuota(c, &v1.Workspace{}), "allowed quota should return true")
	ms.AssertExpectations(t)
}

func TestCheckProxyQuota_RequestReservationDenied_Returns429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ms := new(mocks.MockMeteringService)
	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(10), nil)
	ms.On("ReserveQuota", mock.Anything, mock.Anything, "llm_request", int64(1)).Return(false, int64(0), nil)
	h := &ProxyHandler{logger: &testLogger{}, meteringSvc: ms}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Set("userID", "user-123")

	assert.False(t, h.checkProxyQuota(c, &v1.Workspace{}), "denied reservation should return false")
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

// #768b: quota enforcement fails CLOSED. The pre-fix behavior (fail-open
// on DB error — a transient outage silently disabled quota entirely) is
// the bug; this test is the inverted pin.
func TestCheckProxyQuota_CheckQuotaError_FailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ms := new(mocks.MockMeteringService)
	ms.On("CheckQuota", mock.Anything, mock.Anything, "llm_tokens").Return(true, int64(0), context.DeadlineExceeded)
	h := &ProxyHandler{logger: &testLogger{}, meteringSvc: ms}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Set("userID", "user-123")

	assert.False(t, h.checkProxyQuota(c, &v1.Workspace{}),
		"DB error must fail closed — enforcement unavailable, not disabled (#768b)")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}
