// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_quiet_source_test.go — the #2047 quiet seam's pins: QuietFor
// answers unknown as ok=false (the NEVER-alert shape — hub restart,
// unindexed), worker frames refresh the wall clock while opaque frames
// never do (post-#2042 the index is worker-activity-only — a quiet
// stamp is a TRUE quiet), and the batched AssigneeQuiet rides the same
// identity/distrust guards as BusyAssignees.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/board"
)

func TestQuietFor_UnknownStampIsFalse(t *testing.T) {
	hub := newBoardLivenessHub(nil)
	q, ok := hub.QuietFor("ses_never_seen")
	assert.False(t, ok, "no stamp this process = unknown, never quiet evidence")
	assert.Zero(t, q)
	_, ok = hub.QuietFor("")
	assert.False(t, ok)
}

func TestQuietFor_WorkerFrameStampsWallClock(t *testing.T) {
	hub := newBoardLivenessHub(nil)
	hub.ObserveFrame("ws", &abiv1.Event{
		Type:              abiv1.EventType_EVENT_TYPE_PART_START,
		InternalSessionId: "ses_worker",
	})
	q, ok := hub.QuietFor("ses_worker")
	require.True(t, ok)
	assert.Less(t, q, time.Minute, "a fresh worker frame reads quiet-for ~now")

	// Age the clock directly (the wall companion is the quiet source's
	// own state — the sweep's evidence clock under test).
	hub.mu.Lock()
	hub.framesWorkerAt["ses_worker"] = time.Now().Add(-45 * time.Minute)
	hub.mu.Unlock()
	q, ok = hub.QuietFor("ses_worker")
	require.True(t, ok)
	assert.GreaterOrEqual(t, q, 45*time.Minute, "the silent-hang evidence shape: quiet past the threshold")
}

func TestQuietFor_OpaqueFramesNeverRefresh(t *testing.T) {
	hub := newBoardLivenessHub(nil)
	hub.ObserveFrame("ws", &abiv1.Event{
		Type:              abiv1.EventType_EVENT_TYPE_PART_START,
		InternalSessionId: "ses_quiet",
	})
	hub.mu.Lock()
	hub.framesWorkerAt["ses_quiet"] = time.Now().Add(-45 * time.Minute)
	hub.mu.Unlock()

	// A settle/status frame arrives (the incident's second poisoning
	// leg): it must NOT refresh the quiet clock.
	hub.ObserveFrame("ws", &abiv1.Event{
		Type:              abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
		InternalSessionId: "ses_quiet",
	})
	q, ok := hub.QuietFor("ses_quiet")
	require.True(t, ok)
	assert.GreaterOrEqual(t, q, 45*time.Minute, "opaque frames carry zero quiet authority")
}

func TestQuietFor_BothIdentityKeysCarry(t *testing.T) {
	hub := newBoardLivenessHub(nil)
	hub.ObserveFrame("ws", &abiv1.Event{
		Type:              abiv1.EventType_EVENT_TYPE_MESSAGE_END,
		InternalSessionId: "ses_raw",
		SessionId:         "phrase-teapot-carries",
	})
	_, okRaw := hub.QuietFor("ses_raw")
	_, okPhrase := hub.QuietFor("phrase-teapot-carries")
	assert.True(t, okRaw, "the raw id key carries the stamp")
	assert.True(t, okPhrase, "the wire phrase key carries the stamp")
}

// Unit: no pool ⇒ identity resolve degrades ⇒ every ref unknown (nil
// answers, never a fabricated figure — and never a panic).
func TestAssigneeQuiet_DegradedResolveIsAllUnknown(t *testing.T) {
	hub := newBoardLivenessHub(nil)
	hub.SetBusySource(func(_, _ string) (bool, bool) { return true, true })
	out := hub.AssigneeQuiet(context.Background(), []string{"ses_a", "ses_b"})
	assert.Empty(t, out, "a hub that cannot resolve identity answers unknown for everyone")
}

func TestAssigneeQuiet_EmptyInputs(t *testing.T) {
	hub := newBoardLivenessHub(nil)
	assert.Empty(t, hub.AssigneeQuiet(context.Background(), nil))
	assert.Empty(t, hub.AssigneeQuiet(context.Background(), []string{"", ""}))
}

func TestQuietFor_PruneLockstep(t *testing.T) {
	hub := newBoardLivenessHub(nil)
	// Over the cap: the oldest ordinal entries are evicted — the
	// wall-clock companion must leave with them (a quiet answer must
	// never outlive its movement evidence).
	hub.mu.Lock()
	for i := 0; i < boardLivenessMaxEntries+64; i++ {
		key := "ses_" + time.Duration(i).String()
		hub.counter++
		hub.framesWorker[key] = hub.counter
		hub.framesWorkerAt[key] = time.Now()
	}
	hub.pruneLocked()
	require.Less(t, len(hub.framesWorker), boardLivenessMaxEntries+64, "over-cap worker index pruned")
	assert.Equal(t, len(hub.framesWorker), len(hub.framesWorkerAt),
		"the wall-clock companion stays in lockstep with the ordinal index")
	hub.mu.Unlock()
}

