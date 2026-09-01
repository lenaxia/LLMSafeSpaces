// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package tests

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	apiErrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	mocklogger "github.com/lenaxia/llmsafespaces/mocks/logger"
)

func TestErrorHandlerMiddleware_APIError(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	mockLogger := mocklogger.NewMockLogger()
	mockLogger.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()
	mockLogger.On("Warn", mock.Anything, mock.Anything).Maybe()
	mockLogger.On("Info", mock.Anything, mock.Anything).Maybe()

	config := middleware.ErrorHandlerConfig{
		IncludeStackTrace: false,
	}

	router := gin.New()
	router.Use(middleware.ErrorHandlerMiddleware(mockLogger, config))

	// Test validation error
	router.POST("/validate", func(c *gin.Context) {
		err := apiErrors.NewValidationError("Invalid input", map[string]interface{}{
			"field": "This field is required",
		}, nil)
		middleware.HandleAPIError(c, err)
	})

	// Test not found error
	router.GET("/notfound", func(c *gin.Context) {
		err := apiErrors.NewNotFoundError("Resource", "resource123", nil)
		middleware.HandleAPIError(c, err)
	})

	// Test internal error
	router.GET("/internal", func(c *gin.Context) {
		err := apiErrors.NewInternalError("Something went wrong", errors.New("database error"))
		middleware.HandleAPIError(c, err)
	})

	// Execute validation error request
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/validate", nil)
	router.ServeHTTP(w, req)

	// Assert validation error response
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	var response map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	assert.NoError(t, err)
	assert.Equal(t, "validation_error", response["error"].(map[string]interface{})["code"])
	assert.Contains(t, response["error"].(map[string]interface{})["details"], "field")

	// Execute not found error request
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/notfound", nil)
	router.ServeHTTP(w, req)

	// Assert not found error response
	assert.Equal(t, http.StatusNotFound, w.Code)
	err = json.Unmarshal(w.Body.Bytes(), &response)
	assert.NoError(t, err)
	assert.Equal(t, "not_found", response["error"].(map[string]interface{})["code"])

	// Execute internal error request
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/internal", nil)
	router.ServeHTTP(w, req)

	// Assert internal error response
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	err = json.Unmarshal(w.Body.Bytes(), &response)
	assert.NoError(t, err)
	assert.Equal(t, "internal_error", response["error"].(map[string]interface{})["code"])

	mockLogger.AssertExpectations(t)
}

func TestErrorHandlerMiddleware_StackTrace(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	mockLogger := mocklogger.NewMockLogger()
	mockLogger.On("Error", mock.Anything, mock.Anything, mock.Anything).Once()

	config := middleware.ErrorHandlerConfig{
		IncludeStackTrace: true,
		LogStackTrace:     true,
	}

	router := gin.New()
	router.Use(middleware.ErrorHandlerMiddleware(mockLogger, config))

	router.GET("/error", func(c *gin.Context) {
		err := apiErrors.NewInternalError("Internal server error", errors.New("database connection failed"))
		middleware.HandleAPIError(c, err)
	})

	// Execute
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/error", nil)
	router.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	var response map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	assert.NoError(t, err)

	// Check that stack trace is included
	errorDetails := response["error"].(map[string]interface{})["details"].(map[string]interface{})
	assert.Contains(t, errorDetails, "stack")
	assert.NotEmpty(t, errorDetails["stack"])

	mockLogger.AssertExpectations(t)
}

func TestErrorHandlerMiddleware_SensitiveDataRedaction(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	mockLogger := mocklogger.NewMockLogger()

	// Capture log fields
	var logFields []interface{}
	mockLogger.On("Error", mock.Anything, mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		logFields = args.Get(2).([]interface{})
	}).Once()

	config := middleware.ErrorHandlerConfig{
		SensitiveFields: []string{"password", "token", "apiKey"},
	}

	router := gin.New()
	router.Use(middleware.ErrorHandlerMiddleware(mockLogger, config))

	router.POST("/login", func(c *gin.Context) {
		// Simulate error after processing request with sensitive data
		err := apiErrors.NewAuthenticationError("Authentication failed", nil)
		middleware.HandleAPIError(c, err)
	})

	// Execute
	w := httptest.NewRecorder()
	reqBody := `{"username": "testuser", "password": "secret123", "apiKey": "abc123"}`
	req, _ := http.NewRequest("POST", "/login", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// Find request body in log fields
	var requestBody map[string]interface{}
	for i := 0; i < len(logFields); i += 2 {
		if logFields[i] == "request_body" {
			if body, ok := logFields[i+1].(map[string]interface{}); ok {
				requestBody = body
			}
		}
	}

	// Check that sensitive fields are masked
	assert.NotNil(t, requestBody)
	assert.Equal(t, "se...23", requestBody["password"])
	assert.Equal(t, "********", requestBody["apiKey"])
	assert.Equal(t, "testuser", requestBody["username"])

	mockLogger.AssertExpectations(t)
}
