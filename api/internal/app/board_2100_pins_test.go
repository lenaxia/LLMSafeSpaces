// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_2100_pins_test.go — issue #2100's born-red pins (the
// lease-evidence-core design's Phase 1, reports/specs/
// lease-evidence-core-low-level-design.md §1.1/§1.3): the masking fix
// (cursor rebaseline at due-mark time) and the channel heartbeat's
// fold counter. The DECISIVE timeline pin — the live 2026-11-17
// incident (park 16:56:20Z → nudge 18:26:55Z = 90m35s = exactly two
// 45m lease windows) — compresses park→nudge to ≤ 2 sweeps, asserted
// as row/state deltas against a REAL engine + the production hub.
//
//	2100-B   the masking composition: the parking turn's own frames
//	         (pre-mark movement in the delta window) + due-mark +
//	         Rebaseline ⇒ the next observation reports
//	         MovedSinceLastSweep=false (pre-fix: the first-observation
//	         zero-frame-baseline reads those frames as movement and
//	         ladderRenew erases the fresh mark).
//	2100     the incident, end-to-end through the PRODUCTION wiring
//	         (Service.ConsumeTurnEnd sink + the hub + a real
//	         Reconciler): claim, park (turn-end, no advance), assert
//	         the due-mark fired (trigger=event), and the FIRST sweep
//	         NUDGES (not renews) — nudge_id recorded, one sentinel.
//	         This is the 90m → ≤1 sweep compression, pinned.
//	folds    the consumer's fold counter (the channel heartbeat's
//	         load-bearing gauge under transport v2): one per accepted
//	         turn-end fold (post-watermark), the adoption spine's
//	         numerator.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// TestPin_2100B_DueMarkThenRebaseline_NextSweepIsQuiet — the masking
// composition pin (#2100-B). The hub's first-observation posture
// baselines the FRAME legs at zero by design (any stamp this process
// ever saw reads as movement on first sight — the busy-worker
// protection); the defect is that a due-mark landing BEFORE the first
// observation gets erased by that posture one sweep later. The fix:
// the due-mark's dispatch rebaselines the cursor to the CURRENT
// stamps, so the next observation measures from the mark, not from
// process start. Asserted on the real hub over a real session_index
// row; no wall-clock anywhere.
func TestPin_2100B_DueMarkThenRebaseline_NextSweepIsQuiet(t *testing.T) {
	pool := livenessPool(t)
	ctx := context.Background()
	ws := "ws-2100b-" + uuid.New().String()[:8]
	_, err := pool.Exec(ctx, `DELETE FROM session_index WHERE workspace_id = $1`, ws)
	require.NoError(t, err)

	hub := newBoardLivenessHub(pool)
	const raw = "ses_parked_worker"
	seedLivenessSession(t, pool, ws, raw, "parked-teapot-verifies", 7, false)

	// The parking turn's trailing frames: worker activity the process
	// observed BEFORE the due-mark (turn 1's PART stream — the exact
	// masking input from the live incident).
	hub.ObserveFrame(ws, &abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_PART_START, InternalSessionId: raw,
	})
	hub.ObserveFrame(ws, &abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_MESSAGE_END, InternalSessionId: raw,
	})

	// The due-mark: sink true ⇒ the consumer rebaselines (production
	// wiring: the same hub the sweeps read).
	rebaselined := make(chan time.Time, 1)
	sink := func(context.Context, board.TurnEndDueMark) (bool, error) {
		rebaselined <- time.Now()
		return true, nil
	}
	consumer := newBoardTurnEndConsumer(sink, nil, hub)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go consumer.Run(runCtx)
	consumer.ObserveFrame(ws, &abiv1.Event{
		Type:              abiv1.EventType_EVENT_TYPE_BOARD_TURN_END,
		InternalSessionId: raw,
		BoardTurnEnd:      &abiv1.BoardTurnEnd{Epoch: 1, Advanced: false, TurnStartedAtUnixMs: time.Now().UTC().Add(-time.Minute).UnixMilli()},
	})
	select {
	case <-rebaselined:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn-end dispatch never fired")
	}

	// THE pin: the next sweep's observation is QUIET — the parked
	// worker's pre-mark frames are behind the rebaselined cursor, so
	// MovedSinceLastSweep stays false and the ladder's quiet arms (not
	// ladderRenew) own the row. Pre-fix this reads TRUE (zero-baseline
	// movement) and renews the just-marked lease for a full window.
	res := hub.ObserveAssignees(ctx, board.SessionObservations{Refs: []board.AssigneeRef{{SessionID: raw}}})
	require.Contains(t, res, raw)
	assert.True(t, res[raw].Observed, "the session row is live")
	assert.True(t, res[raw].Exists)
	assert.False(t, res[raw].MovedSinceLastSweep,
		"post-rebaseline the next observation is quiet — ladderRenew must NOT erase a fresh due-mark")

	// And the episode stays measurable: a REAL post-mark frame still
	// reads as movement (the rebaseline moved the baseline, not the
	// worker's ability to prove life).
	hub.ObserveFrame(ws, &abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_PART_START, InternalSessionId: raw,
	})
	res = hub.ObserveAssignees(ctx, board.SessionObservations{Refs: []board.AssigneeRef{{SessionID: raw}}})
	assert.True(t, res[raw].MovedSinceLastSweep,
		"a frame AFTER the rebaseline is movement — a live worker still renews")
}

