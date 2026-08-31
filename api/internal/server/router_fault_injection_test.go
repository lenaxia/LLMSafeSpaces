// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type faultFixtureReviewer struct{}

func (faultFixtureReviewer) Review(_ context.Context, _ string) (string, error) {
	return "system:serviceaccount:llmsafespace:workspace-ws-1", nil
}

type faultFixtureInjector struct{}

func (faultFixtureInjector) InjectSecretsForPodBootstrap(_ context.Context, _, _ string) ([]byte, error) {
	return []byte(`[{"type":"llm-provider","name":"model"}]`), nil
}

type faultFixtureLookup struct{}

func (faultFixtureLookup) GetWorkspace(_ context.Context, _ string) (*types.WorkspaceMetadata, error) {
	return &types.WorkspaceMetadata{ID: "ws-1", UserID: "user-1"}, nil
}

func newFaultInjectionRouter(t *testing.T, rules []middleware.FaultInjectionRule) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	log, err := apilogger.New(false, "error", "json")
	require.NoError(t, err)

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) { c.Next() })).Maybe()
	auth.On("GetUserID", mock.Anything).Return("").Maybe()

	svc := &healthMockServices{auth: auth, metrics: met}

	bootstrap := handlers.NewPodBootstrapHandler(
		faultFixtureReviewer{},
		faultFixtureInjector{},
		faultFixtureLookup{},
		nil,
		"llmsafespace",
	)

	return NewRouter(svc, log, nil, RouterConfig{
		Debug:               false,
		FaultInjectionRules: rules,
		PodBootstrapHandler: bootstrap,
	})
}

func doBootstrapFaultRequest(r *gin.Engine) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/pod-bootstrap", strings.NewReader(`{"workspaceID":"ws-1"}`))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

// TestRouter_FaultInjectionCoversPodBootstrap pins that the middleware is
// wired onto the engine chain — /internal/v1/pod-bootstrap is registered on
// the ROOT router (router.go), so only engine-level middleware covers it.
// First COUNT matches fail 500 with the marker body, then the request
// reaches the real handler and succeeds.
func TestRouter_FaultInjectionCoversPodBootstrap(t *testing.T) {
	rules, err := middleware.ParseFaultInjectionRules("2:POST:/internal/v1/pod-bootstrap")
	require.NoError(t, err)
	router := newFaultInjectionRouter(t, rules)

	for i := 0; i < 2; i++ {
		w := doBootstrapFaultRequest(router)
		require.Equal(t, http.StatusInternalServerError, w.Code, "injected failure %d", i+1)
		assert.Contains(t, w.Body.String(), "fault injection: POST /internal/v1/pod-bootstrap")
	}

	w := doBootstrapFaultRequest(router)
	require.Equal(t, http.StatusOK, w.Code, "after budget exhaustion the request must reach the handler")
	assert.Contains(t, w.Body.String(), "llm-provider")
}

// TestRouter_FaultInjectionAbsentWithoutRules pins the default-off
// contract: no rules in RouterConfig → no middleware in the engine chain,
// byte-identical passthrough, and one more handler in the chain when the
// feature is enabled.
func TestRouter_FaultInjectionAbsentWithoutRules(t *testing.T) {
	plain := newFaultInjectionRouter(t, nil)
	w := doBootstrapFaultRequest(plain)
	require.Equal(t, http.StatusOK, w.Code, "unset feature must not touch the request")
	assert.NotContains(t, w.Body.String(), "fault injection")

	rules, err := middleware.ParseFaultInjectionRules("1:POST:/internal/v1/pod-bootstrap")
	require.NoError(t, err)
	faulted := newFaultInjectionRouter(t, rules)
	w = doBootstrapFaultRequest(faulted)
	require.Equal(t, http.StatusInternalServerError, w.Code)

	assert.Equal(t, len(plain.Handlers)+1, len(faulted.Handlers),
		"the fault middleware must be added to the chain ONLY when a rule is configured")
}
