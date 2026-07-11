// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/version"
)

// healthMockServices wires Database and Cache mocks (which the workspace
// fixture leaves nil) so that /readyz can call Ping on them.
type healthMockServices struct {
	auth     *imocks.MockAuthMiddlewareService
	metrics  *imocks.MockMetricsService
	database *imocks.MockDatabaseService
	cache    *imocks.MockCacheService
}

func (s *healthMockServices) GetAuth() interfaces.AuthService { return s.auth }
func (s *healthMockServices) GetDatabase() interfaces.DatabaseService {
	if s.database == nil {
		return nil
	}
	return s.database
}
func (s *healthMockServices) GetCache() interfaces.CacheService {
	if s.cache == nil {
		return nil
	}
	return s.cache
}
func (s *healthMockServices) GetMetrics() interfaces.MetricsService { return s.metrics }
func (s *healthMockServices) GetWorkspace() interfaces.WorkspaceService {
	return nil
}
func (s *healthMockServices) GetRateLimiter() interfaces.RateLimiterService { return nil }
func (s *healthMockServices) GetMetering() interfaces.MeteringService       { return nil }

func newHealthFixture(t *testing.T) (*gin.Engine, *healthMockServices) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	log, err := apilogger.New(false, "error", "json")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	db := &imocks.MockDatabaseService{}
	ca := &imocks.MockCacheService{}

	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) { c.Next() }))
	auth.On("GetUserID", mock.Anything).Return("")

	svc := &healthMockServices{auth: auth, metrics: met, database: db, cache: ca}
	router := NewRouter(svc, log, nil, RouterConfig{Debug: false})
	return router, svc
}

// /livez always returns 200 if the process is responding. Independent of
// upstream dependencies — this is the liveness probe.
func TestLivez_ReturnsOK(t *testing.T) {
	router, _ := newHealthFixture(t)

	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "ok")
}

// /livez includes the build version in the JSON response so operators can
// verify which version is running via `curl /livez` without needing
// kubectl exec. The version comes from pkg/version.Version, which is
// overridden at build time via -ldflags by the release pipeline.
func TestLivez_IncludesVersionField(t *testing.T) {
	router, _ := newHealthFixture(t)

	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body),
		"response must be valid JSON: %s", rec.Body.String())
	assert.Equal(t, "ok", body.Status)
	assert.Equal(t, version.Version, body.Version,
		"version field must match pkg/version.Version (got %q, want %q)",
		body.Version, version.Version)
}

// /health is a legacy alias for /livez and must include the same version
// field — operators with existing probes pointed at /health should see the
// same response shape.
func TestHealth_LegacyAlias_IncludesVersionField(t *testing.T) {
	router, _ := newHealthFixture(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, version.Version, body.Version,
		"/health (legacy alias) must include the same version field as /livez")
}

// /readyz returns 200 when both Postgres and Redis pings succeed.
func TestReadyz_AllDependenciesHealthy(t *testing.T) {
	router, svc := newHealthFixture(t)

	svc.database.On("Ping", mock.Anything).Return(nil).Once()
	svc.cache.On("Ping", mock.Anything).Return(nil).Once()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	svc.database.AssertExpectations(t)
	svc.cache.AssertExpectations(t)
}

// /readyz returns 503 when the database ping fails.
func TestReadyz_DatabaseDown(t *testing.T) {
	router, svc := newHealthFixture(t)

	svc.database.On("Ping", mock.Anything).Return(errors.New("connection refused")).Once()
	svc.cache.On("Ping", mock.Anything).Return(nil).Maybe()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "database")
}

// /readyz returns 503 when the cache ping fails (database OK).
func TestReadyz_CacheDown(t *testing.T) {
	router, svc := newHealthFixture(t)

	svc.database.On("Ping", mock.Anything).Return(nil).Once()
	svc.cache.On("Ping", mock.Anything).Return(errors.New("redis: nil")).Once()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "cache")
}

// /readyz returns 503 when both deps are down (and reports both in body).
func TestReadyz_BothDown(t *testing.T) {
	router, svc := newHealthFixture(t)

	svc.database.On("Ping", mock.Anything).Return(errors.New("db gone")).Once()
	svc.cache.On("Ping", mock.Anything).Return(errors.New("cache gone")).Once()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "database")
	assert.Contains(t, body, "cache")
}

// The legacy /health endpoint is preserved as an alias of /livez.
func TestHealth_LegacyAlias(t *testing.T) {
	router, _ := newHealthFixture(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

// Probes must work even if Database or Cache services are unavailable
// (nil Services accessor) — used in tests and stripped-down deployments.
// /livez must still respond; /readyz must return 503.
func TestReadyz_NilDatabase_Returns503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, _ := apilogger.New(false, "error", "json")

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) { c.Next() }))
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	svc := &healthMockServices{auth: auth, metrics: met, database: nil, cache: nil}
	router := NewRouter(svc, log, nil, RouterConfig{Debug: false})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// Kubelet probes do NOT send an Authorization header. If any middleware ever
// requires auth on probe paths, kubelet will receive 401 and mark the pod
// unready. This test mounts the production auth middleware on the root group
// (the worst-case scenario) and verifies probes still succeed.
//
// The mock auth middleware in newHealthFixture passes everything through, so
// it cannot catch this regression. We use a stricter mock here that 401s on
// any request without an Authorization header.
func TestProbes_BypassAuthMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, _ := apilogger.New(false, "error", "json")

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	db := &imocks.MockDatabaseService{}
	ca := &imocks.MockCacheService{}

	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	db.On("Ping", mock.Anything).Return(nil).Maybe()
	ca.On("Ping", mock.Anything).Return(nil).Maybe()

	// Strict auth: 401 when no Authorization header.
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "auth required"})
			return
		}
		c.Set("userID", "u1")
		c.Next()
	}))
	auth.On("GetUserID", mock.Anything).Return("u1")

	svc := &healthMockServices{auth: auth, metrics: met, database: db, cache: ca}
	router := NewRouter(svc, log, nil, RouterConfig{Debug: false})

	for _, path := range []string{"/livez", "/readyz", "/health"} {
		req := httptest.NewRequest(http.MethodGet, path, nil) // no Authorization header
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		assert.NotEqualf(t, http.StatusUnauthorized, rec.Code,
			"probe %s must not 401 without auth header (got %d): %s",
			path, rec.Code, rec.Body.String())
	}
}

// MetricsMiddleware must skip probe paths to avoid polluting Prometheus
// cardinality with kubelet probe traffic (10s intervals × 3 probes × N pods).
// This test verifies RecordRequest is NOT called for probe paths.
func TestProbes_NotRecordedInMetrics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, _ := apilogger.New(false, "error", "json")

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	db := &imocks.MockDatabaseService{}
	ca := &imocks.MockCacheService{}

	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) { c.Next() }))
	db.On("Ping", mock.Anything).Return(nil).Maybe()
	ca.On("Ping", mock.Anything).Return(nil).Maybe()
	// NO .On("RecordRequest", ...). If the middleware records anything for these
	// paths, the strict mock will fail the test.

	svc := &healthMockServices{auth: auth, metrics: met, database: db, cache: ca}
	router := NewRouter(svc, log, nil, RouterConfig{Debug: false})

	for _, path := range []string{"/livez", "/readyz", "/health", "/metrics"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
	}

	met.AssertNotCalled(t, "RecordRequest", mock.Anything, mock.Anything,
		mock.Anything, mock.Anything, mock.Anything)
}
