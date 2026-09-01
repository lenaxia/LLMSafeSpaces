// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// TestHandleSecretError_Table tests the handleSecretError mapping for
// every migrated sentinel. US-46.4: locks in the status-code + message
// contract end-to-end, including the wrapped-sentinel case (errors.As
// chain traversal) and the validation-detail case (400 uses err.Error()).
func TestHandleSecretError_Table(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
	}{
		{
			name:       "ErrSecretNotFound direct",
			err:        secrets.ErrSecretNotFound,
			wantStatus: http.StatusNotFound,
			wantBody:   "secret not found",
		},
		{
			name:       "ErrSecretNotFound wrapped",
			err:        fmt.Errorf("lookup my-secret: %w", secrets.ErrSecretNotFound),
			wantStatus: http.StatusNotFound,
			wantBody:   "secret not found",
		},
		{
			name:       "ErrWorkspaceNotOwned",
			err:        secrets.ErrWorkspaceNotOwned,
			wantStatus: http.StatusNotFound,
			wantBody:   "workspace not found",
		},
		{
			name:       "ErrDuplicateSecret direct",
			err:        secrets.ErrDuplicateSecret,
			wantStatus: http.StatusConflict,
			wantBody:   "secret with this name already exists",
		},
		{
			name:       "ErrDuplicateSecret wrapped",
			err:        fmt.Errorf("create test-secret: %w", secrets.ErrDuplicateSecret),
			wantStatus: http.StatusConflict,
			wantBody:   "secret with this name already exists",
		},
		{
			name:       "ErrDEKUnavailable",
			err:        secrets.ErrDEKUnavailable,
			wantStatus: http.StatusForbidden,
			wantBody:   "encryption key not available; re-authenticate",
		},
		{
			name:       "ErrUserKeysMissing",
			err:        secrets.ErrUserKeysMissing,
			wantStatus: http.StatusPreconditionFailed,
			wantBody:   "user key material not initialized; please re-login",
		},
		{
			name:       "ErrCiphertextDecryptFailed",
			err:        secrets.ErrCiphertextDecryptFailed,
			wantStatus: http.StatusConflict,
			wantBody:   "this secret cannot be decrypted",
		},
		{
			name:       "ErrInvalidSecretType direct",
			err:        secrets.ErrInvalidSecretType,
			wantStatus: http.StatusBadRequest,
			wantBody:   "invalid secret type",
		},
		{
			name:       "ErrInvalidSecretType wrapped with detail",
			err:        fmt.Errorf("invalid secret type: type 'foo' not in valid set: %w", secrets.ErrInvalidSecretType),
			wantStatus: http.StatusBadRequest,
			wantBody:   "invalid secret type: type 'foo' not in valid set",
		},
		{
			name:       "ErrInvalidMetadata wrapped with detail",
			err:        fmt.Errorf("invalid secret metadata: ssh-key requires metadata with key_type field: %w", secrets.ErrInvalidMetadata),
			wantStatus: http.StatusBadRequest,
			wantBody:   "invalid secret metadata: ssh-key requires metadata with key_type field",
		},
		{
			name:       "ErrInvalidPassword",
			err:        secrets.ErrInvalidPassword,
			wantStatus: http.StatusForbidden,
			wantBody:   "access denied",
		},
		{
			name:       "unknown error → 500",
			err:        fmt.Errorf("unexpected database failure"),
			wantStatus: http.StatusInternalServerError,
			wantBody:   "internal error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("GET", "/", nil)

			handleSecretError(c, tt.err)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}

			var resp map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to unmarshal response body: %v", err)
			}
			if !contains(resp["error"], tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", resp["error"], tt.wantBody)
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || (len(s) > len(substr) && containsSubstring(s, substr)))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
