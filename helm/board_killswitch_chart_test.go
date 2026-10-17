// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// The board kill-switch render pins (design 0067, ruling R6; PR #1719
// r2-S3): the platform knob must actually reach the api env — an
// EXPLICIT board.enabled=false renders "false" (sprig's `default true`
// coerces false back to true; the chart's sanctioned fix is dig, the
// relayOnlyKeyDelivery.api.enabled precedent), overrides thread, and the
// chart's own values.yaml defaults hold when the section is untouched.

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// An explicit false MUST render false — the kill-switch lever. Fails on
// the `default true` shape (renders "true") — the r2 red-first pin.
func TestBoardKillSwitch_ExplicitFalseHonored(t *testing.T) {
	docs := helmTemplate(t, "board:\n  enabled: false\n")
	env := apiEnv(t, apiDeployment(t, docs))
	assert.Equal(t, "false", env["LLMSAFESPACES_BOARD_ENABLED"],
		"board.enabled=false is the kill-switch: `default true` coerces false back to true — dig honors it (the chart's false-honoring pattern)")
	// The numeric knobs still render (only the engine gate is off).
	assert.Equal(t, "3", env["LLMSAFESPACES_BOARD_ATTEMPTSCAP"])
}

// The chart defaults render untouched (values.yaml carries them; dig
// falls back only on a MISSING key, never on explicit false).
func TestBoardKillSwitch_ChartDefaults(t *testing.T) {
	env := apiEnv(t, apiDeployment(t, helmTemplate(t, "")))
	assert.Equal(t, "true", env["LLMSAFESPACES_BOARD_ENABLED"])
	assert.Equal(t, "3", env["LLMSAFESPACES_BOARD_ATTEMPTSCAP"])
	assert.Equal(t, "900", env["LLMSAFESPACES_BOARD_LEASESECONDS"])
	assert.Equal(t, "300", env["LLMSAFESPACES_BOARD_THROTTLEBACKOFFSECONDS"])
	assert.Equal(t, "1", env["LLMSAFESPACES_BOARD_DRRQUANTUM"])
	assert.Equal(t, "30", env["LLMSAFESPACES_BOARD_SWEEPSECONDS"])
}

// Knob overrides thread chart → env — ALL six (the r3 review's pin gap:
// throttleBackoffSeconds/sweepSeconds were unasserted, and a wrong-key
// dig on those two is invisible to ChartDefaults because dig's fallback
// byte-equals the values.yaml default).
func TestBoardKillSwitch_OverridesThread(t *testing.T) {
	docs := helmTemplate(t, "board:\n  enabled: true\n  attemptsCap: 5\n  leaseSeconds: 42\n  throttleBackoffSeconds: 77\n  drrQuantum: 2\n  sweepSeconds: 15\n")
	env := apiEnv(t, apiDeployment(t, docs))
	assert.Equal(t, "true", env["LLMSAFESPACES_BOARD_ENABLED"])
	assert.Equal(t, "5", env["LLMSAFESPACES_BOARD_ATTEMPTSCAP"])
	assert.Equal(t, "42", env["LLMSAFESPACES_BOARD_LEASESECONDS"])
	assert.Equal(t, "77", env["LLMSAFESPACES_BOARD_THROTTLEBACKOFFSECONDS"])
	assert.Equal(t, "2", env["LLMSAFESPACES_BOARD_DRRQUANTUM"])
	assert.Equal(t, "15", env["LLMSAFESPACES_BOARD_SWEEPSECONDS"])
}
