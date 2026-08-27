// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/services/ratelimit"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// newUploadsRouterFixture wires the production router with a mock auth
// middleware (401 without an Authorization header) and a WorkspaceAccess
// middleware backed by a mockable workspace service, plus a zero-value
// ProxyHandler stub — the tests below only exercise middleware-level
// rejections, never the handler body.
func newUploadsRouterFixture(t *testing.T, rl interfaces.RateLimiterService, ws *imocks.MockWorkspaceService, cfg RouterConfig) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	log, err := apilogger.New(false, "error", "json")
	require.NoError(t, err)

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	met.On("IncrementActiveConnections", mock.Anything, mock.Anything).Maybe()
	met.On("DecrementActiveConnections", mock.Anything, mock.Anything).Maybe()
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		c.Set("userID", "test-user")
		c.Next()
	}))
	auth.On("GetUserID", mock.Anything).Return("test-user")

	svc := &uploadsMockServices{auth: auth, metrics: met, ws: ws, rl: rl}
	return NewRouter(svc, log, &handlers.ProxyHandler{}, cfg)
}

type uploadsMockServices struct {
	auth    *imocks.MockAuthMiddlewareService
	metrics *imocks.MockMetricsService
	ws      *imocks.MockWorkspaceService
	rl      interfaces.RateLimiterService
}

func (s *uploadsMockServices) GetAuth() interfaces.AuthService               { return s.auth }
func (s *uploadsMockServices) GetDatabase() interfaces.DatabaseService       { return nil }
func (s *uploadsMockServices) GetCache() interfaces.CacheService             { return nil }
func (s *uploadsMockServices) GetMetrics() interfaces.MetricsService         { return s.metrics }
func (s *uploadsMockServices) GetWorkspace() interfaces.WorkspaceService     { return s.ws }
func (s *uploadsMockServices) GetRateLimiter() interfaces.RateLimiterService { return s.rl }
func (s *uploadsMockServices) GetMetering() interfaces.MeteringService       { return nil }

// TestUploadsRoute_RequireAuth (I1): no JWT → 401, before the handler (a
// zero-value ProxyHandler would panic if reached — its survival proves the
// middleware gate).
func TestUploadsRoute_RequireAuth(t *testing.T) {
	ws := &imocks.MockWorkspaceService{}
	ws.On("ResolveWorkspace", mock.Anything, mock.Anything).
		Return(&types.WorkspaceMetadata{ID: "ws-1", UserID: "test-user"}, nil).Maybe()
	ws.On("CheckOwnership", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	router := newUploadsRouterFixture(t, nil, ws, RouterConfig{Debug: false})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/uploads", strings.NewReader("x"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// TestUploadsRoute_OwnershipEnforced (I1): the access middleware rejects
// unknown workspaces with 404 and foreign workspaces with 403 — the
// verifyOwner error-mapping contract — before the upload handler runs.
func TestUploadsRoute_OwnershipEnforced(t *testing.T) {
	t.Run("unknown workspace 404", func(t *testing.T) {
		ws := &imocks.MockWorkspaceService{}
		ws.On("ResolveWorkspace", mock.Anything, "ws-missing").
			Return((*types.WorkspaceMetadata)(nil), apierrors.NewNotFoundError("workspace", "ws-missing", assert.AnError)).Maybe()

		router := newUploadsRouterFixture(t, nil, ws, RouterConfig{Debug: false})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-missing/uploads", strings.NewReader("x"))
		req.Header.Set("Authorization", "Bearer t")
		req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("foreign workspace 403", func(t *testing.T) {
		ws := &imocks.MockWorkspaceService{}
		ws.On("ResolveWorkspace", mock.Anything, "ws-1").
			Return(&types.WorkspaceMetadata{ID: "ws-1", UserID: "someone-else"}, nil).Maybe()
		ws.On("CheckOwnership", mock.Anything, "test-user", mock.Anything).
			Return(apierrors.NewForbiddenError("workspace access denied", assert.AnError)).Maybe()

		router := newUploadsRouterFixture(t, nil, ws, RouterConfig{Debug: false})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/uploads", strings.NewReader("x"))
		req.Header.Set("Authorization", "Bearer t")
		req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusForbidden, w.Code)
	})
}

// TestUploadsRoute_RateLimited (U1.2.22): the uploads route participates in
// the global rate-limiting stack — a tight burst limit rejects the excess
// with 429 before the handler runs.
func TestUploadsRoute_RateLimited(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	log, _ := apilogger.New(true, "error", "console")
	rlSvc := ratelimit.NewWithRedisClient(log, client)
	defer rlSvc.Stop()

	ws := &imocks.MockWorkspaceService{}
	ws.On("ResolveWorkspace", mock.Anything, mock.Anything).
		Return(&types.WorkspaceMetadata{ID: "ws-1", UserID: "test-user"}, nil).Maybe()
	ws.On("CheckOwnership", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	cfg := DefaultRouterConfig()
	cfg.RateLimitConfig = middleware.RateLimitConfig{
		Enabled:       true,
		DefaultLimit:  3,
		DefaultWindow: time.Minute,
		BurstSize:     3,
		Strategy:      "token_bucket",
	}
	cfg.SecurityConfig.RequireHTTPS = false
	cfg.SecurityConfig.Development = true

	router := newUploadsRouterFixture(t, rlSvc, ws, cfg)

	passed, rejected := 0, 0
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/uploads", strings.NewReader("x"))
		req.Header.Set("Authorization", "Bearer t")
		req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
		req.RemoteAddr = "10.0.0.7:1234"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		switch w.Code {
		case http.StatusTooManyRequests:
			rejected++
		default:
			passed++
		}
	}

	assert.Equal(t, 3, passed, "burst (3) requests should pass the global rate limiter")
	assert.Equal(t, 3, rejected, "excess requests must be rate-limited (429)")
}
