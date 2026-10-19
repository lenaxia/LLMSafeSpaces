// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// stage.go — the stage machine (design §4.1 "Stage semantics"):
// backlog → in_progress → in_review → done is forward-moving under worker
// control, with exactly one enumerated backward transition — in_review →
// in_progress (the review bounce or RC2 closed-unmerged drift; both are
// re-claim-class writes). pending_input is enterable from in_progress/
// in_review, from the claim path (escalation), and from cancel; its exits
// are manager-surface-only human writes and are the ONE attempts reset.
//
// Lease lifecycle totality (§4.4): every write landing a row in
// in_progress sets/re-arms the lease in the same statement and stamps the
// claim-provenance flag in the same statement; every forward exit clears
// token+lease; pending_input keeps its token (clock suspended by the stage
// predicate); the resume is a NO-MINT preserve-case (r11 fix).

// moveStageResult is the event payload + replay outcome.
type moveStageResult struct {
	TaskID         string `json:"task_id"`
	FromStage      string `json:"from_stage"`
	RequestedTo    string `json:"requested_to"`
	ResultingStage string `json:"resulting_stage"`
	Reclaimed      bool   `json:"reclaimed"`
}

// MoveStage is the server-arbitrated stage machine write: a drag that
// loses a race returns the authoritative outcome and the UI snaps back.
func (s *Service) MoveStage(ctx context.Context, actor Actor, taskID, shortIDOrID, toStage, evidenceURL, requestID string) (*Task, error) {
	ref := pick(taskID, shortIDOrID)
	t, err := s.resolveTaskForActor(ctx, actor, ref)
	if err != nil {
		return nil, err
	}
	p, err := s.store.resolveProject(ctx, s.q(), "", t.ProjectID)
	if err != nil {
		return nil, err
	}
	if err := requireOwner(actor, p.UserID); err != nil {
		return nil, err
	}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "move_stage", itemKind: "task", itemID: t.ID, actor: actor}
	var pendingNudge nudge
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		res, n, err := s.moveStageTx(ctx, tx, actor, t.ID, toStage, evidenceURL)
		pendingNudge = n
		if err != nil {
			return nil, err
		}
		return structMap(res), nil
	})
	if err != nil {
		return nil, err
	}
	// Deliveries fire only AFTER commit (r1-C5): a token handed out for a
	// mint that rolled back would strand a worker holding a dead claim.
	pendingNudge.fire(ctx, s)
	// Event-driven drive: a →done transition may unblock dependents (the
	// reverse-edge cascade); a requeue re-enters the ready set once its
	// cooldown expires (the sweep observes the expiry).
	if toStage == StageDone {
		s.pokeDrive(p.ID)
	}
	_ = dup // 200-with-original: the reloaded row IS the original outcome
	out, err := s.store.resolveTask(ctx, s.q(), p.ID, t.ID)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func structMap(r moveStageResult) map[string]any {
	return map[string]any{
		"task_id": r.TaskID, "from_stage": r.FromStage, "requested_to": r.RequestedTo,
		"resulting_stage": r.ResultingStage, "reclaimed": r.Reclaimed,
	}
}

