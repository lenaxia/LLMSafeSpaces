// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// evidence_core_2100_integration_test.go — issue #2100's engine-side
// born-red pins (lease-evidence-core design Phases 2/3, scoped by the
// fix charter): the store-native evidence transport's consumption half
// (advance derived store-side inside the guarded due-mark write), the
// decoupled nudge measurement window, response-observed presence (the
// watermark-less heartbeat arrival), and the channel heartbeat's
// board_health line. Every pin asserts state deltas, never wall-clock
// kindness beyond the sub-floor measure windows the knobs carry.
//
//	P2-advance    an advance-class board_transitions row by the
//	              assignee INSIDE the turn window suppresses the
//	              due-mark (advanced store-side, telemetry-only — the
//	              agentd-side ledger/tail derivation is no longer the
//	              only path; pre-fix on main this shape derives
//	              agentd-side and a missed tail due-marks the row).
//	P2-pure       a worker claim's turn-end due-marks with NO registry
//	              entries anywhere — the pure assignee-match path (the
//	              class-kill's core promise).
//	P2-user       a user chat's turn-end matches no row (the ungated
//	              emission's consumer-side claim filter).
//	window        the nudge rung extends the lease by
//	              NudgeMeasureWindow, NOT LeaseDuration — and the wedge
//	              verdict lands at window+sweep, not lease+sweep.
//	late          a responder INSIDE the window renews (the challenge
//	              survives the shorter window — response-observed).
//	arrival       a heartbeat with NO declared watermark is a valid
//	              response: renews, clears the slot, and stamps the
//	              progress clock with the SERVER clock (pre-fix on
//	              main a zero stamp stores year-1 — the client-hoped
//	              clock, exactly what the store must not trust).
//	health        the board_health event_channel line: stale fold age
//	              + active claims ⇒ degraded:true (the #2100
//	              observability gap — claims existed, events folded 0,
//	              nothing compared them).

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// pin2100Recon builds a reconciler with a CUSTOM engine config (the
// window pins need LeaseDuration ≫ NudgeMeasureWindow in one process).
func (s *BoardSuite) pin2100Recon(cfg board.EngineConfig, lives board.AssigneeLivenessSource, nudger board.NudgeSender) *board.Reconciler {
	return board.NewReconciler(s.pool, cfg,
		board.WithLivenessSource(lives),
		board.WithNudgeSender(nudger),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
	)
}

// wedgeQuiet is the positioned-sentinel silence the verdict reads.
func wedgeQuiet() board.AssigneeObservation {
	return board.AssigneeObservation{Observed: true, Exists: true, NudgeMarkKnown: true, EventsAfterNudge: false}
}

func (s *BoardSuite) TestPin_P2_AdvanceFromStore_SuppressesDueMark() {
	ws, c := s.seedClaimedTask("advance-from-store")
	s.armLiveLease(c.TaskID)
	assignee := s.reloadTask(c.TaskID).AssigneeSession
	windowStart := time.Now().UTC().Add(-2 * time.Minute)

	// The advance: a stage-transition-shaped row by the ASSIGNEE inside
	// the turn window (what every advance-class board call lands —
	// timestamped, actor-attributed; the store owns this fact already).
	_, err := s.pool.Exec(context.Background(), `
		INSERT INTO board_transitions (task_id, from_stage, to_stage, actor, at)
		VALUES ($1, 'in_progress', 'in_progress', $2::jsonb, now())`,
		c.TaskID, `{"kind":"user","session_id":"`+assignee+`"}`)
	require.NoError(s.T(), err)

	advancesBefore := testutil.ToFloat64(board.TurnAdvancesCounter())
	dueBefore := dueMarks(board.DueMarkTriggerEvent)

	marked, err := s.svc.ConsumeTurnEnd(context.Background(), board.TurnEndDueMark{
		WorkspaceID: ws, SessionRefs: []string{assignee}, Epoch: 1,
		Advanced: false, TurnStartedAt: windowStart, ObservedAt: time.Now().UTC(),
	})
	require.NoError(s.T(), err)

	require.False(s.T(), marked, "the store observed the advance — the row is NOT due-marked")
	var lease time.Time
	require.NoError(s.T(), s.pool.QueryRow(context.Background(),
		`SELECT lease_expires_at FROM tasks WHERE id=$1`, c.TaskID).Scan(&lease))
	require.True(s.T(), lease.After(time.Now().Add(time.Minute)),
		"the lease is untouched by an advancing turn (R2 telemetry-only)")
	require.Equal(s.T(), advancesBefore+1, testutil.ToFloat64(board.TurnAdvancesCounter()),
		"the store-derived advance is countable telemetry")
	require.Equal(s.T(), dueBefore, dueMarks(board.DueMarkTriggerEvent))
}

