// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// ladder_paused_renewal_audit_2100_integration_test.go — #2100 R3/R4's
// engine-side born-red pins (the post-#2100 prod-regression lane; the
// adapter-side R2 pin lives with the hub it tests, in
// api/internal/app/board_liveness_test.go).
//
//	R3-paused    a PAUSED row reaches its expiry and LAPSES: the ladder
//	             never consults liveness for it, never sends a
//	             sentinel, never renews — the row re-queues to backlog
//	             where the paused flag parks it (pause.go's documented
//	             LAPSE-not-freeze accounting). Prod proof: the probe
//	             paused at 23:30:59 kept taking nudge+renew phases —
//	             nudges, renewals and heartbeat responses for days
//	             while paused. Born-red against v0.44.19 (the victim
//	             scan carried no pause predicate at all).
//	R3-contrast  the same row UNPAUSED still takes its arm (the
//	             exemption exempts the ladder's arms, never the scan:
//	             an unpaused quiet row is nudged exactly as before).
//	R3-scope     the exemption is the EFFECTIVE pause predicate
//	             (task / project / epic) — the same one rule
//	             effectivePausedTx applies to writes.
//	R4-renew     every ladder renewal mints ONE board_events 'renew'
//	             row (source + both lease clocks in the payload): the
//	             audit trail answers "why is this claim still alive"
//	             without a metrics dashboard. Prod proof: the 2.5-day
//	             zombie (~40 renewals, zero nudges, zero events) was
//	             invisible on every external surface. Born-red against
//	             v0.44.19 (the renew arm minted nothing).
//	R4-cadence   consecutive renewal cycles mint DISTINCT rows — the
//	             id keys on the expiry the renewal replaced, so an
//	             orbit reads as its true cycle count (the trail's
//	             flood premise does not survive the ladder's own
//	             cadence: one renewal per expiry sweep, ~2/day/row).

