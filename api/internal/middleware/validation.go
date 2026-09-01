// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"

	"github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// Use a single instance of Validate, it caches struct info
var validate = validator.New()

// ValidationConfig defines configuration for the validation middleware
type ValidationConfig struct {
	// CustomValidators is a map of custom validation functions
	CustomValidators map[string]validator.Func

	// CustomErrorMessages is a map of custom error messages for validation tags
	CustomErrorMessages map[string]string

	// ValidateQueryParams indicates whether to validate query parameters
	ValidateQueryParams bool

	// ValidatePathParams indicates whether to validate path parameters
	ValidatePathParams bool
}

// DefaultValidationConfig returns the default validation configuration
func DefaultValidationConfig() ValidationConfig {
	return ValidationConfig{
		CustomValidators:    make(map[string]validator.Func),
		CustomErrorMessages: make(map[string]string),
		ValidateQueryParams: false,
		ValidatePathParams:  false,
	}
}

func init() {
	// Register custom validation functions
	_ = validate.RegisterValidation("nohtml", validateNoHTML)
	_ = validate.RegisterValidation("alphanum_space", validateAlphanumSpace)
	_ = validate.RegisterValidation("iso8601", validateISO8601)

	// Register custom tag name function
	validate.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
		if name == "-" {
			return ""
		}
		return name
	})
}

// ValidationMiddleware returns a middleware that validates request bodies
func ValidationMiddleware(log interfaces.LoggerInterface, config ...ValidationConfig) gin.HandlerFunc {
	// Use default config if none provided
	cfg := DefaultValidationConfig()
	if len(config) > 0 {
		cfg = config[0]
	}

	// Register custom validators
	for tag, fn := range cfg.CustomValidators {
		_ = validate.RegisterValidation(tag, fn)
	}

	return func(c *gin.Context) {
		// Skip validation for GET, DELETE, and OPTIONS requests
		if c.Request.Method == http.MethodGet ||
			c.Request.Method == http.MethodDelete ||
			c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}

		// Get the model to validate from the context
		model, exists := c.Get("validationModel")
		if !exists {
			c.Next()
			return
		}

		// Read request body
		var bodyBytes []byte
		if c.Request.Body != nil {
			bodyBytes, _ = io.ReadAll(c.Request.Body)
			// Restore the body
			c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		}

		// Skip if no body
		if len(bodyBytes) == 0 {
			c.Next()
			return
		}

		// Create a new instance of the model
		modelType := reflect.TypeOf(model)
		if modelType.Kind() == reflect.Ptr {
			modelType = modelType.Elem()
		}
		modelValue := reflect.New(modelType).Interface()

		// Unmarshal the request body into the model
		if err := json.Unmarshal(bodyBytes, modelValue); err != nil {
			apiErr := errors.NewValidationError("Invalid request body", map[string]interface{}{
				"errors": map[string]string{
					"body": "Invalid JSON format",
				},
			}, err)
			c.AbortWithStatusJSON(apiErr.StatusCode(), gin.H{
				"error": gin.H{
					"code":    apiErr.Code,
					"message": apiErr.Message,
					"details": apiErr.Details,
				},
			})
			return
		}

		// Validate the model
		if err := validate.Struct(modelValue); err != nil {
			// Convert validation errors to a map
			validationErrors := make(map[string]string)
			if verrors, ok := err.(validator.ValidationErrors); ok {
				for _, verr := range verrors {
					// Use the JSON field name (lowercase) instead of struct field name
					fieldName := verr.Field()

					// Get the JSON tag name for this field
					field, _ := modelType.FieldByName(fieldName)
					jsonTag := field.Tag.Get("json")
					if jsonTag != "" {
						// Extract the field name part before any comma
						if commaIdx := strings.Index(jsonTag, ","); commaIdx > 0 {
							fieldName = jsonTag[:commaIdx]
						} else {
							fieldName = jsonTag
						}
					} else {
						// If no JSON tag, use the lowercase field name
						fieldName = strings.ToLower(fieldName[:1]) + fieldName[1:]
					}

					validationErrors[fieldName] = getValidationErrorMessage(verr, cfg.CustomErrorMessages)
				}
			}

			apiErr := errors.NewValidationError("Validation failed", map[string]interface{}{
				"errors": validationErrors,
			}, err)
			c.AbortWithStatusJSON(apiErr.StatusCode(), gin.H{
				"error": gin.H{
					"code":    apiErr.Code,
					"message": apiErr.Message,
					"details": apiErr.Details,
				},
			})
			return
		}

		// Set the validated model in context for handler to use
		c.Set("validatedModel", modelValue)

		// Validate query parameters if configured
		if cfg.ValidateQueryParams {
			if err := validateQueryParams(c); err != nil {
				HandleAPIError(c, err)
				return
			}
		}

		// Validate path parameters if configured
		if cfg.ValidatePathParams {
			if err := validatePathParams(c); err != nil {
				HandleAPIError(c, err)
				return
			}
		}

		c.Next()
	}
}