// moveStageTx performs one guarded transition inside tx. The row is
// locked and re-read (server arbitration): a raced write sees the fresh
// stage and fails cleanly.
func (s *Service) moveStageTx(ctx context.Context, tx pgx.Tx, actor Actor, taskID, toStage, evidenceURL string) (moveStageResult, nudge, error) {
	cur, err := s.store.taskForUpdate(ctx, tx, taskID)
	if err != nil {
		return moveStageResult{}, nudge{}, err
	}
	from := cur.Stage
	res := moveStageResult{TaskID: taskID, FromStage: from, RequestedTo: toStage}

	// The pending_input exits are manager-surface-only (a delegated worker
	// is not the human the exit exists for; its token does not unlock
	// self-exit). This is the ONE remaining agent-surface manager denial
	// (#1747 keeps it: the human exit stays human).
	if from == StagePendingInput && actor.Surface == SurfaceAgent {
		return res, nudge{}, &GuardError{Code: GuardManagerSurface, Detail: "pending_input exits are human decisions (manager surfaces only)"}
	}

	// #1747 pause-must-pause: FORWARD stage-writes refuse on a
	// paused task (its worker was halted; the flag gates progress until
	// resume). Backward/review moves and pending_input entry stay legal —
	// pause gates claiming and forward progress, never the question path.
	if toStage == StageInReview || toStage == StageDone {
		if paused, perr := s.effectivePausedTx(ctx, tx, cur); perr != nil {
			return res, nudge{}, perr
		} else if paused {
			return res, nudge{}, &GuardError{Code: GuardPaused, Detail: "the task is paused — forward stage-writes refuse until resume (its worker was halted)"}
		}
	}

	// Enumerated-transition check FIRST (an illegal drag reports its own
	// class regardless of the token), then the claim-token guard for the
	// legal in-flight writes (§4.2): manager surfaces hold no token — an
	// optimistic drag on a claimed row snaps back.
	legal := map[[2]string]bool{
		{StageInProgress, StageInReview}:     true,
		{StageInProgress, StageDone}:         true,
		{StageInProgress, StagePendingInput}: true,
		{StageInReview, StageInProgress}:     true,
		{StageInReview, StageDone}:           true,
		{StageInReview, StagePendingInput}:   true,
		{StagePendingInput, StageBacklog}:    true,
		{StagePendingInput, StageInProgress}: true,
	}
	if !legal[[2]string{from, toStage}] {
		return res, nudge{}, &GuardError{Code: GuardStage, Detail: "stage transition " + from + "→" + toStage + " is not enumerated in the machine"}
	}
	if from == StageInProgress && actor.ClaimToken != cur.ClaimToken {
		return res, nudge{}, &GuardError{Code: GuardClaimToken, Detail: "stage writes on a claimed task require its live claim token"}
	}

	var resulting string
	var reclaimed bool
	var postNudge nudge
	switch {
	case from == StageInProgress && toStage == StageInReview:
		// Forward exit: work done, awaiting review — nothing is leased.
		// The identity is retained (the bounce/RC2 consumer, §4.1).
		resulting, err = s.updateStageTx(ctx, tx, taskID, from, toStage,
			`claim_token=NULL, lease_expires_at=NULL, claim_incremented_attempts=false`, evidenceURL, actor)

	case from == StageInProgress && toStage == StageDone:
		if err := s.doneGuard(cur, evidenceURL); err != nil {
			return res, nudge{}, err
		}
		resulting, err = s.updateStageTx(ctx, tx, taskID, from, toStage,
			`claim_token=NULL, lease_expires_at=NULL, claim_incremented_attempts=false`, evidenceURL, actor)
		if err == nil {
			err = s.doneCascade(ctx, tx, cur)
		}

	case from == StageInProgress && toStage == StagePendingInput:
		// Enter pending from live work: token + identity retained (the
		// lease clock is suspended by the stage predicate).
		resulting, err = s.updateStageTx(ctx, tx, taskID, from, toStage,
			`pending_origin='worker'`, evidenceURL, actor)

	case from == StageInReview && toStage == StageInProgress:
		// The bounce: re-claim for the recorded assignee — fresh token +
		// lease minted in the same statement, flag=false (its 429 never
		// decrements), attempts unchanged. No identity → refuse (a
		// token-less in_progress row is never created). A paused row
		// refuses the MINT (#1747: no re-entry while paused).
		if cur.Paused {
			return res, nudge{}, &GuardError{Code: GuardPaused, Detail: "the task is paused — the bounce re-claim refuses until resume"}
		}
		if cur.AssigneeSession == "" {
			return res, nudge{}, &GuardError{Code: GuardStage, Detail: "bounce requires a recorded assignee (no token-less in_progress rows)"}
		}
		resulting, postNudge, err = s.reclaimTx(ctx, tx, taskID, cur.AssigneeSession, actor, "bounce")

	case from == StageInReview && toStage == StageDone:
		if err := s.doneGuard(cur, evidenceURL); err != nil {
			return res, nudge{}, err
		}
		resulting, err = s.updateStageTx(ctx, tx, taskID, from, toStage,
			`claim_token=NULL, lease_expires_at=NULL, claim_incremented_attempts=false`, evidenceURL, actor)
		if err == nil {
			err = s.doneCascade(ctx, tx, cur)
		}

	case from == StageInReview && toStage == StagePendingInput:
		resulting, err = s.updateStageTx(ctx, tx, taskID, from, toStage,
			`pending_origin='worker'`, evidenceURL, actor)

	case from == StagePendingInput && (toStage == StageBacklog || toStage == StageInProgress):
		// The human exit — the ONE write that resets attempts (a human
		// re-queue is a fresh judgment). Identity present → re-claim
		// branch (fresh token+lease, nudge); none → backlog branch (the
		// dead worker is never re-nudged). The branch keys on identity
		// presence, never token presence. The re-claim MINT refuses on a
		// paused row (#1747: no re-entry while paused — resume first);
		// the backlog exit stays open (parking is exactly what a paused
		// row wants).
		if toStage == StageInProgress && cur.Paused {
			return res, nudge{}, &GuardError{Code: GuardPaused, Detail: "the task is paused — the re-claim exit refuses until resume (backlog remains open)"}
		}
		if toStage == StageInProgress && cur.AssigneeSession != "" {
			resulting, reclaimed, postNudge, err = s.exitPendingReclaim(ctx, tx, actor, cur)
		} else {
			resulting, err = s.updateStageTx(ctx, tx, taskID, from, StageBacklog,
				`claim_token=NULL, lease_expires_at=NULL, assignee_session=NULL,
				 claim_incremented_attempts=false, attempts=0`, evidenceURL, actor)
		}

	default:
		return res, nudge{}, &GuardError{Code: GuardStage, Detail: "stage transition " + from + "→" + toStage + " is not enumerated in the machine"}
	}
	if err != nil {
		return res, nudge{}, err
	}
	res.ResultingStage = resulting
	res.Reclaimed = reclaimed
	return res, postNudge, nil
}

