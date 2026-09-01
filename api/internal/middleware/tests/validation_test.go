// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/stretchr/testify/assert"

	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	logmock "github.com/lenaxia/llmsafespaces/mocks/logger"
)

// Test model for validation
type TestUser struct {
	Username string `json:"username" validate:"required,min=3,max=50"`
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password" validate:"required,min=8"`
	Age      int    `json:"age" validate:"gte=18"`
	Website  string `json:"website" validate:"omitempty,url"`
	Bio      string `json:"bio" validate:"omitempty,nohtml"`
}

func TestValidationMiddleware_ValidRequest(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()

	router := gin.New()

	router.POST("/users",
		// Set validation model before validation middleware
		func(c *gin.Context) {
			c.Set("validationModel", &TestUser{})
			c.Next()
		},
		middleware.ValidationMiddleware(mockLogger),
		func(c *gin.Context) {
			// Get validated model
			validatedModel, exists := c.Get("validatedModel")
			if !exists {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Validation failed"})
				return
			}

			user := validatedModel.(*TestUser)
			c.JSON(http.StatusOK, gin.H{
				"message": "User created",
				"user":    user,
			})
		},
	)

	// Execute with valid data
	w := httptest.NewRecorder()
	validUser := `{
		"username": "testuser",
		"email": "test@example.com",
		"password": "password123",
		"age": 25,
		"website": "https://example.com",
		"bio": "This is my bio"
	}`
	req, _ := http.NewRequest("POST", "/users", bytes.NewBufferString(validUser))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusOK, w.Code)
	var response map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	assert.NoError(t, err)
	assert.Equal(t, "User created", response["message"])

	mockLogger.AssertExpectations(t)
}

func TestValidationMiddleware_InvalidRequest(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()

	router := gin.New()

	router.POST("/users",
		func(c *gin.Context) {
			c.Set("validationModel", &TestUser{})
			c.Next()
		},
		middleware.ValidationMiddleware(mockLogger),
		func(c *gin.Context) {
			// This should not be reached for invalid data
			c.JSON(http.StatusOK, gin.H{"message": "User created"})
		},
	)

	// Execute with invalid data
	w := httptest.NewRecorder()
	invalidUser := `{
		"username": "t",
		"email": "not-an-email",
		"password": "short",
		"age": 16
	}`
	req, _ := http.NewRequest("POST", "/users", bytes.NewBufferString(invalidUser))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	var response map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	assert.NoError(t, err)

	// Check error structure
	errorObj := response["error"].(map[string]interface{})
	assert.Equal(t, "validation_error", errorObj["code"])
	assert.Contains(t, errorObj["message"], "Validation failed")

	// Check validation errors
	details := errorObj["details"].(map[string]interface{})
	errors := details["errors"].(map[string]interface{})

	assert.Contains(t, errors, "username")
	assert.Contains(t, errors, "email")
	assert.Contains(t, errors, "password")
	assert.Contains(t, errors, "age")

	mockLogger.AssertExpectations(t)
}

func TestValidationMiddleware_InvalidJSON(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()

	router := gin.New()

	router.POST("/users",
		// Set validation model first
		func(c *gin.Context) {
			c.Set("validationModel", &TestUser{})
			c.Next()
		},
		// Then apply validation middleware
		middleware.ValidationMiddleware(mockLogger),
		// This should not be reached for invalid JSON
		func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"message": "User created"})
		},
	)

	// Execute with invalid JSON
	w := httptest.NewRecorder()
	invalidJSON := `{"username": "testuser", "email": "test@example.com", "password": "password123", "age": 25,`
	req, _ := http.NewRequest("POST", "/users", bytes.NewBufferString(invalidJSON))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	var response map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	assert.NoError(t, err)

	// Check error structure
	errorObj := response["error"].(map[string]interface{})
	assert.Equal(t, "validation_error", errorObj["code"])
	assert.Contains(t, errorObj["message"], "Invalid request body")

	mockLogger.AssertExpectations(t)
}

func TestValidationMiddleware_CustomValidator(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()

	// Create custom validation config
	config := middleware.ValidationConfig{
		CustomValidators: map[string]validator.Func{
			"custom_alpha": func(fl validator.FieldLevel) bool {
				// Only allow alphabetic characters
				value := fl.Field().String()
				for _, r := range value {
					if !unicode.IsLetter(r) {
						return false
					}
				}
				return true
			},
		},
		CustomErrorMessages: map[string]string{
			"custom_alpha": "Must contain only alphabetic characters",
		},
	}

	// Test model with custom validator
	type CustomModel struct {
		Name string `json:"name" validate:"required,custom_alpha"`
	}

	router := gin.New()

	router.POST("/custom",
		// Set validation model first
		func(c *gin.Context) {
			c.Set("validationModel", &CustomModel{})
			c.Next()
		},
		// Then apply validation middleware with custom config
		middleware.ValidationMiddleware(mockLogger, config),
		// This handler should only be reached for valid data
		func(c *gin.Context) {
			// Get validated model
			validatedModel, exists := c.Get("validatedModel")
			if !exists {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Validation failed"})
				return
			}

			model := validatedModel.(*CustomModel)
			c.JSON(http.StatusOK, gin.H{
				"message": "Valid data",
				"data":    model,
			})
		},
	)

	// Execute with invalid data for custom validator
	w := httptest.NewRecorder()
	invalidData := `{"name": "John123"}`
	req, _ := http.NewRequest("POST", "/custom", bytes.NewBufferString(invalidData))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	var response map[string]interface{}
	err := json.Unmarshal(w.Body.Bytes(), &response)
	assert.NoError(t, err)

	// Check custom error message
	errorObj := response["error"].(map[string]interface{})
	details := errorObj["details"].(map[string]interface{})
	errors := details["errors"].(map[string]interface{})

	assert.Contains(t, errors["name"], "Must contain only alphabetic characters")

	mockLogger.AssertExpectations(t)
}
