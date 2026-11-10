// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// busy_ttl_integration_test.go — RC-C S2 pins (the 2026-10-30 incident's
// dishonest busy badge): the spinner must reflect LIVENESS, never claim
// state, and a claim past its lease without renewal must stop reporting
// busy whatever the harness's busy truth says (the busy TTL).
//
// The ladder-side budget semantics (boardBusyRenewCap bounds busy-ONLY
// renewals; movement renews uncapped) are NOT re-pinned here — pin 12
// (lease_pins_test.go) owns them; the chain pin below asserts the READ
// side mirrors the ladder: busy through the busy-only renewals (lease
// re-armed ⇒ badge honestly mid-turn), quiet the moment no renewal
// lands and the lease passes (the TTL), gone once the verdict clears
// the claim.

import (
	"context"
	"sync"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// fakeBusySource scripts the badge seam: per-ref busy answers plus the
// asked-ref tape (the TTL gate's pin — an ineligible row is never even
// asked).
type fakeBusySource struct {
	mu    sync.Mutex
	busy  map[string]bool
	asked []string
}

func (f *fakeBusySource) BusyAssignees(_ context.Context, refs []string) map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, refs...)
	out := make(map[string]bool, len(refs))
	for _, r := range refs {
		if f.busy[r] {
			out[r] = true
		}
	}
	return out
}

func (f *fakeBusySource) set(ref string, busy bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy == nil {
		f.busy = map[string]bool{}
	}
	f.busy[ref] = busy
}

func (f *fakeBusySource) askedRefs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// busySvc builds the suite service plus the badge seam (SetupTest's
// option set verbatim, one addition).
func (s *BoardSuite) busySvc(src board.AssigneeBusySource) *board.Service {
	return board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(s.spawner.Spawn),
		board.WithWorkerAborter(s.aborter.Abort),
		board.WithPRDriftSource(s.drift),
		board.WithPendingInputSource(s.pending),
		board.WithFailpoint(s.fps.fp),
		board.WithRoleDirectory(personaRoleDirectory()),
		board.WithBusySource(src),
	)
}