// nudge is a deferred post-commit delivery (r1-C5: a rollback must never
// emit it). Under the merged design's re-spawn semantics the delivery is
// a SPAWN of the fresh worker that receives the token in its charter
// (optionally aborting a superseded live session first — the interrupt
// seam); there are no nudges to dead sessions anywhere.
type nudge struct {
	taskID, token string
	spawn         bool
	abortAssignee string
}

func (n nudge) fire(ctx context.Context, s *Service) {
	if !n.spawn {
		return
	}
	if n.abortAssignee != "" && s.aborter != nil {
		_ = s.aborter(ctx, n.taskID, n.abortAssignee)
	}
	s.spawnFor(ctx, n.taskID, n.token)
}

// doneGuard: Done = ALL acceptance criteria met + evidence (ruling R2).
func (s *Service) doneGuard(t *Task, evidenceURL string) error {
	if !criteriaAllMet(t) {
		return &GuardError{Code: GuardDone, Detail: "all acceptance criteria must be met before done (ruling R2)"}
	}
	if evidenceURL == "" {
		return &GuardError{Code: GuardDone, Detail: "done requires evidence_url on the transition (ruling R2)"}
	}
	return nil
}

// doneCascade: the reverse-edge unblock + the RC4 completion check, both
// in the →done transaction.
func (s *Service) doneCascade(ctx context.Context, tx pgx.Tx, t *Task) error {
	if _, err := unblockDependents(ctx, txQ{tx}, t.ID, true); err != nil {
		return err
	}
	if _, err := epicCompletionCheck(ctx, tx, t.EpicID); err != nil {
		return err
	}
	return nil
}

// updateStageTx applies one transition's column updates + the transition
// row. extra is spliced verbatim into SET (pending_origin entries set it
// there; every other transition clears it via the CASE).
func (s *Service) updateStageTx(ctx context.Context, tx pgx.Tx, taskID, from, toStage, extra, evidenceURL string, actor Actor) (string, error) {
	if toStage != StagePendingInput {
		extra += `, pending_origin=NULL`
	}
	tag, err := tx.Exec(ctx, `
		UPDATE tasks SET stage=$2, stage_entered_at=now(), `+extra+`
		WHERE id=$1 AND stage=$3`, taskID, toStage, from)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", &GuardError{Code: GuardStage, Detail: "the row moved — server-arbitrated refusal (or terminal: RC5)"}
	}
	if err := writeTransition(ctx, txQ{tx}, taskID, from, toStage, evidenceURL, actor); err != nil {
		return "", err
	}
	return toStage, nil
}

