// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// reconciler.go — the self-healing convergence sweep (design §4.5):
//
//	RC1 lease reclaims — guarded UPDATE, NULL-lease-inclusive belt;
//	RC2 external-truth drift — pr:N anchors on in_review rows (event-fed
//	   off the watermark: only rows whose in_review transition is newer
//	   than the watermark are checked);
//	pending coupling — worker-origin auto-stage + resolution resume (lease
//	   re-armed, NO mint: the held token and its provenance flag are
//	   untouched, r11 fix);
//	RC3 dependency consistency — is_blocked recompute from the edge table
//	   (the belt beneath in-transaction maintenance);
//	RC4 epic completion net — missed terminal events from the watermark;
//	RC5 terminal immutability — never writes terminal rows (predicates +
//	   the DB stage guard are the belts).
//
// The sweep is idempotent end-to-end: every reconciler write carries a
// deterministic board_events id (reconciler:<action>:<item>:<episode>), so
// a crash between passes replays to the same state (§6 invariant 4). Each
// phase is its own transaction; a fault at any failpoint aborts only that
// phase and the next sweep converges. The reconciler is ownership-agnostic
// (§4.7): it keys on rows and tokens, never on owner identity.

const sweepScope = "default"

// boardNudgeDeliveryConcurrency bounds the concurrent sentinel-delivery
// batch inside one RC1 pass (each delivery carries a ~10s fast-fail
// window; the victims' row locks are held until commit — the batch
// collapses an N-victim stall to ~one window).
const boardNudgeDeliveryConcurrency = 8

// Failpoint names (the kill-test fault seams).
const (
	fpRC1Pre       = "rc1.pre"
	fpRC1Post      = "rc1.post"
	fpPersonaPre   = "persona.pre"
	fpPersonaPost  = "persona.post"
	fpDispatchPre  = "dispatch.pre"
	fpPendingPre   = "pending.pre"
	fpRC2Pre       = "rc2.pre"
	fpRC3Pre       = "rc3.pre"
	fpRC4Pre       = "rc4.pre"
	fpWatermarkPre = "watermark.pre"
)

// Reconciler is the periodic convergence sweep (default 30s cadence; the
// sweep itself is stateless between passes apart from the watermark).
type Reconciler struct {
	svc *Service
	// obsPruneLast stamps the S7 observation-trail prune's last run
	// (zero = never). Instance-scoped on purpose (review r1: no
	// package-level mutable state — test reconcilers must not couple
	// to one shared timestamp); touched only from Start's single
	// sweep loop and manual Sweeps, and a racing duplicate prune is a
	// harmless idempotent DELETE.
	obsPruneLast time.Time
}

// NewReconciler constructs the reconciler over a Service.
func NewReconciler(pool *pgxpool.Pool, cfg EngineConfig, opts ...Option) *Reconciler {
	return &Reconciler{svc: NewService(pool, cfg, opts...)}
}

// SetEventPublisher installs the §4.6 event projection on the underlying
// service (reconciler writes publish through it).
func (r *Reconciler) SetEventPublisher(pub EventPublisher) { r.svc.SetEventPublisher(pub) }

// SweepResultDetail carries per-phase counters (SweepResult is the public
// summary; phases append here).

// Sweep runs one full pass. Phases: RC1 → pending coupling → RC2 → RC3 →
// RC4 net → watermark advance.
func (r *Reconciler) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	s := r.svc

	// S7: the observation trail's retention pass rides the sweep ITSELF
	// (gated hourly inside) — the Start loop simply calls Sweep; a
	// manual sweep enforces the boundary the same way the docs promise.
	r.maybePruneObservations(ctx)

	wm, err := r.readWatermark(ctx)
	if err != nil {
		return res, err
	}
	cutoff := wm
	// The sweep's high-water is captured BEFORE any phase runs (r1-C3):
	// events committed by concurrent writers DURING this sweep are
	// strictly greater than cutoff, so advancing to the pre-captured max
	// (not a post-phase re-read) never skips them — the next sweep
	// re-scans (cutoff, sweepHigh].
	sweepHigh, err := r.maxEventAt(ctx)
	if err != nil {
		return res, err
	}

	// --- RC1: lease reclaims -------------------------------------------
	if err := s.failAt(ctx, fpRC1Pre); err != nil {
		return res, err
	}
	reclaims, err := s.rc1Reclaims(ctx)
	if err != nil {
		return res, err
	}
	res.Reclaims = reclaims
	if err := s.failAt(ctx, fpRC1Post); err != nil {
		return res, err
	}

	// --- stage-persona claim reclaims (in_review lapse + re-arm) --------
	if err := s.failAt(ctx, fpPersonaPre); err != nil {
		return res, err
	}
	personaOut, err := s.rc1PersonaClaims(ctx)
	if err != nil {
		return res, err
	}
	res.PersonaReclaims = personaOut.applied
	if err := s.failAt(ctx, fpPersonaPost); err != nil {
		return res, err
	}
	// Post-commit (r1-C5): the re-arm mints committed above — the
	// fresh persona workers spawn only now.
	for _, n := range personaOut.spawns {
		n.fire(ctx, s)
	}

	// --- pending-input coupling (worker origin) ------------------------
	if err := s.failAt(ctx, fpPendingPre); err != nil {
		return res, err
	}
	staged, resumed, err := s.pendingCoupling(ctx)
	if err != nil {
		return res, err
	}
	res.PendingStaged = staged
	res.Resumes = resumed

	// --- dispatch drive (§4.4, r1-C2): the sweep backstop ---------------
	// "Event-driven with a sweep backstop": RC1's reclaims (and any work
	// the event path missed) are re-fed to workers HERE — a reclaimed
	// task never sits in backlog. Runs only with a spawn seam installed
	// (Lane 1's app wiring; without a spawner, granting would mint leases
	// nobody holds and churn reclaims every sweep).
	if err := s.failAt(ctx, fpDispatchPre); err != nil {
		return res, err
	}
	dispatched, err := s.dispatchDrive(ctx)
	if err != nil {
		return res, err
	}
	res.Dispatched = dispatched

	// --- RC2: external-truth drift (event-fed) --------------------------
	if err := s.failAt(ctx, fpRC2Pre); err != nil {
		return res, err
	}
	if s.drift != nil {
		done, review, reentry, err := s.rc2Drift(ctx, cutoff)
		if err != nil {
			return res, err
		}
		res.DriftDone, res.DriftReview, res.DriftReentry = done, review, reentry
	}

	// --- RC3: dependency consistency ------------------------------------
	if err := s.failAt(ctx, fpRC3Pre); err != nil {
		return res, err
	}
	repairs, err := s.rc3Dependencies(ctx)
	if err != nil {
		return res, err
	}
	res.DepRepairs = repairs

	// --- RC4 net: missed terminal events ---------------------------------
	if err := s.failAt(ctx, fpRC4Pre); err != nil {
		return res, err
	}
	completions, err := s.rc4Net(ctx, cutoff)
	if err != nil {
		return res, err
	}
	res.EpicCompletions = completions

	// --- watermark advance ------------------------------------------------
	if err := s.failAt(ctx, fpWatermarkPre); err != nil {
		return res, err
	}
	if err := r.advanceWatermark(ctx, cutoff, sweepHigh); err != nil {
		return res, err
	}
	return res, nil
}

// failAt runs the fault seam (kill-tests).
func (s *Service) failAt(ctx context.Context, where string) error {
	if s.failpoint == nil {
		return nil
	}
	if err := s.failpoint(where); err != nil {
		return fmt.Errorf("board: reconciler fault at %s: %w", where, err)
	}
	return nil
}