// ValidateRequest validates a request body against a model
func ValidateRequest(c *gin.Context, model interface{}) error {
	// Set the validation model in the context
	c.Set("validationModel", model)

	// Read request body
	var bodyBytes []byte
	if c.Request.Body != nil {
		bodyBytes, _ = io.ReadAll(c.Request.Body)
		// Restore the body
		c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
	}

	// Skip if no body
	if len(bodyBytes) == 0 {
		return errors.NewValidationError("Request body is required", nil, nil)
	}

	// Unmarshal the request body into the model
	if err := json.Unmarshal(bodyBytes, model); err != nil {
		return errors.NewValidationError("Invalid request body", map[string]interface{}{
			"errors": map[string]string{
				"body": "Invalid JSON format",
			},
		}, err)
	}

	// Validate the model
	if err := validate.Struct(model); err != nil {
		// Convert validation errors to a map
		validationErrors := make(map[string]string)
		if verrors, ok := err.(validator.ValidationErrors); ok {
			for _, verr := range verrors {
				fieldName := strings.ToLower(verr.Field())
				validationErrors[fieldName] = getValidationErrorMessage(verr, nil)
			}
		}

		return errors.NewValidationError("Validation failed", map[string]interface{}{
			"errors": validationErrors,
		}, err)
	}

	return nil
}

// validateQueryParams validates query parameters
func validateQueryParams(c *gin.Context) error {
	// Get query parameter validation rules from context
	_, exists := c.Get("queryParamRules")
	if !exists {
		return nil
	}

	// Validate query parameters
	validationErrors := make(map[string]string)

	// Implement query parameter validation logic here
	// This is a placeholder for the actual implementation

	if len(validationErrors) > 0 {
		return errors.NewValidationError("Invalid query parameters", map[string]interface{}{
			"errors": validationErrors,
		}, nil)
	}

	return nil
}

// validatePathParams validates path parameters
func validatePathParams(c *gin.Context) error {
	// Get path parameter validation rules from context
	_, exists := c.Get("pathParamRules")
	if !exists {
		return nil
	}

	// Validate path parameters
	validationErrors := make(map[string]string)

	// Implement path parameter validation logic here
	// This is a placeholder for the actual implementation

	if len(validationErrors) > 0 {
		return errors.NewValidationError("Invalid path parameters", map[string]interface{}{
			"errors": validationErrors,
		}, nil)
	}

	return nil
}

// getValidationErrorMessage returns a human-readable error message for a validation error
//
// DUPLICATION NOTE: This switch is intentionally duplicated with
// bindingErrorMessage in api/internal/handlers/binding_errors.go. The two
// paths emit different envelope shapes; collapsing them requires new
// abstractions. They MUST be kept in sync: if you add or change a case
// here, update the handlers version too.
func getValidationErrorMessage(err validator.FieldError, customMessages map[string]string) string {
	// Check for custom error message
	if customMessages != nil {
		if msg, ok := customMessages[err.Tag()]; ok {
			return msg
		}
	}

	switch err.Tag() {
	case "required":
		return "This field is required"
	case "email":
		return "Invalid email address"
	case "min":
		if err.Type().Kind() == reflect.String {
			return fmt.Sprintf("Must be at least %s characters long", err.Param())
		}
		return fmt.Sprintf("Must be greater than or equal to %s", err.Param())
	case "max":
		if err.Type().Kind() == reflect.String {
			return fmt.Sprintf("Must be at most %s characters long", err.Param())
		}
		return fmt.Sprintf("Must be less than or equal to %s", err.Param())
	case "len":
		return fmt.Sprintf("Must be exactly %s characters long", err.Param())
	case "oneof":
		return fmt.Sprintf("Must be one of: %s", err.Param())
	case "alphanum":
		return "Must contain only alphanumeric characters"
	case "alphanum_space":
		return "Must contain only alphanumeric characters and spaces"
	case "nohtml":
		return "HTML tags are not allowed"
	case "iso8601":
		return "Must be a valid ISO8601 date/time"
	case "uuid":
		return "Must be a valid UUID"
	case "url":
		return "Must be a valid URL"
	case "json":
		return "Must be valid JSON"
	case "file":
		return "Must be a valid file"
	case "custom_alpha":
		return "Must contain only alphabetic characters"
	default:
		return "Invalid value"
	}
}

// Custom validation functions

// validateNoHTML validates that a string does not contain HTML tags
func validateNoHTML(fl validator.FieldLevel) bool {
	field := fl.Field()
	if field.Kind() != reflect.String {
		return true
	}

	value := field.String()
	return !strings.Contains(value, "<") && !strings.Contains(value, ">")
}

// validateAlphanumSpace validates that a string contains only alphanumeric characters and spaces
func validateAlphanumSpace(fl validator.FieldLevel) bool {
	field := fl.Field()
	if field.Kind() != reflect.String {
		return true
	}

	value := field.String()
	for _, r := range value {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// validateISO8601 validates that a string is a valid ISO8601 date/time
func validateISO8601(fl validator.FieldLevel) bool {
	field := fl.Field()
	if field.Kind() != reflect.String {
		return true
	}

	value := field.String()
	_, err := time.Parse(time.RFC3339, value)
	return err == nil
}