// armLease deterministically future-arms a claim's lease (the suite's
// 200ms LeaseDuration makes "recently claimed" racy; pins must not
// depend on read latency).
func (s *BoardSuite) armLease(taskID string) {
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET lease_expires_at = now() + interval '1 hour' WHERE id=$1`, taskID)
	s.Require().NoError(err)
}

// busyOf lists the project through the busy-seamed service and returns
// the row's AssigneeBusy by short id.
func (s *BoardSuite) busyOf(svc *board.Service, shortID, projectID string) bool {
	rows, err := svc.ListTasks(context.Background(), projectID, "", "")
	s.Require().NoError(err)
	for _, r := range rows {
		if r.ShortID == shortID {
			return r.AssigneeBusy
		}
	}
	s.T().Fatalf("row %s not found in list", shortID)
	return false
}

// TestBusyBadge_ClaimedBusySpinsClaimedQuietQuiets — the honesty pin:
// with a live lease, the badge is exactly the seam's answer (busy ⇒
// busy; quiet ⇒ quiet), on the list AND the single read.
func (s *BoardSuite) TestBusyBadge_ClaimedBusySpinsClaimedQuietQuiets() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "badge-honesty", nil, false)

	src := &fakeBusySource{}
	svc := s.busySvc(src)
	claim, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	s.Require().NotNil(claim.Task)
	assignee := s.reloadTask(task.ID).AssigneeSession
	s.Require().NotEmpty(assignee, "the claim stamps the assignee (S1)")
	s.armLease(task.ID)

	// BUSY: the harness says mid-turn → the row says busy.
	src.set(assignee, true)
	s.True(s.busyOf(svc, task.ShortID, p.ID), "claimed+live-lease+mid-turn ⇒ busy")
	got, err := svc.GetTask(context.Background(), task.ID, "")
	s.Require().NoError(err)
	s.True(got.AssigneeBusy, "GetTask carries the same truth as the list")

	// QUIET: the harness says idle → the row says quiet (claimed but
	// quiet — the honest idle shape, never the spin).
	src.set(assignee, false)
	s.False(s.busyOf(svc, task.ShortID, p.ID), "claimed+live-lease+idle ⇒ quiet")
}

// TestBusyBadge_TTL_PastLeaseWithoutRenewalIsNotBusy — the TTL pin: a
// claim past its lease without renewal is NOT busy even when the busy
// source is latched busy-true (the dead-pod stale-gate shape); a claim
// with no lease at all is equally not working; and neither row is ever
// CONSULTED (the gate filters before the seam).
func (s *BoardSuite) TestBusyBadge_TTL_PastLeaseWithoutRenewalIsNotBusy() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	live := s.seedTask(e, "ttl-live", nil, false)
	lapsed := s.seedTask(e, "ttl-lapsed", nil, false)
	leaseless := s.seedTask(e, "ttl-leaseless", nil, false)

	src := &fakeBusySource{}
	svc := s.busySvc(src)
	for range []*board.Task{live, lapsed, leaseless} {
		_, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
		s.Require().NoError(err)
	}
	// Three claims minted FIFO (creation order): reload all assignees.
	liveA := s.reloadTask(live.ID).AssigneeSession
	lapsedA := s.reloadTask(lapsed.ID).AssigneeSession
	leaselessA := s.reloadTask(leaseless.ID).AssigneeSession
	s.Require().NotEmpty(liveA)
	s.Require().NotEmpty(lapsedA)
	s.Require().NotEmpty(leaselessA)
	s.Require().NotEqual(liveA, lapsedA, "distinct claims stamp distinct sessions")

	s.armLease(live.ID)
	s.expireLease(lapsed.ID)
	_, err := s.pool.Exec(context.Background(),
		`UPDATE tasks SET lease_expires_at=NULL WHERE id=$1`, leaseless.ID)
	s.Require().NoError(err)

	// The pathological source: busy-true for EVERY ref.
	src.set(liveA, true)
	src.set(lapsedA, true)
	src.set(leaselessA, true)

	rows, err := svc.ListTasks(context.Background(), p.ID, "", "")
	s.Require().NoError(err)
	byShort := map[string]bool{}
	for _, r := range rows {
		byShort[r.ShortID] = r.AssigneeBusy
	}
	s.True(byShort[live.ShortID], "live lease + busy source ⇒ busy (the TTL passes it through)")
	s.False(byShort[lapsed.ShortID], "PAST LEASE WITHOUT RENEWAL ⇒ NOT busy, whatever the source says")
	s.False(byShort[leaseless.ShortID], "no lease at all ⇒ not busy")

	// The gate filters BEFORE the seam: only the live claim's assignee
	// was ever asked (an ineligible row is not a badge question).
	s.Equal([]string{liveA}, src.askedRefs(), "the TTL gate consults only the live-lease claim")
}

// TestBusyBadge_InertSeamNeverBusy — the Nil contract (liveness.go's
// house pattern): with no seam wired, no row is ever busy; claimed rows
// render claimed-but-quiet — the honest degrade, never the spin.
func (s *BoardSuite) TestBusyBadge_InertSeamNeverBusy() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "inert-seam", nil, false)

	s.claimOrFail(ws) // the suite's default service: no busy seam
	s.armLease(task.ID)
	s.False(s.busyOf(s.svc, task.ShortID, p.ID),
		"no busy source wired ⇒ never busy (unknown quiets — the inert contract)")
}

// TestBusyBadge_RenewalBudgetChain — the read side mirrors the ladder
// through a stuck-busy source: busy-ONLY renewals (within the cap) keep
// the lease alive and the badge honestly busy; when the budget
// exhausts and the verdict lands, the claim is gone and the badge
// cannot spin. The ladder's own writes are pin 12's; this pin walks the
// SAME shape reading between sweeps.
func (s *BoardSuite) TestBusyBadge_RenewalBudgetChain() {
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 4)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "stuck-busy-chain", nil, false)

	src := &fakeBusySource{}
	svc := s.busySvc(src)
	_, err := svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	assignee := s.reloadTask(task.ID).AssigneeSession
	s.Require().NotEmpty(assignee)

	// The pathological pair, agreeing: the sweep's liveness says
	// busy-true forever; the badge's busy source says the same.
	lives := &fakeLiveness{}
	nudger := &fakeNudger{}
	recon := s.pinRecon(lives, nudger.Send)
	stuckBusy := board.AssigneeObservation{Observed: true, Exists: true, Busy: true}
	lives.set(assignee, stuckBusy)
	src.set(assignee, true)

	// Within the budget: busy-only renewals re-arm the lease and the
	// badge stays honestly busy (mid-turn on a LIVE lease).
	for round := 0; round < 3; round++ {
		s.expireLease(task.ID)
		s.rc1Sweep(recon)
		s.Equal(board.StageInProgress, s.rawTask(task.ID).Stage,
			"round %d: busy-only renewal within the cap (pin 12's semantics, unchanged)", round)
		s.leaseFuture(task.ID)
		s.True(s.busyOf(svc, task.ShortID, p.ID),
			"round %d: renewed lease + busy source ⇒ the badge reports the mid-turn", round)
	}

	// Past the cap the ladder stops honoring Busy (pin 12): the quiet
	// arms proceed — the sentinel is positioned first.
	s.expireLease(task.ID)
	s.rc1Sweep(recon)
	s.Equal(1, nudger.count(), "past the cap the quiet arms proceed: the nudge rung (pin 12's semantics, unchanged)")
	nid, _ := s.nudgeState(task.ID)
	s.Require().NotEmpty(nid, "the exhaustion opened a measurable episode")

	// The verdict sweep: positioned sentinel, the SAME stuck-busy
	// silence — Busy no longer saves it, the claim is cleared (no
	// lease, no assignee, no in_progress: nothing left for the badge
	// to spin on, whatever the stale source still says).
	lives.set(assignee, board.AssigneeObservation{
		Observed: true, Exists: true, Busy: true,
		NudgeMarkKnown: true, EventsAfterNudge: false,
	})
	s.expireLease(task.ID)
	s.rc1Sweep(recon)
	s.Equal(board.StageFailed, s.rawTask(task.ID).Stage, "pin 12's verdict: stuck-busy cannot hold the lease forever")
	s.False(s.busyOf(svc, task.ShortID, p.ID), "the verdict'd row reports busy nowhere")

	// And the honest middle: the same stuck source, a lapsed lease with
	// NO renewal landed (the pre-verdict quiet window) — the TTL quiets
	// the badge even while the row still says in_progress elsewhere.
	other := s.seedTask(e, "stuck-busy-past-lease", nil, false)
	_, err = svc.ClaimNext(context.Background(), "worker-"+ws, ws)
	s.Require().NoError(err)
	otherAssignee := s.reloadTask(other.ID).AssigneeSession
	s.Require().NotEmpty(otherAssignee)
	src.set(otherAssignee, true)
	s.expireLease(other.ID)
	s.False(s.busyOf(svc, other.ShortID, p.ID),
		"stale busy truth + past lease + no renewal ⇒ quiet (the TTL's exact shape)")
}
