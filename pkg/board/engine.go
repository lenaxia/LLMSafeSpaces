// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// engine.go — the dispatch engine (design §4.4). No in-memory scheduler
// state: the dispatcher is SQL plus a spawner; any API replica can run it
// and Postgres arbitrates every race. The engine is ownership-agnostic by
// construction (§4.7): it keys on rows, tokens, and the caller's WORKSPACE
// — never on user identity.

// eligibleProjectsSQL locks the least-served candidate-eligible project in
// the caller's workspace (the p CTE, design-verbatim): the EXISTS keeps a
// candidate-less project from winning the lock and starving others; the
// slot count is read under the project row lock so concurrent claimers
// serialize; the slot_cooldown predicate pauses throttled projects.
const eligibleProjectsSQL = `
	SELECT p.id FROM projects p
	WHERE p.status = 'active'
	  AND p.target_workspace_id = $1::uuid
	  AND (p.slot_cooldown_until IS NULL OR p.slot_cooldown_until < now())
	  AND (SELECT count(*) FROM tasks t2
	       WHERE t2.project_id = p.id AND t2.stage = 'in_progress') < p.worker_slots
	  AND EXISTS (SELECT 1 FROM tasks t3 JOIN epics e3 ON e3.id = t3.epic_id
	              WHERE t3.project_id = p.id AND e3.status = 'active'
	                AND e3.deficit < e3.quantum
	                AND t3.stage = 'backlog' AND NOT t3.is_blocked
	                AND (t3.cooldown_until IS NULL OR t3.cooldown_until < now()))
	ORDER BY (SELECT min(e.vft) FROM epics e WHERE e.project_id = p.id AND e.status = 'active'
	          AND e.deficit < e.quantum
	          AND EXISTS (SELECT 1 FROM tasks t4 WHERE t4.epic_id = e.id
	                      AND t4.stage = 'backlog' AND NOT t4.is_blocked
	                      AND (t4.cooldown_until IS NULL OR t4.cooldown_until < now()))), p.id
	FOR UPDATE OF p SKIP LOCKED LIMIT 1`

// candidateSQL picks the ready-set winner in the locked project:
// (priority, virtual_finish_time, created_at) — the ruling's order, with
// the DRR round-eligibility predicate (deficit < quantum).
const candidateSQL = `
	SELECT t.id, t.attempts FROM tasks t
	JOIN epics e ON e.id = t.epic_id AND e.status = 'active'
	WHERE t.project_id = $1::uuid
	  AND t.stage = 'backlog' AND NOT t.is_blocked
	  AND (t.cooldown_until IS NULL OR t.cooldown_until < now())
	  AND e.deficit < e.quantum
	ORDER BY t.priority ASC, e.vft ASC, t.created_at ASC
	LIMIT 1 FOR UPDATE OF t SKIP LOCKED`

// ClaimNext is the dispatch-engine entry (worker surface; engine-internal
// per ruling R9 — the dispatch path claims-then-spawns). Caller-workspace
// binding is structural: a worker only ever claims projects whose target
// workspace IS its own. Returns a claim with a nil Task on an empty sweep
// — a normal outcome, not an error condition.
func (s *Service) ClaimNext(ctx context.Context, workerID, workspaceID string) (*Claim, error) {
	for round := 0; round < 2; round++ {
		claim, status, projectID, err := s.claimOnce(ctx, workerID, workspaceID)
		if err != nil {
			return nil, err
		}
		switch status {
		case "granted", "escalated":
			return claim, nil
		case "project_empty":
			// Project-level rollover: the claimer locked p but the
			// candidate raced away (SKIP LOCKED) — reset THAT project's
			// deficits and retry once (bounded; never a loop).
			if round == 0 {
				if err := s.resetDeficits(ctx, &projectID, workspaceID); err != nil {
					return nil, err
				}
				continue
			}
			return &Claim{}, nil
		case "none":
			// Dispatcher-level rollover: nothing claimable while at-quota
			// epics with candidates exist in scope — reset every deficit
			// in the claim's scope and retry once.
			if round == 0 {
				atQuota, err := s.atQuotaCandidatesExist(ctx, workspaceID)
				if err != nil {
					return nil, err
				}
				if atQuota {
					if err := s.resetDeficits(ctx, nil, workspaceID); err != nil {
						return nil, err
					}
					continue
				}
			}
			return &Claim{}, nil
		}
	}
	return &Claim{}, nil
}