// reclaimTx mints a re-claim-class token+lease with the recorded identity
// CLEARED (the merged design's re-spawn semantics: "the recorded identity
// is cleared at mint and the fresh identity stamped by the guarded
// spawn-stamp"; flag=false — the throttle never rolls these back). The
// fresh worker is spawned after commit via spawnAfter — "there are no
// nudges to dead sessions anywhere".
func (s *Service) reclaimTx(ctx context.Context, tx pgx.Tx, taskID, assignee string, actor Actor, class string) (string, nudge, error) {
	token := uuidNew()
	tag, err := tx.Exec(ctx, `
		UPDATE tasks SET stage='in_progress', stage_entered_at=now(),
			claim_token=$2, lease_expires_at=now()+make_interval(secs=>$3::float8), last_progress_at=now(),
			assignee_session=NULL, claim_incremented_attempts=false, pending_origin=NULL
		WHERE id=$1 AND NOT paused`,
		taskID, token, s.cfg.LeaseDuration.Seconds())
	if err != nil {
		return "", nudge{}, err
	}
	if tag.RowsAffected() == 0 {
		return "", nudge{}, &GuardError{Code: GuardStage, Detail: "row is terminal — the write is a no-op (RC5)"}
	}
	_ = assignee
	return StageInProgress, nudge{taskID: taskID, token: token, spawn: true}, nil
}

// exitPendingReclaim is the pending_input exit's re-claim branch: attempts
// reset, fresh token+lease with the recorded identity cleared (the
// interim-state shape), the superseded session aborted (its question is
// void with the exit — the interrupt seam, merged-doc §4.1), and the
// fresh worker spawned after commit.
func (s *Service) exitPendingReclaim(ctx context.Context, tx pgx.Tx, actor Actor, cur *Task) (string, bool, nudge, error) {
	token := uuidNew()
	tag, err := tx.Exec(ctx, `
		UPDATE tasks SET stage='in_progress', stage_entered_at=now(),
			claim_token=$2, lease_expires_at=now()+make_interval(secs=>$3::float8), last_progress_at=now(),
			assignee_session=NULL, claim_incremented_attempts=false, pending_origin=NULL, attempts=0
		WHERE id=$1 AND stage='pending_input'`,
		cur.ID, token, s.cfg.LeaseDuration.Seconds())
	if err != nil {
		return "", false, nudge{}, err
	}
	if tag.RowsAffected() == 0 {
		return "", false, nudge{}, &GuardError{Code: GuardStage, Detail: "row moved — the exit raced (server-arbitrated)"}
	}
	if err := writeTransition(ctx, txQ{tx}, cur.ID, StagePendingInput, StageInProgress, "", actor); err != nil {
		return "", false, nudge{}, err
	}
	return StageInProgress, true, nudge{taskID: cur.ID, token: token, spawn: true, abortAssignee: cur.AssigneeSession}, nil
}

// --- Heartbeat + throttle -----------------------------------------------------------

// Heartbeat refreshes the lease ONLY when it carries new progress evidence
// (the worker's message-level watermark moved — #1685: elapsed time is not
// liveness). Requires the matching claim token.
func (s *Service) Heartbeat(ctx context.Context, actor Actor, taskID, shortIDOrID string, observedProgressAt time.Time) (HeartbeatResult, error) {
	t, err := s.resolveTaskForActor(ctx, actor, pick(taskID, shortIDOrID))
	if err != nil {
		return HeartbeatResult{}, err
	}
	if t.Stage != StageInProgress || actor.ClaimToken == "" || actor.ClaimToken != t.ClaimToken {
		return HeartbeatResult{}, &GuardError{Code: GuardClaimToken, Detail: "heartbeat requires a live claim (stage + token)"}
	}
	// Evidence gate: extend only when the watermark moved past the stored
	// progress watermark.
	extends := t.LastProgressAt == nil || observedProgressAt.After(*t.LastProgressAt)
	var lease time.Time
	if extends {
		if err := s.q().QueryRow(ctx, `
			UPDATE tasks SET last_progress_at=$2, lease_expires_at=now()+make_interval(secs=>$3::float8)
			WHERE id=$1 AND stage='in_progress' AND claim_token=$4
			RETURNING lease_expires_at`,
			t.ID, observedProgressAt, s.cfg.LeaseDuration.Seconds(), actor.ClaimToken).Scan(&lease); err != nil {
			return HeartbeatResult{}, err
		}
		s.writeAudit(ctx, t.ProjectID, "heartbeat", "task", t.ID, actor)
	} else {
		lease = derefTime(t.LeaseExpiresAt)
	}
	progress := derefTime(t.LastProgressAt)
	if extends {
		progress = observedProgressAt
	}
	return HeartbeatResult{Extended: extends, LeaseExpiresAt: lease, LastProgressAt: progress}, nil
}

