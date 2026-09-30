// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package tests

import (
	"bytes"
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

func TestLoggingMiddleware_RequestResponse(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Info", "Request received", mock.Anything).Once()
	mockLogger.On("Info", "Request completed", mock.Anything).Once()

	config := middleware.LoggingConfig{
		LogRequestBody:  true,
		LogResponseBody: true,
		MaxBodyLogSize:  1024,
	}

	router := gin.New()
	router.Use(middleware.LoggingMiddleware(mockLogger, config))
	router.POST("/test", func(c *gin.Context) {
		var data map[string]interface{}
		if err := c.ShouldBindJSON(&data); err == nil {
			c.JSON(http.StatusOK, gin.H{"message": "success", "data": data})
		}
	})

	// Execute
	w := httptest.NewRecorder()
	reqBody := `{"name": "test", "value": 123}`
	req, _ := http.NewRequest("POST", "/test", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "success")

	mockLogger.AssertExpectations(t)
}

func TestLoggingMiddleware_SensitiveDataRedaction(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()

	// Capture the log fields for inspection
	var requestFields []interface{}
	var responseFields []interface{}

	mockLogger.On("Info", "Request received", mock.Anything).Run(func(args mock.Arguments) {
		// Get the variadic arguments
		requestFields = args.Get(1).([]interface{})
		t.Logf("Request fields captured: %+v", requestFields)
	}).Once()

	mockLogger.On("Info", "Request completed", mock.Anything).Run(func(args mock.Arguments) {
		// Get the variadic arguments
		responseFields = args.Get(1).([]interface{})
		t.Logf("Response fields captured: %+v", responseFields)
	}).Once()

	config := middleware.LoggingConfig{
		LogRequestBody:  true,
		LogResponseBody: true,
		SensitiveFields: []string{"password", "token", "email", "api_key", "credit_card"},
		MaxBodyLogSize:  4096, // Ensure bodies aren't truncated
	}

	router := gin.New()
	router.Use(middleware.LoggingMiddleware(mockLogger, config))
	router.POST("/login", func(c *gin.Context) {
		var data map[string]interface{}
		if err := c.ShouldBindJSON(&data); err == nil {
			c.JSON(http.StatusOK, gin.H{
				"message": "logged in",
				"token":   "test_eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ",
				"user":    data["username"],
				"email":   data["email"],
				"api_key": "test_api_key",
			})
		}
	})

	// Execute
	w := httptest.NewRecorder()
	reqBody := `{
		"username": "testuser", 
		"password": "secret123", 
		"email": "user@example.com", 
		"credit_card": "4242-4242-4242-4242",
		"api_key": "pk_test_51NXxbTLxmNAjIcThJV9PmvWR9ybXlPfVzBkgJqhcRnWM5ujZEAiLwwrgvgUgtGgQXqnPwGKpK1R"
	}`
	req, _ := http.NewRequest("POST", "/login", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusOK, w.Code)

	// Find request body in log fields
	var requestBody map[string]interface{}
	for i := 0; i < len(requestFields); i += 2 {
		t.Logf("Request field %d: %v = %v", i/2, requestFields[i], requestFields[i+1])
		if requestFields[i] == "request_body" {
			t.Logf("Found request_body at index %d", i)
			if body, ok := requestFields[i+1].(map[string]interface{}); ok {
				requestBody = body
				t.Logf("Successfully cast request_body to map: %+v", requestBody)
			} else {
				t.Logf("Failed to cast request_body to map, type: %T, value: %v", requestFields[i+1], requestFields[i+1])
			}
		}
	}

	// Find response body in log fields
	var responseBody map[string]interface{}
	for i := 0; i < len(responseFields); i += 2 {
		t.Logf("Response field %d: %v = %v", i/2, responseFields[i], responseFields[i+1])
		if responseFields[i] == "response_body" {
			t.Logf("Found response_body at index %d", i)
			if body, ok := responseFields[i+1].(map[string]interface{}); ok {
				responseBody = body
				t.Logf("Successfully cast response_body to map: %+v", responseBody)
			} else {
				t.Logf("Failed to cast response_body to map, type: %T, value: %v", responseFields[i+1], responseFields[i+1])
			}
		}
	}

	// Check that sensitive fields are masked
	assert.NotNil(t, requestBody, "Request body should not be nil")
	if requestBody != nil {
		assert.NotEqual(t, "secret123", requestBody["password"], "Password should be masked")
		assert.Contains(t, requestBody["password"].(string), "...", "Password should use MaskString format")
		assert.Equal(t, "testuser", requestBody["username"], "Username should be preserved")
		assert.NotEqual(t, "user@example.com", requestBody["email"], "Email should be masked")
		assert.Contains(t, requestBody["email"].(string), "...", "Email should use MaskString format")
		assert.NotEqual(t, "4242-4242-4242-4242", requestBody["credit_card"], "Credit card should be masked")
		assert.Contains(t, requestBody["credit_card"].(string), "...", "Credit card should use MaskString format")
		assert.NotEqual(t, "pk_test_51NXxbTLxmNAjIcThJV9PmvWR9ybXlPfVzBkgJqhcRnWM5ujZEAiLwwrgvgUgtGgQXqnPwGKpK1R", requestBody["api_key"], "API key should be masked")
		assert.Contains(t, requestBody["api_key"].(string), "...", "API key should use MaskString format")
	}

	assert.NotNil(t, responseBody, "Response body should not be nil")
	if responseBody != nil {
		assert.NotEqual(t, "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ", responseBody["token"], "Token should be masked")
		assert.Contains(t, responseBody["token"].(string), "...", "Token should use MaskString format")
		assert.Equal(t, "logged in", responseBody["message"], "Message should be preserved")
		assert.NotEqual(t, "user@example.com", responseBody["email"], "Email should be masked")
		assert.Contains(t, responseBody["email"].(string), "...", "Email should use MaskString format")
		assert.NotEqual(t, "test_api_key", responseBody["api_key"], "API key should be masked")
		assert.Contains(t, responseBody["api_key"].(string), "...", "API key should use MaskString format")
	}

	mockLogger.AssertExpectations(t)
}

func TestLoggingMiddleware_BodySizeTruncation(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()

	// Capture the log fields for inspection
	var requestFields []interface{}

	mockLogger.On("Info", "Request received", mock.Anything).Run(func(args mock.Arguments) {
		requestFields = args.Get(1).([]interface{})
	}).Once()

	mockLogger.On("Info", "Request completed", mock.Anything).Once()

	config := middleware.LoggingConfig{
		LogRequestBody: true,
		MaxBodyLogSize: 20, // Very small to force truncation
	}

	router := gin.New()
	router.Use(middleware.LoggingMiddleware(mockLogger, config))
	router.POST("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	// Execute with large body
	w := httptest.NewRecorder()
	largeBody := strings.Repeat("abcdefghij", 10) // 100 characters
	req, _ := http.NewRequest("POST", "/test", bytes.NewBufferString(largeBody))
	router.ServeHTTP(w, req)

	// Assert
	assert.Equal(t, http.StatusOK, w.Code)

	// Find request body in log fields
	var requestBodyStr string
	var requestBodySize int
	for i := 0; i < len(requestFields); i += 2 {
		if requestFields[i] == "request_body" {
			if body, ok := requestFields[i+1].(string); ok {
				requestBodyStr = body
			}
		}
		if requestFields[i] == "request_body_size" {
			if size, ok := requestFields[i+1].(int); ok {
				requestBodySize = size
			}
		}
	}

	// Check that body was truncated
	assert.Contains(t, requestBodyStr, "... (truncated)")
	assert.Equal(t, 100, requestBodySize)
	assert.True(t, len(requestBodyStr) < 100)

	mockLogger.AssertExpectations(t)
}

// TestLoggingMiddleware_RequestIDFromContext verifies that LoggingMiddleware reads the
// request_id from the Gin context (set by TracingMiddleware / RequestIDMiddleware) rather
// than generating its own. This is the contract that eliminates the dual-ID problem.
func TestLoggingMiddleware_RequestIDFromContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()

	const injectedID = "550e8400-e29b-41d4-a716-446655440000"

	var requestFields, responseFields []interface{}
	mockLogger.On("Info", "Request received", mock.Anything).Run(func(args mock.Arguments) {
		requestFields = args.Get(1).([]interface{})
	}).Once()
	mockLogger.On("Info", "Request completed", mock.Anything).Run(func(args mock.Arguments) {
		responseFields = args.Get(1).([]interface{})
	}).Once()

	router := gin.New()
	// Simulate TracingMiddleware setting request_id before LoggingMiddleware runs.
	router.Use(func(c *gin.Context) {
		c.Set("request_id", injectedID)
		c.Next()
	})
	router.Use(middleware.LoggingMiddleware(mockLogger, middleware.LoggingConfig{}))
	router.GET("/test", func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// Both request and response log lines must carry the injected UUID, not a
	// random 8-char string.
	findField := func(fields []interface{}, key string) interface{} {
		for i := 0; i+1 < len(fields); i += 2 {
			if fields[i] == key {
				return fields[i+1]
			}
		}
		return nil
	}

	assert.Equal(t, injectedID, findField(requestFields, "request_id"),
		"request log must carry the context request_id, not a generated one")
	assert.Equal(t, injectedID, findField(responseFields, "request_id"),
		"response log must carry the same context request_id")

	mockLogger.AssertExpectations(t)
}

// TestLoggingMiddleware_UserIDInResponseLog verifies that user_id is included in the
// response log line when the auth middleware has set it in the Gin context.
func TestLoggingMiddleware_UserIDInResponseLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()

	const testUserID = "user-abc-123"

	var responseFields []interface{}
	mockLogger.On("Info", "Request received", mock.Anything).Once()
	mockLogger.On("Info", "Request completed", mock.Anything).Run(func(args mock.Arguments) {
		responseFields = args.Get(1).([]interface{})
	}).Once()

	router := gin.New()
	router.Use(middleware.LoggingMiddleware(mockLogger, middleware.LoggingConfig{}))
	router.GET("/test", func(c *gin.Context) {
		// Simulate auth middleware having set userID before handler runs.
		c.Set("userID", testUserID)
		c.String(http.StatusOK, "ok")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	findField := func(fields []interface{}, key string) interface{} {
		for i := 0; i+1 < len(fields); i += 2 {
			if fields[i] == key {
				return fields[i+1]
			}
		}
		return nil
	}

	assert.Equal(t, testUserID, findField(responseFields, "user_id"),
		"response log must include user_id when set by auth middleware")

	mockLogger.AssertExpectations(t)
}

// TestLoggingMiddleware_NoUserIDWhenUnauthenticated verifies that user_id is absent
// from the response log when the request is unauthenticated.
func TestLoggingMiddleware_NoUserIDWhenUnauthenticated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()

	var responseFields []interface{}
	mockLogger.On("Info", "Request received", mock.Anything).Once()
	mockLogger.On("Info", "Request completed", mock.Anything).Run(func(args mock.Arguments) {
		responseFields = args.Get(1).([]interface{})
	}).Once()

	router := gin.New()
	router.Use(middleware.LoggingMiddleware(mockLogger, middleware.LoggingConfig{}))
	router.GET("/test", func(c *gin.Context) {
		// No userID set — unauthenticated request.
		c.String(http.StatusOK, "ok")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	for i := 0; i+1 < len(responseFields); i += 2 {
		assert.NotEqual(t, "user_id", responseFields[i],
			"user_id must not appear in response log for unauthenticated requests")
	}

	mockLogger.AssertExpectations(t)
}

func TestLoggingMiddleware_SkipPaths(t *testing.T) {
	// Setup
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	// No log calls expected for skipped paths

	config := middleware.LoggingConfig{
		SkipPaths: []string{"/health", "/metrics"},
	}

	router := gin.New()
	router.Use(middleware.LoggingMiddleware(mockLogger, config))
	router.GET("/health", func(c *gin.Context) {
		c.String(http.StatusOK, "healthy")
	})
	router.GET("/api", func(c *gin.Context) {
		c.String(http.StatusOK, "api")
	})

	// Execute request to skipped path
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/health", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	// Execute request to non-skipped path
	mockLogger.On("Info", "Request received", mock.Anything).Once()
	mockLogger.On("Info", "Request completed", mock.Anything).Once()

	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	mockLogger.AssertExpectations(t)
}

// --- G25: secret value field redaction in logs ---

// TestLoggingMiddleware_G25_SecretsPathBodyNotLogged is the G25 core
// regression: requests to /api/v1/secrets/* carry the plaintext secret
// in the "value" field of the JSON body. The middleware MUST skip
// logging entirely for this path (no Info call) so the body never
// reaches the log pipeline.
//
// Pre-fix: the body was logged verbatim with only SensitiveFields
// masking applied (which didn't include "value"). A request to create
// a secret logged the plaintext API key in the application log —
// visible to anyone with log access (operators, SRE, log aggregator).
//
// Defense in depth: even if the prefix-skip is bypassed (e.g. a new
// secrets endpoint added without updating SkipPathPrefixes), the
// SensitiveFields list now includes "value" so the field is masked.
// Both layers are tested.
func TestLoggingMiddleware_G25_SecretsPathBodyNotLogged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	// No Info calls expected at all — the path is in SkipPathPrefixes
	// so the middleware short-circuits before logging.

	// DefaultLoggingConfig is what production uses — we want to verify
	// the default behavior, not a hand-tuned test config.
	config := middleware.DefaultLoggingConfig()

	router := gin.New()
	router.Use(middleware.LoggingMiddleware(mockLogger, config))
	router.POST("/api/v1/secrets", func(c *gin.Context) {
		c.JSON(http.StatusCreated, gin.H{"id": "sec-1"})
	})

	w := httptest.NewRecorder()
	// Body contains a real-looking API key in the "value" field — the
	// exact shape that was leaking pre-fix.
	reqBody := `{"name":"my-openai-key","type":"llm-provider","value":"sk-proj-abc123_REAL_LOOKING_KEY_for_testing"}`
	req, _ := http.NewRequest("POST", "/api/v1/secrets", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusCreated, w.Code, "handler should still run")
	// Critical assertion: NO log call was made — the prefix-skip is
	// the primary G25 control. If this fails, either the prefix list
	// was edited or the prefix-matching logic regressed.
	mockLogger.AssertNotCalled(t, "Info", mock.Anything, mock.Anything)
}

// TestLoggingMiddleware_G25_SkipPathPrefixes_MatchesNestedPaths
// confirms that SkipPathPrefixes uses prefix matching (not exact),
// so /api/v1/secrets/ as a prefix catches /api/v1/secrets/:id/reveal
// and similar nested paths. Without prefix matching, an exhaustive
// list of every secrets sub-path would be required.
func TestLoggingMiddleware_G25_SkipPathPrefixes_MatchesNestedPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	// No log calls expected at all for the prefix-skipped path.

	config := middleware.LoggingConfig{
		SkipPathPrefixes: []string{"/api/v1/secrets/"},
	}

	router := gin.New()
	router.Use(middleware.LoggingMiddleware(mockLogger, config))
	router.POST("/api/v1/secrets/sec-abc/reveal", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"decrypted": "would-be-secret"})
	})

	w := httptest.NewRecorder()
	reqBody := `{"password":"user-password"}`
	req, _ := http.NewRequest("POST", "/api/v1/secrets/sec-abc/reveal", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	// If the middleware logged anything, the mock's strict expectations
	// would fail. AssertExpectations verifies that no Info calls were
	// made on the logger for this request.
	mockLogger.AssertNotCalled(t, "Info", mock.Anything, mock.Anything)
}

