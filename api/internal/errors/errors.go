// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package errors

import (
	"fmt"
	"net/http"
)

// ErrNoAgentStateRow is returned by MarkAgentReloaded when the
// workspace_agent_state row does not exist for the given workspace.
// Both database.go (returns it) and handler code (checks via errors.Is)
// import this shared package — neither imports the other.
//
// It is a *APIError (not a plain sentinel) so the centralized error handler
// (respondWithError) can map it to HTTP 409 Conflict automatically via
// StatusCode(). Callers can still use errors.Is for backwards compat and
// errors.As for the new typed-error path.
var ErrNoAgentStateRow = &APIError{
	Type:    ErrorTypeConflict,
	Code:    "no_pending_agent_reload",
	Message: "workspace has no pending credentials to reload",
}

// ErrorType defines the type of error
type ErrorType string

const (
	// ErrorTypeValidation represents validation errors
	ErrorTypeValidation ErrorType = "validation_error"

	// ErrorTypeAuth represents authentication errors
	ErrorTypeAuth ErrorType = "auth_error"

	// ErrorTypeNotFound represents resource not found errors
	ErrorTypeNotFound ErrorType = "not_found"

	// ErrorTypeForbidden represents permission denied errors
	ErrorTypeForbidden ErrorType = "forbidden"

	// ErrorTypeConflict represents resource conflict errors
	ErrorTypeConflict ErrorType = "conflict"

	// ErrorTypeRateLimit represents rate limiting errors
	ErrorTypeRateLimit ErrorType = "rate_limited"

	// ErrorTypeInternal represents internal server errors
	ErrorTypeInternal ErrorType = "internal_error"

	// ErrorTypeBadRequest represents bad request errors
	ErrorTypeBadRequest ErrorType = "bad_request"
)

// APIError represents an API error
type APIError struct {
	Type    ErrorType              `json:"-"`
	Code    string                 `json:"code"`
	Message string                 `json:"message"`
	Details map[string]interface{} `json:"details,omitempty"`
	Err     error                  `json:"-"`
}

// Error implements the error interface
func (e *APIError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Err.Error())
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap returns the wrapped error
func (e *APIError) Unwrap() error {
	return e.Err
}

// StatusCode returns the HTTP status code for the error
func (e *APIError) StatusCode() int {
	switch e.Type {
	case ErrorTypeValidation:
		return http.StatusUnprocessableEntity
	case ErrorTypeAuth:
		return http.StatusUnauthorized
	case ErrorTypeNotFound:
		return http.StatusNotFound
	case ErrorTypeForbidden:
		return http.StatusForbidden
	case ErrorTypeConflict:
		return http.StatusConflict
	case ErrorTypeRateLimit:
		return http.StatusTooManyRequests
	case ErrorTypeBadRequest:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// NewValidationError creates a new validation error
func NewValidationError(message string, details map[string]interface{}, err error) *APIError {
	return &APIError{
		Type:    ErrorTypeValidation,
		Code:    "validation_error",
		Message: message,
		Details: details,
		Err:     err,
	}
}

// NewAuthenticationError creates a new authentication error
func NewAuthenticationError(message string, err error) *APIError {
	return &APIError{
		Type:    ErrorTypeAuth,
		Code:    "unauthorized",
		Message: message,
		Err:     err,
	}
}

// NewNotFoundError creates a new not found error
func NewNotFoundError(resourceType, resourceID string, err error) *APIError {
	return &APIError{
		Type:    ErrorTypeNotFound,
		Code:    "not_found",
		Message: fmt.Sprintf("%s %s not found", resourceType, resourceID),
		Details: map[string]interface{}{
			"resourceType": resourceType,
			"resourceId":   resourceID,
		},
		Err: err,
	}
}

// NewForbiddenError creates a new forbidden error
func NewForbiddenError(message string, err error) *APIError {
	return &APIError{
		Type:    ErrorTypeForbidden,
		Code:    "forbidden",
		Message: message,
		Err:     err,
	}
}

// NewConflictError creates a new conflict error
func NewConflictError(resourceType, resourceID string, err error) *APIError {
	return &APIError{
		Type:    ErrorTypeConflict,
		Code:    "conflict",
		Message: fmt.Sprintf("%s %s already exists", resourceType, resourceID),
		Details: map[string]interface{}{
			"resourceType": resourceType,
			"resourceId":   resourceID,
		},
		Err: err,
	}
}

// NewRateLimitError creates a new rate limit error
func NewRateLimitError(message string, limit int, reset int64, err error) *APIError {
	return &APIError{
		Type:    ErrorTypeRateLimit,
		Code:    "rate_limited",
		Message: message,
		Details: map[string]interface{}{
			"limit": limit,
			"reset": reset,
		},
		Err: err,
	}
}

// NewInternalError creates a new internal server error
func NewInternalError(message string, err error) *APIError {
	return &APIError{
		Type:    ErrorTypeInternal,
		Code:    "internal_error",
		Message: message,
		Err:     err,
	}
}

// NewBadRequestError creates a new bad request error
func NewBadRequestError(message string, err error) *APIError {
	return &APIError{
		Type:    ErrorTypeBadRequest,
		Code:    "bad_request",
		Message: message,
		Err:     err,
	}
}

// NewNotImplementedError creates a new not implemented error
func NewNotImplementedError(code string, message string, err error) *APIError {
	return &APIError{
		Type:    ErrorTypeInternal,
		Code:    code,
		Message: message,
		Err:     err,
	}
}

// IsWorkspaceNotFoundError checks if the error is a WorkspaceNotFoundError
func IsWorkspaceNotFoundError(err error) bool {
	if apiErr, ok := err.(*APIError); ok {
		return apiErr.Type == ErrorTypeNotFound && apiErr.Details["resourceType"] == "workspace"
	}
	return false
}
