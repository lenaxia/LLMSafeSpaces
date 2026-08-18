// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetEnvDuration_InvalidValue verifies the fallback when the env var holds
// an unparseable duration string (the err != nil branch in getEnvDuration).
func TestGetEnvDuration_InvalidValue(t *testing.T) {
	t.Setenv("BAD_DURATION", "not-a-duration")
	got := getEnvDuration("BAD_DURATION", 42*time.Second)
	assert.Equal(t, 42*time.Second, got,
		"invalid duration env must fall back to the default")
}

// TestGetEnvDuration_ValidValue verifies the happy path.
func TestGetEnvDuration_ValidValue(t *testing.T) {
	t.Setenv("GOOD_DURATION", "7m")
	got := getEnvDuration("GOOD_DURATION", 42*time.Second)
	assert.Equal(t, 7*time.Minute, got)
}

// TestGetEnvDuration_EmptyEnv verifies empty env falls back to default.
func TestGetEnvDuration_EmptyEnv(t *testing.T) {
	t.Setenv("EMPTY_DURATION", "")
	got := getEnvDuration("EMPTY_DURATION", 42*time.Second)
	assert.Equal(t, 42*time.Second, got)
}

// TestGetEnv_Fallback verifies getEnv returns the fallback when env is empty.
func TestGetEnv_Fallback(t *testing.T) {
	assert.Equal(t, "default", getEnv("NONEXISTENT_KEY_12345", "default"))
}

// TestGetEnv_SetValue verifies getEnv returns the env value when set.
func TestGetEnv_SetValue(t *testing.T) {
	t.Setenv("MY_KEY", "from-env")
	assert.Equal(t, "from-env", getEnv("MY_KEY", "default"))
}

// TestDefaultHTTPClient verifies the constructor returns a non-nil client with
// a configured transport. Previously 0% coverage (only called in main()).
func TestDefaultHTTPClient(t *testing.T) {
	c := defaultHTTPClient()
	if assert.NotNil(t, c) {
		assert.NotNil(t, c.Transport, "client must have a transport configured")
	}
}

// TestDefaultHTTPClientHeadTimeout is the #911 regression pin for the
// relay-proxy half of the fix: the upstream transport must bound the
// response-header phase so a stalled upstream returns a bounded error instead
// of hanging the proxy handler with no response and no log. The test asserts
// ResponseHeaderTimeout is configured and that no total body Timeout is set
// (long streaming generations must never be truncated).
func TestDefaultHTTPClientHeadTimeout(t *testing.T) {
	c := defaultHTTPClient()
	tr, ok := c.Transport.(*http.Transport)
	require.True(t, ok, "defaultHTTPClient must use a configured *http.Transport")
	assert.Zero(t, c.Timeout, "no total body timeout — long streams must never be cut")
	require.NotNil(t, tr.ResponseHeaderTimeout, "ResponseHeaderTimeout must be set (issue #911)")
	assert.Positive(t, tr.ResponseHeaderTimeout, "ResponseHeaderTimeout must be non-zero")
}
