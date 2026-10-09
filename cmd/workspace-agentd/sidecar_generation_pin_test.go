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
// status poller's generation callback to ALL THREE edges — the
// tracker's D2 reset (onOpencodeGenerationStart), the authority's
// retrying generation reseed (startStateAuthorityReseed with
// ReseedReasonGenerationChange), and the health cache's episode
// re-arm (noteAgentGeneration, #1632 r3: deleting the edge leaves the
// boot-kill loop unpinned-by-wiring). Deleting the callback (the
// pre-fix state: no edge ever fired in split mode) fails this pin.
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
	assert.Contains(t, src, "deps.healthCache.noteAgentGeneration()",
		"the generation callback must fire the health cache's episode re-arm (#1632) — deleting this edge re-opens the boot-kill loop and stays green without this pin")
}

// TestAgentzEpisodeReArmWiredSingleContainer: main.go's
// onChildStarted hook (the single-container generation edge) must
// carry the same episode re-arm — the r3 review found both production
// sites revertible-green. Also pins the admin-mux registration of
// agentz in wireHTTPServers (a dropped route or missing bearer wrap
// reads as permanent failure → liveness restart loop).
func TestAgentzEpisodeReArmWiredSingleContainer(t *testing.T) {
	mainSrc, err := os.ReadFile("main.go")
	require.NoError(t, err)
	mainSrcStr := string(mainSrc)
	assert.Contains(t, mainSrcStr, "healthCache.noteAgentGeneration()",
		"single-container onChildStarted must fire the episode re-arm at every child start")

	serverSrc, err := os.ReadFile("server.go")
	require.NoError(t, err)
	serverSrcStr := string(serverSrc)
	assert.Contains(t, serverSrcStr, `adminMux.Handle("/v1/agentz", requireBearerToken(adminToken,`,
		"wireHTTPServers must register /v1/agentz bearer-gated on the admin mux (a bare Handle would 200 unauthenticated; a missing route 404s into a restart loop)")
	assert.Contains(t, serverSrcStr, "buildAgentzHandler(deps)",
		"the registered agentz handler must be the episode-clock handler wired to the served deps")
}
