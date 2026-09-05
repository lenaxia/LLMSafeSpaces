// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package tests

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	logmock "github.com/lenaxia/llmsafespaces/mocks/logger"
)

// TestMiddlewareExecutionOrder tests that middleware executes in the correct order
func TestMiddlewareExecutionOrder(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Info", mock.Anything, mock.Anything).Maybe()
	mockLogger.On("With", mock.Anything, mock.Anything).Return(mockLogger).Maybe()

	// Create a slice to track execution order
	var executionOrder []string

	router := gin.New()
	router.Use(
		// First middleware
		func(c *gin.Context) {
			executionOrder = append(executionOrder, "request_id")
			c.Set("test_value", "set_by_first")
			c.Next()
			// This should execute last (after handler)
			executionOrder = append(executionOrder, "request_id_after")
		},
		// Second middleware
		middleware.TracingMiddleware(mockLogger),
		// Third middleware
		func(c *gin.Context) {
			executionOrder = append(executionOrder, "logging")
			// Check if previous middleware set values correctly
			val, exists := c.Get("test_value")
			if exists {
				c.Set("test_value", val.(string)+"_and_second")
			}
			c.Next()
			executionOrder = append(executionOrder, "logging_after")
		},
		// Fourth middleware
		func(c *gin.Context) {
			executionOrder = append(executionOrder, "auth")
			// Check if previous middleware set values correctly
			val, exists := c.Get("test_value")
			if exists {
				c.Set("test_value", val.(string)+"_and_third")
			}
			c.Next()
			executionOrder = append(executionOrder, "auth_after")
		},
	)

	router.GET("/test", func(c *gin.Context) {
		executionOrder = append(executionOrder, "handler")
		val, exists := c.Get("test_value")
		if exists {
			c.String(http.StatusOK, val.(string))
		} else {
			c.String(http.StatusInternalServerError, "Value not found")
		}
	})

	// Execute
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "set_by_first_and_second_and_third", w.Body.String())

	// Check execution order
	expectedOrder := []string{
		"request_id",
		"logging",
		"auth",
		"handler",
		"auth_after",
		"logging_after",
		"request_id_after",
	}
	assert.Equal(t, expectedOrder, executionOrder)

	mockLogger.AssertExpectations(t)
}

// TestMiddlewareErrorPropagation tests that errors are properly propagated through middleware
func TestMiddlewareErrorPropagation(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()
	mockLogger.On("Info", mock.Anything, mock.Anything).Maybe()
	mockLogger.On("With", mock.Anything, mock.Anything).Return(mockLogger).Maybe()

	// Create a slice to track execution
	var executionOrder []string

	router := gin.New()
	router.Use(
		// Error handler middleware should be first
		middleware.ErrorHandlerMiddleware(mockLogger),
		// Request ID middleware
		func(c *gin.Context) {
			executionOrder = append(executionOrder, "request_id")
			c.Set("request_id", "test-request-id")
			c.Next()
			executionOrder = append(executionOrder, "request_id_after")
		},
		// Logging middleware
		func(c *gin.Context) {
			executionOrder = append(executionOrder, "logging")
			c.Next()
			executionOrder = append(executionOrder, "logging_after")
		},
		// Auth middleware that will generate an error
		func(c *gin.Context) {
			executionOrder = append(executionOrder, "auth")
			// Simulate auth error
			apiErr := errors.NewAuthenticationError("Authentication failed", nil)
			middleware.HandleAPIError(c, apiErr)
		},
		// This middleware should not execute due to the error
		func(c *gin.Context) {
			executionOrder = append(executionOrder, "validation")
			c.Next()
			executionOrder = append(executionOrder, "validation_after")
		},
	)

	router.GET("/test", func(c *gin.Context) {
		executionOrder = append(executionOrder, "handler")
		c.String(http.StatusOK, "This should not execute")
	})

	// Execute
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// Parse response
	var response map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	assert.NoError(t, err)
	assert.Contains(t, response, "error")
	// #1281: error values are string-shaped; the code rides a sibling key.
	assert.Equal(t, "unauthorized", response["code"])
	errVal, ok := response["error"].(string)
	assert.True(t, ok, "error must be string-shaped (#1281), got %T", response["error"])
	assert.NotEmpty(t, errVal)

	// Check execution order - validation, handler, and anything after auth should not be called
	expectedOrder := []string{
		"request_id",
		"logging",
		"auth",
	}
	assert.Equal(t, expectedOrder, executionOrder[:len(expectedOrder)])
	assert.NotContains(t, executionOrder, "validation")
	assert.NotContains(t, executionOrder, "handler")
	assert.NotContains(t, executionOrder, "validation_after")

	mockLogger.AssertExpectations(t)
}

