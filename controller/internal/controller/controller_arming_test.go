// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package controller

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Design 0061 §3 (M1 — crash-loud arming): the four-shape matrix.
//
//   - not-armed exits with the DISTINCT code (85 — the doctrine ladder's
//     fifth rung; a bare 1 is indistinguishable from any boot failure
//     and the #1548 split-brain ships exactly that ambiguity)
//   - the code never collides with the ladder's existing rungs
//   - flag-off is byte-identical: nil config, nil error, ZERO manager
//     interaction (a nil mgr is safe BECAUSE the !enabled branch
//     returns before any mgr use — that ordering is the pin)

func TestRelayStagingNotArmedExitCode_IsTheDistinctFifthRung(t *testing.T) {
	assert.Equal(t, 85, RelayStagingNotArmedExitCode)
	for _, rung := range []int{81, 82, 83, 84} {
		assert.NotEqual(t, rung, RelayStagingNotArmedExitCode,
			"exit %d is the ladder's existing rung — 85 must not collide", rung)
	}
}

// The flag-off shape: nil-nil with a NIL manager — safe iff the
// !enabled branch returns before any mgr deref. If a future edit moves
// mgr use before the branch, this test panics (nil deref) instead of
// shipping the crash to every flag-off boot.
func TestSetupRelayStaging_FlagOffIsByteIdenticalNilPath(t *testing.T) {
	cfg, err := SetupRelayStaging(nil, false, "", "", 0, "", "")
	require.NoError(t, err)
	assert.Nil(t, cfg, "flag off: no staging config (zero behavior change)")
}

// The armed-window shape: the 30s startup-guard timeout IS the bounded
// window (design §3: "no new timer machinery") — pin that the guard's
// budget stays the design's window.
func TestArmingWindow_IsTheStartupGuardBudget(t *testing.T) {
	// The guard's timeout is constructed inline (controller.go's
	// guardCtx); the pin asserts the design constant relationship: the
	// window the design names (30s) equals what the guard uses. The
	// source-truth binding lives in local/m1_arming_source_test.go
	// (main.go's exit wiring) — here we pin the semantic constant the
	// design fixes.
	assert.Equal(t, 30*time.Second, ArmingStartupGuardWindow,
		"the design's bounded startup window — the guard's existing timeout, no new timer")
}

// The decision seam (r1's ask, the opencodeOverlayDecision precedent):
// the error→code mapping is unit-asserted, not source-grepped. One
// code, one meaning — any enabled failure class maps to 85.
func TestRelayStagingExitCodeFor_MapsEveryEnabledFailureToTheRung(t *testing.T) {
	assert.Equal(t, 0, RelayStagingExitCodeFor(nil), "armed: exit 0")
	for name, err := range map[string]error{
		"flag-invalid (ttl out of range)": fmt.Errorf("--relay-token-ttl must be within 1s..7d"),
		"config construction":             fmt.Errorf("relay staging redactor: boom"),
		"startup guard (router dead)":     fmt.Errorf("startup guard FAILED — refusing to start: router unreachable"),
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, 85, RelayStagingExitCodeFor(err))
		})
	}
}
