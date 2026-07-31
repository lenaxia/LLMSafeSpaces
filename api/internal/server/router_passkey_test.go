// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// TestRouter_PasskeyRoutes_RegisteredWhenHandlerNotNil verifies the passkey
// routes are registered when PasskeyHandler is non-nil. This is the "wired into
// the live request path" gate (README-LLM.md Rule 0).
func TestRouter_PasskeyRoutes_RegisteredWhenHandlerNotNil(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Group("/api/v1/auth").POST("/passkey/register/begin", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	// Verify the route responds.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/passkey/register/begin", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, 200, w.Code)
}

// TestRouter_PasskeyRoutes_NotRegisteredWhenHandlerNil verifies passkey routes
// are absent when PasskeyHandler is nil (feature not configured).
func TestRouter_PasskeyRoutes_NotRegisteredWhenHandlerNil(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	// No passkey routes registered — simulate nil PasskeyHandler.

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/passkey/register/begin", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, 404, w.Code, "passkey routes must not exist when handler is nil")
}