// TestLoggingMiddleware_G25_SkipPathPrefixes_DoesNotMatchUnrelatedPaths
// confirms that the prefix-skip is specific — requests to OTHER paths
// still get logged normally. Without this assertion, a typo in the
// prefix (e.g. "/api/") would silently disable logging everywhere.
func TestLoggingMiddleware_G25_SkipPathPrefixes_DoesNotMatchUnrelatedPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Info", "Request received", mock.Anything).Once()
	mockLogger.On("Info", "Request completed", mock.Anything).Once()

	config := middleware.LoggingConfig{
		SkipPathPrefixes: []string{"/api/v1/secrets/"},
	}

	router := gin.New()
	router.Use(middleware.LoggingMiddleware(mockLogger, config))
	router.GET("/api/v1/workspaces", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"workspaces": []string{}})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/workspaces", nil)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	mockLogger.AssertExpectations(t)
}

// TestLoggingMiddleware_G25_ValueFieldInSensitiveFields confirms that
// the default SensitiveFields list now includes "value" — defense in
// depth for paths that are NOT in SkipPathPrefixes but happen to carry
// sensitive values in a "value" field (e.g. a future endpoint added
// without updating the skip list). The "value" field of any logged
// JSON body is masked.
func TestLoggingMiddleware_G25_ValueFieldInSensitiveFields(t *testing.T) {
	config := middleware.DefaultLoggingConfig()

	// "value" must be in the default SensitiveFields list.
	found := false
	for _, f := range config.SensitiveFields {
		if f == "value" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("G25: DefaultLoggingConfig.SensitiveFields must include \"value\"; got %v", config.SensitiveFields)
	}

	// SkipPathPrefixes must include the secrets path.
	found = false
	for _, p := range config.SkipPathPrefixes {
		if p == "/api/v1/secrets/" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("G25: DefaultLoggingConfig.SkipPathPrefixes must include \"/api/v1/secrets/\"; got %v", config.SkipPathPrefixes)
	}
}

