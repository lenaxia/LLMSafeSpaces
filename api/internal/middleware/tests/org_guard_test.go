// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
)

type mockOrgMemberChecker struct {
	isMember bool
	isAdmin  bool
	err      error
}

func (m *mockOrgMemberChecker) IsOrgMember(_ context.Context, _, _ string) (bool, error) {
	return m.isMember, m.err
}

func (m *mockOrgMemberChecker) IsOrgAdmin(_ context.Context, _, _ string) (bool, error) {
	return m.isAdmin, m.err
}

func TestOrgMemberGuard_MemberAllowed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	checker := &mockOrgMemberChecker{isMember: true}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	r.GET("/orgs/:id/test", middleware.OrgMemberGuard(checker), func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/orgs/org-1/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200 for member, got %d: %s", w.Code, w.Body.String())
	}
}

func TestOrgMemberGuard_NonMemberGets403(t *testing.T) {
	gin.SetMode(gin.TestMode)
	checker := &mockOrgMemberChecker{isMember: false}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	r.GET("/orgs/:id/test", middleware.OrgMemberGuard(checker), func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/orgs/org-1/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != 403 {
		t.Errorf("expected 403 for non-member, got %d", w.Code)
	}
}

func TestOrgMemberGuard_UnauthenticatedGets401(t *testing.T) {
	gin.SetMode(gin.TestMode)
	checker := &mockOrgMemberChecker{isMember: true}
	r := gin.New()
	r.GET("/orgs/:id/test", middleware.OrgMemberGuard(checker), func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/orgs/org-1/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("expected 401 for unauthenticated, got %d", w.Code)
	}
}

func TestOrgMemberGuard_DBErrorGets500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	checker := &mockOrgMemberChecker{err: context.DeadlineExceeded}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	r.GET("/orgs/:id/test", middleware.OrgMemberGuard(checker), func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/orgs/org-1/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != 500 {
		t.Errorf("expected 500 for DB error, got %d", w.Code)
	}
}

func TestOrgAdminGuard_AdminAllowed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	checker := &mockOrgMemberChecker{isAdmin: true}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", "admin-1")
		c.Next()
	})
	r.DELETE("/orgs/:id", middleware.OrgAdminGuard(checker), func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/orgs/org-1", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200 for admin, got %d", w.Code)
	}
}

func TestOrgAdminGuard_NonAdminGets403(t *testing.T) {
	gin.SetMode(gin.TestMode)
	checker := &mockOrgMemberChecker{isAdmin: false}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	r.DELETE("/orgs/:id", middleware.OrgAdminGuard(checker), func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/orgs/org-1", nil)
	r.ServeHTTP(w, req)

	if w.Code != 403 {
		t.Errorf("expected 403 for non-admin, got %d", w.Code)
	}
}

func TestOrgAdminGuard_PendingAdminRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	checker := &mockOrgMemberChecker{isAdmin: false}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", "pending-admin")
		c.Next()
	})
	r.DELETE("/orgs/:id", middleware.OrgAdminGuard(checker), func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/orgs/org-1", nil)
	r.ServeHTTP(w, req)

	if w.Code != 403 {
		t.Errorf("expected 403 for pending admin, got %d", w.Code)
	}
}

func TestOrgAdminGuard_UnauthenticatedGets401(t *testing.T) {
	gin.SetMode(gin.TestMode)
	checker := &mockOrgMemberChecker{isAdmin: true}
	r := gin.New()
	r.DELETE("/orgs/:id", middleware.OrgAdminGuard(checker), func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("DELETE", "/orgs/org-1", nil)
	r.ServeHTTP(w, req)

	if w.Code != 401 {
		t.Errorf("expected 401 for unauthenticated, got %d", w.Code)
	}
}