// Integration (PG): the batched quiet answer — the composed truth over
// identity + worker stamps + the busy leg, unknown on every degraded
// shape, and the purity contract (no observation state consumed).
func TestIntegrationQuietSource_ComposedTruthAndPurity(t *testing.T) {
	pool := livenessPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `DELETE FROM session_index WHERE workspace_id = 'ws-quiet-2047'`)
	require.NoError(t, err)

	const ws = "ws-quiet-2047"
	seedLivenessSession(t, pool, ws, "ses_framed", "framed-teapot-quiet", 3, false)
	seedLivenessSession(t, pool, ws, "ses_unframed", "unframed-teapot-quiet", 3, false)
	seedLivenessSession(t, pool, ws, "ses_dead", "dead-teapot-quiet", 3, true)

	hub := newBoardLivenessHub(pool)
	tape := &tapeBusyFn{answers: map[string]bool{"ses_framed": true}}
	hub.SetBusySource(tape.busy)

	// ses_framed emits one worker frame, then goes quiet past the
	// alert threshold (the incident's exact shape — aged directly: the
	// wall companion is this test's clock under control).
	hub.ObserveFrame(ws, &abiv1.Event{
		Type:              abiv1.EventType_EVENT_TYPE_PART_START,
		InternalSessionId: "ses_framed",
		SessionId:         "framed-teapot-quiet",
	})
	hub.mu.Lock()
	hub.framesWorkerAt["ses_framed"] = time.Now().Add(-45 * time.Minute)
	hub.framesWorkerAt["framed-teapot-quiet"] = time.Now().Add(-45 * time.Minute)
	hub.mu.Unlock()

	out := hub.AssigneeQuiet(ctx, []string{
		"framed-teapot-quiet", // the phrase form (the read edge's displayed ref)
		"unframed-teapot-quiet",
		"dead-teapot-quiet",
	})

	// The framed session: quiet KNOWN past the threshold, and — busy
	// truth true — busy-seconds set (busy-while-quiet is ALIVE; the
	// ALERT arm vetsoes on Busy, the read edge shows both figures).
	framed, ok := out["framed-teapot-quiet"]
	require.True(t, ok)
	require.NotNil(t, framed.QuietSeconds, "a worker frame seen here is a known quiet clock")
	assert.GreaterOrEqual(t, *framed.QuietSeconds, 45*60, "the evidence shape: quiet past the threshold")
	require.NotNil(t, framed.BusySeconds, "derived-busy vouches the quiet figure as busy-while-silent")
	assert.Equal(t, *framed.QuietSeconds, *framed.BusySeconds)

	// Unknown on every degraded shape — nil, never 0, never a guess.
	unframed, ok := out["unframed-teapot-quiet"]
	require.True(t, ok)
	assert.Nil(t, unframed.QuietSeconds, "never observed in this process: unknown")
	assert.Nil(t, unframed.BusySeconds)
	dead, ok := out["dead-teapot-quiet"]
	require.True(t, ok)
	assert.Nil(t, dead.QuietSeconds, "provably gone: unknown")
	assert.Nil(t, dead.BusySeconds)

	// PURITY (the engine contract): the quiet query consumed no
	// observation state — it minted no cursor under either ref form,
	// so the sweep that follows still measures from its OWN baseline
	// (#2100-R2 re-pin: "not blinded" is cursor-absence plus the
	// sweep's own delta — first observation baselines every leg now,
	// so a pre-claim frame is never a free first-sight renewal).
	require.NotContains(t, hub.cursors, "ses_framed", "the quiet query minted no cursor (raw-ref form)")
	require.NotContains(t, hub.cursors, "framed-teapot-quiet", "…nor under the phrase form the read edge hands it")
	res := hub.ObserveAssignees(ctx, board.SessionObservations{Refs: []board.AssigneeRef{{SessionID: "ses_framed"}}})
	o := res["ses_framed"]
	require.True(t, o.Observed)
	assert.False(t, o.MovedSinceLastSweep, "first observation baselines the frame leg (#2100-R2)")

	hub.ObserveFrame(ws, &abiv1.Event{
		Type:              abiv1.EventType_EVENT_TYPE_PART_DELTA,
		InternalSessionId: "ses_framed",
	})
	res = hub.ObserveAssignees(ctx, board.SessionObservations{Refs: []board.AssigneeRef{{SessionID: "ses_framed"}}})
	assert.True(t, res["ses_framed"].MovedSinceLastSweep,
		"the quiet query consumed no state: the sweep's OWN delta still renews (the ladder is not blinded)")
}