func (s *BoardSuite) TestPin_P2_NoRegistryNeeded_PureAssigneeMatch() {
	ws, c := s.seedClaimedTask("no-registry-needed")
	s.armLiveLease(c.TaskID)
	assignee := s.reloadTask(c.TaskID).AssigneeSession

	dueBefore := dueMarks(board.DueMarkTriggerEvent)
	marked, err := s.svc.ConsumeTurnEnd(context.Background(), board.TurnEndDueMark{
		WorkspaceID: ws, SessionRefs: []string{assignee}, Epoch: 1,
		Advanced: false, TurnStartedAt: time.Now().UTC().Add(-time.Minute),
		ObservedAt: time.Now().UTC(),
	})
	require.NoError(s.T(), err)
	require.True(s.T(), marked, "the pure assignee-match path due-marks with NO registry anywhere")
	require.Equal(s.T(), dueBefore+1, dueMarks(board.DueMarkTriggerEvent))

	var lease time.Time
	require.NoError(s.T(), s.pool.QueryRow(context.Background(),
		`SELECT lease_expires_at FROM tasks WHERE id=$1`, c.TaskID).Scan(&lease))
	require.False(s.T(), lease.After(time.Now().Add(time.Second)), "the lease collapsed to due")
}

func (s *BoardSuite) TestPin_P2_UserChatInert_UnderStoreDerivation() {
	_, c := s.seedClaimedTask("user-chat-inert")
	s.armLiveLease(c.TaskID)

	dueBefore := dueMarks(board.DueMarkTriggerEvent)
	marked, err := s.svc.ConsumeTurnEnd(context.Background(), board.TurnEndDueMark{
		// A well-formed but foreign workspace uuid: no row matches it.
		WorkspaceID: uuid.NewString(), SessionRefs: []string{"ses_user_chat_no_claim"}, Epoch: 1,
		Advanced: false, TurnStartedAt: time.Now().UTC().Add(-time.Minute),
		ObservedAt: time.Now().UTC(),
	})
	require.NoError(s.T(), err)
	require.False(s.T(), marked, "a user chat's turn-end matches no row — inert post-un gating")
	require.Equal(s.T(), dueBefore, dueMarks(board.DueMarkTriggerEvent))

	var lease time.Time
	require.NoError(s.T(), s.pool.QueryRow(context.Background(),
		`SELECT lease_expires_at FROM tasks WHERE id=$1`, c.TaskID).Scan(&lease))
	require.True(s.T(), lease.After(time.Now().Add(time.Minute)), "the unrelated row is untouched")
}

func (s *BoardSuite) TestPin_NudgeMeasureWindow_NudgeRungExtendsByWindowNotLease() {
	_, c := s.seedClaimedTask("nudge-window-extension")
	assignee := s.reloadTask(c.TaskID).AssigneeSession
	require.NotEmpty(s.T(), assignee)

	cfg := s.cfg
	cfg.LeaseDuration = 10 * time.Minute
	cfg.NudgeMeasureWindow = 300 * time.Millisecond

	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	nudger := &fakeNudger{}
	recon := s.pin2100Recon(cfg, lives, nudger.Send)

	s.expireLease(c.TaskID)
	s.rc1Sweep(recon)

	require.Equal(s.T(), 1, nudger.count(), "the quiet row is nudged")
	var lease time.Time
	require.NoError(s.T(), s.pool.QueryRow(context.Background(),
		`SELECT lease_expires_at FROM tasks WHERE id=$1`, c.TaskID).Scan(&lease))
	require.True(s.T(), lease.After(time.Now()), "the window re-arms the lease (a clean measurement point)")
	require.True(s.T(), lease.Before(time.Now().Add(time.Minute)),
		"the extension is the MEASURE WINDOW (300ms-class), not the 10m lease — decoupled")
}

func (s *BoardSuite) TestPin_NudgeMeasureWindow_WedgeAtWindowNotLease() {
	_, c := s.seedClaimedTask("nudge-window-wedge")
	assignee := s.reloadTask(c.TaskID).AssigneeSession

	cfg := s.cfg
	cfg.LeaseDuration = 10 * time.Minute
	cfg.NudgeMeasureWindow = 300 * time.Millisecond

	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	nudger := &fakeNudger{}
	recon := s.pin2100Recon(cfg, lives, nudger.Send)

	s.expireLease(c.TaskID)
	s.rc1Sweep(recon)
	require.Equal(s.T(), 1, nudger.count())

	// The sentinel positioned: the next observation answers the ordinal
	// question with silence.
	lives.set(assignee, wedgeQuiet())
	time.Sleep(450 * time.Millisecond) // the window elapses (≪ the 10m lease)
	s.rc1Sweep(recon)

	row := s.rawTask(c.TaskID)
	require.Equal(s.T(), board.StageFailed, row.Stage,
		"the wedge verdict lands at window+sweep, NOT lease+sweep — the challenge's cost is the window")
	require.Nil(s.T(), row.ClaimToken, "the verdict clears the dead claim")
}

