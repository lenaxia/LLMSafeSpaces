// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
)

// TestRouter_InternalAPIRoutesNotSSLRedirected pins the router↔middleware
// SEAM — the exact place the 2026-10-01 staging outage lived: US-72.3
// registered the controller-facing endpoints under /api/v1/internal/ while
// the security middleware's SSL-redirect exemption knew only /internal/,
// so every in-cluster plain-HTTP call was 301'd to
// https://<same-host>:8080 (TLS on the plaintext port) and relay staging
// failed for every workspace with
// "http: server gave HTTP response to HTTPS client".
//
// The middleware unit tests pin the middleware; the OpenAPI contract test
// pins route registration; THIS test composes them — the real NewRouter
// wiring (global middleware chain, router.go) serving the real internal
// routes over plain HTTP. A future route-registration drift of the same
// class (a new internal endpoint under an unexempted prefix) fails here
// only if it shares the prefix; the narrowness negatives in the middleware
// suite guard the other direction.
func TestRouter_InternalAPIRoutesNotSSLRedirected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, err := apilogger.New(false, "error", "json")
	require.NoError(t, err, "logger")

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) { c.Next() }))
	auth.On("GetUserID", mock.Anything).Return("")
	svc := &healthMockServices{auth: auth, metrics: met}

	// Zero-value handler stubs: registration shape is what matters. The
	// handlers fail closed (403) with no LLMSAFESPACES_INTERNAL_TOKEN —
	// which is exactly the assertion: the request must REACH the handler
	// (403), not die in the middleware (301).
	//
	// The config MUST be based on DefaultRouterConfig(): NewRouter uses
	// the provided config verbatim with NO defaults merge, and the zero
	// RouterConfig carries a zero SecurityConfig — RequireHTTPS=false —
	// which disarms SSLRedirect entirely and turns this test into a
	// tautology (it would pass with the exemption deleted). The default
	// config is the production posture: RequireHTTPS=true.
	rc := DefaultRouterConfig()
	rc.InternalLLMProvidersHandler = &handlers.InternalLLMProvidersHandler{}
	rc.InternalOrgStatusHandler = &handlers.InternalOrgStatusHandler{}
	router := NewRouter(svc, log, nil, rc)

	t.Setenv("LLMSAFESPACES_INTERNAL_TOKEN", "")

	for _, tc := range []struct{ name, path string }{
		{"llm-providers (US-72.3 staging credential source)", "/api/v1/internal/workspaces/ws_1/llm-providers?ownerUserID=u_1"},
		{"org-status (US-43.19)", "/api/v1/internal/orgs/org_1/status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			// No X-Forwarded-Proto: this is a direct in-cluster call,
			// exactly how the controller's http.Client makes it.
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusForbidden, rec.Code,
				"plain-HTTP internal call must reach the handler's fail-closed auth (403), "+
					"never the SSL 301 (got %d, Location %q)", rec.Code, rec.Header().Get("Location"))
			assert.Empty(t, rec.Header().Get("Location"),
				"no redirect may be issued for in-cluster internal routes")
		})
	}

	// Production-parity posture pin through the same real wiring: with
	// RequireHTTPS armed (the DefaultRouterConfig posture above), a
	// NON-internal route must still 301. If a future config regression
	// disarms SSLRedirect router-wide (zeroed SecurityConfig), the
	// internal-route subtests above would keep passing while the
	// redirect guarantee silently vanished — this subtest catches that.
	t.Run("non-internal route still SSL-redirects", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/config", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusMovedPermanently, rec.Code,
			"public routes must still redirect under RequireHTTPS; got %d", rec.Code)
		assert.NotEmpty(t, rec.Header().Get("Location"))
	})
}