// TestPin_2100_Timeline_ParkToNudgeTwoSweeps — the incident, end to
// end, state-delta pinned. Claim a task, park the worker (turn-end,
// no advance) while its lease is armed far into the future, and assert
// the FIRST sweep processes the row through the quiet arms: the
// sentinel goes out (nudge recorded on the row, one delivery), the row
// stays in_progress. Pre-fix the same sweep ladderRenews on the
// parking turn's trailing frames and no sentinel exists — the 90m35s
// shape, compressed to one sweep.
func TestPin_2100_Timeline_ParkToNudgeTwoSweeps(t *testing.T) {
	pool := livenessPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		TRUNCATE TABLE board_events, board_transitions, board_dependencies, board_sweeps, tasks, epics, projects
		CASCADE
	`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, active, role)
		VALUES ('pin-2100-user', 'pin-2100-user', 'p2100@example.com', 'hash', true, 'user')
		ON CONFLICT (id) DO NOTHING
	`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO board_config_defaults (user_id, key, value)
		VALUES ('pin-2100-user', 'stagePersonas', '{}'::jsonb)
		ON CONFLICT (user_id, key) DO NOTHING
	`)
	require.NoError(t, err)

	hub := newBoardLivenessHub(pool)
	// A LONG lease: the ONLY thing that can due this row is the event
	// (the incident's shape — the worker parked at 16:56 with ~43m of
	// armed lease left). The nudge measurement window is the DEFAULT
	// (10m) — irrelevant to the front half being pinned here.
	cfg := board.EngineConfig{
		AttemptsCap:     3,
		LeaseDuration:   time.Hour,
		SweepInterval:   time.Second,
		ThrottleBackoff: time.Hour,
		FailureBackoff:  500 * time.Millisecond,
		DRRQuantum:      1,
	}
	svc := board.NewService(pool, cfg, board.WithLivenessSource(hub))
	owner := board.Actor{Kind: board.ActorUser, UserID: "pin-2100-user", Surface: board.SurfaceREST}

	ws := uuid.New().String()
	_, err = pool.Exec(ctx, `
		INSERT INTO workspaces (id, user_id, name, namespace, created_at, updated_at)
		VALUES ($1::uuid, 'pin-2100-user', $2, 'default', now(), now())`, ws, "ws-"+ws[:8])
	require.NoError(t, err)
	p, err := svc.CreateProject(ctx, owner, board.ProjectInput{
		Name: "proj-pin-2100", TargetWorkspaceID: ws, WorkerSlots: 2, Tags: []string{},
	}, "")
	require.NoError(t, err)
	e, err := svc.CreateEpic(ctx, owner, p.ID, "", board.EpicInput{Title: "ep", Outcome: "ship"}, "")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE epics SET status='active' WHERE id=$1`, e.ID)
	require.NoError(t, err)
	task, err := svc.CreateTask(ctx, owner, e.ID, "", board.TaskInput{
		Title: "parked-worker", Description: "charter", Tags: []string{},
		AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "c1"}},
	}, "")
	require.NoError(t, err)

	c, err := svc.ClaimNext(ctx, "worker-pin-2100", ws)
	require.NoError(t, err)
	require.Equal(t, task.ID, c.TaskID)
	assignee := "ses_parked_" + strings.ReplaceAll(task.ShortID, "-", "_")
	_, err = pool.Exec(ctx, `UPDATE tasks SET assignee_session=$2 WHERE id=$1`, task.ID, assignee)
	require.NoError(t, err)
	seedLivenessSession(t, pool, ws, assignee, "", 2, false)

	// The PRODUCTION nudger composition (scope + V1 sender + the hub's
	// send-mark), wrapped only to count — the ordinal question needs
	// the mark the real sender records.
	var nudges atomicInt
	prodNudger := newBoardNudger(boardScopeFor(pool), &recordingV1Sender{}, hub, nil)
	recon := board.NewReconciler(pool, cfg,
		board.WithLivenessSource(hub),
		board.WithNudgeSender(func(ctx context.Context, sessionID string, p board.NudgePayload) error {
			nudges.add(1)
			return prodNudger(ctx, sessionID, p)
		}),
	)

	// Park: the turn's trailing frames land (turn 1's PART stream),
	// then the turn ends with NO advance-class board call — the hub
	// stamps the frames, the consumer dispatches the due-mark, the
	// sink marks the row, and the rebaseline runs.
	hub.ObserveFrame(ws, &abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_PART_START, InternalSessionId: assignee,
	})
	hub.ObserveFrame(ws, &abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_MESSAGE_END, InternalSessionId: assignee,
	})

	consumer := newBoardTurnEndConsumer(svc.ConsumeTurnEnd, nil, hub)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go consumer.Run(runCtx)

	before := dueMarksEventTotal(t)
	consumer.ObserveFrame(ws, &abiv1.Event{
		Type:              abiv1.EventType_EVENT_TYPE_BOARD_TURN_END,
		InternalSessionId: assignee,
		BoardTurnEnd:      &abiv1.BoardTurnEnd{Epoch: 1, Advanced: false, TurnStartedAtUnixMs: time.Now().UTC().Add(-2 * time.Minute).UnixMilli()},
	})

	// The due-mark landed (trigger=event) — bounded wait on the ROW,
	// not the clock's kindness: lease_expires_at collapses to now().
	dueDeadline := time.After(5 * time.Second)
	for {
		var due bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT lease_expires_at <= now() + interval '1 second' FROM tasks WHERE id=$1`, task.ID).Scan(&due))
		if due {
			break
		}
		select {
		case <-dueDeadline:
			t.Fatal("the due-mark never landed (trigger=event path dead)")
		case <-time.After(20 * time.Millisecond):
		}
	}
	assert.Greater(t, dueMarksEventTotal(t), before, "the event-triggered due-mark counter moved")

	// ONE sweep. Pre-fix: first-observation movement (the parking
	// turn's frames) ⇒ ladderRenew ⇒ no sentinel, nudge=0. Post-fix:
	// the rebaselined cursor ⇒ quiet arms ⇒ the sentinel goes out.
	_, err = recon.Sweep(ctx)
	require.NoError(t, err)

	assert.Equal(t, 1, nudges.get(), "the FIRST sweep after park sends the sentinel (≤ 2 sweeps; the 90m35s shape compressed)")
	var stage, nudgeID *string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT stage::text, nudge_id::text FROM tasks WHERE id=$1`, task.ID).Scan(&stage, &nudgeID))
	require.NotNil(t, stage)
	assert.Equal(t, board.StageInProgress, *stage, "the nudge episode opened; the row is not reclaimed")
	require.NotNil(t, nudgeID, "the sentinel is recorded on the row (nudge_id)")
	assert.NotEmpty(t, *nudgeID)
}