import (
	"context"
	"encoding/json"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// renewEvents reads one task's renewal audit rows (the R4 surface).
func (s *BoardSuite) renewEvents(taskID string) []map[string]any {
	t := s.T()
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT payload FROM board_events WHERE action='renew' AND item_id=$1 ORDER BY id`, taskID)
	s.Require().NoError(err)
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var payload []byte
		s.Require().NoError(rows.Scan(&payload))
		var m map[string]any
		s.Require().NoError(json.Unmarshal(payload, &m))
		out = append(out, m)
	}
	s.Require().NoError(rows.Err())
	return out
}

// TestPin_2100R3_PausedRowLapsesNotRenews — the headline R3 pin.
func (s *BoardSuite) TestPin_2100R3_PausedRowLapsesNotRenews() {
	_, c := s.seedClaimedTask("paused-ladder-exempt")
	taskID := c.TaskID
	assignee := s.reloadTask(taskID).AssigneeSession
	require.NotEmpty(s.T(), assignee)

	_, err := s.svc.PauseTask(context.Background(), s.owner(), taskID, "", "")
	s.Require().NoError(err)

	// A row whose renewal arms WOULD fire: observed, present, with
	// movement evidence (the rung that renewed the prod zombie every
	// ~45m). Pre-fix this shape renewed forever and never lapsed.
	lives := &fakeLiveness{}
	lives.set(assignee, liveMoving())
	nudger := &fakeNudger{}
	recon, rec := s.pinReconWithSink(lives, nudger.Send)

	s.expireLease(taskID)
	s.rc1Sweep(recon)

	s.Equal(board.StageBacklog, s.rawTask(taskID).Stage,
		"BORN-RED: the paused row LAPSES at its first expiry sweep (pause.go: a long pause naturally reclaims — the paused flag parks the re-queued row in backlog); pre-fix the ladder renewed it forever")
	s.Nil(s.rawTask(taskID).ClaimToken, "the lapse clears the dead claim")
	s.Empty(s.reloadTask(taskID).AssigneeSession, "the lapse releases the assignee")
	s.Equal(0, nudger.count(), "PAUSE MUST PAUSE the ladder: no sentinel was ever sent to a worker the HALT directive already stopped")
	s.Zero(lives.askedFor(assignee),
		"the paused row never reached the observation plane either — the arms it is exempt from are exactly the arms that consult it")

	var failureCount int
	s.Require().NoError(s.pool.QueryRow(context.Background(),
		`SELECT failure_count FROM tasks WHERE id=$1`, taskID).Scan(&failureCount))
	s.Equal(1, failureCount,
		"the lapse IS lapse accounting (the lease clock runs during a pause): #1813's attempt count, never a freeze")

	events, _ := rec.snapshot()
	s.Require().Len(events, 1, "the lapse projects one reclaim decision")
	s.Equal(board.LeaseEventReclaimed, events[0].Kind)
	s.Equal(board.ReclaimReasonPausedLapse, events[0].Reason,
		"and names WHY (paused_lapse: the row was never asked whether it was alive)")
}

// TestPin_2100R3_UnpausedRowStillTakesTheLadder — the contrast arm: the
// exemption covers the ladder's arms, never the scan. An unpaused
// quiet row is nudged exactly as before, and the observation plane is
// consulted for it.
func (s *BoardSuite) TestPin_2100R3_UnpausedRowStillTakesTheLadder() {
	_, c := s.seedClaimedTask("unpaused-still-nudged")
	taskID := c.TaskID
	assignee := s.reloadTask(taskID).AssigneeSession

	lives := &fakeLiveness{}
	lives.set(assignee, liveQuiet())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	s.expireLease(taskID)
	s.rc1Sweep(recon)

	s.Equal(board.StageInProgress, s.rawTask(taskID).Stage, "the unpaused row stays claimed (the ladder owns it)")
	s.Equal(1, nudger.count(), "the quiet unpaused row takes its nudge arm (no regression)")
	s.NotZero(lives.askedFor(assignee), "and the observation plane was consulted for it")
}

// TestPin_2100R3_PauseScopeIsEffective — the exemption reads the SAME
// one-predicate rule effectivePausedTx applies to writes: the row, its
// PROJECT, or its EPIC.
func (s *BoardSuite) TestPin_2100R3_PauseScopeIsEffective() {
	for _, kind := range []struct {
		name string
		arm  func(s *BoardSuite, taskID, projectID, epicID string)
	}{{
		name: "task",
		arm: func(s *BoardSuite, taskID, projectID, epicID string) {
			_, err := s.svc.PauseTask(context.Background(), s.owner(), taskID, "", "")
			s.Require().NoError(err)
		},
	}, {
		name: "project",
		arm: func(s *BoardSuite, taskID, projectID, epicID string) {
			_, err := s.svc.PauseProject(context.Background(), s.owner(), projectID, "", "")
			s.Require().NoError(err)
		},
	}, {
		name: "epic",
		arm: func(s *BoardSuite, taskID, projectID, epicID string) {
			_, err := s.pool.Exec(context.Background(), `UPDATE epics SET status='paused' WHERE id=$1`, epicID)
			s.Require().NoError(err)
		},
	}} {
		s.Run(kind.name, func() {
			_, c := s.seedClaimedTask("pause-scope-" + kind.name)
			taskID := c.TaskID
			assignee := s.reloadTask(taskID).AssigneeSession
			var projectID, epicID string
			s.Require().NoError(s.pool.QueryRow(context.Background(),
				`SELECT project_id::text, epic_id::text FROM tasks WHERE id=$1`, taskID).Scan(&projectID, &epicID))

			lives := &fakeLiveness{}
			lives.set(assignee, liveMoving())
			nudger := &fakeNudger{}
			recon := s.pinRecon(lives, nudger.Send)

			kind.arm(s, taskID, projectID, epicID)
			s.expireLease(taskID)
			s.rc1Sweep(recon)

			s.Equal(board.StageBacklog, s.rawTask(taskID).Stage,
				"the %s-level pause exempts the ladder exactly as the row-level one does", kind.name)
			s.Equal(0, nudger.count(), "no sentinel under a %s-level pause", kind.name)
			s.Zero(lives.askedFor(assignee), "no observation consult under a %s-level pause", kind.name)
		})
	}
}

// TestPin_2100R4_RenewalMintsAuditEvent — the R4 pin: the ladder's
// renewal is auditable on the trail itself (the charter's chosen
// surface; the per-source counters stay as the aggregate signal).
func (s *BoardSuite) TestPin_2100R4_RenewalMintsAuditEvent() {
	_, c := s.seedClaimedTask("renew-audit")
	taskID := c.TaskID
	assignee := s.reloadTask(taskID).AssigneeSession
	s.armLiveLease(taskID)

	lives := &fakeLiveness{}
	lives.set(assignee, liveMoving())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	renewsBefore := testutil.ToFloat64(board.LeaseRenewals().WithLabelValues(board.RenewSourceMovement))

	s.expireLease(taskID)
	s.rc1Sweep(recon)

	s.Equal(board.StageInProgress, s.rawTask(taskID).Stage, "the moving row renewed (it is alive)")
	s.leaseFuture(taskID)
	s.Equal(0, nudger.count(), "movement renews without a sentinel")
	s.Equal(renewsBefore+1, testutil.ToFloat64(board.LeaseRenewals().WithLabelValues(board.RenewSourceMovement)),
		"the aggregate counter still moves (the counters stay; the trail gains per-claim attribution)")

	events := s.renewEvents(taskID)
	s.Require().Len(events, 1,
		"BORN-RED: the renewal minted exactly ONE audit row — pre-fix it minted zero, and the 2.5-day zombie was invisible on every external surface an operator reads")
	m := events[0]
	s.Equal(taskID, m["task_id"])
	s.Equal(board.RenewSourceMovement, m["source"], "the row says WHICH rung answered alive")
	s.NotEmpty(m["lapsed_lease_expires_at"], "…the expiry this renewal replaced…")
	s.NotEmpty(m["lease_expires_at"], "…and the one it minted (the next expiry sweep's schedule)")

	// The row-scoped read surface: the same trail is one task read
	// away — no metrics dashboard, no log forensics.
	s.Equal(1, s.countEventsFor("renew", taskID), "the trail answers per-claim, not only in aggregate")
}

// TestPin_2100R4_RenewalCadenceIsDistinctRows — the trail is per-renewal,
// not per-claim: an orbit reads as its true cycle count (the zombie's
// ~40 cycles would have been ~40 rows on one task), at the ladder's own
// cadence — one renewal per expiry sweep, ~2/day/row at the 45m
// default, the same order as the nudge/reclaim rows the trail already
// carries per lapse cycle.
func (s *BoardSuite) TestPin_2100R4_RenewalCadenceIsDistinctRows() {
	_, c := s.seedClaimedTask("renew-audit-cadence")
	taskID := c.TaskID
	assignee := s.reloadTask(taskID).AssigneeSession
	s.armLiveLease(taskID)

	lives := &fakeLiveness{}
	lives.set(assignee, liveMoving())
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)

	for cycle := 1; cycle <= 3; cycle++ {
		s.expireLease(taskID)
		s.rc1Sweep(recon)
		s.Equal(board.StageInProgress, s.rawTask(taskID).Stage, "cycle %d: still alive, still claimed", cycle)
		s.Len(s.renewEvents(taskID), cycle,
			"cycle %d: distinct rows per renewal cycle — the orbit is VISIBLE at its true rate (the id keys on the expiry each renewal replaced)", cycle)
	}
	s.Equal(0, nudger.count(), "a moving row never takes a sentinel")
}
