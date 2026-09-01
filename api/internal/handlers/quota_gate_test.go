// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/mocks"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// --- #768: quota gate contract (checkProxyQuota) ---

func newQuotaTestContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "user-1")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/message", nil)
	c.Request = req
	return c, w
}

func quotaTestWorkspace() *v1.Workspace {
	return &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Owner: v1.WorkspaceOwner{UserID: "user-1"}},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.0.1"},
	}
}

func quotaOwner() types.BillingOwner {
	return types.BillingOwner{ID: "user-1", Type: types.OwnerTypeUser}
}

// TestCheckProxyQuota_TokenQuotaExceeded: when the owner is at their
// llm_tokens limit, new requests are denied with 429 before the request
// slot is even reserved (#768a).
func TestCheckProxyQuota_TokenQuotaExceeded(t *testing.T) {
	env := newTestEnv(t)
	ms := new(mocks.MockMeteringService)
	env.handler.SetMeteringService(ms)

	ms.On("CheckQuota", mock.Anything, quotaOwner(), "llm_tokens").
		Return(false, int64(0), nil)

	c, w := newQuotaTestContext(t)
	allowed := env.handler.checkProxyQuota(c, quotaTestWorkspace())

	assert.False(t, allowed)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Contains(t, w.Body.String(), "llm_tokens")
	ms.AssertNotCalled(t, "ReserveQuota", mock.Anything, mock.Anything, mock.Anything, mock.Anything,
		"a token-denied request must not reserve a request slot")
}

// TestCheckProxyQuota_RequestReservationDenied: at the llm_request
// limit, the atomic reservation denies the slot (#768c).
func TestCheckProxyQuota_RequestReservationDenied(t *testing.T) {
	env := newTestEnv(t)
	ms := new(mocks.MockMeteringService)
	env.handler.SetMeteringService(ms)

	ms.On("CheckQuota", mock.Anything, quotaOwner(), "llm_tokens").
		Return(true, int64(1000), nil)
	ms.On("ReserveQuota", mock.Anything, quotaOwner(), "llm_request", int64(1)).
		Return(false, int64(0), nil)

	c, w := newQuotaTestContext(t)
	allowed := env.handler.checkProxyQuota(c, quotaTestWorkspace())

	assert.False(t, allowed)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Contains(t, w.Body.String(), "llm_request")
}

// TestCheckProxyQuota_TokenCheckFails_FailsClosed pins #768b: a DB
// error in the token check denies the request (503), not silently
// allows it. The pre-fix code logged and returned true — a transient DB
// outage disabled quota enforcement entirely.
func TestCheckProxyQuota_TokenCheckFails_FailsClosed(t *testing.T) {
	env := newTestEnv(t)
	ms := new(mocks.MockMeteringService)
	env.handler.SetMeteringService(ms)

	ms.On("CheckQuota", mock.Anything, quotaOwner(), "llm_tokens").
		Return(true, int64(0), errors.New("db down"))

	c, w := newQuotaTestContext(t)
	allowed := env.handler.checkProxyQuota(c, quotaTestWorkspace())

	assert.False(t, allowed, "quota checks must fail closed (#768b)")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	ms.AssertNotCalled(t, "ReserveQuota", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestCheckProxyQuota_ReservationFails_FailsClosed: same contract on
// the reservation leg.
func TestCheckProxyQuota_ReservationFails_FailsClosed(t *testing.T) {
	env := newTestEnv(t)
	ms := new(mocks.MockMeteringService)
	env.handler.SetMeteringService(ms)

	ms.On("CheckQuota", mock.Anything, quotaOwner(), "llm_tokens").
		Return(true, int64(1000), nil)
	ms.On("ReserveQuota", mock.Anything, quotaOwner(), "llm_request", int64(1)).
		Return(false, int64(0), errors.New("db down"))

	c, w := newQuotaTestContext(t)
	allowed := env.handler.checkProxyQuota(c, quotaTestWorkspace())

	assert.False(t, allowed, "reservation failures must fail closed (#768b)")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

// TestCheckProxyQuota_Allowed: both gates pass → request proceeds, no
// response written by the quota gate.
func TestCheckProxyQuota_Allowed(t *testing.T) {
	env := newTestEnv(t)
	ms := new(mocks.MockMeteringService)
	env.handler.SetMeteringService(ms)

	ms.On("CheckQuota", mock.Anything, quotaOwner(), "llm_tokens").
		Return(true, int64(1000), nil)
	ms.On("ReserveQuota", mock.Anything, quotaOwner(), "llm_request", int64(1)).
		Return(true, int64(9), nil)

	c, w := newQuotaTestContext(t)
	allowed := env.handler.checkProxyQuota(c, quotaTestWorkspace())

	assert.True(t, allowed)
	assert.Equal(t, http.StatusOK, w.Code, "quota gate must not write a response on allow")
}

// TestCheckProxyQuota_NoTokenLimitRow_Allowed: absence of an llm_tokens
// limit row means unlimited tokens — the token gate must not 429
// deployments that never configured one (back-compat).
func TestCheckProxyQuota_NoTokenLimitRow_Allowed(t *testing.T) {
	env := newTestEnv(t)
	ms := new(mocks.MockMeteringService)
	env.handler.SetMeteringService(ms)

	// (true, 0, nil) is exactly what CheckQuota returns on ErrNoRows.
	ms.On("CheckQuota", mock.Anything, quotaOwner(), "llm_tokens").
		Return(true, int64(0), nil)
	ms.On("ReserveQuota", mock.Anything, quotaOwner(), "llm_request", int64(1)).
		Return(true, int64(0), nil)

	c, w := newQuotaTestContext(t)
	allowed := env.handler.checkProxyQuota(c, quotaTestWorkspace())

	require.True(t, allowed)
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestCheckProxyQuota_CanaryWorkspace_SkipsQuota pins the existing
// canary bypass through the new gate.
func TestCheckProxyQuota_CanaryWorkspace_SkipsQuota(t *testing.T) {
	env := newTestEnv(t)
	ms := new(mocks.MockMeteringService)
	env.handler.SetMeteringService(ms)

	ws := quotaTestWorkspace()
	ws.Labels = map[string]string{"llmsafespaces.dev/canary": "true"}

	c, w := newQuotaTestContext(t)
	allowed := env.handler.checkProxyQuota(c, ws)

	require.True(t, allowed)
	assert.Equal(t, http.StatusOK, w.Code)
	ms.AssertNotCalled(t, "CheckQuota", mock.Anything, mock.Anything, mock.Anything)
}
