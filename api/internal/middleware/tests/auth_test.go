// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type MockAuthService struct {
	mock.Mock
}

func (m *MockAuthService) Start() error {
	args := m.Called()
	return args.Error(0)
}

func (m *MockAuthService) Stop() error {
	args := m.Called()
	return args.Error(0)
}

func (m *MockAuthService) ValidateToken(ctx context.Context, token string) (string, error) {
	args := m.Called(token)
	return args.String(0), args.Error(1)
}

// RevokeToken mock — added for the AuthService interface change introduced
// by G18 (Epic 17 Phase 4 RT-4.13). Most middleware tests do not exercise
// the revocation path, so the mock satisfies the interface and returns
// whatever the test configures.
func (m *MockAuthService) RevokeToken(_ context.Context, token string) error {
	args := m.Called(token)
	return args.Error(0)
}

// MarkUserSuspended / ClearUserSuspended mocks — added for the F4 (US-43.19)
// per-user revocation-marker methods on AuthService. Middleware tests do not
// exercise the suspend path, so the mock satisfies the interface and returns
// whatever the test configures.
func (m *MockAuthService) MarkUserSuspended(ctx context.Context, userID string) error {
	args := m.Called(ctx, userID)
	return args.Error(0)
}

func (m *MockAuthService) ClearUserSuspended(ctx context.Context, userID string) error {
	args := m.Called(ctx, userID)
	return args.Error(0)
}

func (m *MockAuthService) GetUserID(c *gin.Context) string {
	args := m.Called(c)
	return args.String(0)
}

func (m *MockAuthService) CheckResourceAccess(userID, resourceType, resourceID, action string) bool {
	args := m.Called(userID, resourceType, resourceID, action)
	return args.Bool(0)
}

func (m *MockAuthService) GenerateToken(userID string) (string, error) {
	args := m.Called(userID)
	return args.String(0), args.Error(1)
}

func (m *MockAuthService) AuthenticateAPIKey(ctx context.Context, apiKey string) (string, error) {
	args := m.Called(ctx, apiKey)
	return args.String(0), args.Error(1)
}

func (m *MockAuthService) Register(ctx context.Context, req types.RegisterRequest) (*types.AuthResponse, error) {
	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.AuthResponse), args.Error(1)
}

func (m *MockAuthService) Login(ctx context.Context, req types.LoginRequest) (*types.AuthResponse, error) {
	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.AuthResponse), args.Error(1)
}

func (m *MockAuthService) CreateAPIKey(ctx context.Context, userID string, req types.CreateAPIKeyRequest, sessionID string, matchedSigningKey []byte) (*types.APIKey, error) {
	args := m.Called(ctx, userID, req, sessionID, matchedSigningKey)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.APIKey), args.Error(1)
}

func (m *MockAuthService) ListAPIKeys(ctx context.Context, userID string) ([]*types.APIKey, error) {
	args := m.Called(ctx, userID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*types.APIKey), args.Error(1)
}

func (m *MockAuthService) DeleteAPIKey(ctx context.Context, userID, keyID string) error {
	args := m.Called(ctx, userID, keyID)
	return args.Error(0)
}

func (m *MockAuthService) AuthMiddleware() gin.HandlerFunc {
	args := m.Called()
	return args.Get(0).(gin.HandlerFunc)
}

func (m *MockAuthService) OptionalAuthMiddleware() gin.HandlerFunc {
	args := m.Called()
	return args.Get(0).(gin.HandlerFunc)
}

func TestAuthMiddleware(t *testing.T) {
	mockAuthService := new(MockAuthService)
	mockLogger := pkginterfaces.LoggerInterface(nil) // Use a no-op logger

	t.Cleanup(func() {
		mockAuthService.AssertExpectations(t)
	})

	// Test case: Valid token
	mockAuthService.On("ValidateToken", "valid_token").Return("user_id", nil)
	r := setupTestRouter(mockAuthService, mockLogger)
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer valid_token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "user_id", w.Header().Get("X-User-ID"))

	// Test case: Invalid token
	mockAuthService.On("ValidateToken", "invalid_token").Return("", errors.NewAuthenticationError("Invalid token", nil))
	r = setupTestRouter(mockAuthService, mockLogger)
	req, _ = http.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer invalid_token")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// Test case: No token provided
	r = setupTestRouter(mockAuthService, mockLogger)
	req, _ = http.NewRequest("GET", "/test", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// Clean up
	mockAuthService.AssertExpectations(t)
}

func setupTestRouter(authService interfaces.AuthService, logger pkginterfaces.LoggerInterface) *gin.Engine {
	r := gin.New()
	r.Use(
		middleware.ErrorHandlerMiddleware(logger),
		middleware.AuthMiddleware(authService, logger),
	)
	r.GET("/test", func(c *gin.Context) {
		userID, _ := c.Get("userID")
		c.Writer.Header().Set("X-User-ID", userID.(string))
		c.Status(http.StatusOK)
	})
	return r
}
