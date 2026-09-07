// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// bootstrap_retry_test.go — pins the #1300 first-boot fetch retry:
// a transient API outage at pod start must not degrade the batch to
// empty when a retry succeeds, while a persistent outage keeps the
// degrade (never-block-boot holds).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunBootstrapCommand_TransientFailureRetried(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "secrets.json")
	tokenPath := writeBootstrapToken(t, dir)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// First attempt: the observed production failure mode —
			// connection-level error behavior approximated by a hard
			// 503; the retry loop must absorb it and succeed on the
			// second attempt.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(bootstrapResponse{
			Secrets: json.RawMessage(`[{"name":"k","type":"env-secret","plaintext":"v"}]`),
		})
	}))
	defer srv.Close()

	code := runBootstrap([]string{
		"--workspace-id", "ws-retry",
		"--api-url", srv.URL,
		"--token-file", tokenPath,
		"--out", outPath,
	})
	require.Equal(t, 0, code)

	data, err := os.ReadFile(outPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "env-secret", "retry must land the real batch, not the empty degrade")
	assert.Equal(t, int32(2), calls.Load(), "exactly one retry expected")
}

func TestRunBootstrapCommand_PersistentFailureDegradesAfterRetries(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "secrets.json")
	tokenPath := writeBootstrapToken(t, dir)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	start := time.Now()
	code := runBootstrap([]string{
		"--workspace-id", "ws-hard",
		"--api-url", srv.URL,
		"--token-file", tokenPath,
		"--out", outPath,
	})
	require.Equal(t, 0, code, "never-block-boot must hold")

	data, _ := os.ReadFile(outPath)
	assert.Equal(t, "[]", string(data), "persistent outage still degrades to empty")
	assert.Equal(t, int32(bootstrapFetchAttempts), calls.Load(), "bounded attempts only")
	// Worst-case added latency: attempts-1 backoffs of 2s,4s.
	maxExtra := time.Duration(0)
	for i := 1; i < bootstrapFetchAttempts; i++ {
		maxExtra += bootstrapFetchRetryBackoff * time.Duration(i)
	}
	assert.LessOrEqual(t, time.Since(start), maxExtra+5*time.Second, "retry budget must stay bounded")
}

func TestRunBootstrapCommand_UnauthorizedNotRetried(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "secrets.json")
	tokenPath := writeBootstrapToken(t, dir)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	code := runBootstrap([]string{
		"--workspace-id", "ws-401",
		"--api-url", srv.URL,
		"--token-file", tokenPath,
		"--out", outPath,
	})
	require.Equal(t, 0, code)

	assert.Equal(t, int32(1), calls.Load(), "401 must not be retried")
	data, _ := os.ReadFile(outPath)
	assert.Equal(t, "[]", string(data))
}

func TestRunBootstrapCommand_LastGoodBatchSkipsRetry(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "secrets.json")
	tokenPath := writeBootstrapToken(t, dir)

	// A prior batch on disk is the last-good state; a failing pull must
	// keep it after exactly ONE attempt (the resync path is the heal —
	// retrying a resumable boot only delays it).
	if err := os.WriteFile(outPath, []byte(`{"entries":[{"name":"k"}],"revision":{"seq":7,"manifestHash":"h"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	code := runBootstrap([]string{
		"--workspace-id", "ws-lastgood",
		"--api-url", srv.URL,
		"--token-file", tokenPath,
		"--out", outPath,
	})
	require.Equal(t, 0, code)

	assert.Equal(t, int32(1), calls.Load(), "last-good batch must not be retried")
	data, err := os.ReadFile(outPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"manifestHash":"h"`, "last-good batch preserved byte-for-byte")
}
