// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// scrub_chain_wiring_test.go — US-72.6 r1 (the #1570 review's deletion
// proof): the PR's essence is the WIRING — the supervisor's boot call,
// the control-socket seam, the store mirror, and the healthz
// override-wins selection. Removing every one of those lines kept the
// full suite green, i.e. the regression run 36135708380 caught could be
// reintroduced undetected. These tests cross the REAL seams (a real
// TCP socket; the real client decode struct; the real selection
// helper), so each wiring line has a red mode:
//
//   - newSupervisorControlServer's seam        → server-side payload over TCP
//   - controlClient.Status + controlStatus     → the client decode (tag agreement)
//   - supervisorStatusStore.legacyScrubHealth  → the mirror (+ error path)
//   - legacyScrubHealthSnapshot                → override-wins (extracted r1)
//   - bootLegacyScrub at the supervisor path   → the global the seam reads

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// TestScrubChainOverRealSocket: a REAL control socket serving the seam's
// report, read by the REAL client — the tag agreement end-to-end (a
// typo in either direction fails here, which the r1 locally-declared
// struct test could not catch).
func TestScrubChainOverRealSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	rep := &agentd.LegacyScrubHealth{RanAt: 77, AuthKeysRemoved: 1, ConfigKeysRemoved: 2}
	srv := &controlSocketServer{ln: ln, proc: &fakeRestartProc{}, legacyScrubSnapshot: func() *agentd.LegacyScrubHealth { return rep }}
	go srv.serve()

	cc := newControlClient(ln.Addr().String())
	st, err := cc.Status(context.Background())
	require.NoError(t, err, "the real client must decode the real server's status")
	require.NotNil(t, st.LegacyScrub, "the report must survive the wire (tag agreement)")
	assert.Equal(t, 1, st.LegacyScrub.AuthKeysRemoved)
	assert.Equal(t, 2, st.LegacyScrub.ConfigKeysRemoved)
	assert.Equal(t, int64(77), st.LegacyScrub.RanAt)
}

// TestScrubChainErrorPathOverSocket: a report carrying Error traverses
// the chain intact (the mirror's ScrubError arm depends on it).
func TestScrubChainErrorPathOverSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	srv := &controlSocketServer{ln: ln, proc: &fakeRestartProc{}, legacyScrubSnapshot: func() *agentd.LegacyScrubHealth {
		return &agentd.LegacyScrubHealth{RanAt: 5, Error: "scrub legacy auth.json: boom"}
	}}
	go srv.serve()

	cc := newControlClient(ln.Addr().String())
	st, err := cc.Status(context.Background())
	require.NoError(t, err)
	require.NotNil(t, st.LegacyScrub)
	assert.Equal(t, "scrub legacy auth.json: boom", st.LegacyScrub.Error)

	// and the store mirror surfaces it verbatim.
	store := &supervisorStatusStore{}
	store.set(st)
	got := store.legacyScrubHealth()
	require.NotNil(t, got)
	assert.Equal(t, "scrub legacy auth.json: boom", got.Error)
}

// TestLegacyScrubHealthSnapshotOverrideWins: the extracted selection —
// the override (sidecar) wins when set; the tracker fallback when not
// (single-container); nil-safe when neither.
func TestLegacyScrubHealthSnapshotOverrideWins(t *testing.T) {
	override := func() *agentd.LegacyScrubHealth { return &agentd.LegacyScrubHealth{RanAt: 1} }
	got := legacyScrubHealthSnapshot(serverDeps{legacyScrubSnapshot: override})
	require.NotNil(t, got())
	assert.Equal(t, int64(1), got().RanAt, "the override must win when set (sidecar mode)")

	// single-container: the tracker path.
	tr := newLegacyScrubTracker(t.TempDir())
	tr.runOnce()
	got2 := legacyScrubHealthSnapshot(serverDeps{legacyScrub: tr})
	require.NotNil(t, got2(), "the tracker fallback must serve the in-process report")

	// neither: nil-safe (a server-only deps shape).
	assert.Nil(t, legacyScrubHealthSnapshot(serverDeps{})())
}

// TestSupervisorControlServerSeamWired: newSupervisorControlServer (the
// supervisor path's actual constructor) installs the legacyScrubSnapshot
// seam reading the supervisor global — the deletion of either the
// constructor wiring or the global-Store line has a red mode here.
func TestSupervisorControlServerSeamWired(t *testing.T) {
	adapter := &managedProcAdapter{}
	srv, err := newSupervisorControlServer("127.0.0.1:0", adapter, nil)
	require.NoError(t, err)
	defer srv.ln.Close()
	require.NotNil(t, srv.legacyScrubSnapshot, "the supervisor control server must carry the scrub seam")

	// The seam reads the supervisor global: set it, fire the boot call,
	// and the snapshot must surface the report (the supervise-opencode
	// path's two lines — tracker creation + bootLegacyScrub — are
	// mirrored here through the same globals they write).
	tr := newLegacyScrubTracker(t.TempDir())
	old := supervisorLegacyScrub.Swap(tr)
	defer supervisorLegacyScrub.Store(old)
	bootLegacyScrub(tr)
	require.Eventually(t, func() bool { return srv.legacyScrubSnapshot() != nil },
		2*time.Second, 5*time.Millisecond,
		"the seam must surface the booted supervisor's report (the supervise-opencode wiring)")
}

// TestScrubClientDecodeTagAgreement: the real raw decode struct's tag —
// the r1 test's locally-declared struct could not catch a typo here.
func TestScrubClientDecodeTagAgreement(t *testing.T) {
	res := map[string]any{"legacy_scrub": map[string]any{"ranAt": float64(3), "configKeysRemoved": float64(1)}}
	dec, err := decodeControlStatus(res)
	require.NoError(t, err)
	require.NotNil(t, dec.LegacyScrub)
	assert.Equal(t, 1, dec.LegacyScrub.ConfigKeysRemoved)
}
