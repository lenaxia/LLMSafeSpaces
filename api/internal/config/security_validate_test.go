// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CORS hardening: the combination AllowedOrigins=["*"] + AllowCredentials=true
// is forbidden by the CORS spec (RFC 6454 + Fetch §3.2.1) because it lets any
// website read authenticated responses from this API in a victim's browser.
// Browsers reject the combo anyway, but relying on the browser to enforce it
// is not a security posture. The API must refuse to start in that state —
// fail-closed at boot, not "logged-but-broken at runtime".
//
// This mirrors the existing Turnstile fail-closed guard (applyTurnstileEnv).

func TestValidateSecurity_RejectsWildcardWithCredentials(t *testing.T) {
	c := &Config{}
	c.Security.AllowedOrigins = []string{"*"}
	c.Security.AllowCredentials = true

	err := validateSecurity(c)
	require.Error(t, err)
	assert.True(t,
		strings.Contains(strings.ToLower(err.Error()), "credential") ||
			strings.Contains(strings.ToLower(err.Error()), "wildcard"),
		"error must explain the unsafe combo, got: %v", err)
}

func TestValidateSecurity_AllowsWildcardWithoutCredentials(t *testing.T) {
	c := &Config{}
	c.Security.AllowedOrigins = []string{"*"}
	c.Security.AllowCredentials = false

	assert.NoError(t, validateSecurity(c),
		"wildcard without credentials is fine — credentials are the dangerous half")
}

func TestValidateSecurity_AllowsExplicitOriginsWithCredentials(t *testing.T) {
	c := &Config{}
	c.Security.AllowedOrigins = []string{"https://app.example.com"}
	c.Security.AllowCredentials = true

	assert.NoError(t, validateSecurity(c),
		"explicit origins with credentials is the normal authenticated-deploy shape")
}

func TestValidateSecurity_AllowsEmptyConfig(t *testing.T) {
	c := &Config{}
	// No origins, no credentials — the chart default.
	assert.NoError(t, validateSecurity(c))
}

func TestValidateSecurity_WildcardAmongOtherOriginsAlsoRejected(t *testing.T) {
	// If someone configures ["*", "https://app.example.com"] with credentials,
	// the wildcard entry still enables the dangerous combo.
	c := &Config{}
	c.Security.AllowedOrigins = []string{"*", "https://app.example.com"}
	c.Security.AllowCredentials = true

	err := validateSecurity(c)
	require.Error(t, err)
}

func TestLoad_FailClosedOnWildcardCredentialsCombo(t *testing.T) {
	// End-to-end: a config file with the dangerous combo must cause Load to
	// return an error, not a usable *Config.
	dir := t.TempDir()
	tmp := filepath.Join(dir, "config.yaml")
	configBody := `
security:
  allowedOrigins:
    - "*"
  allowCredentials: true
`
	require.NoError(t, os.WriteFile(tmp, []byte(configBody), 0o600))

	_, err := Load(tmp)
	require.Error(t, err)
}
