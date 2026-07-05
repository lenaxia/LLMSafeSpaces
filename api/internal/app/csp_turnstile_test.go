// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAddTurnstileToCSP_ExtendsScriptSrcAndFrameSrc verifies that the
// production default CSP is correctly augmented with the Turnstile
// origin in both script-src and frame-src. Without this transform,
// enabling Turnstile makes registration impossible: the browser
// blocks the widget script (script-src 'self') AND its iframe
// (frame-src falls back to default-src 'self'), so onToken never
// fires and the submit button stays permanently disabled.
//
// PR #501 review round 4 flagged this as a hard blocker.
func TestAddTurnstileToCSP_ExtendsScriptSrcAndFrameSrc(t *testing.T) {
	// Real production default CSP from server.DefaultRouterConfig().
	original := "default-src 'self'; connect-src 'self' wss:; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; object-src 'none'; frame-ancestors 'none'; form-action 'self'; base-uri 'self'; block-all-mixed-content"
	out := addTurnstileToCSP(original)

	require.Contains(t, out, "script-src 'self' https://challenges.cloudflare.com",
		"script-src must include Turnstile origin so the widget script (api.js) can load")
	require.Contains(t, out, "frame-src",
		"frame-src must be added — the challenge iframe would otherwise fall back to default-src 'self' and be blocked")
	require.Contains(t, out, "frame-src 'self' https://challenges.cloudflare.com",
		"frame-src must include Turnstile origin")

	// Preserve the rest of the policy — no other directive should be
	// altered.
	for _, must := range []string{
		"default-src 'self'",
		"connect-src 'self' wss:",
		"style-src 'self'",
		"object-src 'none'",
		"frame-ancestors 'none'",
		"base-uri 'self'",
		"block-all-mixed-content",
	} {
		require.Contains(t, out, must, "directive %q must not be removed", must)
	}
}

// TestAddTurnstileToCSP_Idempotent verifies calling the transform
// twice on the same input doesn't duplicate the origin. Guards
// against a wire-up bug that applies the transform per-request
// instead of once at config time.
func TestAddTurnstileToCSP_Idempotent(t *testing.T) {
	original := "script-src 'self'"
	once := addTurnstileToCSP(original)
	twice := addTurnstileToCSP(once)

	// Should be exactly one occurrence of the origin in script-src.
	scriptSrcRuns := strings.Count(twice, "script-src 'self' https://challenges.cloudflare.com")
	require.Equal(t, 1, scriptSrcRuns, "script-src directive contains Turnstile origin exactly once, got %d in: %s", scriptSrcRuns, twice)
	// And exactly one occurrence in frame-src.
	frameSrcRuns := strings.Count(twice, "frame-src")
	require.Equal(t, 1, frameSrcRuns, "frame-src directive appears exactly once")
}

// TestAddTurnstileToCSP_AddsFrameSrcWhenAbsent verifies the function
// synthesizes a frame-src directive when the input has none — the
// production default CSP doesn't include frame-src (it relies on
// frame-ancestors + default-src for anti-clickjacking), so we must
// add one explicitly rather than relying on default-src's fallback.
func TestAddTurnstileToCSP_AddsFrameSrcWhenAbsent(t *testing.T) {
	original := "default-src 'self'; script-src 'self'; frame-ancestors 'none'"
	out := addTurnstileToCSP(original)

	require.Contains(t, out, "frame-src 'self' https://challenges.cloudflare.com",
		"frame-src must be synthesized when absent — the challenge iframe cannot rely on default-src fallback")
}

// TestAddTurnstileToCSP_AddsScriptSrcWhenAbsent covers the edge case
// of a CSP with no script-src at all (unusual but possible in
// hand-written configs).
func TestAddTurnstileToCSP_AddsScriptSrcWhenAbsent(t *testing.T) {
	original := "default-src 'self'; frame-ancestors 'none'"
	out := addTurnstileToCSP(original)

	require.Contains(t, out, "script-src 'self' https://challenges.cloudflare.com",
		"script-src must be synthesized when absent")
}