// TestLoggingMiddleware_G25_ValueFieldMaskedOnUnlistedPath confirms the
// defense-in-depth layer: when a request carries a "value" field on a
// path that is NOT in SkipPathPrefixes, the SensitiveFields masking
// kicks in and replaces the value with the mask. This catches the case
// where a future endpoint accepts sensitive values in a "value" field
// without updating the skip list.
func TestLoggingMiddleware_G25_ValueFieldMaskedOnUnlistedPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()

	var requestFields []interface{}
	mockLogger.On("Info", "Request received", mock.Anything).Run(func(args mock.Arguments) {
		requestFields = args.Get(1).([]interface{})
	}).Once()
	mockLogger.On("Info", "Request completed", mock.Anything).Once()

	// Use DefaultLoggingConfig — production settings.
	config := middleware.DefaultLoggingConfig()

	router := gin.New()
	router.Use(middleware.LoggingMiddleware(mockLogger, config))
	// /api/v1/users/me/settings is NOT in SkipPathPrefixes — the body
	// WILL be logged, but the "value" field must be masked.
	router.PUT("/api/v1/users/me/settings/workspace.defaultStorageSize", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	// Body uses a real-looking secret in "value" to verify masking.
	reqBody := `{"value":"sk-proj-SECRET_FOR_TESTING"}`
	req, _ := http.NewRequest("PUT", "/api/v1/users/me/settings/workspace.defaultStorageSize", bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	// Stringify captured fields and assert the secret is NOT present.
	var logOutput string
	for i := 0; i+1 < len(requestFields); i += 2 {
		k, _ := requestFields[i].(string)
		logOutput += k + "="
		switch v := requestFields[i+1].(type) {
		case string:
			logOutput += v + " "
		default:
			logOutput += fmt.Sprintf("%v ", v)
		}
	}
	t.Logf("captured log fields: %s", logOutput)

	assert.NotContains(t, logOutput, "sk-proj-SECRET_FOR_TESTING",
		"G25 defense-in-depth: 'value' field must be masked even on paths not in SkipPathPrefixes")
	// MaskString (pkg/utilities/masking.go:35) shows the first/last few
	// chars with "..." in between for strings > 8 chars. The full secret
	// is unrecoverable from the masked form. We assert the mask marker
	// is present rather than a specific mask shape so the test doesn't
	// break if MaskString's format changes.
	assert.Contains(t, logOutput, "...",
		"masked 'value' field should contain MaskString's '...' marker")
}

// chunkedBody serves a fixed byte budget and exposes when its LAST byte
// has been served — the observable for the streaming-contract tests: a
// pre-handler full-body buffer can only run the handler after
// lastServed is closed; a bounded capture runs it while bytes remain.
type chunkedBody struct {
	data       []byte
	offset     int
	lastServed chan struct{}
}

func (r *chunkedBody) Read(p []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	if r.offset >= len(r.data) {
		close(r.lastServed)
	}
	return n, nil
}

// TestLoggingMiddleware_SlowBodyStreamsToHandler pins run 36749714653's
// root cause: the logging middleware's pre-handler body read must be
// BOUNDED, so a client-paced body (the SR-6B trickled holders, 1MiB at
// 64k/s) streams through to the handler while it is still arriving.
// The unbounded io.ReadAll the fix replaced swallowed the whole
// transfer before the upload handler — and thus agentd's admission —
// ever started, holding nothing (nightlies 36135708380..36740521434).
func TestLoggingMiddleware_SlowBodyStreamsToHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()
	mockLogger.On("Info", "Request received", mock.Anything).Once()
	mockLogger.On("Info", "Request completed", mock.Anything).Once()

	config := middleware.LoggingConfig{LogRequestBody: true, MaxBodyLogSize: 1024}

	const bodySize = 64 * 1024
	body := &chunkedBody{
		data:       bytes.Repeat([]byte{0xA5}, bodySize),
		lastServed: make(chan struct{}),
	}

	handlerRanAfterFullBody := false
	var handlerReadTotal int64

	router := gin.New()
	router.Use(middleware.LoggingMiddleware(mockLogger, config))
	router.POST("/upload", func(c *gin.Context) {
		buf := make([]byte, 1)
		if _, err := c.Request.Body.Read(buf); err != nil {
			t.Errorf("handler first read failed: %v", err)
		}
		select {
		case <-body.lastServed:
			handlerRanAfterFullBody = true
		default:
		}
		n, _ := io.Copy(io.Discard, c.Request.Body)
		handlerReadTotal = int64(n) + 1
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/upload", body)
	req.ContentLength = int64(bodySize)
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, handlerRanAfterFullBody,
		"the handler must start while the body is still arriving — a pre-handler full-body buffer swallows client-paced uploads (design 0060's never-buffered upload contract; nightly SR-6B seat, run 36749714653)")
	assert.Equal(t, int64(bodySize), handlerReadTotal,
		"the handler must observe the COMPLETE body (capture replays, remainder streams)")
	mockLogger.AssertExpectations(t)
}

// TestLoggingMiddleware_OversizedBodyCaptureNotLogged pins the logging
// fidelity contract of the bounded capture: a body larger than the
// capture limit logs its declared size but NEVER body bytes — the
// truncated-string branch would bypass JSON field masking.
func TestLoggingMiddleware_OversizedBodyCaptureNotLogged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mockLogger := logmock.NewMockLogger()

	var requestFields []interface{}
	mockLogger.On("Info", "Request received", mock.Anything).Run(func(args mock.Arguments) {
		requestFields = args.Get(1).([]interface{})
	}).Once()
	mockLogger.On("Info", "Request completed", mock.Anything).Once()

	config := middleware.LoggingConfig{LogRequestBody: true, MaxBodyLogSize: 1024}

	var handlerReadTotal int64
	router := gin.New()
	router.Use(middleware.LoggingMiddleware(mockLogger, config))
	router.POST("/upload", func(c *gin.Context) {
		n, _ := io.Copy(io.Discard, c.Request.Body)
		handlerReadTotal = n
		c.String(http.StatusOK, "ok")
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/upload", bytes.NewReader(bytes.Repeat([]byte{0x5A}, 8192)))
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, int64(8192), handlerReadTotal, "the handler must see the full oversized body")

	var hasRequestBody, hasSize bool
	var sizeValue int
	for i := 0; i+1 < len(requestFields); i += 2 {
		switch requestFields[i] {
		case "request_body":
			hasRequestBody = true
		case "request_body_size":
			hasSize = true
			if v, ok := requestFields[i+1].(int); ok {
				sizeValue = v
			}
		}
	}
	assert.False(t, hasRequestBody,
		"an oversized capture must not log body bytes — the truncated-string branch bypasses JSON masking")
	assert.True(t, hasSize, "the declared size must still be logged")
	assert.Equal(t, 8192, sizeValue, "request_body_size must carry the true declared size")
	mockLogger.AssertExpectations(t)
}