// TaskStart is the manual one-off dispatch (§4.2): targets a NAMED task,
// bypasses exactly three things (the p CTE's active-epic-project gate, the
// candidate predicate's e.status='active' requirement — scoped to
// draft∪active epics — and the DRR deficit<quantum eligibility; the DRR
// counters still advance). All other arbitration is identical to
// claim_next: atomic statement, slots cap, caller-workspace binding,
// token minted, attempts incremented.
func (s *Service) TaskStart(ctx context.Context, actor Actor, taskID, shortIDOrID, workerWorkspaceID, requestID string) (*Claim, error) {
	t, err := s.store.resolveTask(ctx, s.q(), "", pick(taskID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	p, err := s.store.resolveProject(ctx, s.q(), "", t.ProjectID)
	if err != nil {
		return nil, err
	}
	// Workers delegate as the owning user (R7): the authz matrix is the
	// same for both manager and worker surfaces on this action.
	if err := requireOwner(actor, p.UserID); err != nil {
		return nil, err
	}
	if p.Status == ProjectArchived {
		return nil, &GuardError{Code: GuardProjectState, Detail: "archived projects refuse task_start (terminal storage stays closed)"}
	}
	var epicStatus string
	if err := s.q().QueryRow(ctx, `SELECT status FROM epics WHERE id=$1`, t.EpicID).Scan(&epicStatus); err != nil {
		return nil, err
	}
	if epicStatus != EpicDraft && epicStatus != EpicActive {
		return nil, &GuardError{Code: GuardEpicState,
			Detail: "task_start is scoped to draft∪active epics (paused/completed/archived refuse; pause stops new claims of every kind)"}
	}
	if workerWorkspaceID == "" {
		workerWorkspaceID = p.TargetWorkspaceID
	}
	if workerWorkspaceID != p.TargetWorkspaceID {
		return nil, &GuardError{Code: GuardWorkspaceBinding, Detail: "worker workspace must equal the project's target workspace (403)"}
	}
	if t.Stage != StageBacklog || t.IsBlocked {
		return nil, &GuardError{Code: GuardStage, Detail: "task_start targets a ready backlog task"}
	}

	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Slots cap under the project row lock (the same arbitration as
	// claim_next — a manual dispatch is not a slots bypass).
	var locked bool
	if err := tx.QueryRow(ctx, `
		SELECT p.id IS NOT NULL FROM projects p
		WHERE p.id=$1 AND p.status='active'
		  AND (p.slot_cooldown_until IS NULL OR p.slot_cooldown_until < now())
		  AND (SELECT count(*) FROM tasks t2 WHERE t2.project_id=p.id AND t2.stage='in_progress') < p.worker_slots
		FOR UPDATE OF p SKIP LOCKED`, p.ID).Scan(&locked); err != nil {
		if err == pgx.ErrNoRows {
			return nil, &GuardError{Code: GuardProjectState, Detail: "project at capacity or cooled down — no slot for a manual dispatch"}
		}
		return nil, err
	}

	cur, err := s.store.taskForUpdate(ctx, tx, t.ID)
	if err != nil {
		return nil, err
	}
	if cur.Stage != StageBacklog {
		return nil, &GuardError{Code: GuardStage, Detail: "the task left backlog — server-arbitrated refusal"}
	}

	// The escalation check mirrors the claim (pre-increment attempts >= N).
	if cur.Attempts >= s.cfg.AttemptsCap {
		if _, err := tx.Exec(ctx, `
			UPDATE tasks SET stage='pending_input', stage_entered_at=now(), pending_origin='escalated'
			WHERE id=$1 AND stage='backlog'`, t.ID); err != nil {
			return nil, err
		}
		if err := writeTransition(ctx, txQ{tx}, t.ID, StageBacklog, StagePendingInput, "",
			Actor{Kind: ActorSystem, Surface: SurfaceReconciler}); err != nil {
			return nil, err
		}
		if err := insertEventTx(ctx, tx, p.ID, engineEventID("escalate", t.ID, cur.Attempts), "escalate",
			"task", t.ID, Actor{Kind: ActorSystem, Surface: "engine"}, map[string]any{"task_id": t.ID, "attempts": cur.Attempts}); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &Claim{TaskID: t.ID, Escalated: true, Task: s.mustTask(ctx, t.ID)}, nil
	}

	var c Claim
	var lease time.Time
	var attemptsDone int
	if err := tx.QueryRow(ctx, `
		UPDATE tasks t SET stage='in_progress', stage_entered_at=now(),
			claim_token=gen_random_uuid(), assignee_session=$2,
			lease_expires_at=now()+make_interval(secs=>$3::float8),
			last_progress_at=now(), attempts=attempts+1,
			claim_incremented_attempts=true
		WHERE t.id=$1
		RETURNING t.id, t.short_id, t.project_id, t.epic_id, t.claim_token::text,
		          t.assignee_session, t.lease_expires_at, t.attempts`,
		t.ID, "manual:"+actor.UserID, s.cfg.LeaseDuration.Seconds()).
		Scan(&c.TaskID, &c.ShortID, &c.ProjectID, &c.EpicID, &c.ClaimToken, &c.Assignee, &lease, &attemptsDone); err != nil {
		return nil, err
	}
	c.LeaseExpiresAt = lease
	if _, err := tx.Exec(ctx, `
		UPDATE epics SET vft=vft+1, deficit=deficit+1
		WHERE id=(SELECT epic_id FROM tasks WHERE id=$1)`, t.ID); err != nil {
		return nil, err
	}
	if err := writeTransition(ctx, txQ{tx}, t.ID, StageBacklog, StageInProgress, "",
		Actor{Kind: ActorUser, UserID: actor.UserID, Surface: actor.Surface}); err != nil {
		return nil, err
	}
	evtID := requestID
	if evtID == "" {
		evtID = engineEventID("task_start", t.ID, attemptsDone)
	}
	if err := insertEventTx(ctx, tx, p.ID, evtID, "task_start", "task", t.ID,
		actor, map[string]any{"task_id": t.ID, "attempts": attemptsDone}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	s.spawnAfter(ctx, &c)
	c.Task = s.mustTask(ctx, t.ID)
	return &c, nil
}

// engineEventID builds a deterministic system event id (grant episodes are
// unique by task+attempts; replays are inert by the (project_id, id) PK).
func engineEventID(action, taskID string, attempts int) string {
	return "engine:" + action + ":" + taskID + ":" + itoa(attempts)
}

// claimOnce runs one claim round in its own transaction: lock project →
// lock candidate → grant (the ONLY attempts increment) or escalate
// (pre-increment attempts >= N withholds the grant) → DRR counters.
func (s *Service) claimOnce(ctx context.Context, workerID, workspaceID string) (*Claim, string, string, error) {
	tx, err := s.store.pool.Begin(ctx)
	if err != nil {
		return nil, "", "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var projectID string
	if err := tx.QueryRow(ctx, eligibleProjectsSQL, workspaceID).Scan(&projectID); err != nil {
		if err == pgx.ErrNoRows {
			return nil, "none", "", nil
		}
		return nil, "", "", err
	}

	var taskID string
	var attempts int
	if err := tx.QueryRow(ctx, candidateSQL, projectID).Scan(&taskID, &attempts); err != nil {
		if err == pgx.ErrNoRows {
			// Locked the project, no candidate: project-level emptiness.
			return nil, "project_empty", projectID, nil
		}
		return nil, "", "", err
	}

	sysActor := Actor{Kind: ActorSystem, Surface: "engine"}

	// The escalation check rides the same transaction: the (N+1)th claim
	// attempt (pre-increment attempts >= N) grants nothing — the task
	// lands pending_input/escalated instead (§4.4). No lease, no token,
	// no spawn; attempts stay at N.
	if attempts >= s.cfg.AttemptsCap {
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET stage='pending_input', stage_entered_at=now(),
				pending_origin='escalated'
			WHERE id=$1 AND stage='backlog'`, taskID)
		if err != nil {
			return nil, "", "", err
		}
		if tag.RowsAffected() == 0 {
			return nil, "project_empty", projectID, nil // raced away: retry
		}
		if err := writeTransition(ctx, txQ{tx}, taskID, StageBacklog, StagePendingInput, "", sysActor); err != nil {
			return nil, "", "", err
		}
		if err := insertEventTx(ctx, tx, projectID, engineEventID("escalate", taskID, attempts), "escalate",
			"task", taskID, sysActor, map[string]any{"task_id": taskID, "attempts": attempts}); err != nil {
			return nil, "", "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, "", "", err
		}
		return &Claim{TaskID: taskID, Escalated: true, Task: s.mustTask(ctx, taskID)}, "escalated", projectID, nil
	}

	// The grant: one statement, one arbiter (D8-class). attempts has
	// exactly ONE increment site on the claim path: here.
	var c Claim
	var lease time.Time
	var attemptsDone int
	if err := tx.QueryRow(ctx, `
		UPDATE tasks t SET stage='in_progress', stage_entered_at=now(),
			claim_token=gen_random_uuid(), assignee_session=$2,
			lease_expires_at=now()+make_interval(secs=>$3::float8),
			last_progress_at=now(), attempts=attempts+1,
			claim_incremented_attempts=true
		WHERE t.id=$1
		RETURNING t.id, t.short_id, t.project_id, t.epic_id, t.claim_token::text,
		          t.assignee_session, t.lease_expires_at, t.attempts`,
		taskID, workerID, s.cfg.LeaseDuration.Seconds()).
		Scan(&c.TaskID, &c.ShortID, &c.ProjectID, &c.EpicID, &c.ClaimToken, &c.Assignee, &lease, &attemptsDone); err != nil {
		return nil, "", "", err
	}
	c.LeaseExpiresAt = lease

	// DRR: unit cost per claim keeps vft globally comparable (it is simply
	// claims served); the deficit consumes the round allowance.
	if _, err := tx.Exec(ctx, `
		UPDATE epics SET vft = vft + 1, deficit = deficit + 1
		WHERE id = (SELECT epic_id FROM tasks WHERE id=$1)`, taskID); err != nil {
		return nil, "", "", err
	}
	if err := writeTransition(ctx, txQ{tx}, taskID, StageBacklog, StageInProgress, "", sysActor); err != nil {
		return nil, "", "", err
	}
	if err := insertEventTx(ctx, tx, c.ProjectID, engineEventID("claim", taskID, attemptsDone), "claim",
		"task", taskID, sysActor, map[string]any{"task_id": taskID, "worker": workerID, "attempts": attemptsDone}); err != nil {
		return nil, "", "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", "", err
	}

	// R9 fresh-per-task: claim-then-spawn, AFTER commit. A spawn failure
	// never revokes the claim — the lease-lapse path heals.
	s.spawnAfter(ctx, &c)
	c.Task = s.mustTask(ctx, taskID)
	return &c, "granted", c.ProjectID, nil
}

// resetDeficits rolls the DRR round over: one project (project-level
// rollover) or every active project in the caller's scope
// (dispatcher-level).
func (s *Service) resetDeficits(ctx context.Context, projectID *string, workspaceID string) error {
	if projectID != nil {
		_, err := s.q().Exec(ctx, `UPDATE epics SET deficit=0 WHERE project_id=$1 AND deficit > 0`, *projectID)
		return err
	}
	_, err := s.q().Exec(ctx, `
		UPDATE epics e SET deficit=0 FROM projects p
		WHERE e.project_id=p.id AND e.deficit > 0
		  AND p.target_workspace_id=$1::uuid AND p.status='active'`, workspaceID)
	return err
}

// atQuotaCandidatesExist reports whether epics at their round quota still
// hold claimable candidates in the caller's scope (the dispatcher-level
// rollover precondition).
func (s *Service) atQuotaCandidatesExist(ctx context.Context, workspaceID string) (bool, error) {
	var exists bool
	err := s.q().QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM epics e
			JOIN projects p ON p.id = e.project_id
			JOIN tasks t ON t.epic_id = e.id
			WHERE p.target_workspace_id = $1::uuid AND p.status='active'
			  AND e.status='active' AND e.deficit >= e.quantum
			  AND t.stage='backlog' AND NOT t.is_blocked
			  AND (t.cooldown_until IS NULL OR t.cooldown_until < now()))`, workspaceID).Scan(&exists)
	return exists, err
}

// spawnAfter invokes the fresh-worker seam and records the spawned session
// as the assignee (guarded: a concurrently reclaimed row drops the spawn).
func (s *Service) spawnAfter(ctx context.Context, claim *Claim) {
	if s.spawner == nil {
		return
	}
	sessionID, err := s.spawner(ctx, claim)
	if err != nil {
		slog.WarnContext(ctx, "board: worker spawn failed — lease lapse heals",
			"task_id", claim.TaskID, "err", err.Error())
		return
	}
	if sessionID == "" {
		return
	}
	tag, err := s.q().Exec(ctx, `
		UPDATE tasks SET assignee_session=$2 WHERE id=$1 AND claim_token=$3 AND stage='in_progress'`,
		claim.TaskID, sessionID, claim.ClaimToken)
	if err != nil {
		slog.WarnContext(ctx, "board: assignee record failed", "task_id", claim.TaskID, "err", err.Error())
		return
	}
	if tag.RowsAffected() > 0 {
		claim.Assignee = sessionID
	}
}

func (s *Service) mustTask(ctx context.Context, taskID string) *Task {
	t, err := s.store.resolveTask(ctx, s.q(), "", taskID)
	if err != nil {
		return nil
	}
	return t
}

func itoa(n int) string { return strconv.Itoa(n) }
