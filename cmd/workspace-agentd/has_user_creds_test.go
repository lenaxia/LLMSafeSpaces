// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	secretpkg "github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

// TestHasUserCreds_ReturnsFalseWhenCacheAbsent covers the fresh-pod
// state: the last-reload-secrets.json cache doesn't exist yet, so
// agentd has never received a live push and therefore has no user-DEK
// content materialized. Must report FALSE.
//
// This is the load-bearing signal for the API's watcher-driven
// auto-push: on a fresh pod boot, the API needs to know "agentd has
// no user creds; push them now."
func TestHasUserCreds_ReturnsFalseWhenCacheAbsent(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "does-not-exist.json")

	got := hasUserCreds(cachePath)
	assert.False(t, got,
		"absent cache MUST report false — signals to the API that a "+
			"push is needed to deliver user-DEK content")
}

// TestHasUserCreds_ReturnsFalseWhenCacheEmpty covers the case where
// the cache file exists but contains an empty batch (the last push
// was a user unbinding all their secrets). Agentd has no user-DEK
// content; API's push would also be empty, so no action is needed.
// FALSE is correct.
func TestHasUserCreds_ReturnsFalseWhenCacheEmpty(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "last-reload-secrets.json")
	require.NoError(t, os.WriteFile(cachePath, []byte("[]"), 0o600))

	got := hasUserCreds(cachePath)
	assert.False(t, got,
		"empty-batch cache MUST report false — no user-DEK entries "+
			"are materialized, and a push carrying [] would be a no-op")
}

// TestHasUserCreds_ReturnsTrueWhenCacheHasUserDEKEntries covers the
// steady-state case: a prior push delivered user-DEK content, cache
// was persisted. Any subsequent healthz query MUST see the true
// signal so the API's watcher doesn't spuriously re-push.
func TestHasUserCreds_ReturnsTrueWhenCacheHasUserDEKEntries(t *testing.T) {
	cases := []struct {
		name string
		typ  string
	}{
		{"env-secret", "env-secret"},
		{"ssh-key", "ssh-key"},
		{"secret-file", "secret-file"},
		{"git-credential", "git-credential"},
		{"llm-provider", "llm-provider"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cachePath := filepath.Join(dir, "last-reload-secrets.json")
			batch := []secretpkg.Secret{
				{Type: tc.typ, Name: "test", Plaintext: "value"},
			}
			data, err := json.Marshal(batch)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(cachePath, data, 0o600))

			got := hasUserCreds(cachePath)
			assert.True(t, got,
				"cache containing %s entry MUST report true — user-DEK "+
					"content is present on disk", tc.typ)
		})
	}
}

// TestHasUserCreds_ReturnsFalseOnCorruptCache pins the safe-default
// behavior for corrupt cache: agentd cannot know what's on disk if
// it can't parse the cache, so it MUST report FALSE. The API's push
// then re-materializes and rewrites the cache cleanly — better than
// FALSE→push→re-cache than TRUE→skip-push→user sees empty secrets.
func TestHasUserCreds_ReturnsFalseOnCorruptCache(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "last-reload-secrets.json")
	require.NoError(t, os.WriteFile(cachePath, []byte("this is not JSON"), 0o600))

	got := hasUserCreds(cachePath)
	assert.False(t, got,
		"corrupt cache MUST fail-safe to false — the API's push "+
			"will re-materialize AND re-cache; treating corrupt as "+
			"true would suppress the recovery push")
}

// TestHealthzHandler_ReportsUserCredsPresent is the wire-shape test:
// the /v1/healthz response body MUST include the userCredsPresent
// field and it must reflect hasUserCreds' return value. This locks
// in the API/controller consumers' contract.
func TestHealthzHandler_ReportsUserCredsPresent(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "last-reload-secrets.json")

	// Case 1: cache absent → healthz reports userCredsPresent=false.
	handler := healthzHandler(time.Now(), cachePath)
	req := httptest.NewRequest("GET", "/v1/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var resp agentd.HealthzResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.False(t, resp.UserCredsPresent,
		"absent cache MUST surface userCredsPresent=false on the wire")

	// Case 2: cache with an env-secret → healthz reports true.
	batch := []secretpkg.Secret{{Type: "env-secret", Name: "TOKEN", Plaintext: "v"}}
	data, err := json.Marshal(batch)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cachePath, data, 0o600))

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.UserCredsPresent,
		"cache-with-user-DEK MUST surface userCredsPresent=true on the wire")
}

// TestHealthzHandler_UserCredsPresentDoesNotBlockLiveness proves that
// even if hasUserCreds errors internally (e.g. permission denied on
// the cache file), the handler still returns HTTP 200 with
// Healthy=true. The user-creds signal is observability data, NOT a
// liveness gate — a hasUserCreds failure MUST NOT cause kubelet to
// kill the pod.
func TestHealthzHandler_UserCredsPresentDoesNotBlockLiveness(t *testing.T) {
	// Point at a path we can't read.
	handler := healthzHandler(time.Now(), "/proc/1/root/etc/shadow-nonexistent")

	req := httptest.NewRequest("GET", "/v1/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code,
		"healthz MUST remain 200 regardless of hasUserCreds errors — "+
			"the field is observability, not liveness")
	var resp agentd.HealthzResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Healthy)
	assert.False(t, resp.UserCredsPresent, "error case fails safe to false")
}
