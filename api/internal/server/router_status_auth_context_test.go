// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/api/internal/services/agentpush"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// TestGetStatusRoute_AttachesAgentpushAuthContext is the router-side
// regression test for PR #494's #493 fix: the GET /workspaces/:id/status
// closure must extract sessionID (gin key: "sessionID") and
// matchedSigningKey (gin key: "jwt_signing_key") from the gin context
// and attach them to the request context via agentpush.WithAuth before
// calling GetWorkspaceStatus. Without this, the downstream fire-and-
// forget auto-push has no DEK — every user-DEK entry silently
// degrades to skip-with-audit.
//
// A key-name mismatch (e.g. someone renaming "jwt_signing_key" to
// "signing_key" on the middleware side) would break the DEK retrieval
// with zero visible symptoms. This test pins the exact keys.
func TestGetStatusRoute_AttachesAgentpushAuthContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		wantSessionID  = "sess-42"
		wantSigningKey = "signing-key-bytes"
	)

	// Custom router with a bespoke auth middleware that plants the
	// exact gin-context keys the production AuthMiddleware writes.
	log, err := apilogger.New(false, "error", "json")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	ws := &imocks.MockWorkspaceService{}

	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	met.On("IncrementActiveConnections", mock.Anything, mock.Anything).Maybe()
	met.On("DecrementActiveConnections", mock.Anything, mock.Anything).Maybe()
	ws.On("ResolveWorkspace", mock.Anything, mock.Anything).
		Return(&types.WorkspaceMetadata{ID: "ws-1", UserID: "test-user"}, nil).Maybe()
	ws.On("CheckOwnership", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	// The mock middleware sets ONLY these two keys — matching production
	// AuthMiddleware's post-JWT-validation gin.Context state.
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		c.Set("userID", "test-user")
		c.Set("sessionID", wantSessionID)
		c.Set("jwt_signing_key", []byte(wantSigningKey))
		c.Next()
	}))
	auth.On("GetUserID", mock.Anything).Return("test-user")

	// Capture the ctx that GetWorkspaceStatus receives so we can assert
	// its agentpush-auth carries the exact values.
	var capturedSession string
	var capturedKey []byte
	ws.On("GetWorkspaceStatus", mock.Anything, "test-user", "ws-1").
		Run(func(args mock.Arguments) {
			ctx := args.Get(0).(context.Context)
			capturedSession, capturedKey = agentpush.AuthFromContext(ctx)
		}).
		Return(&types.WorkspaceStatusResult{Phase: "Active"}, nil)

	svc := &mockServices{auth: auth, metrics: met, workspace: ws}
	router := NewRouter(svc, log, nil, RouterConfig{Debug: false})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/status", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, wantSessionID, capturedSession,
		"sessionID must round-trip from gin context key \"sessionID\" "+
			"through the router closure into agentpush.WithAuth")
	assert.Equal(t, []byte(wantSigningKey), capturedKey,
		"matchedSigningKey must round-trip from gin context key "+
			"\"jwt_signing_key\" through the router closure into agentpush.WithAuth")
}

// TestGetStatusRoute_NoAuthValues_ContextStillCarriesZeros proves the
// unauthenticated-fallback case: when the request auth path DOESN'T
// set sessionID/jwt_signing_key (e.g. legacy API-key without
// DecryptAccess), the router still passes an agentpush-auth ctx —
// just with empty values. Downstream, InjectSecrets sees an empty
// sessionID and degrades to skip-with-audit for user-DEK entries.
// This test locks in that empty auth doesn't panic and doesn't
// short-circuit the status read.
func TestGetStatusRoute_NoAuthValues_ContextStillCarriesZeros(t *testing.T) {
	gin.SetMode(gin.TestMode)

	log, err := apilogger.New(false, "error", "json")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	ws := &imocks.MockWorkspaceService{}

	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	met.On("IncrementActiveConnections", mock.Anything, mock.Anything).Maybe()
	met.On("DecrementActiveConnections", mock.Anything, mock.Anything).Maybe()
	ws.On("ResolveWorkspace", mock.Anything, mock.Anything).
		Return(&types.WorkspaceMetadata{ID: "ws-1", UserID: "test-user"}, nil).Maybe()
	ws.On("CheckOwnership", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	// Middleware sets ONLY userID — no sessionID, no jwt_signing_key.
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		c.Set("userID", "test-user")
		c.Next()
	}))
	auth.On("GetUserID", mock.Anything).Return("test-user")

	var capturedSession string
	var capturedKey []byte
	ws.On("GetWorkspaceStatus", mock.Anything, "test-user", "ws-1").
		Run(func(args mock.Arguments) {
			ctx := args.Get(0).(context.Context)
			capturedSession, capturedKey = agentpush.AuthFromContext(ctx)
		}).
		Return(&types.WorkspaceStatusResult{Phase: "Active"}, nil)

	svc := &mockServices{auth: auth, metrics: met, workspace: ws}
	router := NewRouter(svc, log, nil, RouterConfig{Debug: false})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/status", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "", capturedSession, "no sessionID must surface as \"\"")
	assert.Nil(t, capturedKey, "no signing key must surface as nil")
}

// Ensure the interfaces package is imported (helps auditors read imports).
var _ = interfaces.CacheService(nil)