// TestPin_2100_ConsumerFoldsCounter — the channel heartbeat's gauge
// (design §1.3/§2.3): every turn-end fold the consumer ACCEPTS (past
// the epoch watermark) increments board_turn_end_events_folded_total —
// the surface that, compared against claims-active in board_health,
// makes a dead fold leg read DEGRADED instead of invisible (#2100's
// observability gap: claims existed, events folded 0, nothing
// compared them).
func TestPin_2100_ConsumerFoldsCounter(t *testing.T) {
	sink := newRecordingSink()
	c := newBoardTurnEndConsumer(sink.deliver, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	before := boardTurnEndsFolded.Value()
	// Accepted fold (new session key, epoch 1).
	c.ObserveFrame(turnEndFrame("ws", "ses_f", "phrase-f", 1, false))
	// Dropped at the watermark (duplicate ordinal): NOT a fold.
	c.ObserveFrame(turnEndFrame("ws", "ses_f", "phrase-f", 1, false))
	// Not a turn-end at all: not a fold.
	c.ObserveFrame("ws", &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_PART_DELTA, InternalSessionId: "ses_f"})

	select {
	case <-sink.next:
	case <-time.After(5 * time.Second):
		t.Fatal("the accepted fold never dispatched")
	}
	assert.Equal(t, before+1, boardTurnEndsFolded.Value(),
		"exactly the accepted fold counts (watermark-dropped and non-turn-end frames do not)")
}

// dueMarksEventTotal reads the trigger=event counter (the delta basis).
func dueMarksEventTotal(t *testing.T) float64 {
	t.Helper()
	return testutil.ToFloat64(board.DueMarks().WithLabelValues(board.DueMarkTriggerEvent))
}
