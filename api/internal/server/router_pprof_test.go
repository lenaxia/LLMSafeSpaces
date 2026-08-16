// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

// #901 G6 / #906 review: pprof is admin-gated AND the named profiles
// (goroutine/heap — the incident's exact artifacts) must serve profile
// data, not the HTML index. The plain-StripPrefix mount served the index
// for every named profile (review rounds 1-3, reproduced empirically).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/services/auth"
)

func newPprofRouter(t *testing.T, role string, authed bool) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	authSvc := &imocks.MockAuthMiddlewareService{}
	authSvc.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		if !authed {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Set("userID", "u-pprof")
		c.Set("userRole", role)
		c.Next()
	})).Maybe()

	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	svc := &adminSessionMockServices{auth: authSvc, met: met}
	cfg := RouterConfig{Debug: false}
	apilog, err := apilogger.New(false, "error", "json")
	require.NoError(t, err)
	router := NewRouter(svc, apilog, nil, cfg)
	return router
}

func TestRouter_Pprof_AdminGate(t *testing.T) {
	tests := []struct {
		name   string
		authed bool
		role   string
		want   int
	}{
		// AdminGuard deliberately 404s non-admins (no route-existence
		// leak — admin_guard.go:13).
		{"unauthenticated is 401", false, "", http.StatusUnauthorized},
		{"non-admin is 404 (route existence hidden)", true, "user", http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router := newPprofRouter(t, tc.role, tc.authed)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/debug/pprof/", nil))
			require.Equal(t, tc.want, w.Code)
		})
	}
}

func TestRouter_Pprof_NamedProfilesServeData(t *testing.T) {
	router := newPprofRouter(t, "admin", true)

	// goroutine profile (debug=1 → plain text): the exact artifact the
	// 2026-08-16 incident needed and the pre-fix mount served as HTML.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/debug/pprof/goroutine?debug=1", nil))
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	require.False(t, strings.Contains(body, "<html"), "named profile must not serve the index page")
	require.Contains(t, body, "goroutine", "goroutine profile must contain stack data")

	// heap profile: binary pprof payload (application/octet-stream-ish),
	// definitely not HTML.
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/api/v1/admin/debug/pprof/heap", nil))
	require.Equal(t, http.StatusOK, w2.Code)
	require.False(t, strings.Contains(w2.Body.String(), "<html"), "heap must be a profile, not the index")

	// cmdline (explicit handler): plain text.
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, httptest.NewRequest(http.MethodGet, "/api/v1/admin/debug/pprof/cmdline", nil))
	require.Equal(t, http.StatusOK, w3.Code)
}

// mock compliance for the unused expectations
var _ = mock.Anything
var _ = auth.Service{}
