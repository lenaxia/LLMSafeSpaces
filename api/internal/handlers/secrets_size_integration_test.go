// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// W8 bind-time size validation, HTTP integration: the per-secret value
// cap must surface as a 400 carrying the machine-readable size_exceeded
// reason on BOTH create and update — the author gets told at bind time,
// not as a spawn-time degrade.

func oversizedValue(n int) string { return strings.Repeat("x", n) }

func doSecretsJSON(t *testing.T, router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestHandler_CreateSecret_SizeExceeded_400(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	body, err := json.Marshal(map[string]any{
		"name":     "oversized-secret-file",
		"type":     secrets.SecretTypeSecretFile,
		"value":    oversizedValue(secrets.MaxSecretValueBytes + 1),
		"metadata": map[string]string{"mount_path": "big.bin"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec := doSecretsJSON(t, router, http.MethodPost, "/api/v1/secrets", string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create over-cap: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "size_exceeded") {
		t.Fatalf("create over-cap must carry the machine-readable reason, got: %s", rec.Body.String())
	}
}

func TestHandler_CreateSecret_AtCapValue_201(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	body, err := json.Marshal(map[string]any{
		"name":     "at-cap-secret-file",
		"type":     secrets.SecretTypeSecretFile,
		"value":    oversizedValue(secrets.MaxSecretValueBytes),
		"metadata": map[string]string{"mount_path": "cap.bin"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec := doSecretsJSON(t, router, http.MethodPost, "/api/v1/secrets", string(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create at-cap: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), oversizedValue(64)) {
		t.Fatal("response must never echo the value")
	}
}

func TestHandler_UpdateSecret_SizeExceeded_400(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	create, err := json.Marshal(map[string]any{
		"name":     "grow-me",
		"type":     secrets.SecretTypeSecretFile,
		"value":    "small",
		"metadata": map[string]string{"mount_path": "grow.bin"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := doSecretsJSON(t, router, http.MethodPost, "/api/v1/secrets", string(create))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created secrets.SecretResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create: %v", err)
	}

	update, err := json.Marshal(map[string]any{
		"value": oversizedValue(secrets.MaxSecretValueBytes + 1),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec = doSecretsJSON(t, router, http.MethodPut, "/api/v1/secrets/"+created.ID, string(update))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update over-cap: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "size_exceeded") {
		t.Fatalf("update over-cap must carry the machine-readable reason, got: %s", rec.Body.String())
	}
}

func TestHandler_CreateSecret_SizeExceeded_ValueTypeSweep(t *testing.T) {
	router, _, _ := setupTestRouter(t)

	cases := []struct {
		secretType secrets.SecretType
		metadata   map[string]string
	}{
		{secrets.SecretTypeEnvSecret, map[string]string{"var_name": "BIG_VAR"}},
		{secrets.SecretTypeGitCredential, nil},
		{secrets.SecretTypeAPIKey, map[string]string{"kind": "openai", "slug": "openai"}},
	}
	for _, tc := range cases {
		t.Run(string(tc.secretType), func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"name":     fmt.Sprintf("big-%s", tc.secretType),
				"type":     tc.secretType,
				"value":    oversizedValue(secrets.MaxSecretValueBytes + 1),
				"metadata": tc.metadata,
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			rec := doSecretsJSON(t, router, http.MethodPost, "/api/v1/secrets", string(body))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "size_exceeded") {
				t.Fatalf("%s over-cap: expected 400 + size_exceeded, got %d: %s",
					tc.secretType, rec.Code, rec.Body.String())
			}
		})
	}
}
