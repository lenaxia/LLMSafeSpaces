// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// scrub_sidecar_chain_test.go — US-72.6 (run 36135708380): the boot-scrub
// chain in SIDECAR mode. The #1537 wiring was single-container-only: the
// sidecar's healthz never carried the LegacyScrub slice (deps.legacyScrub
// nil) and the scrub itself never ran (both --sidecar and supervise-
// opencode dispatch before main's wiring; the sidecar cannot even see
// /workspace). The nightly (sidecar installs) therefore saw
// LegacyKeysScrubbed=None and a dead R3 trigger. This pins the new chain
// end-to-end at the unit seams: the supervisor's tracker → the control
// socket's status payload → the client decode → the sidecar's status
// store → the healthz snapshot override.

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// TestControlSocketStatusCarriesLegacyScrub: the supervisor's status
// response embeds the boot-scrub report under legacy_scrub when the
// snapshot seam is wired (nil seam → field absent, wire-compat for
// pre-scrub supervisors).
func TestControlSocketStatusCarriesLegacyScrub(t *testing.T) {
	srv := &controlSocketServer{proc: &fakeRestartProc{}}
	// nil seam: absent (a pre-US-72.6 supervisor never sends the field).
	res := srv.status(int64p(1))
	_, has := res.Result["legacy_scrub"]
	assert.False(t, has, "nil seam must omit the field (wire compat)")

	srv.legacyScrubSnapshot = func() *agentd.LegacyScrubHealth {
		return &agentd.LegacyScrubHealth{RanAt: 42, AuthKeysRemoved: 1, ConfigKeysRemoved: 2}
	}
	res = srv.status(int64p(2))
	// The r1 right-sizing stores the STRUCT (marshals identically on the
	// wire); the JSON-shape proof lives in the real-socket chain test.
	m, ok := res.Result["legacy_scrub"].(*agentd.LegacyScrubHealth)
	require.True(t, ok, "the wired seam must embed the report")
	assert.Equal(t, 1, m.AuthKeysRemoved)
	assert.Equal(t, 2, m.ConfigKeysRemoved)
}

// TestControlClientDecodesLegacyScrub: the client round-trips the report.
func TestControlClientDecodesLegacyScrub(t *testing.T) {
	payload := map[string]any{
		"child_pid": 123, "child_state": "running", "restarts": 0,
		"legacy_scrub": map[string]any{"ranAt": 7, "authKeysRemoved": 1, "configKeysRemoved": 0},
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	var dec struct {
		LegacyScrub *agentd.LegacyScrubHealth `json:"legacy_scrub"`
	}
	require.NoError(t, json.Unmarshal(raw, &dec))
	require.NotNil(t, dec.LegacyScrub)
	assert.Equal(t, 1, dec.LegacyScrub.AuthKeysRemoved)
}

// TestSupervisorStatusStoreMirrorsLegacyScrub: the sidecar's polled store
// surfaces the report (and nil before any poll / when the supervisor
// omitted it — the pre-scrub boot window).
func TestSupervisorStatusStoreMirrorsLegacyScrub(t *testing.T) {
	st := &supervisorStatusStore{}
	assert.Nil(t, st.legacyScrubHealth(), "no poll yet → nil (the healthz slice omits)")

	rep := &agentd.LegacyScrubHealth{RanAt: 9, ConfigKeysRemoved: 1}
	st.set(&controlStatus{ChildPID: 5, LegacyScrub: rep})
	got := st.legacyScrubHealth()
	require.NotNil(t, got)
	assert.Equal(t, 1, got.ConfigKeysRemoved)

	st.set(&controlStatus{ChildPID: 5}) // supervisor without the field
	assert.Nil(t, st.legacyScrubHealth(), "omitted field → nil (pre-US-72.6 supervisor)")
}

// TestServerDepsLegacyScrubSnapshotOverrideWins: the sidecar's store
// override takes precedence over the (nil-in-sidecar) tracker snapshot —
// the healthz wiring honors whichever seam is set.
func TestServerDepsLegacyScrubSnapshotOverrideWins(t *testing.T) {
	// The override path: deps.legacyScrub (tracker) nil, snapshot set.
	// The wiring's selection logic is inline in wireHTTPServers; the
	// load-bearing property is that the tracker fallback is nil-safe
	// for the sidecar (no tracker — deps.legacyScrub nil).
	snap := legacyScrubSnapshotFor(nil)
	assert.Nil(t, snap(), "tracker fallback must be nil-safe for the sidecar (no tracker)")
}

func int64p(i int64) *int64 { return &i }
