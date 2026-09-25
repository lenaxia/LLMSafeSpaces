// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// scrub_boot_trigger_test.go — US-72.6 sweep fix (run 36135708380): the
// boot-scrub trigger must not depend on the relay monitor's first
// Present=true observation. Design 0061 M2's migration-mode fail-open
// fallback delivers a RAW batch on any boot where controller staging
// has not converged — on those boots the batch carries no relay-fronted
// (router-URL) entries, the monitor never evaluates Present=true, and
// the #1537 first-Present hook never fires: the residue-boot migration
// (the sweep's R3) is dead exactly when it must run (the resumed pod
// booting with legacy PVC residue), and LegacyKeysScrubbed stays at the
// stale boot mirror (reason empty/Clean) while the residue survives
// (run 36135708380: "canary still present 1 time(s) after the residue
// boot").
//
// The fix fires the tracker unconditionally at boot (main.go); the
// first-Present hook REMAINS wired as belt-and-braces — the tracker's
// sync.Once makes whichever fires first the only execution. The scrub
// itself is safe on every posture: it strips key material ONLY from
// legacy platform-shaped residue files (the pre-US-35.7 regular-file
// auth.json and agent-config.json copies under .local) — never from
// the live delivery surfaces, so a flag-off/raw pod loses nothing it
// needs.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"os"
)

// TestBootLegacyScrub_FiresWithoutRelayPresent: the boot trigger's
// load-bearing property — bootLegacyScrub (main()'s startup call)
// produces a tracker report with NO relay-liveness Present observation
// whatsoever (the M2 fallback boot: raw batch, monitor never Present,
// hook never invoked). Pre-fix this was red: the hook-only wiring left
// snapshot() nil on exactly those boots (run 36135708380 R3).
func TestBootLegacyScrub_FiresWithoutRelayPresent(t *testing.T) {
	tr := newLegacyScrubTracker(t.TempDir())
	assert.Nil(t, tr.snapshot(), "pre-boot: no report yet")

	bootLegacyScrub(tr) // main()'s unconditional startup call

	require.Eventually(t, func() bool { return tr.snapshot() != nil },
		2*time.Second, 5*time.Millisecond,
		"the BOOT call must produce a report with NO relay observation — the M2 fallback boot is the migration's real-world case (run 36135708380 R3)")
	assert.NotZero(t, tr.snapshot().RanAt, "the report must be timestamped")
}

// TestLegacyScrubTracker_BootCallIdempotentWithHook: the boot call and
// the first-Present hook share the tracker's sync.Once — whichever
// fires first wins, the other is a no-op, and exactly one report exists.
func TestLegacyScrubTracker_BootCallIdempotentWithHook(t *testing.T) {
	tr := newLegacyScrubTracker(t.TempDir())
	boot := tr.runOnce
	hook := tr.runOnce // the same method the monitor hook wraps
	boot()
	hook()
	boot()
	rep := tr.snapshot()
	require.NotNil(t, rep)
	first := rep.RanAt
	time.Sleep(10 * time.Millisecond)
	hook()
	assert.Equal(t, first, tr.snapshot().RanAt, "the sync.Once must collapse boot + hook + repeat calls into ONE scrub")
}

// TestLegacyScrubTracker_BootFindsSurface2Residue: the actual R3 shape —
// a Surface-2 agent-config.json copy under .local/config/opencode (the
// plant init-fs never touches) is removed by the BOOT call alone.
func TestLegacyScrubTracker_BootFindsSurface2Residue(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, mkdirAllFor(root+"/.local/config/opencode/agent-config.json"))
	require.NoError(t, os.WriteFile(root+"/.local/config/opencode/agent-config.json",
		[]byte(`{"provider": {"legacy2": {"options": {"apiKey": "sk-RESIDUE"}}}}`), 0o644))

	tr := newLegacyScrubTracker(root)
	tr.runOnce() // the boot call — no relay observation anywhere

	rep := tr.snapshot()
	require.NotNil(t, rep)
	assert.Equal(t, 1, rep.ConfigKeysRemoved, "the boot scrub must strip the Surface-2 residue (configKeysRemoved=1 — the R3 assertion's expected report)")
	data, err := os.ReadFile(root + "/.local/config/opencode/agent-config.json")
	require.NoError(t, err)
	assert.NotContains(t, string(data), "sk-RESIDUE", "the residue key material is gone")
}
