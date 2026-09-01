// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package tests

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
)

func TestAdminGuard_AdminAllowed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userRole", "admin")
		c.Next()
	})
	r.Use(middleware.AdminGuard())
	r.GET("/admin/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/admin/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200 for admin, got %d", w.Code)
	}
}

func TestAdminGuard_NonAdminGets404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userRole", "user")
		c.Next()
	})
	r.Use(middleware.AdminGuard())
	r.GET("/admin/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/admin/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404 for non-admin, got %d", w.Code)
	}
}

func TestAdminGuard_NoRoleGets404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// No userRole set in context
	r.Use(middleware.AdminGuard())
	r.GET("/admin/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/admin/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404 when no role set, got %d", w.Code)
	}
}

func TestAdminGuard_EmptyRoleGets404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userRole", "")
		c.Next()
	})
	r.Use(middleware.AdminGuard())
	r.GET("/admin/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/admin/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404 for empty role, got %d", w.Code)
	}
}
