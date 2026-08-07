// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
)

// fakeOrgAdminChecker implements middleware.orgMemberChecker for route-level
// integration tests. Controls who is an org admin.
type fakeOrgAdminChecker struct {
	admins map[string]bool // "userID:orgID" → true
}

func (f *fakeOrgAdminChecker) IsOrgAdmin(_ context.Context, orgID, userID string) (bool, error) {
	return f.admins[userID+":"+orgID], nil
}

func (f *fakeOrgAdminChecker) IsOrgMember(_ context.Context, orgID, userID string) (bool, error) {
	return false, nil
}

// --- Integration: route middleware guards (design/0047 D1) ---
//
// These tests verify the OrgAdminGuard and AdminGuard middleware correctly
// gate the new org-scoped and platform-scoped config creation routes.
// The handler methods (CreateOrgConfig, CreatePlatformConfig) do no own
// admin checks — the security boundary is the middleware.

func TestOrgConfigRoute_OrgAdmin_CanAccess(t *testing.T) {
	t.Parallel()
	store := s4Store()
	disp := &fakeDispatcher{ghRunID: 1}
	orgs := &fakeOrgResolver{}
	h := NewImageFactoryHandler(store, orgs)
	h.SetDispatcher(disp)

	checker := &fakeOrgAdminChecker{admins: map[string]bool{"admin-1:org-1": true}}

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "admin-1"); c.Next() })
	orgGroup := r.Group("/api/v1/orgs/:id/image-factory")
	orgGroup.Use(middleware.OrgAdminGuard(checker))
	orgGroup.POST("/configs", h.CreateOrgConfig)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/orgs/org-1/image-factory/configs", nil)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	// 400/422 is fine (empty body) — the point is it's NOT 403 (middleware passed)
	assert.NotEqual(t, http.StatusForbidden, w.Code, "org admin should pass OrgAdminGuard")
	assert.NotEqual(t, http.StatusUnauthorized, w.Code, "org admin should pass auth")
}

func TestOrgConfigRoute_NonAdmin_Blocked(t *testing.T) {
	t.Parallel()
	store := s4Store()
	orgs := &fakeOrgResolver{}
	h := NewImageFactoryHandler(store, orgs)

	checker := &fakeOrgAdminChecker{admins: map[string]bool{}} // member-1 is NOT admin

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "member-1"); c.Next() })
	orgGroup := r.Group("/api/v1/orgs/:id/image-factory")
	orgGroup.Use(middleware.OrgAdminGuard(checker))
	orgGroup.POST("/configs", h.CreateOrgConfig)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/orgs/org-1/image-factory/configs", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code, "non-admin should be blocked by OrgAdminGuard")
}

func TestPlatformConfigRoute_PlatformAdmin_CanAccess(t *testing.T) {
	t.Parallel()
	store := s4Store()
	disp := &fakeDispatcher{ghRunID: 1}
	orgs := &fakeOrgResolver{}
	h := NewImageFactoryHandler(store, orgs)
	h.SetDispatcher(disp)

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "admin-1"); c.Set("userRole", "admin"); c.Next() })
	adminGroup := r.Group("/api/v1/admin/image-factory")
	adminGroup.Use(middleware.AdminGuard())
	adminGroup.POST("/configs", h.CreatePlatformConfig)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/admin/image-factory/configs", nil)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	// 400/422 is fine (empty body) — the point is it's NOT 403
	assert.NotEqual(t, http.StatusForbidden, w.Code, "platform admin should pass AdminGuard")
}

func TestPlatformConfigRoute_RegularUser_Blocked(t *testing.T) {
	t.Parallel()
	store := s4Store()
	orgs := &fakeOrgResolver{}
	h := NewImageFactoryHandler(store, orgs)

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Set("userRole", "member"); c.Next() })
	adminGroup := r.Group("/api/v1/admin/image-factory")
	adminGroup.Use(middleware.AdminGuard())
	adminGroup.POST("/configs", h.CreatePlatformConfig)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/admin/image-factory/configs", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "regular user should be blocked by AdminGuard (404 hides the route)")
}