// readWatermark loads (or seeds) the incremental cursor. The seed is the
// epoch so the first sweep replays history once — drift checks are
// bounded by the in_review predicate, and the RC4 net by done events.
func (r *Reconciler) readWatermark(ctx context.Context) (time.Time, error) {
	var wm time.Time
	pool := r.svc.store.pool
	err := pool.QueryRow(ctx, `
		INSERT INTO board_sweeps (scope, last_event_at) VALUES ($1, to_timestamp(0))
		ON CONFLICT (scope) DO NOTHING
		RETURNING last_event_at`, sweepScope).Scan(&wm)
	if errors.Is(err, pgx.ErrNoRows) {
		err = pool.QueryRow(ctx, `SELECT last_event_at FROM board_sweeps WHERE scope=$1`, sweepScope).Scan(&wm)
	}
	if err != nil {
		return time.Time{}, err
	}
	return wm, nil
}

// maxEventAt reads the current max event timestamp (the sweep's
// high-water capture point).
func (r *Reconciler) maxEventAt(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := r.svc.q().QueryRow(ctx, `SELECT max(created_at) FROM board_events`).Scan(&t)
	return t, err
}

func (r *Reconciler) advanceWatermark(ctx context.Context, cutoff, sweepHigh time.Time) error {
	// Advance to the sweep's PRE-CAPTURED high-water (r1-C3): events
	// committed after the capture stay strictly above the watermark and
	// are re-scanned next sweep — never skipped. Monotonic (never below
	// the existing cursor).
	next := sweepHigh
	if next.IsZero() || next.Before(cutoff) {
		next = cutoff
	}
	_, err := r.svc.q().Exec(ctx, `
		UPDATE board_sweeps SET last_event_at=$2 WHERE scope=$1 AND last_event_at < $2`,
		sweepScope, next)
	return err
}

// boardBusyRenewCap bounds the busy-gate's deferral engine-wide (the
// ladder's own bound, rc1's review round 1 blocking finding): past
// this many consecutive busy-ONLY renewals in one claim epoch, Busy
// evidence is exhausted — the ladder proceeds as if busy were false,
// and S5's depose guard (depose.go) refuses on the SAME exhausted
// truth (a source latched busy-true forever must neither hold a lease
// forever nor lock the salvage lever out of the zombie row).
const boardBusyRenewCap = 3

