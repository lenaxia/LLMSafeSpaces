// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package tests

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	logmock "github.com/lenaxia/llmsafespaces/mocks/logger"
)

func TestTracingMiddleware_RequestID(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("With", mock.Anything).Return(mockLogger).Maybe()

	config := middleware.TracingConfig{
		HeaderName:          "X-Request-ID",
		PropagateHeader:     true,
		GenerateIfMissing:   true,
		UseUUID:             true,
		EnableOpenTelemetry: false,
	}

	router := gin.New()
	router.Use(middleware.TracingMiddleware(mockLogger, config))
	router.GET("/test", func(c *gin.Context) {
		requestID, exists := c.Get("request_id")
		if exists {
			c.String(http.StatusOK, "Request ID: %s", requestID)
		} else {
			c.String(http.StatusInternalServerError, "No request ID found")
		}
	})

	// Execute
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Request ID:")

	// Check that request ID is in response header
	requestID := w.Header().Get("X-Request-ID")
	assert.NotEmpty(t, requestID)

	mockLogger.AssertExpectations(t)
}

func TestTracingMiddleware_ExistingRequestID(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("With", mock.Anything).Return(mockLogger).Maybe()

	config := middleware.TracingConfig{
		HeaderName:        "X-Request-ID",
		PropagateHeader:   true,
		GenerateIfMissing: true,
	}

	router := gin.New()
	router.Use(middleware.TracingMiddleware(mockLogger, config))
	router.GET("/test", func(c *gin.Context) {
		requestID, exists := c.Get("request_id")
		if exists {
			c.String(http.StatusOK, "Request ID: %s", requestID)
		} else {
			c.String(http.StatusInternalServerError, "No request ID found")
		}
	})

	// Execute with existing request ID
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Request-ID", "existing-id-12345")
	router.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Request ID: existing-id-12345")
	assert.Equal(t, "existing-id-12345", w.Header().Get("X-Request-ID"))

	mockLogger.AssertExpectations(t)
}

func TestTracingMiddleware_LoggerContext(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("With", mock.MatchedBy(func(args []interface{}) bool {
		// Check that the first argument is "request_id"
		return len(args) >= 2 && args[0] == "request_id"
	})).Return(mockLogger).Maybe()

	config := middleware.TracingConfig{
		HeaderName:        "X-Request-ID",
		PropagateHeader:   true,
		GenerateIfMissing: true,
	}

	router := gin.New()
	router.Use(middleware.TracingMiddleware(mockLogger, config))
	router.GET("/test", func(c *gin.Context) {
		_, exists := c.Get("logger")
		if exists {
			c.String(http.StatusOK, "Logger found")
		} else {
			c.String(http.StatusInternalServerError, "No logger found")
		}
	})

	// Execute
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Logger found")

	mockLogger.AssertExpectations(t)
}

func TestTracingMiddleware_StartTime(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("With", mock.Anything).Return(mockLogger)

	config := middleware.TracingConfig{
		HeaderName:        "X-Request-ID",
		PropagateHeader:   true,
		GenerateIfMissing: true,
	}

	router := gin.New()
	router.Use(middleware.TracingMiddleware(mockLogger, config))
	router.GET("/test", func(c *gin.Context) {
		_, exists := c.Get("start_time")
		if exists {
			c.String(http.StatusOK, "Start time found")
		} else {
			c.String(http.StatusInternalServerError, "No start time found")
		}
	})

	// Execute
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Start time found")

	mockLogger.AssertExpectations(t)
}