func (s *BoardSuite) TestPin_P3_LateResponderInsideWindowRenews() {
	_, c := s.seedClaimedTask("late-responder-inside-window")
	assignee := s.reloadTask(c.TaskID).AssigneeSession

	cfg := s.cfg
	cfg.LeaseDuration = 10 * time.Minute
	cfg.NudgeMeasureWindow = 2 * time.Second

	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	nudger := &fakeNudger{}
	recon := s.pin2100Recon(cfg, lives, nudger.Send)

	s.expireLease(c.TaskID)
	s.rc1Sweep(recon)
	require.Equal(s.T(), 1, nudger.count())
	slotID, _ := s.nudgeState(c.TaskID)
	require.NotEmpty(s.T(), slotID, "the sentinel is positioned")

	// The response lands INSIDE the window (window−~1.9s), declared
	// watermark stale (motionless — below cap, the budget counts).
	res, err := s.svc.Heartbeat(context.Background(), s.agent(assignee, c.ClaimToken), c.TaskID, "", s.staleDeclare(c.TaskID))
	require.NoError(s.T(), err)
	require.True(s.T(), res.Extended, "a responder inside the window renews — the challenge survives the shorter window")

	slotAfter, _ := s.nudgeState(c.TaskID)
	require.Empty(s.T(), slotAfter, "the slot cleared — the wedge clock is dead")
	s.leaseFuture(c.TaskID)
}

func (s *BoardSuite) TestPin_P2_HeartbeatArrivalOnlyRenewsAndStampsServerClock() {
	_, c := s.seedClaimedTask("arrival-only-heartbeat")
	assignee := s.reloadTask(c.TaskID).AssigneeSession

	cfg := s.cfg
	cfg.NudgeMeasureWindow = 2 * time.Second
	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	nudger := &fakeNudger{}
	recon := s.pin2100Recon(cfg, lives, nudger.Send)

	s.expireLease(c.TaskID)
	s.rc1Sweep(recon)
	slotID, _ := s.nudgeState(c.TaskID)
	require.NotEmpty(s.T(), slotID, "the sentinel is positioned")

	// RESPONSE-OBSERVED PRESENCE: no declared watermark at all (the
	// zero time) — the arrival itself is the response; the STORE's
	// clock is the only one written.
	res, err := s.svc.Heartbeat(context.Background(), s.agent(assignee, c.ClaimToken), c.TaskID, "", time.Time{})
	require.NoError(s.T(), err)
	require.True(s.T(), res.Extended, "a watermark-less heartbeat response renews the nudged row")

	slotAfter, _ := s.nudgeState(c.TaskID)
	require.Empty(s.T(), slotAfter, "the slot cleared")
	s.leaseFuture(c.TaskID)

	var progress time.Time
	require.NoError(s.T(), s.pool.QueryRow(context.Background(),
		`SELECT last_progress_at FROM tasks WHERE id=$1`, c.TaskID).Scan(&progress))
	mint := s.orbitMint(c.TaskID)
	require.True(s.T(), progress.After(mint),
		"the arrival stamps the SERVER clock (post-mint) — pre-fix a zero stamp stores year-1, the client-hoped clock")
	require.WithinDuration(s.T(), time.Now().UTC(), progress, 2*time.Minute,
		"the stamp is the server's observation, not a declared watermark")
}

func (s *BoardSuite) TestPin_2100_HealthEventChannelLine() {
	_, c := s.seedClaimedTask("health-event-channel")

	// No provider wired: the line is ABSENT (back-compat for every
	// existing health consumer).
	h, err := s.svc.BoardHealth(context.Background(), testUser)
	require.NoError(s.T(), err)
	require.Nil(s.T(), h.EventChannel, "unwired source ⇒ no event_channel line")

	svc2 := board.NewService(s.pool, s.cfg, board.WithLivenessSource(&fakeLiveness{}))
	stale := func() time.Time { return time.Now().UTC().Add(-20 * time.Minute) }
	svc2.SetTurnEndFoldSource(stale)

	// The task claims exist but under the SUITE's user; scope via the
	// workspace surface (BoardHealthInWorkspace) so the claim counts.
	var wsID string
	require.NoError(s.T(), s.pool.QueryRow(context.Background(),
		`SELECT p.target_workspace_id::text FROM tasks t JOIN projects p ON p.id=t.project_id WHERE t.id=$1`,
		c.TaskID).Scan(&wsID))

	h2, err := svc2.BoardHealthInWorkspace(context.Background(), wsID)
	require.NoError(s.T(), err)
	require.NotNil(s.T(), h2.EventChannel, "the line renders when the source is wired")
	require.Equal(s.T(), 1, h2.EventChannel.ClaimsActive, "claims-active counts the live claim")
	require.True(s.T(), h2.EventChannel.Degraded, "stale fold age + active claims ⇒ DEGRADED (the #2100 gap, closed)")
	require.GreaterOrEqual(s.T(), h2.EventChannel.LastFoldAgeSeconds, float64(1190),
		"the stale age renders in seconds")

	fresh := func() time.Time { return time.Now().UTC() }
	svc2.SetTurnEndFoldSource(fresh)
	h3, err := svc2.BoardHealthInWorkspace(context.Background(), wsID)
	require.NoError(s.T(), err)
	require.NotNil(s.T(), h3.EventChannel)
	require.False(s.T(), h3.EventChannel.Degraded, "a fresh fold channel is healthy")
}