// ReportThrottle is the 429 path (§4.4): requeue to backlog WITHOUT
// attempt penalty + cooldown on both the task and the project's slots.
// One UPDATE guarded by stage='in_progress' AND token match — a pending_
// input or lapsed row is rejected exactly like the race case.
func (s *Service) ReportThrottle(ctx context.Context, actor Actor, taskID, shortIDOrID string, retryAfter time.Duration, requestID string) (*Task, error) {
	t, err := s.resolveTaskForActor(ctx, actor, pick(taskID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	if t.Stage != StageInProgress || actor.ClaimToken == "" || actor.ClaimToken != t.ClaimToken {
		return nil, &GuardError{Code: GuardClaimToken, Detail: "a throttle concerns a live claim (stage + token guard)"}
	}
	if retryAfter <= 0 {
		retryAfter = s.cfg.ThrottleBackoff
	}
	p, err := s.store.resolveProject(ctx, s.q(), "", t.ProjectID)
	if err != nil {
		return nil, err
	}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "report_throttle", itemKind: "task", itemID: t.ID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		// The rollback consumes the provenance flag: an incrementing
		// grant's 429 rolls its increment back (net zero); a re-claim-
		// class token (flag=false) requeues with NO decrement — never
		// undercounted, never negative.
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET stage='backlog', stage_entered_at=now(),
				cooldown_until=now()+make_interval(secs=>$2::float8),
				claim_token=NULL, lease_expires_at=NULL, assignee_session=NULL,
				claim_incremented_attempts=false,
				attempts = attempts - CASE WHEN claim_incremented_attempts THEN 1 ELSE 0 END
			WHERE id=$1 AND stage='in_progress' AND claim_token=$3`,
			t.ID, retryAfter.Seconds(), actor.ClaimToken)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, &GuardError{Code: GuardClaimToken, Detail: "the claim lapsed or moved — the throttle race case"}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE projects SET slot_cooldown_until=now()+make_interval(secs=>$2::float8) WHERE id=$1`,
			p.ID, retryAfter.Seconds()); err != nil {
			return nil, err
		}
		if err := writeTransition(ctx, txQ{tx}, t.ID, StageInProgress, StageBacklog, "", actor); err != nil {
			return nil, err
		}
		return map[string]any{"task_id": t.ID, "stage": StageBacklog}, nil
	})
	if err != nil {
		return nil, err
	}
	_ = dup
	return s.store.resolveTask(ctx, s.q(), p.ID, t.ID)
}

// resolveTaskForActor resolves a task through the actor's scope: the
// agent surface reads workspace-scoped (r1-S1 — an unscoped read leaked
// cross-tenant rows and claim tokens); manager surfaces resolve by id
// (ownership follows via requireOwner).
func (s *Service) resolveTaskForActor(ctx context.Context, actor Actor, ref string) (*Task, error) {
	if actor.Surface == SurfaceAgent && actor.WorkspaceID != "" {
		return s.store.resolveTaskInWorkspace(ctx, s.q(), actor.WorkspaceID, ref)
	}
	return s.store.resolveTask(ctx, s.q(), "", ref)
}

// effectivePausedTx reports the #1744 pause state as ONE predicate: a
// task is effectively paused when IT is paused, its EPIC is paused, or
// its PROJECT is paused (query scoping — no duplicated flags; the claim
// predicates and the write guards read the same rule).
func (s *Service) effectivePausedTx(ctx context.Context, tx pgx.Tx, t *Task) (bool, error) {
	var epicStatus string
	var projectPaused bool
	if err := tx.QueryRow(ctx, `
		SELECT e.status, COALESCE(p.paused, false)
		FROM tasks t
		JOIN epics e ON e.id = t.epic_id
		JOIN projects p ON p.id = t.project_id
		WHERE t.id = $1`, t.ID).Scan(&epicStatus, &projectPaused); err != nil {
		return false, err
	}
	return t.Paused || epicStatus == EpicPaused || projectPaused, nil
}

// writeAudit persists a non-mutating action's audit row (reads/heartbeat).
func (s *Service) writeAudit(ctx context.Context, projectID, action, itemKind, itemID string, actor Actor) {
	ew := &eventWriter{svc: s, projectID: projectID, requestID: "", action: action, itemKind: itemKind, itemID: itemID, actor: actor}
	_, _, _ = ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		return map[string]any{"id": itemID}, nil
	})
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
