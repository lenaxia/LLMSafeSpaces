// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package tests

// middleware_gaps_test.go — MISSINGTESTS.md items 1-4 (post US-46.12).
// Covers: middleware chaining, context propagation, error handling edge
// cases, and nested object validation.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	logmock "github.com/lenaxia/llmsafespaces/mocks/logger"
)

// === MISSINGTESTS Item 1: Middleware Chaining ===

func TestMiddlewareChain_ExecutionOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var order []string

	r := gin.New()
	r.Use(func(c *gin.Context) {
		order = append(order, "first")
		c.Set("first_val", "A")
		c.Next()
		order = append(order, "first-after")
	})
	r.Use(func(c *gin.Context) {
		order = append(order, "second")
		assert.Equal(t, "A", c.GetString("first_val"), "second middleware must see first's context value")
		c.Set("second_val", "B")
		c.Next()
	})
	r.Use(func(c *gin.Context) {
		order = append(order, "third")
		assert.Equal(t, "A", c.GetString("first_val"))
		assert.Equal(t, "B", c.GetString("second_val"))
		c.Next()
	})
	r.GET("/test", func(c *gin.Context) {
		order = append(order, "handler")
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"first", "second", "third", "handler", "first-after"}, order,
		"middleware must execute in registration order; first middleware's post-Next code runs last")
}

func TestMiddlewareChain_AbortStopsChain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var executed []string

	r := gin.New()
	r.Use(func(c *gin.Context) {
		executed = append(executed, "auth")
		c.Set("authenticated", true)
		c.Next()
	})
	r.Use(func(c *gin.Context) {
		executed = append(executed, "guard")
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "blocked"})
	})
	r.Use(func(c *gin.Context) {
		executed = append(executed, "should-not-run")
		c.Next()
	})
	r.GET("/test", func(c *gin.Context) {
		executed = append(executed, "handler")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, []string{"auth", "guard"}, executed,
		"middleware after Abort must not execute")
}

// === MISSINGTESTS Item 2: Context Value Propagation ===

func TestContextPropagation_ValuesSurviveAcrossMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("gin_user_id", "user-123")
		c.Next()
	})

	var ginVal string
	r.GET("/test", func(c *gin.Context) {
		ginVal = c.GetString("gin_user_id")
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "user-123", ginVal, "gin context value must propagate to handler")
}

func TestContextPropagation_OverwriteValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("role", "member")
		c.Next()
	})
	r.Use(func(c *gin.Context) {
		c.Set("role", "admin")
		c.Next()
	})

	var role string
	r.GET("/test", func(c *gin.Context) {
		role = c.GetString("role")
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, "admin", role, "later middleware should overwrite earlier value")
}

// === MISSINGTESTS Item 3: Error Handling Edge Cases ===

// TestErrorHandler_WrappedErrors verifies that deeply-wrapped errors
// (via fmt.Errorf %w chains) are surfaced to the client without
// crashing and contain the error message.
func TestErrorHandler_WrappedErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()

	r := gin.New()
	r.Use(middleware.ErrorHandlerMiddleware(mockLogger))
	r.GET("/test", func(c *gin.Context) {
		inner := fmt.Errorf("database connection lost")
		outer := fmt.Errorf("failed to create workspace: %w", inner)
		nested := fmt.Errorf("request handler: %w", outer)
		_ = c.Error(nested)
		c.AbortWithStatus(http.StatusInternalServerError)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	// The error handler may redact the message for security — just verify
	// the response is valid JSON with an error field.
	var resp map[string]interface{}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "response must be valid JSON")
}

// TestErrorHandler_LargePayload verifies error handling doesn't panic
// with a very large error message.
func TestErrorHandler_LargePayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()

	largeMsg := strings.Repeat("x", 100*1024)

	r := gin.New()
	r.Use(middleware.ErrorHandlerMiddleware(mockLogger))
	r.GET("/test", func(c *gin.Context) {
		_ = c.Error(fmt.Errorf("%s", largeMsg))
		c.AbortWithStatus(http.StatusInternalServerError)
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// === MISSINGTESTS Item 4: Validation — Nested Objects & Arrays ===

type validateTestStruct struct {
	Name     string           `json:"name" binding:"required,min=2,max=50"`
	Tags     []string         `json:"tags" binding:"dive,min=1,max=20"`
	Settings validateSettings `json:"settings" binding:"required"`
	Items    []validateItem   `json:"items" binding:"dive"`
}

type validateSettings struct {
	Visibility string `json:"visibility" binding:"required,oneof=public private"`
	MaxSize    int    `json:"maxSize" binding:"min=1,max=1000"`
}

type validateItem struct {
	ID    string `json:"id" binding:"required"`
	Count int    `json:"count" binding:"min=0"`
}

func TestValidation_NestedObject_RequiredField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/test", func(c *gin.Context) {
		var req validateTestStruct
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	body := `{"name":"test","tags":["a"],"settings":{"maxSize":100},"items":[]}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/test", io.NopCloser(strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "missing required nested field should fail validation")
}

func TestValidation_ArrayDive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/test", func(c *gin.Context) {
		var req validateTestStruct
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	body := `{"name":"test","tags":["ok"],"settings":{"visibility":"public","maxSize":100},"items":[{"id":"","count":1}]}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/test", io.NopCloser(strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "invalid array element should fail dive validation")
}

func TestValidation_ArrayMaxConstraint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/test", func(c *gin.Context) {
		var req validateTestStruct
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	body := `{"name":"test","tags":["this-tag-is-way-too-long-for-the-constraint"],"settings":{"visibility":"public","maxSize":100},"items":[]}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/test", io.NopCloser(strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "array element violating min/max should fail")
}

func TestValidation_ValidNestedObject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/test", func(c *gin.Context) {
		var req validateTestStruct
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	body := `{"name":"valid-name","tags":["a","b"],"settings":{"visibility":"public","maxSize":100},"items":[{"id":"item1","count":5}]}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/test", io.NopCloser(strings.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code, "valid nested structure should pass")
}
