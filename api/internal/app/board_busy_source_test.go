// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_busy_source_test.go — the RC-C S2 badge seam's pins: the hub's
// BusyAssignees is a PURE read-edge query (busy.go's contract — no
// cursors advance, no movement delta is consumed), quiet on every
// unknown (unindexed, unframed, deleted, unwired, degraded), busy only
// on the composed truth (frame seen here + the derived-busy leg), keyed
// by the ref exactly as passed (raw or phrase).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// tapeBusyFn records the (workspace, raw) pairs the hub consulted.
type tapeBusyFn struct {
	answers map[string]bool // keyed by raw session id
	asked   [][2]string
}

func (t *tapeBusyFn) busy(workspaceID, harnessSessionID string) (bool, bool) {
	t.asked = append(t.asked, [2]string{workspaceID, harnessSessionID})
	busy, known := t.answers[harnessSessionID]
	return busy, known
}

// Unit: no pool ⇒ the identity resolve degrades ⇒ every ref quiets
// (never a panic, never busy — the safe direction).
func TestBusySource_DegradedResolveIsAllQuiet(t *testing.T) {
	hub := newBoardLivenessHub(nil)
	hub.SetBusySource(func(_, _ string) (bool, bool) { return true, true })
	out := hub.BusyAssignees(context.Background(), []string{"ses_a", "ses_b"})
	assert.Empty(t, out, "a hub that cannot resolve identity answers quiet for everyone")
}

// Unit: nil/empty inputs are inert.
func TestBusySource_EmptyInputs(t *testing.T) {
	hub := newBoardLivenessHub(nil)
	assert.Empty(t, hub.BusyAssignees(context.Background(), nil))
	assert.Empty(t, hub.BusyAssignees(context.Background(), []string{"", ""}))
}

// Integration (PG): the composed answer and the purity contract.
func TestIntegrationBusySource_ComposedTruthAndPurity(t *testing.T) {
	pool := livenessPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `DELETE FROM session_index WHERE workspace_id = 'ws-busy-badge'`)
	require.NoError(t, err)

	const ws = "ws-busy-badge"
	seedLivenessSession(t, pool, ws, "ses_busy", "busy-teapot-verifies", 3, false)
	seedLivenessSession(t, pool, ws, "ses_idle", "idle-teapot-verifies", 3, false)         // framed, gate idle
	seedLivenessSession(t, pool, ws, "ses_unframed", "unframed-teapot-verifies", 3, false) // never framed here
	seedLivenessSession(t, pool, ws, "ses_dead", "dead-teapot-verifies", 3, true)          // framed, provably gone

	hub := newBoardLivenessHub(pool)
	tape := &tapeBusyFn{answers: map[string]bool{
		"ses_busy":     true,
		"ses_idle":     false, // known, idle
		"ses_dead":     true,  // the pathological latch — must not survive the deleted guard
		"ses_unframed": true,  // never observed here — must not survive the frame guard
	}}
	hub.SetBusySource(tape.busy)

	// Frames: busy, idle, dead observed in THIS process; unframed not.
	for _, raw := range []string{"ses_busy", "ses_idle", "ses_dead"} {
		hub.ObserveFrame(ws, &abiv1.Event{
			Type:              abiv1.EventType_EVENT_TYPE_PART_START,
			InternalSessionId: raw,
		})
	}

	// The PHRASE form of the busy ref (the read edge hands the badge
	// the displayed value — phrase after #1922 translation).
	out := hub.BusyAssignees(ctx, []string{"busy-teapot-verifies", "idle-teapot-verifies", "unframed-teapot-verifies", "dead-teapot-verifies"})

	assert.Equal(t, map[string]bool{"busy-teapot-verifies": true}, out,
		"busy ONLY on frame-seen ∧ derived-busy; idle, unframed, and deleted all quiet — keyed by the ref as passed")

	// The busy leg was consulted for exactly the three framed, present
	// sessions (unframed and deleted never reach it).
	assert.Len(t, tape.asked, 3, "the deleted and unframed refs never reach the busy leg")

	// PURITY (the engine contract): the badge query consumed no
	// observation state — the next ObserveAssignees for the framed,
	// busy session is still a FIRST observation (the frame leg
	// baselines at zero, so the already-delivered PART_START frame is
	// movement evidence; had BusyAssignees advanced the cursor, the
	// delta would be silently eaten and the lease ladder blinded).
	res := hub.ObserveAssignees(ctx, board.SessionObservations{Refs: []board.AssigneeRef{{SessionID: "ses_busy"}}})
	o := res["ses_busy"]
	require.True(t, o.Observed)
	assert.True(t, o.MovedSinceLastSweep,
		"the badge query must not consume the sweep's movement delta (purity)")
	assert.True(t, o.Busy, "the sweep's own busy answer still composes (unchanged semantics)")
}