// TestMiddlewareContextPropagation tests that context values are properly propagated through middleware
func TestMiddlewareContextPropagation(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Info", mock.Anything, mock.Anything).Maybe()
	mockLogger.On("With", mock.Anything, mock.Anything).Return(mockLogger).Maybe()

	// Create maps to store context values at different stages
	contextValues := make(map[string]map[string]interface{})
	contextValues["tracing"] = make(map[string]interface{})
	contextValues["logging"] = make(map[string]interface{})
	contextValues["auth"] = make(map[string]interface{})
	contextValues["handler"] = make(map[string]interface{})

	router := gin.New()
	router.Use(
		// First middleware - sets request_id
		func(c *gin.Context) {
			c.Set("request_id", "test-request-id")
			c.Set("tracing_value", "set_by_tracing")
			// Store current context values
			for k, v := range c.Keys {
				contextValues["tracing"][k] = v
			}
			c.Next()
		},
		// Second middleware - sets logger
		func(c *gin.Context) {
			c.Set("logger", mockLogger)
			c.Set("logging_value", "set_by_logging")
			// Store current context values
			for k, v := range c.Keys {
				contextValues["logging"][k] = v
			}
			c.Next()
		},
		// Third middleware - sets user_id
		func(c *gin.Context) {
			c.Set("user_id", "test-user-id")
			c.Set("auth_value", "set_by_auth")
			// Store current context values
			for k, v := range c.Keys {
				contextValues["auth"][k] = v
			}
			c.Next()
		},
	)

	router.GET("/test", func(c *gin.Context) {
		// Store all context values in handler
		for k, v := range c.Keys {
			contextValues["handler"][k] = v
		}

		// Build response from context values
		response := make(map[string]interface{})
		response["request_id"] = c.GetString("request_id")
		response["user_id"] = c.GetString("user_id")
		response["tracing_value"] = c.GetString("tracing_value")
		response["logging_value"] = c.GetString("logging_value")
		response["auth_value"] = c.GetString("auth_value")

		c.JSON(http.StatusOK, response)
	})

	// Execute
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusOK, w.Code)

	// Parse response
	var response map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	assert.NoError(t, err)

	// Check response values
	assert.Equal(t, "test-request-id", response["request_id"])
	assert.Equal(t, "test-user-id", response["user_id"])
	assert.Equal(t, "set_by_tracing", response["tracing_value"])
	assert.Equal(t, "set_by_logging", response["logging_value"])
	assert.Equal(t, "set_by_auth", response["auth_value"])

	// Check context propagation
	assert.Contains(t, contextValues["tracing"], "request_id")
	assert.Contains(t, contextValues["tracing"], "tracing_value")

	assert.Contains(t, contextValues["logging"], "request_id")
	assert.Contains(t, contextValues["logging"], "tracing_value")
	assert.Contains(t, contextValues["logging"], "logger")
	assert.Contains(t, contextValues["logging"], "logging_value")

	assert.Contains(t, contextValues["auth"], "request_id")
	assert.Contains(t, contextValues["auth"], "tracing_value")
	assert.Contains(t, contextValues["auth"], "logger")
	assert.Contains(t, contextValues["auth"], "logging_value")
	assert.Contains(t, contextValues["auth"], "user_id")
	assert.Contains(t, contextValues["auth"], "auth_value")

	assert.Contains(t, contextValues["handler"], "request_id")
	assert.Contains(t, contextValues["handler"], "tracing_value")
	assert.Contains(t, contextValues["handler"], "logger")
	assert.Contains(t, contextValues["handler"], "logging_value")
	assert.Contains(t, contextValues["handler"], "user_id")
	assert.Contains(t, contextValues["handler"], "auth_value")

	mockLogger.AssertExpectations(t)
}
