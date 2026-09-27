// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// sidecar_generation_pin_test.go — the #1576 layer-1 production
// WIRING pin (r1's missing regression: the poller test injects its
// OWN callback, so reverting the production callback argument in
// sidecar_mode.go left the whole suite green — for a fix whose root
// cause was a missing wire, the revert must go red). Source pin in
// the TestGenerationChangeReseedWiredToRetryingDriver /
// orphan_reason_pin_test.go discipline.

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSidecarGenerationWiringPinned: sidecar_mode.go must wire the
// status poller's generation callback to BOTH edges — the tracker's
// D2 reset (onOpencodeGenerationStart) and the authority's
// retrying generation reseed (startStateAuthorityReseed with
// ReseedReasonGenerationChange). Deleting the callback (the pre-fix
// state: neither edge ever fired in split mode) fails this pin.
func TestSidecarGenerationWiringPinned(t *testing.T) {
	body, err := os.ReadFile("sidecar_mode.go")
	require.NoError(t, err)
	src := string(body)

	assert.Contains(t, src, "deps.sseTracker.onOpencodeGenerationStart()",
		"the generation callback must fire the tracker's D2 reset — the #1573 live finding was that it NEVER fired in split mode")
	assert.Contains(t, src, "go startStateAuthorityReseed(bgCtx, sidecarAuthority, sessionstate.ReseedReasonGenerationChange)",
		"the generation callback must fire the authority's retrying generation reseed at the same edge")
	assert.Contains(t, src, "startSupervisorStatusPollerWithInterval(",
		"the sidecar must run the status poller (the generation signal's channel)")
}
