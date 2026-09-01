// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package tests

// auth_rbac_test.go — US-46.12: Auth Middleware RBAC tests.
//
// These tests cover the AuthorizationMiddleware + RequirePermissions RBAC
// pipeline, exercising the permission hierarchy that the existing
// admin_guard_test.go does not (AdminGuard only checks userRole=="admin").
//
// Coverage matrix:
//   - User with required permission → 200
//   - User missing one required permission → 403
//   - User with no permissions set → 403 (authz required)
//   - Multiple required permissions, all present → 200
//   - Multiple required permissions, one missing → 403
//   - Permission escalation: member role attempts admin action → 403
//   - OPTIONS request bypasses authorization
//   - No requiredPermissions set → passthrough (no authz check)
//   - Org member vs org admin vs platform admin matrix

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	logmock "github.com/lenaxia/llmsafespaces/mocks/logger"
)

// buildRBACRouter constructs a Gin router with RequirePermissions +
// AuthorizationMiddleware wired, simulating the production authz pipeline.
// The caller pre-sets "permissions" via the contextSetter closure so each
// test case can simulate a different user role.
func buildRBACRouter(requiredPerms []string, contextSetter func(c *gin.Context)) *gin.Engine {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	// AuthorizationMiddleware calls log.Warn on 403 paths; allow any calls.
	mockLogger.On("Warn", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	r := gin.New()
	// ErrorHandlerMiddleware must be first — it provides the deferred error
	// writer that HandleAPIError (called by AuthorizationMiddleware on 403)
	// relies on to write the response.
	r.Use(middleware.ErrorHandlerMiddleware(nil))
	r.Use(func(c *gin.Context) {
		if contextSetter != nil {
			contextSetter(c)
		}
		c.Next()
	})
	r.Use(middleware.RequirePermissions(requiredPerms...))
	r.Use(middleware.AuthorizationMiddleware(nil, mockLogger))
	r.GET("/protected", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func TestRBAC_UserHasRequiredPermission_200(t *testing.T) {
	r := buildRBACRouter([]string{"workspace:read"}, func(c *gin.Context) {
		c.Set("permissions", []string{"workspace:read", "workspace:write"})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/protected", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "user with required permission should be allowed")
}

func TestRBAC_UserMissingRequiredPermission_403(t *testing.T) {
	r := buildRBACRouter([]string{"admin:users"}, func(c *gin.Context) {
		c.Set("permissions", []string{"workspace:read"})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/protected", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code, "user without required permission should get 403")
}

func TestRBAC_NoPermissionsInContext_403(t *testing.T) {
	// Simulates a user who authenticated but has no permissions loaded
	// into context (e.g. a newly registered user with no role).
	r := buildRBACRouter([]string{"workspace:read"}, func(c *gin.Context) {
		// permissions NOT set — simulates missing authz context
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/protected", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code, "missing permissions context should get 403")
}

func TestRBAC_AllMultipleRequiredPresent_200(t *testing.T) {
	r := buildRBACRouter([]string{"workspace:read", "workspace:write"}, func(c *gin.Context) {
		c.Set("permissions", []string{"workspace:read", "workspace:write", "workspace:delete"})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/protected", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "user with all required permissions should be allowed")
}

func TestRBAC_OneOfMultipleRequiredMissing_403(t *testing.T) {
	r := buildRBACRouter([]string{"workspace:read", "workspace:write"}, func(c *gin.Context) {
		c.Set("permissions", []string{"workspace:read"}) // missing workspace:write
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/protected", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code, "missing one of multiple required permissions should get 403")
}

func TestRBAC_PermissionEscalation_MemberAttemptsAdminAction_403(t *testing.T) {
	// A regular member tries to access an admin-only endpoint.
	// Their permissions only include member-level perms, not admin perms.
	r := buildRBACRouter([]string{"admin:billing", "admin:users"}, func(c *gin.Context) {
		c.Set("permissions", []string{"workspace:read", "workspace:write", "session:read"})
		c.Set("userRole", "member")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/protected", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code, "member attempting admin action should get 403")
}

func TestRBAC_PlatformAdminAccessesAdminEndpoint_200(t *testing.T) {
	// Platform admin has all admin permissions.
	r := buildRBACRouter([]string{"admin:billing", "admin:users"}, func(c *gin.Context) {
		c.Set("permissions", []string{"admin:billing", "admin:users", "admin:settings"})
		c.Set("userRole", "admin")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/protected", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "platform admin should be allowed")
}

func TestRBAC_OptionsBypassesAuthorization(t *testing.T) {
	// CORS preflight (OPTIONS) must bypass authz — browsers send OPTIONS
	// before the actual request without auth headers.
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Warn", mock.Anything, mock.Anything, mock.Anything).Maybe()
	r := gin.New()
	r.Use(middleware.RequirePermissions("admin:secret"))
	r.Use(middleware.AuthorizationMiddleware(nil, mockLogger))
	r.GET("/protected", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("OPTIONS", "/protected", nil)
	r.ServeHTTP(w, req)

	// Gin returns 404 for unregistered OPTIONS routes by default in test mode,
	// but the middleware should NOT abort with 403. The key assertion is that
	// authz did not block the request (no 403).
	assert.NotEqual(t, http.StatusForbidden, w.Code, "OPTIONS should bypass authorization")
}

func TestRBAC_NoRequiredPermissions_Passthrough(t *testing.T) {
	// When RequirePermissions is not used (no requiredPermissions in context),
	// AuthorizationMiddleware should passthrough without checking.
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	r := gin.New()
	r.Use(middleware.AuthorizationMiddleware(nil, mockLogger))
	r.GET("/public", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/public", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "no requiredPermissions set should passthrough")
}

// --- Org permission hierarchy matrix ---

// TestRBAC_OrgPermissionMatrix exercises the full org hierarchy:
// member < admin for both OrgMemberGuard and OrgAdminGuard.
// This is a table-driven test covering the permission escalation
// attempts documented in MISSINGTESTS.md item 5.
func TestRBAC_OrgPermissionMatrix(t *testing.T) {
	tests := []struct {
		name       string
		isMember   bool
		isAdmin    bool
		memberErr  error
		adminErr   error
		useAdmin   bool // true = test OrgAdminGuard, false = test OrgMemberGuard
		wantStatus int
	}{
		{
			name:       "member accesses member-guarded endpoint",
			isMember:   true,
			isAdmin:    false,
			useAdmin:   false,
			wantStatus: http.StatusOK,
		},
		{
			name:       "non-member blocked from member-guarded endpoint",
			isMember:   false,
			isAdmin:    false,
			useAdmin:   false,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "admin accesses admin-guarded endpoint",
			isMember:   true,
			isAdmin:    true,
			useAdmin:   true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "member (not admin) blocked from admin-guarded endpoint",
			isMember:   true,
			isAdmin:    false,
			useAdmin:   true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "non-member blocked from admin-guarded endpoint",
			isMember:   false,
			isAdmin:    false,
			useAdmin:   true,
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			r := gin.New()
			mockStore := &mockOrgStore{
				isMember: tt.isMember,
				isAdmin:  tt.isAdmin,
			}
			r.Use(func(c *gin.Context) {
				c.Set("userID", "user-123")
				c.Next()
			})
			if tt.useAdmin {
				r.Use(middleware.OrgAdminGuard(mockStore))
			} else {
				r.Use(middleware.OrgMemberGuard(mockStore))
			}
			r.GET("/orgs/:id/settings", func(c *gin.Context) {
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})

			w := httptest.NewRecorder()
			req, _ := http.NewRequest("GET", "/orgs/org-456/settings", nil)
			r.ServeHTTP(w, req)

			assert.Equal(t, tt.wantStatus, w.Code)
		})
	}
}

// mockOrgStore implements orgMemberChecker for testing OrgMemberGuard/OrgAdminGuard.
type mockOrgStore struct {
	isMember bool
	isAdmin  bool
	err      error
}

func (m *mockOrgStore) IsOrgMember(ctx context.Context, orgID, userID string) (bool, error) {
	return m.isMember, m.err
}

func (m *mockOrgStore) IsOrgAdmin(ctx context.Context, orgID, userID string) (bool, error) {
	return m.isAdmin, m.err
}
