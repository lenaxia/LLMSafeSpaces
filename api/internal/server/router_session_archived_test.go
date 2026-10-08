// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
)

// --- PUT /api/v1/workspaces/:id/sessions/:sessionId/archived (#1627) ---

func TestSetSessionArchived_ArchiveSuccess(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("SetSessionArchived", mock.Anything, "test-user", "ws-1", "sess-1", true).Return(nil)

	body, _ := json.Marshal(map[string]bool{"archived": true})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/sessions/sess-1/archived", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	svc.workspace.AssertCalled(t, "SetSessionArchived", mock.Anything, "test-user", "ws-1", "sess-1", true)
}

func TestSetSessionArchived_UnarchiveSuccess(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("SetSessionArchived", mock.Anything, "test-user", "ws-1", "sess-1", false).Return(nil)

	body, _ := json.Marshal(map[string]bool{"archived": false})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/sessions/sess-1/archived", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestSetSessionArchived_MissingBody_Returns400(t *testing.T) {
	router, _ := newRouterFixture(t)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/sessions/sess-1/archived", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSetSessionArchived_UnindexedSession_Returns404(t *testing.T) {
	router, svc := newRouterFixture(t)

	svc.workspace.On("SetSessionArchived", mock.Anything, "test-user", "ws-1", "sess-ghost", true).
		Return(apierrors.NewNotFoundError("session", "sess-ghost", nil))

	body, _ := json.Marshal(map[string]bool{"archived": true})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/sessions/sess-ghost/archived", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSetSessionArchived_Unauthorized_Returns401(t *testing.T) {
	router, _ := newRouterFixture(t)

	body, _ := json.Marshal(map[string]bool{"archived": true})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/sessions/sess-1/archived", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// Review r1 (PR2) missing-test 5: the three pod-identity session
// routes are registered when the handler is wired — a dropped
// registration is a silent 404 for the agentd tools.
func TestInternalSessionRoutes_Registered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Registration-only: nil deps — no request is served; the routes'
	// presence is the pin.
	h := handlers.NewPodSessionArchiveHandler(nil, nil, nil, nil, nil, "llmsafespace")
	registerInternalPodRoutes(r, RouterConfig{PodSessionArchiveHandler: h})

	registered := map[string]bool{}
	for _, rt := range r.Routes() {
		registered[rt.Path] = true
	}
	for _, path := range []string{"/internal/v1/session-archive", "/internal/v1/session-delete", "/internal/v1/session-archived"} {
		assert.True(t, registered[path], "%s must be registered when the handler is wired", path)
	}
}
