// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
//	   (the belt beneath in-transaction maintenance) + the
//	   canceled-dependency transition class;
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

// Failpoint names (the kill-test fault seams).
const (
	fpRC1Pre       = "rc1.pre"
	fpRC1Post      = "rc1.post"
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
}

// NewReconciler constructs the reconciler over a Service.
func NewReconciler(pool *pgxpool.Pool, cfg EngineConfig, opts ...Option) *Reconciler {
	return &Reconciler{svc: NewService(pool, cfg, opts...)}
}

// SweepResultDetail carries per-phase counters (SweepResult is the public
// summary; phases append here).

// Sweep runs one full pass. Phases: RC1 → pending coupling → RC2 → RC3 →
// RC4 net → watermark advance.
func (r *Reconciler) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	s := r.svc

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
	repairs, abandoned, err := s.rc3Dependencies(ctx)
	if err != nil {
		return res, err
	}
	res.DepRepairs = repairs
	res.AbandonedStaged = abandoned

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

	rows, err := tx.Query(ctx, `
		SELECT t.id, t.project_id, COALESCE(t.lease_expires_at::text, 'null')
		FROM tasks t
		WHERE t.stage='in_progress' AND (t.lease_expires_at IS NULL OR t.lease_expires_at < now())
		FOR UPDATE OF t SKIP LOCKED`)
	if err != nil {
		return 0, err
	}
	type victim struct {
		id, project, episode string
	}
	var victims []victim
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.id, &v.project, &v.episode); err != nil {
			rows.Close()
			return 0, err
		}
		victims = append(victims, v)
	}
	rows.Close()

	sys := Actor{Kind: ActorSystem, Surface: SurfaceReconciler}
	applied := 0
	for _, v := range victims {
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET stage='backlog', stage_entered_at=now(),
				claim_token=NULL, lease_expires_at=NULL, assignee_session=NULL,
				claim_incremented_attempts=false
			WHERE id=$1 AND stage='in_progress' AND (lease_expires_at IS NULL OR lease_expires_at < now())`, v.id)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 0 {
			continue // raced a concurrent writer: converged by them
		}
		if err := writeTransition(ctx, txQ{tx}, v.id, StageInProgress, StageBacklog, "", sys); err != nil {
			return 0, err
		}
		if err := insertEventTx(ctx, tx, v.project, "reconciler:reclaim:"+v.id+":"+v.episode,
			"reclaim", "task", v.id, sys, map[string]any{"task_id": v.id}); err != nil {
			return 0, err
		}
		applied++ // r1-C5: count what landed, not what was scanned
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return applied, nil
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
			UPDATE tasks SET stage='pending_input', stage_entered_at=now(), pending_origin='worker'
			WHERE id=$1 AND stage='in_progress'`, r.id)
		if err != nil {
			_ = tx.Rollback(ctx)
			return staged, resumed, err
		}
		if tag.RowsAffected() > 0 {
			if err := writeTransition(ctx, txQ{tx}, r.id, StageInProgress, StagePendingInput, "", sys); err != nil {
				_ = tx.Rollback(ctx)
				return staged, resumed, err
			}
			if err := insertEventTx(ctx, tx, r.project, "reconciler:pending:"+r.id, "pending_stage",
				"task", r.id, sys, map[string]any{"task_id": r.id}); err != nil {
				_ = tx.Rollback(ctx)
				return staged, resumed, err
			}
			staged++
		}
		if err := tx.Commit(ctx); err != nil {
			return staged, resumed, err
		}
	}

	// Resume: pending_input rows (worker origin) whose session's question
	// resolved — back to in_progress with the lease re-armed; the held
	// token is unchanged, so the provenance flag is untouched.
	rows, err = s.q().Query(ctx, `
		SELECT id, project_id, assignee_session FROM tasks
		WHERE stage='pending_input' AND pending_origin='worker' AND assignee_session IS NOT NULL`)
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
			UPDATE tasks SET stage='in_progress', stage_entered_at=now(), pending_origin=NULL,
				lease_expires_at=now()+make_interval(secs=>$2::float8)
			WHERE id=$1 AND stage='pending_input' AND pending_origin='worker'`,
			r.id, s.cfg.LeaseDuration.Seconds())
		if err != nil {
			_ = tx.Rollback(ctx)
			return staged, resumed, err
		}
		if tag.RowsAffected() > 0 {
			if err := writeTransition(ctx, txQ{tx}, r.id, StagePendingInput, StageInProgress, "", sys); err != nil {
				_ = tx.Rollback(ctx)
				return staged, resumed, err
			}
			if err := insertEventTx(ctx, tx, r.project, "reconciler:resume:"+r.id, "resume",
				"task", r.id, sys, map[string]any{"task_id": r.id}); err != nil {
				_ = tx.Rollback(ctx)
				return staged, resumed, err
			}
			resumed++
		}
		if err := tx.Commit(ctx); err != nil {
			return staged, resumed, err
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
						claim_token=NULL, lease_expires_at=NULL, claim_incremented_attempts=false
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
					if err := insertEventTx(ctx, tx, t.ProjectID, "reconciler:drift_done:"+t.ID, "drift_done",
						"task", t.ID, sys, map[string]any{"task_id": t.ID, "pr": prNum, "evidence": pr.URL}); err != nil {
						_ = tx.Rollback(ctx)
						return done, review, reentry, err
					}
					done++
				}
				if err := tx.Commit(ctx); err != nil {
					return done, review, reentry, err
				}
			} else {
				tx, err := s.store.pool.Begin(ctx)
				if err != nil {
					return done, review, reentry, err
				}
				tag, err := tx.Exec(ctx, `
					UPDATE tasks SET stage='pending_input', stage_entered_at=now(), pending_origin='drift_review'
					WHERE id=$1 AND stage='in_review'`, t.ID)
				if err != nil {
					_ = tx.Rollback(ctx)
					return done, review, reentry, err
				}
				if tag.RowsAffected() > 0 {
					if err := writeTransition(ctx, txQ{tx}, t.ID, StageInReview, StagePendingInput, pr.URL, sys); err != nil {
						_ = tx.Rollback(ctx)
						return done, review, reentry, err
					}
					if err := insertEventTx(ctx, tx, t.ProjectID, "reconciler:drift_review:"+t.ID, "drift_review",
						"task", t.ID, sys, map[string]any{"task_id": t.ID, "pr": prNum}); err != nil {
						_ = tx.Rollback(ctx)
						return done, review, reentry, err
					}
					review++
				}
				if err := tx.Commit(ctx); err != nil {
					return done, review, reentry, err
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
						claim_incremented_attempts=false, pending_origin=NULL
					WHERE id=$1 AND stage='in_review'`,
					t.ID, newToken, s.cfg.LeaseDuration.Seconds())
				if err != nil {
					_ = tx.Rollback(ctx)
					return done, review, reentry, err
				}
				affected = tag.RowsAffected() > 0
			} else {
				tag, err := tx.Exec(ctx, `
					UPDATE tasks SET stage='backlog', stage_entered_at=now(), pending_origin=NULL
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
				if err := insertEventTx(ctx, tx, t.ProjectID, "reconciler:drift_reentry:"+t.ID, "drift_reentry",
					"task", t.ID, sys, map[string]any{"task_id": t.ID, "pr": prNum}); err != nil {
					_ = tx.Rollback(ctx)
					return done, review, reentry, err
				}
				reentry++
			}
			if err := tx.Commit(ctx); err != nil {
				return done, review, reentry, err
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

// rc3Dependencies recomputes is_blocked from the edge table (non-terminal
// rows only — terminal rows' is_blocked is meaningless and stable) and
// stages the canceled-dependency transition class (skipping rows already
// pending: origin preserved, no oscillation).
func (s *Service) rc3Dependencies(ctx context.Context) (repairs, abandoned int, err error) {
	sys := Actor{Kind: ActorSystem, Surface: SurfaceReconciler}

	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE tasks t SET is_blocked = EXISTS(
			SELECT 1 FROM board_dependencies d
			JOIN tasks dep ON dep.id = d.depends_on_task_id
			WHERE d.task_id = t.id AND dep.stage <> 'done')
		WHERE t.stage <> 'cancelled'
		  AND t.is_blocked IS DISTINCT FROM EXISTS(
			SELECT 1 FROM board_dependencies d
			JOIN tasks dep ON dep.id = d.depends_on_task_id
			WHERE d.task_id = t.id AND dep.stage <> 'done')`)
	if err != nil {
		return 0, 0, err
	}
	repairs = int(tag.RowsAffected())

	// Abandoned-edge class: dependents of canceled tasks that are still
	// in a non-terminal, non-pending stage.
	rows, err := tx.Query(ctx, `
		SELECT d.task_id, t.stage, t.project_id FROM board_dependencies d
		JOIN tasks dep ON dep.id = d.depends_on_task_id
		JOIN tasks t ON t.id = d.task_id
		WHERE dep.stage='cancelled'
		  AND t.stage NOT IN ('cancelled','pending_input','done')
		FOR UPDATE OF t SKIP LOCKED`)
	if err != nil {
		return 0, 0, err
	}
	type victim struct {
		id, stage, project string
	}
	var victims []victim
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.id, &v.stage, &v.project); err != nil {
			rows.Close()
			return 0, 0, err
		}
		victims = append(victims, v)
	}
	rows.Close()
	for _, v := range victims {
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET stage='pending_input', stage_entered_at=now(), pending_origin='dependency_abandoned'
			WHERE id=$1 AND stage NOT IN ('cancelled','pending_input','done')`, v.id)
		if err != nil {
			return 0, 0, err
		}
		if tag.RowsAffected() > 0 {
			if err := writeTransition(ctx, txQ{tx}, v.id, v.stage, StagePendingInput, "", sys); err != nil {
				return 0, 0, err
			}
			if err := insertEventTx(ctx, tx, v.project, "reconciler:abandoned:"+v.id, "abandon_stage",
				"task", v.id, sys, map[string]any{"task_id": v.id}); err != nil {
				return 0, 0, err
			}
			abandoned++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return repairs, abandoned, nil
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
			if err := insertEventTx(ctx, tx, r.project, "reconciler:epic_complete:"+r.id, "epic_complete",
				"epic", r.id, sys, map[string]any{"epic_id": r.id}); err != nil {
				_ = tx.Rollback(ctx)
				return n, err
			}
			n++
		}
		if err := tx.Commit(ctx); err != nil {
			return n, err
		}
	}
	return n, nil
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
			if res.Reclaims+res.DriftDone+res.DriftReview+res.DriftReentry+res.DepRepairs+
				res.AbandonedStaged+res.PendingStaged+res.Resumes+res.EpicCompletions > 0 {
				slog.InfoContext(ctx, "board: reconciler sweep",
					"reclaims", res.Reclaims, "drift_done", res.DriftDone, "drift_review", res.DriftReview,
					"drift_reentry", res.DriftReentry, "dep_repairs", res.DepRepairs,
					"abandoned_staged", res.AbandonedStaged, "pending_staged", res.PendingStaged,
					"resumes", res.Resumes, "epic_completions", res.EpicCompletions)
			}
		}
	}
}