// rc1Reclaims reclaims every lapsed in_progress row: guarded UPDATE
// (stage predicate re-checked at write time; SKIP LOCKED lets concurrent
// sweeps divide the work), attempts untouched (the increment belongs to
// the next claim), deterministic episode event ids.
func (s *Service) rc1Reclaims(ctx context.Context) (int, error) {
	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var pending []Event

	rows, err := tx.Query(ctx, `
		SELECT t.id, t.project_id, COALESCE(t.lease_expires_at::text, 'null'), t.failure_count,
			COALESCE(t.assignee_session,''), COALESCE(t.nudge_id,''), COALESCE(t.short_id,''),
			t.nudged_at, t.last_heartbeat_at, t.busy_renews
		FROM tasks t
		WHERE t.stage='in_progress' AND NOT t.pending_input
		  AND (t.lease_expires_at IS NULL OR t.lease_expires_at < now())
		FOR UPDATE OF t SKIP LOCKED`)
	if err != nil {
		return 0, err
	}
	type victim struct {
		id, project, episode string
		failures             int32
		assignee, nudgeID    string
		shortID              string
		nudgedAt             *time.Time
		lastHeartbeat        *time.Time
		busyRenews           int32
	}
	var victims []victim
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.id, &v.project, &v.episode, &v.failures, &v.assignee, &v.nudgeID, &v.shortID,
			&v.nudgedAt, &v.lastHeartbeat, &v.busyRenews); err != nil {
			rows.Close()
			return 0, err
		}
		victims = append(victims, v)
	}
	rows.Close()

	// --- the seamless-lease ladder (000050) ------------------------------
	// Observe FIRST; the wall-clock reclaim below is the fallback for
	// evidence-LESS rows only. Renewals/nudges/wedges write no overlay
	// and no transition-row churn; the ladder's decisions are idempotent
	// under re-sweep (a raced victim simply re-decides next sweep).
	lives := s.lives
	if lives == nil {
		lives = NilLiveness{}
	}
	refs := make([]AssigneeRef, 0, len(victims))
	for _, v := range victims {
		if v.assignee != "" {
			refs = append(refs, AssigneeRef{SessionID: v.assignee, NudgeID: v.nudgeID})
		}
	}
	observations := lives.ObserveAssignees(ctx, SessionObservations{Refs: refs})
	type ladderDecision uint8
	const (
		ladderReclaim ladderDecision = iota // wall-clock fallback (today's path)
		ladderRenew                         // observed movement / post-sentinel reaction / heartbeat response
		ladderNudge                         // quiet: send the sentinel, measure next expiry
		ladderWedge                         // sentinel positioned, nothing after it: failed
	)
	// boardBusyRenewCap bounds the busy-gate's deferral (review round
	// 1's blocking finding): a source latched busy-true forever (dead
	// pod + stalled recreation keeps the gate's truth stale) must not
	// renew a lease forever. busy_renews counts CONSECUTIVE busy-ONLY
	// renewals in one claim epoch — at the cap the ladder stops
	// honoring Busy and the row falls through to the ordinary quiet
	// arms (nudge, then verdict) as if busy were false. Legitimate
	// mid-turn shapes never approach it: the harness timebox bounds a
	// turn and the next tool call's PART_START is movement evidence
	// (which RESETS the count).
	decisions := make(map[string]ladderDecision, len(victims))
	wedgeEvidence := make(map[string]string, len(victims))
	nudgeIDs := make(map[string]string, len(victims))
	// nudgeRung carries the S7 trail's WHICH SENTINEL (observation.go:
	// first_sentinel vs re_anchor) — set at send time, where the row's
	// pre-existing sentinel state answers it.
	nudgeRung := make(map[string]string, len(victims))
	// renewSource / reclaimReason carry the S4 observation plane's WHY
	// (design 0072): which rung answered alive, why a claim was
	// re-queued — the facts the lease-event sink projects post-commit.
	renewSource := make(map[string]string, len(victims))
	reclaimReason := make(map[string]string, len(victims))
	// busyOnlyRenew marks victims whose renewal evidence was Busy ALONE
	// (no movement, no post-sentinel events, no heartbeat response) —
	// the renew write increments their counter; every other renewal
	// zeroes it (consecutive semantics).
	busyOnlyRenew := make(map[string]bool, len(victims))
	type outboundNudge struct {
		v  victim
		id string
	}
	var outbound []outboundNudge
	send := func(v victim) string {
		id := uuidNew()
		outbound = append(outbound, outboundNudge{v: v, id: id})
		decisions[v.id] = ladderNudge
		// The rung: a row already carrying a sentinel is being
		// RE-ANCHORED (this source could not measure the old mark — a
		// duplicate sentinel is benign under latest-only); an empty
		// slot is the episode's first ask.
		if v.nudgeID != "" {
			nudgeRung[v.id] = NudgeRungReAnchor
		} else {
			nudgeRung[v.id] = NudgeRungFirst
		}
		return id
	}
	for _, v := range victims {
		decisions[v.id] = ladderReclaim
		if v.assignee == "" {
			reclaimReason[v.id] = ReclaimReasonNoAssignee
			continue // no identity to observe or nudge: the honest wall-clock path
		}
		// The heartbeat RESPONSE — OR'd in at EVERY rung (item 6
		// amendment): a board_heartbeat arriving after the sentinel is
		// the strong ALIVE signal, whatever the observation seam says.
		// Both timestamps are server-clock row writes — the comparison
		// is ordinal in one clock domain, never cross-process.
		hbAfterNudge := v.nudgeID != "" && v.nudgedAt != nil && v.lastHeartbeat != nil &&
			v.lastHeartbeat.After(*v.nudgedAt)
		o, ok := observations[v.assignee]
		observed := ok && o.Observed
		// The busy-gate's engine-side bound: past boardBusyRenewCap
		// consecutive busy-only renewals, Busy evidence is EXHAUSTED
		// for this claim epoch — honored as false so the quiet arms
		// (nudge, verdict) proceed. A heartbeat or movement arrival
		// resets the row's counter (the renew write's CASE); only a
		// source stuck busy-true-forever reaches the cap.
		busyHonored := o.Busy && v.busyRenews < boardBusyRenewCap
		switch {
		case hbAfterNudge:
			decisions[v.id] = ladderRenew // the worker answered the nudge on the response channel
			renewSource[v.id] = RenewSourceHeartbeat
			continue
		case !observed:
			// RUNG 3 — no-evidence (Nil source, degraded adapter,
			// unindexed session): NEVER death, but not blindness
			// either when a nudge seam exists (item 6's
			// harness-agnostic wedge floor: deliver-a-message +
			// call-a-tool is the MINIMAL harness capability — the
			// heartbeat is the ONLY wedge detector at this rung).
			if s.nudger == nil {
				reclaimReason[v.id] = ReclaimReasonLapse
				continue // no way to ask: the honest wall-clock path (pin 6)
			}
			if v.nudgeID == "" {
				send(v)
				continue
			}
			decisions[v.id] = ladderWedge
			wedgeEvidence[v.id] = "no response to the sentinel (no post-nudge heartbeat; no observation available)"
			continue
		case o.MovedSinceLastSweep, o.EventsAfterNudge, busyHonored:
			// Alive by delta, by reaction to the sentinel — or MID-TURN
			// (Busy, the 2026-10-29 busy-gate): a session running a tool
			// call is alive however silent it looks from the frame
			// stream (the live-fire: `sleep N && poll` workers, quiet
			// for the whole command, killed as wedges three times). The
			// tool timebox bounds the turn; when it ends, the queued
			// nudge delivers and the ladder proceeds against a session
			// that can actually answer. Busy never WEDGES; it renews —
			// until the consecutive-only cap exhausts it (above).
			decisions[v.id] = ladderRenew
			busyOnlyRenew[v.id] = busyHonored && !o.MovedSinceLastSweep && !o.EventsAfterNudge
			switch {
			case busyOnlyRenew[v.id]:
				renewSource[v.id] = RenewSourceBusy
			case o.MovedSinceLastSweep:
				renewSource[v.id] = RenewSourceMovement
			default:
				renewSource[v.id] = RenewSourceReaction
			}
			continue
		case !o.Exists:
			decisions[v.id] = ladderReclaim // dead-evidence: the fast wall-clock path below
			reclaimReason[v.id] = ReclaimReasonDeadEvidence
			continue
		case s.nudger == nil:
			// observed-quiet but no nudge seam: fall back (the nudge
			// ladder is inert without a sender)
			reclaimReason[v.id] = ReclaimReasonLapse
		case v.nudgeID == "":
			send(v)
		case !o.NudgeMarkKnown:
			// The sentinel is positioned but THIS source never saw it
			// (another replica sent it; a restart evicted the mark) —
			// the ordinal question is unanswerable, and an unmeasured
			// silence is NEVER a wedge verdict. Re-nudge: the fresh
			// sentinel re-anchors the ordinal question at a mark THIS
			// source holds (latest-only makes the duplicate benign).
			send(v)
		case !o.EventsAfterNudge:
			decisions[v.id] = ladderWedge // sentinel positioned, zero events after it
			wedgeEvidence[v.id] = "no session events after the sentinel"
		default:
			decisions[v.id] = ladderRenew // reaction arrived between the arms
			renewSource[v.id] = RenewSourceReaction
		}
	}

	// Deliver the batch CONCURRENTLY (bounded): each delivery carries
	// the bounce seam's fast-fail window (~10s) and the victims' FOR
	// UPDATE locks are held until this transaction commits — sequential
	// delivery would hold the locks N windows during exactly the
	// mass-quiet incident class this ladder answers. The batch runs
	// while the locks are held but collapses the stall to one window;
	// the post-commit-delivery alternative was considered and rejected
	// (it writes nudge state for a sentinel whose delivery outcome is
	// unknown — the phantom-nudge contract violation; worklog §6).
	if len(outbound) > 0 {
		results := make([]error, len(outbound))
		var wg sync.WaitGroup
		sem := make(chan struct{}, boardNudgeDeliveryConcurrency)
		for i, pn := range outbound {
			wg.Add(1)
			go func(i int, assignee string, p NudgePayload) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				results[i] = s.nudger(ctx, assignee, p)
			}(i, pn.v.assignee, NudgePayload{NudgeID: pn.id, TaskID: pn.v.id, TaskShortID: pn.v.shortID})
		}
		wg.Wait()
		for i, pn := range outbound {
			if results[i] == nil {
				nudgeIDs[pn.v.id] = pn.id
			} else {
				// Delivery could not be attempted (bounce/transport):
				// undecidable this sweep — the honest wall-clock path,
				// never a phantom nudge state claiming a sentinel that
				// never landed.
				decisions[pn.v.id] = ladderReclaim
				reclaimReason[pn.v.id] = ReclaimReasonDeliveryFailed
			}
		}
	}

	sys := Actor{Kind: ActorSystem, Surface: SurfaceReconciler}
	applied := 0
	// leaseOut collects the S4 observation-plane decisions (design
	// 0072): emitted strictly POST-COMMIT so the sink never projects a
	// decision the transaction rolled back. obsNudged collects the S7
	// trail's delivered sentinels (the nudge rung is engine-silent on
	// the bus — the app nudger publishes there — but the TRAIL wants
	// it: "were nudges sent?" was the incident's first unanswerable
	// question). Only delivered sentinels enter nudgeIDs (the outbound
	// loop), so every collected row is a SENT sentinel.
	var leaseOut []LeaseEvent
	var obsNudged []ObservationEvent
	leaseEvt := func(v victim, kind string) LeaseEvent {
		return LeaseEvent{
			Kind: kind, TaskID: v.id, TaskShortID: v.shortID, ProjectID: v.project,
			Assignee: v.assignee, FailureCount: v.failures,
		}
	}
	for _, v := range victims {
		switch decisions[v.id] {
		case ladderRenew:
			// Renewal: extend the lease, clear the nudge slot. NO
			// transition, NO board_events row (design: renewals never
			// flood the idempotency ledger), NO overlay. The S7
			// observation trail DOES take the renewal (post-commit,
			// via emitLease — the retained window's bounded by the
			// retention boundary, not the ledger's never). busy_renews
			// carries the consecutive busy-ONLY count (the cap's
			// input): +1 when Busy alone renewed, 0 when stronger
			// evidence did.
			if _, err := tx.Exec(ctx, `
				UPDATE tasks SET lease_expires_at=now()+make_interval(secs=>$2::float8),
					last_progress_at=now(), nudge_id=NULL, nudged_at=NULL, updated_at=now(),
					busy_renews=CASE WHEN $3::bool THEN busy_renews+1 ELSE 0 END
				WHERE id=$1 AND stage='in_progress'`,
				v.id, s.cfg.LeaseDuration.Seconds(), busyOnlyRenew[v.id]); err != nil {
				return 0, err
			}
			ev := leaseEvt(v, LeaseEventRenewed)
			ev.Source = renewSource[v.id]
			leaseOut = append(leaseOut, ev)
			applied++
			continue
		case ladderNudge:
			// The sentinel is OUT (delivered above); record it and
			// extend one window so the NEXT expiry is a clean
			// measurement point for the ordinal question.
			//
			// The send→stamp race is safe BY POSTGRES CLOCK SEMANTICS,
			// not by luck (review round 2, finding 5): now() is
			// TRANSACTION_timestamp — this UPDATE stamps nudged_at at
			// THIS sweep transaction's START, which strictly precedes
			// the delivery calls made inside it. A heartbeat whose
			// call was CAUSED by the nudge therefore starts its own
			// transaction after the delivery, so its last_heartbeat_at
			// (its statement's txn clock) is strictly AFTER nudged_at
			// even when the row write races: the blocked UPDATE waits
			// on this transaction's FOR UPDATE lock, commits after it,
			// and the next sweep's hbAfterNudge comparison reads
			// response-wins. A heartbeat that PREDATES the sweep's
			// start predates the delivery too (it cannot be a
			// response) and stamps earlier. Both directions ordinal,
			// one clock domain, no wall-clock comparison. Pinned:
			// TestPin_LeaseLadder_HeartbeatInSendWindowIsNotLost.
			if _, err := tx.Exec(ctx, `
				UPDATE tasks SET nudge_id=$2, nudged_at=now(),
					lease_expires_at=now()+make_interval(secs=>$3::float8), updated_at=now()
				WHERE id=$1 AND stage='in_progress'`,
				v.id, nudgeIDs[v.id], s.cfg.LeaseDuration.Seconds()); err != nil {
				return 0, err
			}
			// Sentinel-keyed (episode-unique AND re-sweep-idempotent):
			// an id keyed on failure_count collides across episodes —
			// renewals never bump the count and the human re-arm RESETS
			// it — and ON CONFLICT DO NOTHING would silently drop the
			// audit row (the persona escalate path's hazard, fixed
			// there with nanos; the nudge/wedge paths key on the
			// sentinel instead — deterministic AND unique).
			evt, err := recordEventTx(ctx, tx, v.project, "reconciler:nudge:"+v.id+":"+nudgeIDs[v.id], "nudge",
				"task", v.id, sys, map[string]any{"task_id": v.id, "nudge_id": nudgeIDs[v.id]})
			if err != nil {
				return 0, err
			}
			pending = append(pending, evt)
			// The nudge rung itself has no engine-side lease event —
			// the app-side nudger is the nudged publisher (design 0072
			// §9.3; lease_events.go's header): a bus nudged event means
			// the consumer owns delivery, a v1 send means the bus could
			// not be trusted. Double-reporting here would make the
			// stream's nudged count a lie. The S7 TRAIL still takes it
			// post-commit (obsNudged — the durable "were nudges sent?"
			// record; the bus stays the single ephemeral publisher).
			rung := nudgeRung[v.id]
			if rung == "" {
				rung = NudgeRungFirst // the unattributed default (every send() sets its own)
			}
			obsNudged = append(obsNudged, ObservationEvent{
				Kind: ObservationNudged, TaskID: v.id, Session: v.assignee,
				Detail: map[string]any{"nudge_id": nudgeIDs[v.id], "rung": rung},
			})
			applied++
			continue
		case ladderWedge:
			// WEDGE VERDICT: the sentinel is positioned in the session's
			// event order and NOTHING follows it — even a thinking model
			// emits a turn-start; this is a machine verdict, not a
			// guess (000050). Failed + evidence; rearm is the exit.
			tag, err := tx.Exec(ctx, `
				UPDATE tasks SET stage='failed', stage_entered_at=now(),
					pending_origin='escalated', pending_input=false,
					nudge_id=NULL, nudged_at=NULL, last_heartbeat_at=NULL,
					busy_renews=0,
					claim_token=NULL, lease_expires_at=NULL, assignee_session=NULL, implementer_session=NULL,
					claim_incremented_attempts=false, claim_persona=NULL,
					last_attempt_failed=true, failure_count=failure_count+1, last_failure_at=now()
				WHERE id=$1 AND stage='in_progress'`, v.id)
			if err != nil {
				return 0, err
			}
			if tag.RowsAffected() == 0 {
				continue
			}
			if err := writeTransition(ctx, txQ{tx}, v.id, StageInProgress, StageFailed, "", sys); err != nil {
				return 0, err
			}
			evt, err := recordEventTx(ctx, tx, v.project, "reconciler:wedge:"+v.id+":"+v.nudgeID, "wedge",
				"task", v.id, sys,
				map[string]any{"task_id": v.id, "nudge_id": v.nudgeID, "verdict": "wedge",
					"evidence": wedgeEvidence[v.id]})
			if err != nil {
				return 0, err
			}
			pending = append(pending, evt)
			ev := leaseEvt(v, LeaseEventWedged)
			ev.NudgeID = v.nudgeID
			ev.Evidence = wedgeEvidence[v.id]
			ev.FailureCount = v.failures + 1
			leaseOut = append(leaseOut, ev)
			applied++
			continue
		}
		// #1813 failed-state overlay: the lease lapse IS the attempt
		// failure — mark it, count it CONSECUTIVELY, and park the row
		// behind doubling backoff (auto-return-to-ready via the
		// existing cooldown candidacy gate; never a stage).
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET stage='backlog', stage_entered_at=now(),
				claim_token=NULL, lease_expires_at=NULL, assignee_session=NULL, implementer_session=NULL,
				claim_incremented_attempts=false, claim_persona=NULL,
				nudge_id=NULL, nudged_at=NULL, last_heartbeat_at=NULL,
				last_attempt_failed=true, failure_count=failure_count+1, last_failure_at=now(),
				cooldown_until=now()+make_interval(secs=>$2::float8)
			WHERE id=$1 AND stage='in_progress' AND (lease_expires_at IS NULL OR lease_expires_at < now())`,
			v.id, s.cfg.failureBackoff(int(v.failures)+1).Seconds())
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 0 {
			continue // raced a concurrent writer: converged by them
		}
		if err := writeTransition(ctx, txQ{tx}, v.id, StageInProgress, StageBacklog, "", sys); err != nil {
			return 0, err
		}
		evt, err := recordEventTx(ctx, tx, v.project, "reconciler:reclaim:"+v.id+":"+v.episode,
			"reclaim", "task", v.id, sys,
			map[string]any{"task_id": v.id, "failure_count": v.failures + 1,
				"last_attempt_failed": true, "last_failure_at": time.Now().UTC()})
		if err != nil {
			return 0, err
		}
		pending = append(pending, evt)
		ev := leaseEvt(v, LeaseEventReclaimed)
		if ev.Reason = reclaimReason[v.id]; ev.Reason == "" {
			ev.Reason = ReclaimReasonLapse // the unattributed wall-clock default (every named path sets its own)
		}
		ev.FailureCount = v.failures + 1
		leaseOut = append(leaseOut, ev)
		applied++ // r1-C5: count what landed, not what was scanned
	}
	if err := s.commitPublishing(ctx, tx, pending...); err != nil {
		return 0, err
	}
	// POST-COMMIT projection (design 0072 S4): the decisions above are
	// durable; report them to the observation plane. The sink bounds
	// its own work (the app adapter's publish carries a timeout) — a
	// slow bus must never stall the next phase of the sweep. The S7
	// trail rides the same post-commit seam (emitLease records before
	// it publishes; the nudges land here beside them).
	for _, ev := range leaseOut {
		s.emitLease(ctx, ev)
	}
	for _, ev := range obsNudged {
		s.recordObservation(ctx, ev)
	}
	return applied, nil
}

// personaReclaimEvents + spawn nudges are the stage-persona claim
// lifecycle's sweep outputs (collected inside the RC1 transaction,
// published/spawned post-commit).
type personaReclaim struct {
	spawns  []nudge
	applied int
}

// rc1PersonaClaims converges stage-persona claims outside in_progress
// (in_review today — the only other stage a live claim exists in):
//
// (a) LAPSE pass — in_review rows holding an EXPIRED persona lease.
//
//	With the stage's persona still configured: #1813 accounting
//	(consecutive failure count + doubling backoff cooldown), the
//	dead claim cleared, a deterministic reclaim event. With the
//	persona config REMOVED mid-flight: the row converges to today's
//	unclaimed in_review shape (token+lease cleared, no accounting —
//	the mechanism left, not the worker).
//
// (b) RE-ARM pass — unclaimed in_review rows past their cooldown with
//
//	a persona configured: a fresh re-claim-class mint + post-commit
//	spawn of a FRESH persona worker (the dead-persona-worker
//	lease-lapse → fresh-reclaim path). A failure_count at the
//	attempts cap escalates to pending_input/escalated instead (the
//	human-only exit — a dead-persona loop never spins unbounded;
//	attempts themselves are never incremented by persona mints).
//
// Runs in its OWN transaction after rc1Reclaims (a fault at either
// failpoint aborts only its phase; the next sweep converges).
func (s *Service) rc1PersonaClaims(ctx context.Context) (personaReclaim, error) {
	var out personaReclaim
	sys := Actor{Kind: ActorSystem, Surface: SurfaceReconciler}

	// --- (a) lapse pass ---------------------------------------------------
	rows, err := s.q().Query(ctx, `
		SELECT t.id, t.project_id, COALESCE(t.lease_expires_at::text, 'null'), t.failure_count,
			COALESCE(t.short_id,''), COALESCE(t.assignee_session,'')
		FROM tasks t
		WHERE t.stage='in_review' AND t.lease_expires_at IS NOT NULL AND t.lease_expires_at < now()`)
	if err != nil {
		return out, err
	}
	type lapseVictim struct {
		id, project, episode string
		failures             int32
		shortID, assignee    string
	}
	var lapses []lapseVictim
	for rows.Next() {
		var v lapseVictim
		if err := rows.Scan(&v.id, &v.project, &v.episode, &v.failures, &v.shortID, &v.assignee); err != nil {
			rows.Close()
			return out, err
		}
		lapses = append(lapses, v)
	}
	rows.Close()

	for _, v := range lapses {
		var persona *PersonaConfig
		if p := s.projectFor(ctx, v.project); p != nil {
			persona = s.stagePersona(ctx, p, StageInReview)
		}
		tx, err := s.store.pool.Begin(ctx)
		if err != nil {
			return out, err
		}
		if persona == nil {
			// Config removed mid-flight: converge to today's unclaimed
			// in_review shape (no accounting, no transition).
			tag, err := tx.Exec(ctx, `
				UPDATE tasks SET claim_token=NULL, lease_expires_at=NULL, claim_persona=NULL,
					nudge_id=NULL, nudged_at=NULL, last_heartbeat_at=NULL
				WHERE id=$1 AND stage='in_review' AND lease_expires_at < now()`, v.id)
			if err != nil {
				_ = tx.Rollback(ctx)
				return out, err
			}
			if tag.RowsAffected() == 0 {
				_ = tx.Rollback(ctx)
				continue // raced a concurrent writer: converged by them
			}
			evt, err := recordEventTx(ctx, tx, v.project, "reconciler:reclaim:"+v.id+":"+v.episode,
				"reclaim", "task", v.id, sys,
				map[string]any{"task_id": v.id, "stage": StageInReview, "persona_cleared": true})
			if err != nil {
				_ = tx.Rollback(ctx)
				return out, err
			}
			if err := s.commitPublishing(ctx, tx, evt); err != nil {
				return out, err
			}
			// S4: the config-cleared convergence is a reclaim the
			// observation plane reports too (which claim went away and
			// why — the 2026-10-30 incident's invisible-lapse class).
			s.emitLease(ctx, LeaseEvent{
				Kind: LeaseEventReclaimed, TaskID: v.id, TaskShortID: v.shortID, ProjectID: v.project,
				Assignee: v.assignee, Reason: ReclaimReasonPersonaCleared, FailureCount: v.failures,
			})
			out.applied++
			continue
		}
		// #1813: the persona lease lapse IS the attempt failure — mark
		// it, count it CONSECUTIVELY, park behind doubling backoff
		// (the re-arm pass below honors the cooldown; auto-return, never
		// a stage change — the review work is done, only the reviewer
		// died). #1947 (rc1 parity): the lapse also clears the DEAD
		// claim's recorded identity — rc1's in_progress reclaim clears
		// assignee_session on lapse, and the persona pass is its
		// in_review twin. A corpse assignee left on the row poisons the
		// #1832 WS4 stillborn guard for the NEXT claimant: between the
		// re-arm's mint and the fresh reviewer's spawn-stamp, a
		// heartbeat carrying the FRESH token consults the row's assignee
		// — the dead prior worker — and its zero-turn evidence refuses
		// the renewal (the live 03:49Z finding: every re-armed claimant
		// unrenewable, churning forever). #1959: this clear is now
		// bounce-SAFE by construction — the bounce's identity input is
		// implementer_session (captured at the forward exit), which this
		// pass deliberately does NOT touch: the reviewer's death cannot
		// strand the row un-bounceable the way it emptied
		// assignee_session (the #1959 live refusals were verdicts landing
		// in exactly that emptied window).
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET claim_token=NULL, lease_expires_at=NULL, assignee_session=NULL, claim_persona=NULL,
				nudge_id=NULL, nudged_at=NULL, last_heartbeat_at=NULL,
				last_attempt_failed=true, failure_count=failure_count+1, last_failure_at=now(),
				cooldown_until=now()+make_interval(secs=>$2::float8)
			WHERE id=$1 AND stage='in_review' AND lease_expires_at < now()`,
			v.id, s.cfg.failureBackoff(int(v.failures)+1).Seconds())
		if err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		if tag.RowsAffected() == 0 {
			_ = tx.Rollback(ctx)
			continue
		}
		evt, err := recordEventTx(ctx, tx, v.project, "reconciler:reclaim:"+v.id+":"+v.episode,
			"reclaim", "task", v.id, sys,
			map[string]any{"task_id": v.id, "stage": StageInReview, "persona": persona.Label(),
				"failure_count": v.failures + 1, "last_attempt_failed": true, "last_failure_at": time.Now().UTC()})
		if err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		if err := s.commitPublishing(ctx, tx, evt); err != nil {
			return out, err
		}
		// S4: the persona lease lapse is a reclaim on the observation
		// plane — the reviewer's death re-queues the row (the re-arm
		// pass below owns the fresh claim), and the 2026-10-30
		// incident's finding was that this transition left no event.
		s.emitLease(ctx, LeaseEvent{
			Kind: LeaseEventReclaimed, TaskID: v.id, TaskShortID: v.shortID, ProjectID: v.project,
			Assignee: v.assignee, Reason: ReclaimReasonPersonaLapse, FailureCount: v.failures + 1,
		})
		out.applied++
	}

	// --- (b) re-arm pass --------------------------------------------------
	// personaRearmCandidatesSQL (persona.go — the ownership-adjacent
	// seam) selects unclaimed, cooldown-expired, NOT-paused rows with
	// NO config prefilter: under the flipped floor the persona fires
	// with zero stored rows, so any EXISTS gate would exclude the
	// shipped default posture (round-2 review, blocking). Go's
	// stagePersona resolution is the sole authority; a candidate with
	// no resolved persona is skipped without a write. A paused
	// escalated row waits for resume to converge (pause gates claims;
	// pending_input entry while paused is legal either way).
	rows, err = s.q().Query(ctx, personaRearmCandidatesSQL)
	if err != nil {
		return out, err
	}
	type rearmCandidate struct {
		id, project string
		failures    int32
	}
	var candidates []rearmCandidate
	for rows.Next() {
		var v rearmCandidate
		if err := rows.Scan(&v.id, &v.project, &v.failures); err != nil {
			rows.Close()
			return out, err
		}
		candidates = append(candidates, v)
	}
	rows.Close()

	for _, v := range candidates {
		p := s.projectFor(ctx, v.project)
		if p == nil {
			continue
		}
		persona := s.stagePersona(ctx, p, StageInReview)
		if persona == nil {
			continue
		}
		tx, err := s.store.pool.Begin(ctx)
		if err != nil {
			return out, err
		}
		// Claim-time escalation (the AttemptsCap discipline): a persona
		// row whose consecutive failures reached the cap escalates to
		// FAILED (000050: exhaustion is a failure, never an input
		// costume) — rearm arbitrates the loop.
		if int(v.failures) >= s.cfg.AttemptsCap {
			tag, err := tx.Exec(ctx, `
				UPDATE tasks SET stage='failed', stage_entered_at=now(), pending_origin='escalated', pending_input=false,
					cooldown_until=NULL
				WHERE id=$1 AND stage='in_review' AND claim_token IS NULL`, v.id)
			if err != nil {
				_ = tx.Rollback(ctx)
				return out, err
			}
			if tag.RowsAffected() == 0 {
				_ = tx.Rollback(ctx)
				continue
			}
			if err := writeTransition(ctx, txQ{tx}, v.id, StageInReview, StageFailed, "", sys); err != nil {
				_ = tx.Rollback(ctx)
				return out, err
			}
			// Episode-unique id (nanos): failure_count resets on the
			// human re-arm (rearm.go) — an id keyed on the count would
			// collide across escalation episodes and silently drop the
			// audit event (ON CONFLICT DO NOTHING).
			evt, err := recordEventTx(ctx, tx, v.project, "reconciler:escalate:"+v.id+":"+strconv.FormatInt(time.Now().UnixNano(), 10),
				"escalate", "task", v.id, sys,
				map[string]any{"task_id": v.id, "stage": StageInReview, "persona": persona.Label(),
					"failure_count": v.failures, "exhausted": true})
			if err != nil {
				_ = tx.Rollback(ctx)
				return out, err
			}
			if err := s.commitPublishing(ctx, tx, evt); err != nil {
				return out, err
			}
			out.applied++
			continue
		}
		token := uuidNew()
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET `+personaClaimSQL+`
			WHERE id=$1 AND stage='in_review' AND claim_token IS NULL AND NOT paused
			  AND (cooldown_until IS NULL OR cooldown_until < now())`,
			v.id, token, s.cfg.LeaseDuration.Seconds(), persona.Label())
		if err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		if tag.RowsAffected() == 0 {
			_ = tx.Rollback(ctx)
			continue // raced (cooldown/re-arm elsewhere): converged
		}
		evt, err := personaClaimEventTx(ctx, tx, v.project, v.id, StageInReview, persona, "reconciler", int(v.failures))
		if err != nil {
			_ = tx.Rollback(ctx)
			return out, err
		}
		if err := s.commitPublishing(ctx, tx, evt); err != nil {
			return out, err
		}
		// Post-commit spawn (r1-C5): the fresh persona worker's charter
		// carries the minted token; spawnFor re-resolves the persona.
		out.spawns = append(out.spawns, nudge{taskID: v.id, token: token, spawn: true})
	}
	return out, nil
}

// dispatchDrive feeds claimable work to fresh workers via the engine
// (§4.4 "event-driven with a sweep backstop"): for every workspace whose
// projects hold claimable candidates and free slots, claim until the
// arbiter says done. Bounded per sweep; the engine's own DRR/slots logic
// terminates each workspace's loop.
func (s *Service) dispatchDrive(ctx context.Context) (int, error) {
	if s.spawner == nil {
		return 0, nil // no spawn seam installed: the drive is inert (no lease churn)
	}
	rows, err := s.q().Query(ctx, `
		SELECT DISTINCT p.target_workspace_id::text FROM projects p
		WHERE p.status='active' AND p.target_workspace_id IS NOT NULL
		  AND (p.slot_cooldown_until IS NULL OR p.slot_cooldown_until < now())
		  AND (SELECT count(*) FROM tasks t2
		       WHERE t2.project_id=p.id AND t2.stage='in_progress') < p.worker_slots
		  AND EXISTS (SELECT 1 FROM tasks t3 JOIN epics e3 ON e3.id=t3.epic_id
		              WHERE t3.project_id=p.id AND e3.status='active'
		                AND t3.stage='backlog'
		                AND NOT t3.is_blocked
		                AND (t3.cooldown_until IS NULL OR t3.cooldown_until < now()))`)
	if err != nil {
		return 0, err
	}
	var workspaces []string
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			rows.Close()
			return 0, err
		}
		workspaces = append(workspaces, ws)
	}
	rows.Close()

	const maxClaimsPerSweep = 64
	granted := 0
	for _, ws := range workspaces {
		for granted < maxClaimsPerSweep {
			c, err := s.ClaimNext(ctx, "dispatch:"+ws, ws)
			if err != nil {
				if err == ErrNoCandidate {
					break
				}
				return granted, err
			}
			if c.Escalated {
				continue // the escalated row waits for a human; claim the next
			}
			granted++
		}
	}
	return granted, nil
}

// pendingCoupling: worker-origin pending-input auto-stage (token-keeping)
// and resolution resume (lease re-armed, NO mint — the r11 preserve-case).
func (s *Service) pendingCoupling(ctx context.Context) (staged, resumed int, err error) {
	if s.pending == nil {
		return 0, 0, nil
	}
	sys := Actor{Kind: ActorSystem, Surface: SurfaceReconciler}

	// Auto-stage: in_progress rows whose worker session holds an
	// unanswered question/permission. Token + identity + provenance flag
	// are all untouched (the other token-keeping write).
	rows, err := s.q().Query(ctx, `
		SELECT id, project_id, assignee_session FROM tasks
		WHERE stage='in_progress' AND assignee_session IS NOT NULL`)
	if err != nil {
		return 0, 0, err
	}
	type rowT struct {
		id, project, assignee string
	}
	var live []rowT
	for rows.Next() {
		var r rowT
		if err := rows.Scan(&r.id, &r.project, &r.assignee); err != nil {
			rows.Close()
			return 0, 0, err
		}
		live = append(live, r)
	}
	rows.Close()
	for _, r := range live {
		st, err := s.pending.PendingInput(ctx, r.assignee)
		if err != nil || !st.HasPendingInput {
			continue
		}
		tx, err := s.store.pool.Begin(ctx)
		if err != nil {
			return staged, resumed, err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET pending_input=true, pending_origin='worker', updated_at=now()
			WHERE id=$1 AND stage='in_progress' AND NOT pending_input`, r.id)
		if err != nil {
			_ = tx.Rollback(ctx)
			return staged, resumed, err
		}
		if tag.RowsAffected() > 0 {
			evt, err := recordEventTx(ctx, tx, r.project, "reconciler:pending:"+r.id, "pending_stage",
				"task", r.id, sys, map[string]any{"task_id": r.id})
			if err != nil {
				_ = tx.Rollback(ctx)
				return staged, resumed, err
			}
			staged++
			if err := s.commitPublishing(ctx, tx, evt); err != nil {
				return staged, resumed, err
			}
		} else {
			if err := tx.Commit(ctx); err != nil {
				return staged, resumed, err
			}
		}
	}

	// Resume: flagged rows (worker origin) whose session's question
	// resolved — flag cleared, the lease re-armed in place (000050: the
	// stage never moved); the held token is unchanged, so the
	// provenance flag is untouched.
	rows, err = s.q().Query(ctx, `
		SELECT id, project_id, assignee_session FROM tasks
		WHERE pending_input AND pending_origin='worker' AND assignee_session IS NOT NULL`)
	if err != nil {
		return staged, resumed, err
	}
	var pend []rowT
	for rows.Next() {
		var r rowT
		if err := rows.Scan(&r.id, &r.project, &r.assignee); err != nil {
			rows.Close()
			return staged, resumed, err
		}
		pend = append(pend, r)
	}
	rows.Close()
	for _, r := range pend {
		st, err := s.pending.PendingInput(ctx, r.assignee)
		if err != nil || st.HasPendingInput {
			continue
		}
		tx, err := s.store.pool.Begin(ctx)
		if err != nil {
			return staged, resumed, err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET pending_input=false, pending_origin=NULL,
				lease_expires_at=now()+make_interval(secs=>$2::float8), updated_at=now()
			WHERE id=$1 AND pending_input AND pending_origin='worker'`,
			r.id, s.cfg.LeaseDuration.Seconds())
		if err != nil {
			_ = tx.Rollback(ctx)
			return staged, resumed, err
		}
		if tag.RowsAffected() > 0 {
			evt, err := recordEventTx(ctx, tx, r.project, "reconciler:resume:"+r.id, "resume",
				"task", r.id, sys, map[string]any{"task_id": r.id})
			if err != nil {
				_ = tx.Rollback(ctx)
				return staged, resumed, err
			}
			resumed++
			if err := s.commitPublishing(ctx, tx, evt); err != nil {
				return staged, resumed, err
			}
		} else {
			if err := tx.Commit(ctx); err != nil {
				return staged, resumed, err
			}
		}
	}
	return staged, resumed, nil
}

// rc2Drift converges external truth for anchored in_review rows. The scan
// is event-fed: only rows whose in_review transition event is newer than
// the watermark are checked (no rescan of pre-watermark anchors).
func (s *Service) rc2Drift(ctx context.Context, cutoff time.Time) (done, review, reentry int, err error) {
	rows, err := s.q().Query(ctx, `
		SELECT DISTINCT e.item_id FROM board_events e
		WHERE e.action='move_stage' AND e.created_at > $1 AND e.item_kind='task'`, cutoff)
	if err != nil {
		return 0, 0, 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()

	sys := Actor{Kind: ActorSystem, Surface: SurfaceReconciler}
	for _, id := range ids {
		t, err := s.store.resolveTask(ctx, s.q(), "", id)
		if err != nil || t.Stage != StageInReview {
			continue // drifted away already (stage predicate)
		}
		prNum, ok := prAnchor(t.Tags)
		if !ok {
			continue
		}
		pr, err := s.drift.GetPullRequest(ctx, prNum)
		if err != nil {
			slog.WarnContext(ctx, "board: drift host unavailable", "task", t.ShortID, "pr", prNum, "err", err.Error())
			continue
		}
		switch {
		case pr.Merged:
			// Merged: the Done guard applies to drift exactly like any
			// other write — external truth demands a human review
			// decision, it does not override R2's self-defining Done.
			if criteriaAllMet(t) {
				tx, err := s.store.pool.Begin(ctx)
				if err != nil {
					return done, review, reentry, err
				}
				tag, err := tx.Exec(ctx, `
					UPDATE tasks SET stage='done', stage_entered_at=now(),
						claim_token=NULL, lease_expires_at=NULL, claim_incremented_attempts=false, claim_persona=NULL,
						failure_count=0, last_attempt_failed=false, last_failure_at=NULL,
						-- a stage move resolves the flag with the move
						-- (updateStageTx's rule): a drift_review-parked row
						-- whose criteria were met while parked must not
						-- land done still flagged (review round 2,
						-- finding 4's sibling).
						pending_input=false, pending_origin=NULL,
						nudge_id=NULL, nudged_at=NULL, last_heartbeat_at=NULL
					WHERE id=$1 AND stage='in_review'`, t.ID)
				if err != nil {
					_ = tx.Rollback(ctx)
					return done, review, reentry, err
				}
				if tag.RowsAffected() > 0 {
					if err := writeTransition(ctx, txQ{tx}, t.ID, StageInReview, StageDone, pr.URL, sys); err != nil {
						_ = tx.Rollback(ctx)
						return done, review, reentry, err
					}
					if _, err := unblockDependents(ctx, txQ{tx}, t.ID, true); err != nil {
						_ = tx.Rollback(ctx)
						return done, review, reentry, err
					}
					if _, err := epicCompletionCheck(ctx, tx, t.EpicID); err != nil {
						_ = tx.Rollback(ctx)
						return done, review, reentry, err
					}
					// The #1832 review settle rides the drift-done path
					// too (a drift-completed subject completes/parks its
					// anchored review tasks in the same transaction).
					settleEvts, settlePokes, err := s.completeReviewTasks(ctx, tx, t, pr.URL, nil)
					if err != nil {
						_ = tx.Rollback(ctx)
						return done, review, reentry, err
					}
					evt, err := recordEventTx(ctx, tx, t.ProjectID, "reconciler:drift_done:"+t.ID, "drift_done",
						"task", t.ID, sys, map[string]any{"task_id": t.ID, "pr": prNum, "evidence": pr.URL})
					if err != nil {
						_ = tx.Rollback(ctx)
						return done, review, reentry, err
					}
					done++
					if err := s.commitPublishing(ctx, tx, append([]Event{evt}, settleEvts...)...); err != nil {
						return done, review, reentry, err
					}
					for _, pid := range settlePokes {
						s.pokeDrive(pid) // sibling projects of the subject (review r1, finding 5)
					}
				} else {
					if err := tx.Commit(ctx); err != nil {
						return done, review, reentry, err
					}
				}
			} else {
				tx, err := s.store.pool.Begin(ctx)
				if err != nil {
					return done, review, reentry, err
				}
				tag, err := tx.Exec(ctx, `
					UPDATE tasks SET pending_input=true, pending_origin='drift_review', updated_at=now()
					WHERE id=$1 AND stage='in_review' AND NOT pending_input`, t.ID)
				if err != nil {
					_ = tx.Rollback(ctx)
					return done, review, reentry, err
				}
				if tag.RowsAffected() > 0 {
					evt, err := recordEventTx(ctx, tx, t.ProjectID, "reconciler:drift_review:"+t.ID, "drift_review",
						"task", t.ID, sys, map[string]any{"task_id": t.ID, "pr": prNum})
					if err != nil {
						_ = tx.Rollback(ctx)
						return done, review, reentry, err
					}
					review++
					if err := s.commitPublishing(ctx, tx, evt); err != nil {
						return done, review, reentry, err
					}
				} else {
					if err := tx.Commit(ctx); err != nil {
						return done, review, reentry, err
					}
				}
			}
		case pr.Closed:
			// Closed-unmerged: the one enumerated backward drift —
			// in_progress with re-claim semantics for the recorded
			// assignee (fresh token+lease, flag=false). A row with no
			// recorded identity lands backlog (never a token-less
			// in_progress row).
			tx, err := s.store.pool.Begin(ctx)
			if err != nil {
				return done, review, reentry, err
			}
			var affected bool
			var newToken string
			// A recorded identity consumes the re-spawn semantics (the
			// fresh worker receives the token in its charter); no identity
			// lands backlog (never a token-less in_progress row).
			if t.AssigneeSession != "" {
				newToken = uuidNew()
				tag, err := tx.Exec(ctx, `
					UPDATE tasks SET stage='in_progress', stage_entered_at=now(),
						claim_token=$2, lease_expires_at=now()+make_interval(secs=>$3::float8),
						last_progress_at=now(), assignee_session=NULL,
						claim_incremented_attempts=false, claim_persona=NULL,
						-- The re-entry lands a NEW lease episode: resolve the
						-- flag with the move (paired form — a drift_review
						-- park reaching closed-unmerged must not violate
						-- tasks_pending_input_set, review round 2 finding 4)
						-- and reset the episode evidence slots.
						pending_input=false, pending_origin=NULL,
						nudge_id=NULL, nudged_at=NULL, last_heartbeat_at=NULL, busy_renews=0
					WHERE id=$1 AND stage='in_review'`,
					t.ID, newToken, s.cfg.LeaseDuration.Seconds())
				if err != nil {
					_ = tx.Rollback(ctx)
					return done, review, reentry, err
				}
				affected = tag.RowsAffected() > 0
			} else {
				// #1946: the no-identity landing keeps claim_token
				// (pre-existing) but drops the persona provenance —
				// claim_persona rides ONLY a live persona claim.
				tag, err := tx.Exec(ctx, `
					UPDATE tasks SET stage='backlog', stage_entered_at=now(), claim_persona=NULL,
						-- Paired-form flag resolution + episode reset (the
						-- backlog twin of finding 4: a no-identity parked
						-- row lands backlog flag-free, never a constraint
						-- violation that poisons the sweep).
						pending_input=false, pending_origin=NULL,
						nudge_id=NULL, nudged_at=NULL, last_heartbeat_at=NULL
					WHERE id=$1 AND stage='in_review'`, t.ID)
				if err != nil {
					_ = tx.Rollback(ctx)
					return done, review, reentry, err
				}
				affected = tag.RowsAffected() > 0
			}
			if affected {
				to := StageInProgress
				if newToken == "" {
					to = StageBacklog
				}
				if err := writeTransition(ctx, txQ{tx}, t.ID, StageInReview, to, pr.URL, sys); err != nil {
					_ = tx.Rollback(ctx)
					return done, review, reentry, err
				}
				evt, err := recordEventTx(ctx, tx, t.ProjectID, "reconciler:drift_reentry:"+t.ID, "drift_reentry",
					"task", t.ID, sys, map[string]any{"task_id": t.ID, "pr": prNum})
				if err != nil {
					_ = tx.Rollback(ctx)
					return done, review, reentry, err
				}
				reentry++
				if err := s.commitPublishing(ctx, tx, evt); err != nil {
					return done, review, reentry, err
				}
			} else {
				if err := tx.Commit(ctx); err != nil {
					return done, review, reentry, err
				}
			}
			if newToken != "" {
				// After commit: the re-spawn delivers the token to a fresh
				// worker (merged doc: no nudges to dead sessions).
				s.spawnFor(ctx, t.ID, newToken)
			}
		}
	}
	return done, review, reentry, nil
}

// rc3Dependencies recomputes is_blocked from the edge table (RC3): a
// task is blocked iff it holds a dependency whose prerequisite is not
// done. With cancel eliminated (#1747 round 3) there is no such
// class — deleted prerequisites' edges vanish with the row, so this
// recompute is the WHOLE dependency-consistency story.
func (s *Service) rc3Dependencies(ctx context.Context) (repairs int, err error) {
	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE tasks t SET is_blocked = EXISTS(
			SELECT 1 FROM board_dependencies d
			JOIN tasks dep ON dep.id = d.depends_on_task_id
			WHERE d.task_id = t.id AND dep.stage <> 'done')
		WHERE t.stage <> 'done'
		  AND t.is_blocked IS DISTINCT FROM EXISTS(
			SELECT 1 FROM board_dependencies d
			JOIN tasks dep ON dep.id = d.depends_on_task_id
			WHERE d.task_id = t.id AND dep.stage <> 'done')`)
	if err != nil {
		return 0, err
	}
	repairs = int(tag.RowsAffected())
	if err := s.commitPublishing(ctx, tx); err != nil {
		return 0, err
	}
	return repairs, nil
}

// rc4Net completes epics whose done-transition events were missed by the
// in-transaction check (event-fed off the watermark; never a zero-non-
// terminal state scan).
func (s *Service) rc4Net(ctx context.Context, cutoff time.Time) (int, error) {
	rows, err := s.q().Query(ctx, `
		SELECT DISTINCT t.epic_id, t.project_id FROM board_events e
		JOIN tasks t ON t.id = e.item_id
		WHERE e.created_at > $1 AND e.action IN ('move_stage','drift_done') AND e.item_kind='task'`, cutoff)
	if err != nil {
		return 0, err
	}
	type epicRef struct{ id, project string }
	var epics []epicRef
	for rows.Next() {
		var r epicRef
		if err := rows.Scan(&r.id, &r.project); err != nil {
			rows.Close()
			return 0, err
		}
		epics = append(epics, r)
	}
	rows.Close()

	sys := Actor{Kind: ActorSystem, Surface: SurfaceReconciler}
	n := 0
	for _, r := range epics {
		tx, err := s.store.pool.Begin(ctx)
		if err != nil {
			return n, err
		}
		flipped, err := epicCompletionCheck(ctx, tx, r.id)
		if err != nil {
			_ = tx.Rollback(ctx)
			return n, err
		}
		if flipped {
			evt, err := recordEventTx(ctx, tx, r.project, "reconciler:epic_complete:"+r.id, "epic_complete",
				"epic", r.id, sys, map[string]any{"epic_id": r.id})
			if err != nil {
				_ = tx.Rollback(ctx)
				return n, err
			}
			n++
			if err := s.commitPublishing(ctx, tx, evt); err != nil {
				return n, err
			}
		} else {
			if err := tx.Commit(ctx); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

// observationPruneDue reports whether the S7 trail's retention pass is
// due at now — the pure gate maybePruneObservations consults (zero
// value = never run = due). Split out so the gate semantics are
// directly pinnable without waiting an hour.
func (r *Reconciler) observationPruneDue(now time.Time) bool {
	return r.obsPruneLast.IsZero() || now.Sub(r.obsPruneLast) >= ObservationPruneInterval
}

// maybePruneObservations runs the S7 trail's retention pass at most
// once per ObservationPruneInterval (the boundary itself is
// observation.go's contract: per-task cap + TTL; board_events keeps
// the permanent record). Fail-open inside PruneObservationTrail; the
// stamp is written BEFORE the pass runs (a failed pass retried only
// after the interval — the DELETE is idempotent either way).
func (r *Reconciler) maybePruneObservations(ctx context.Context) {
	now := time.Now()
	if !r.observationPruneDue(now) {
		return
	}
	r.obsPruneLast = now
	r.svc.PruneObservationTrail(ctx)
}

// Start runs the periodic sweep until ctx is canceled (the app's
// root-context cancel; any API replica can run it — SKIP LOCKED + guarded
// writes arbitrate multi-replica safety without leader election, the
// workflow-engine lifecycle pattern). A sweep error logs and the cadence
// continues: convergence is the contract, not an error-free pass.
func (r *Reconciler) Start(ctx context.Context) error {
	interval := r.svc.cfg.SweepInterval
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			res, err := r.Sweep(ctx)
			if err != nil {
				slog.WarnContext(ctx, "board: reconciler sweep failed (next pass converges)",
					"error", err.Error())
				continue
			}
			if res.Reclaims+res.PersonaReclaims+res.DriftDone+res.DriftReview+res.DriftReentry+res.DepRepairs+
				res.PendingStaged+res.Resumes+res.EpicCompletions > 0 {
				slog.InfoContext(ctx, "board: reconciler sweep",
					"reclaims", res.Reclaims, "persona_reclaims", res.PersonaReclaims,
					"drift_done", res.DriftDone, "drift_review", res.DriftReview,
					"drift_reentry", res.DriftReentry, "dep_repairs", res.DepRepairs,
					"pending_staged", res.PendingStaged,
					"resumes", res.Resumes, "epic_completions", res.EpicCompletions)
			}
		}
	}
}
