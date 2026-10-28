// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lenaxia/llmsafespaces/pkg/shortid"
)

// stage.go — the stage machine (design §4.1 "Stage semantics"):
// backlog → in_progress → in_review → done is forward-moving under worker
// control, with exactly one enumerated backward transition — in_review →
// in_progress (the review bounce or RC2 closed-unmerged drift; both are
// re-claim-class writes). pending_input is enterable from in_progress/
// in_review and from the claim path (escalation); its exits are
// manager-surface-only human writes and are the ONE attempts reset.
//
// Lease lifecycle totality (§4.4): every write landing a row in
// in_progress sets/re-arms the lease in the same statement and stamps the
// claim-provenance flag in the same statement; every forward exit clears
// token+lease; pending_input keeps its token (clock suspended by the stage
// predicate); the resume is a NO-MINT preserve-case (r11 fix).

// reset_overlay is the #1813 success clause: a completed turn clears
// the failure overlay and its consecutive count.
const reset_overlay = `, failure_count=0, last_attempt_failed=false, last_failure_at=NULL`

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
	// Stage-persona claim-then-spawn (owner directive 2026-10-24):
	// resolve the TARGET stage's persona before the transition — an
	// entry into a persona-bearing stage mints the persona claim inside
	// the same transaction (claim-before-spawn; the spawn itself rides
	// the post-commit nudge, r1-C5). nil persona = today's behavior,
	// byte-identically.
	var entryPersona *PersonaConfig
	if toStage == StageInReview {
		entryPersona = s.stagePersona(ctx, p, StageInReview)
	}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "move_stage", itemKind: "task", itemID: t.ID, actor: actor}
	var pendingNudge nudge
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		res, n, err := s.moveStageTx(ctx, tx, actor, t.ID, toStage, evidenceURL, entryPersona)
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
	// #1922: the stage-move echo rides the read-edge phrase translation.
	s.translateTaskReads(ctx, out)
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
// stage and fails cleanly. entryPersona is the target stage's resolved
// persona for a persona-bearing ENTRY (→in_review today); non-nil, the
// transition mints the persona claim in the same statement set and the
// returned nudge spawns the persona worker post-commit.
func (s *Service) moveStageTx(ctx context.Context, tx pgx.Tx, actor Actor, taskID, toStage, evidenceURL string, entryPersona *PersonaConfig) (moveStageResult, nudge, error) {
	cur, err := s.store.taskForUpdate(ctx, tx, taskID)
	if err != nil {
		return moveStageResult{}, nudge{}, err
	}
	from := cur.Stage
	res := moveStageResult{TaskID: taskID, FromStage: from, RequestedTo: toStage}

	// The pending_input (flag) exits are manager-surface-only for WORKERS
	// (a delegated worker is not the human the exit exists for; its token
	// does not unlock self-exit). #1832 WS2: the pod seam's
	// manager-grade agent surface (SurfaceAgentManager — open to ANY
	// in-workspace session since the #1747 §5 manager grant's repeal,
	// ruling 2026-10-27 / tsk-copper-refectory-generates) is a manager
	// surface; the ordinary-worker rule is unchanged. 000050: the flag
	// replaces the stage — the guard keys on the flag, not the stage.
	if cur.PendingInput && actor.Surface == SurfaceAgent {
		return res, nudge{}, &GuardError{Code: GuardManagerSurface, Detail: "pending_input exits are human or manager decisions (the worker surface is denied; the manager route — board_move_stage without a claim token — is the agent-manager surface)"}
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
	// optimistic drag on a claimed row snaps back. 000050: pending_input
	// is a flag-op alias, not a stored stage — a FLAGGED row (real stage
	// in_progress/in_review, flag set) addresses its exits through the
	// alias: fromKey maps the flagged row onto the pending_input column
	// of the table so the two exit pairs match (a stored 'pending_input'
	// from-stage no longer exists to key on), and every OTHER target
	// from a flagged row refuses here exactly as the stage form did (the
	// owner question gates forward progress until resolved). 'failed' is
	// the new failure terminus (nudge self-fail enters it; rearm exits
	// it).
	legal := map[[2]string]bool{
		{StageInProgress, StageInReview}:     true,
		{StageInProgress, StageDone}:         true,
		{StageInProgress, StageFailed}:       true,
		{StageInReview, StageInProgress}:     true,
		{StageInReview, StageDone}:           true,
		{StageInReview, StageFailed}:         true,
		{StageInProgress, StagePendingInput}: true, // flag set
		{StageInReview, StagePendingInput}:   true, // flag set
		{StagePendingInput, StageBacklog}:    true, // flag clear → re-queue
		{StagePendingInput, StageInProgress}: true, // flag clear → re-claim
	}
	fromKey := from
	if cur.PendingInput {
		fromKey = StagePendingInput // the flag rides the real stage; exits address the alias
	}
	if !legal[[2]string{fromKey, toStage}] {
		return res, nudge{}, &GuardError{Code: GuardStage, Detail: "stage transition " + from + "→" + toStage + " is not enumerated in the machine"}
	}
	// 000050 parity on the token guard: the old flag-exit writes moved
	// FROM the pending_input stage, which the guard never covered — a
	// flagged row keeps its real stage (in_progress) AND its token, so
	// the guard would newly refuse the human exit it always allowed.
	// Skip the guard on the EXIT side only (flagged rows); entry and
	// every other in_progress write keep it.
	if from == StageInProgress && !cur.PendingInput && actor.ClaimToken != cur.ClaimToken {
		return res, nudge{}, &GuardError{Code: GuardClaimToken, Detail: "stage writes on a claimed task require its live claim token"}
	}

	var resulting string
	var reclaimed bool
	var postNudge nudge
	switch {
	case from == StageInProgress && toStage == StageInReview:
		// Forward exit: work done, awaiting review — nothing is leased.
		// The identity is retained (the RC2 drift consumer; §4.1 — the
		// BOUNCE no longer reads it, see the capture note below).
		// #1813: a completed turn is SUCCESS — the failure overlay and
		// its consecutive count reset here.
		//
		// #1959: the forward exit also CAPTURES the bounce-consumer
		// identity — implementer_session=assignee_session (the SET
		// reads the pre-update column: the worker that did the work and
		// moved the row here). assignee_session itself keeps its
		// live-claim lifecycle (the reviewer persona's spawn-stamp
		// overwrites it, the #1947 lapse pass clears it); the bounce no
		// longer reads it — see the bounce arm below.
		//
		// Stage-persona claim-then-spawn: with a persona configured for
		// in_review the ENTRY mints the persona claim (fresh token +
		// lease for the reviewer-class worker — one lease, one token,
		// the same guarded spawn-stamp as every claim) in the SAME
		// transaction, and the nudge spawns post-commit. No persona:
		// today's shape exactly (token+lease cleared, nothing spawned).
		if entryPersona != nil {
			// Same forward-exit statement as the no-persona shape (the
			// implementer's token+lease end here; identity retained),
			// then the persona mint layers the reviewer-class claim on
			// top — one transaction, claim-before-spawn.
			resulting, err = s.updateStageTx(ctx, tx, taskID, from, toStage,
				`claim_token=NULL, lease_expires_at=NULL, claim_incremented_attempts=false, claim_persona=NULL,
				 implementer_session=assignee_session`+reset_overlay, evidenceURL, actor)
			if err == nil {
				var token string
				token, err = s.personaClaimMintTx(ctx, tx, taskID, StageInReview, entryPersona.Name)
				if err == nil {
					var evt Event
					evt, err = personaClaimEventTx(ctx, tx, cur.ProjectID, taskID, StageInReview, entryPersona, "engine", cur.Attempts)
					if err == nil {
						// The mint's event rides the nudge for post-commit
						// SSE publication (§4.6 persist-then-publish).
						postNudge = nudge{taskID: taskID, token: token, spawn: true, events: []Event{evt}}
					}
				}
			}
		} else {
			resulting, err = s.updateStageTx(ctx, tx, taskID, from, toStage,
				`claim_token=NULL, lease_expires_at=NULL, claim_incremented_attempts=false, claim_persona=NULL,
				 implementer_session=assignee_session`+reset_overlay, evidenceURL, actor)
		}

	case from == StageInProgress && toStage == StageDone:
		if err := s.doneGuard(cur, evidenceURL); err != nil {
			return res, nudge{}, err
		}
		resulting, err = s.updateStageTx(ctx, tx, taskID, from, toStage,
			`claim_token=NULL, lease_expires_at=NULL, claim_incremented_attempts=false, claim_persona=NULL`+reset_overlay, evidenceURL, actor)
		if err == nil {
			err = s.doneCascade(ctx, tx, cur, evidenceURL, &postNudge)
		}

	case from == StageInProgress && toStage == StagePendingInput:
		// FLAG SET from live work (000050: pending_input is a flag, not a
		// stage — the row keeps in_progress, the clock suspended by the
		// RC1 scan's flag exclusion). Token + identity retained.
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET pending_input=true, pending_origin='worker', updated_at=now()
			WHERE id=$1 AND stage=$2 AND NOT pending_input`, taskID, from)
		if err != nil {
			return res, nudge{}, err
		}
		if tag.RowsAffected() == 0 {
			return res, nudge{}, &GuardError{Code: GuardStage, Detail: "the row moved or was already flagged — server-arbitrated refusal"}
		}
		if _, err := recordEventTx(ctx, tx, cur.ProjectID, engineEventID("flag", taskID, cur.Attempts), "pending_input_flag",
			"task", taskID, actor, map[string]any{"task_id": taskID, "flag": true, "origin": "worker"}); err != nil {
			return res, nudge{}, err
		}
		resulting = from // the stage is unchanged; the flag rides it

	case from == StageInReview && toStage == StageInProgress && !cur.PendingInput:
		// The bounce (#1959): re-claim for the ORIGINAL IMPLEMENTER —
		// fresh token + lease minted in the same statement, flag=false
		// (its 429 never decrements), attempts unchanged, and
		// assignee_session stamped with the implementer IN the mint
		// (never a token-less in_progress row — the identity is known,
		// not awaited from an async stamp). The re-dispatch turn is
		// delivered to the implementer's EXISTING session post-commit
		// (nudge.redispatch); a gone/unresponsive original (never-turned,
		// delivery failure, or no captured identity) falls back to a
		// fresh-implementer spawn. The pre-#1959 guard — refuse on
		// assignee_session == "" — read the LIVE-CLAIM identity, which
		// the reviewer lifecycle empties for the whole mint→stamp window
		// (the live refusals: prompt reviewers verdict before their own
		// async stamp lands); it is gone: the bounce's identity input is
		// implementer_session (captured at the forward exit, preserved
		// through the review lifecycle). A paused row still refuses the
		// MINT (#1747: no re-entry while paused).
		if cur.Paused {
			return res, nudge{}, &GuardError{Code: GuardPaused, Detail: "the task is paused — the bounce re-claim refuses until resume"}
		}
		resulting, postNudge, err = s.bounceTx(ctx, tx, taskID, bounceImplementer(cur), actor)

	case from == StageInReview && toStage == StageDone:
		if err := s.doneGuard(cur, evidenceURL); err != nil {
			return res, nudge{}, err
		}
		resulting, err = s.updateStageTx(ctx, tx, taskID, from, toStage,
			`claim_token=NULL, lease_expires_at=NULL, claim_incremented_attempts=false, claim_persona=NULL`+reset_overlay, evidenceURL, actor)
		if err == nil {
			err = s.doneCascade(ctx, tx, cur, evidenceURL, &postNudge)
		}

	case from == StageInReview && toStage == StagePendingInput:
		// FLAG SET from review (000050: flag-not-stage; row stays in_review).
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET pending_input=true, pending_origin='worker', updated_at=now()
			WHERE id=$1 AND stage=$2 AND NOT pending_input`, taskID, from)
		if err != nil {
			return res, nudge{}, err
		}
		if tag.RowsAffected() == 0 {
			return res, nudge{}, &GuardError{Code: GuardStage, Detail: "the row moved or was already flagged — server-arbitrated refusal"}
		}
		if _, err := recordEventTx(ctx, tx, cur.ProjectID, engineEventID("flag", taskID, cur.Attempts), "pending_input_flag",
			"task", taskID, actor, map[string]any{"task_id": taskID, "flag": true, "origin": "worker"}); err != nil {
			return res, nudge{}, err
		}
		resulting = from

	case cur.PendingInput && (toStage == StageBacklog || toStage == StageInProgress):
		// FLAG CLEAR — the human exit (000050: the row's REAL stage was
		// never left; the exit clears the flag and takes the enumerated
		// end state). The ONE write that resets attempts (a human
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
				`claim_token=NULL, lease_expires_at=NULL, assignee_session=NULL, implementer_session=NULL, claim_persona=NULL,
				 claim_incremented_attempts=false, attempts=0, failure_count=0, last_attempt_failed=false, last_failure_at=NULL`, evidenceURL, actor)
		}

	case (from == StageInProgress || from == StageInReview) && toStage == StageFailed:
		// The nudge self-fail (000050): a worker that cannot complete
		// declares failure per the lease-nudge prose. Counts as the
		// attempt's failure; rearm is the exit. Token+identity cleared.
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET stage='failed', stage_entered_at=now(), pending_input=false, pending_origin=NULL,
				nudge_id=NULL, nudged_at=NULL,
				claim_token=NULL, lease_expires_at=NULL, assignee_session=NULL, implementer_session=NULL,
				claim_incremented_attempts=false, claim_persona=NULL,
				last_attempt_failed=true, failure_count=failure_count+1, last_failure_at=now()
			WHERE id=$1 AND stage=$2`, taskID, from)
		if err != nil {
			return res, nudge{}, err
		}
		if tag.RowsAffected() == 0 {
			return res, nudge{}, &GuardError{Code: GuardStage, Detail: "the row moved — server-arbitrated refusal"}
		}
		if err := writeTransition(ctx, txQ{tx}, taskID, from, StageFailed, evidenceURL, actor); err != nil {
			return res, nudge{}, err
		}
		resulting = StageFailed

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

// nudge is the deferred post-commit side-effect carrier (r1-C5: a
// rollback must never emit one). Under the merged design's re-spawn
// semantics the delivery is a SPAWN of the fresh worker that receives
// the token in its charter (optionally aborting a superseded live
// session first — the interrupt seam); there are no nudges to dead
// sessions anywhere. #1959 adds the re-dispatch arm: the bounce's
// fresh claim delivered to the ORIGINAL implementer's EXISTING session
// (redispatch target) — the ruling's bounce-target semantics; the
// fresh-spawn arm remains the fallback for a gone/unresponsive
// original. events carries nested settle events (the #1832
// review-complete/park coupling) for post-commit SSE publication;
// pokeProjects carries settle-touched projects for the dispatch drive
// (a settled review may live in a SIBLING project of the subject —
// review r1, finding 5: unblocked dependents there get the event-driven
// drive, not just the sweep's latency).
type nudge struct {
	taskID, token string
	spawn         bool
	abortAssignee string
	redispatch    string // #1959: existing-session re-dispatch target (the ORIGINAL implementer)
	events        []Event
	pokeProjects  []string
}

func (n nudge) fire(ctx context.Context, s *Service) {
	for _, evt := range n.events {
		s.publishCommitted(evt)
	}
	for _, pid := range n.pokeProjects {
		s.pokeDrive(pid)
	}
	if n.redispatch != "" {
		s.redispatchBounce(ctx, n.taskID, n.token, n.redispatch)
		return
	}
	if !n.spawn {
		return
	}
	if n.abortAssignee != "" && s.aborter != nil {
		_ = s.aborter(ctx, n.taskID, n.abortAssignee)
	}
	s.spawnFor(ctx, n.taskID, n.token)
}

// redispatchBounce delivers the #1959 bounce re-dispatch turn to the
// ORIGINAL implementer's existing session: the fresh bounce claim
// token + the embedded verdict chain (findings as iteration history).
// Fallback to a fresh-implementer spawn (the merged design's re-spawn
// semantics) fires ONLY when the original is gone/unresponsive: no
// delivery seam installed (the inert-engine shape), zero turn evidence
// (never-turned), or a delivery error (archived/reaped — the seam's
// own liveness answer).
//
// #1959c: the stored implementer identity is resolved to the RAW
// harness id BEFORE either consumer runs — the turn-evidence consult
// and the delivery seam are both raw-keyed (session_index.session_id;
// opencode's routes), and the row may hold the platform PHRASE (the
// durable identity, #1850/#1816 — the form the live 0.44.3 probe
// indicted). Unresolved upstream of the pre-check, a phrase reads
// "never indexed = never turned" at the evidence arm and the bounce
// falls back to a fresh spawn WITHOUT the delivery seam ever running —
// the ordering defect review F1 pinned (PR #1967 r1). Resolution is
// best-effort by the read edge's degraded posture (resolveImplementerRef):
// a phrase no live session holds keeps the stored ref, and the evidence
// consult's honest zero drives the fallback (a dead original's verdict).
func (s *Service) redispatchBounce(ctx context.Context, taskID, token, implementer string) {
	if s.redispatcher == nil {
		s.spawnFor(ctx, taskID, token) // no delivery seam: the fresh-worker fallback
		return
	}
	// #1959c: resolve the stored identity BEFORE the evidence consult —
	// see the function header (both raw-keyed consumers receive the raw
	// id; unresolved phrases degrade honestly).
	implementer = s.resolveImplementerRef(ctx, taskID, implementer)
	if s.turnEvidence != nil {
		if turns, err := s.turnEvidence.SessionTurnEvidence(ctx, implementer); err == nil && turns <= 0 {
			slog.WarnContext(ctx, "board: bounce re-dispatch skipped — original implementer never turned; falling back to a fresh worker",
				"task_id", taskID, "implementer", implementer)
			s.spawnFor(ctx, taskID, token)
			return
		}
	}
	t := s.mustTask(ctx, taskID)
	if t == nil {
		return
	}
	text := bounceRedispatchText(t, token, s.claimVerdictChain(ctx, t.ProjectID, taskID))
	if err := s.redispatcher(ctx, implementer, text, t.ShortID); err != nil {
		slog.WarnContext(ctx, "board: bounce re-dispatch delivery failed — falling back to a fresh worker",
			"task_id", taskID, "implementer", implementer, "err", err.Error())
		s.spawnFor(ctx, taskID, token)
	}
}

// bounceRedispatchText renders the re-dispatch turn's message: the
// fresh bounce claim token (the prior token died with the verdict),
// the task addressing, and the embedded verdict chain — the same
// iteration-history block the fresh-spawn charter carries
// (RenderVerdictChain), so the original worker sees byte-equivalent
// findings whether it is re-dispatched or replaced.
func bounceRedispatchText(t *Task, token string, verdicts []Verdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "BOARD BOUNCE RE-DISPATCH: your task %s failed review (verdict: fail) and was bounced back to in_progress.\n", t.ShortID)
	fmt.Fprintf(&b, "You are the ORIGINAL implementer — you hold the work's context. Address every outstanding finding below,\n")
	fmt.Fprintf(&b, "then move the task back to in_review with evidence.\n\n")
	fmt.Fprintf(&b, "Your FRESH claim token: %s (your prior token is void; this token authorizes your in-flight writes for this task only).\n\n", token)
	if len(t.AcceptanceCriteria) > 0 {
		b.WriteString("Acceptance criteria (all must be met before done, with evidence):\n")
		for _, crit := range t.AcceptanceCriteria {
			mark := " "
			if crit.Met {
				mark = "x"
			}
			fmt.Fprintf(&b, "  [%s] %s\n", mark, crit.Text)
		}
		b.WriteString("\n")
	}
	b.WriteString(RenderVerdictChain(verdicts))
	return b.String()
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

// doneCascade: the reverse-edge unblock + the RC4 completion check +
// the #1832 review settle (the subject's anchored review tasks complete
// or park with the transition), all in the →done transaction. The
// settle's nested events ride the post-commit nudge for publication.
func (s *Service) doneCascade(ctx context.Context, tx pgx.Tx, t *Task, evidenceURL string, n *nudge) error {
	if _, err := unblockDependents(ctx, txQ{tx}, t.ID, true); err != nil {
		return err
	}
	if _, err := epicCompletionCheck(ctx, tx, t.EpicID); err != nil {
		return err
	}
	evts, pokes, err := s.completeReviewTasks(ctx, tx, t, evidenceURL, nil)
	if err != nil {
		return err
	}
	n.events = append(n.events, evts...)
	n.pokeProjects = append(n.pokeProjects, pokes...)
	return nil
}

// updateStageTx applies one transition's column updates + the transition
// row. extra is spliced verbatim into SET. 000050: every STAGE
// transition clears the flag + nudge state (the flag ops above never
// route through here — a stage move off a flagged row resolves the
// flag with the move).
func (s *Service) updateStageTx(ctx context.Context, tx pgx.Tx, taskID, from, toStage, extra, evidenceURL string, actor Actor) (string, error) {
	extra += `, pending_origin=NULL, pending_input=false, nudge_id=NULL, nudged_at=NULL`
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

// bounceImplementer resolves the bounce's re-dispatch target (#1959):
// the ORIGINAL IMPLEMENTER. implementer_session is the forward exit's
// capture (the worker that moved the row to in_review) and survives the
// whole review lifecycle. The fallback preserves the LEGACY no-persona
// retain semantics for pre-migration rows (their retained
// assignee_session IS the implementer — a live persona claim means the
// column holds the reviewer's identity or the mint→stamp window's NULL,
// neither of which is a legal bounce target). "" = no original known:
// the bounce falls back to a fresh-implementer spawn.
func bounceImplementer(cur *Task) string {
	if cur.ImplementerSession != "" {
		return cur.ImplementerSession
	}
	if cur.ClaimPersona == "" && cur.AssigneeSession != "" {
		return cur.AssigneeSession
	}
	return ""
}

// resolveImplementerRef maps a PHRASE-form implementer identity to the
// raw harness id (#1959c — the consumption-side resolution feeding both
// raw-keyed bounce consumers; see redispatchBounce). Raw/legacy refs
// pass through unchanged and consult nothing (the #1663 lesson:
// resolution never makes an id unaddressable); a phrase resolves via
// the platform registry (session_index) scoped to the task's project's
// target workspace. Best-effort by the read edge's degraded posture:
// an unresolved phrase keeps the stored ref — the turn-evidence
// consult's zero then drives the fresh-worker fallback, the honest
// verdict for an original no live session holds.
func (s *Service) resolveImplementerRef(ctx context.Context, taskID, ref string) string {
	if !shortid.IsShortIDForm(ref) {
		return ref
	}
	var ws string
	if err := s.q().QueryRow(ctx,
		`SELECT p.target_workspace_id::text FROM tasks t JOIN projects p ON p.id = t.project_id
		 WHERE t.id::text = $1`, taskID).Scan(&ws); err != nil {
		// Transient DB error and genuinely-unknown workspace share the
		// degraded posture (stored ref kept), but the log names which
		// one fired — never misattribute an outage (review r2 minor).
		slog.WarnContext(ctx, "board: bounce implementer phrase unresolved — task workspace lookup failed; keeping the stored ref",
			"task_id", taskID, "implementer", ref, "err", err.Error())
		return ref
	}
	if ws == "" {
		slog.WarnContext(ctx, "board: bounce implementer phrase unresolved — task workspace unknown; keeping the stored ref",
			"task_id", taskID, "implementer", ref)
		return ref
	}
	raw, err := LookupSessionIDByPhrase(ctx, s.q(), ws, ref)
	if err != nil {
		slog.WarnContext(ctx, "board: bounce implementer phrase lookup failed — keeping the stored ref",
			"task_id", taskID, "implementer", ref, "err", err.Error())
		return ref
	}
	if raw == "" {
		slog.WarnContext(ctx, "board: bounce implementer phrase resolves to no live session — keeping the stored ref (the evidence consult's zero drives the fallback)",
			"task_id", taskID, "implementer", ref)
		return ref
	}
	return raw
}

// bounceTx mints the #1959 bounce re-claim: fresh token + lease,
// attempts untouched (flag=false — the throttle never rolls it back),
// claim persona cleared (the bounced claim is implementer-class), and —
// when the original implementer is known — assignee_session stamped
// with it IN the mint statement (the never-token-less invariant holds
// by construction; no async stamp is awaited). The returned nudge
// carries either the re-dispatch target (existing-session delivery
// post-commit) or the fresh-spawn arm (no identity). implementer=""
// mints assignee_session=NULL for the fallback's fresh worker, whose
// guarded spawn-stamp then records the fresh identity.
func (s *Service) bounceTx(ctx context.Context, tx pgx.Tx, taskID, implementer string, actor Actor) (string, nudge, error) {
	token := uuidNew()
	var assignee *string // nil = the fresh-worker fallback's unstamped shape
	if implementer != "" {
		assignee = &implementer
	}
	tag, err := tx.Exec(ctx, `
		UPDATE tasks SET stage='in_progress', stage_entered_at=now(),
			claim_token=$2, lease_expires_at=now()+make_interval(secs=>$3::float8), last_progress_at=now(),
			assignee_session=$4, claim_incremented_attempts=false, claim_persona=NULL, pending_origin=NULL
		WHERE id=$1 AND NOT paused`,
		taskID, token, s.cfg.LeaseDuration.Seconds(), assignee)
	if err != nil {
		return "", nudge{}, err
	}
	if tag.RowsAffected() == 0 {
		return "", nudge{}, &GuardError{Code: GuardStage, Detail: "row is terminal — the write is a no-op (RC5)"}
	}
	if implementer != "" {
		return StageInProgress, nudge{taskID: taskID, token: token, redispatch: implementer}, nil
	}
	return StageInProgress, nudge{taskID: taskID, token: token, spawn: true}, nil
}

// exitPendingReclaim is the flag-clear's re-claim branch (000050: this
// clears the flag, resets the ledger, LANDS the enumerated in_progress
// end state, and mints the fresh claim): fresh token+lease with the
// recorded identity cleared (the interim-state shape), the superseded
// session aborted (its question is void with the exit — the interrupt
// seam, merged-doc §4.1), and the fresh worker spawned after commit.
func (s *Service) exitPendingReclaim(ctx context.Context, tx pgx.Tx, actor Actor, cur *Task) (string, bool, nudge, error) {
	token := uuidNew()
	// stage='in_progress' is explicit: a no-op for the common
	// worker-origin park (the row kept in_progress), and the enumerated
	// LANDING for a drift_review park on in_review — the real stage moves
	// with the re-claim, and the transition row below records that move.
	tag, err := tx.Exec(ctx, `
		UPDATE tasks SET stage='in_progress', stage_entered_at=now(), pending_input=false, pending_origin=NULL,
			nudge_id=NULL, nudged_at=NULL,
			claim_token=$2, lease_expires_at=now()+make_interval(secs=>$3::float8), last_progress_at=now(),
			assignee_session=NULL, implementer_session=NULL, claim_incremented_attempts=false, claim_persona=NULL, attempts=0,
			failure_count=0, last_attempt_failed=false, last_failure_at=NULL
		WHERE id=$1 AND pending_input`,
		cur.ID, token, s.cfg.LeaseDuration.Seconds())
	if err != nil {
		return "", false, nudge{}, err
	}
	if tag.RowsAffected() == 0 {
		return "", false, nudge{}, &GuardError{Code: GuardStage, Detail: "row moved — the exit raced (server-arbitrated)"}
	}
	if cur.Stage != StageInProgress {
		// A real stage move accompanies the flag-clear (the drift_review
		// shape): the transition row audits the MOVE; the flag event
		// below audits the flag side for every exit.
		if err := writeTransition(ctx, txQ{tx}, cur.ID, cur.Stage, StageInProgress, "", actor); err != nil {
			return "", false, nudge{}, err
		}
	}
	if _, err := recordEventTx(ctx, tx, cur.ProjectID, engineEventID("flagclear", cur.ID, cur.Attempts), "pending_input_flag",
		"task", cur.ID, actor, map[string]any{"task_id": cur.ID, "flag": false, "reclaimed": true}); err != nil {
		return "", false, nudge{}, err
	}
	return StageInProgress, true, nudge{taskID: cur.ID, token: token, spawn: true, abortAssignee: cur.AssigneeSession}, nil
}

// --- Heartbeat (nudge response) + throttle -------------------------------------------

// heartbeatFutureTolerance bounds honest client-clock drift: an
// observedProgressAt up to this far ahead of the SERVER clock is clamped
// to the server clock (a FUTURE stamp is never stored — the C1 poison
// direction); a stamp beyond it is a hallucinated estimate, not
// evidence, and rejects outright (#1847 PR1: the C1 incident class).
// PAST-skewed client stamps remain client-trusted. The tolerance
// SURVIVES the repurposing (item 6 amendment): worker stamps are
// RESPONSE events now, not the liveness input — but a response stamp
// is still stored on the row, so the future direction stays clamped.
const heartbeatFutureTolerance = 2 * time.Minute

// Heartbeat is the NUDGE-RESPONSE channel (item 6 amendment,
// 2026-10-28: "when we nudge, we also require the call to
// board_heartbeat, rather than having to history dive").
//
// The PERIODIC self-report semantics are DEAD: a heartbeat on a row
// carrying NO live nudge never extends the lease — there is no standing
// renewal discipline anywhere (liveness is platform-observed through
// the AssigneeLivenessSource; the reconciler renews on movement). The
// call still validates the claim (409 claim_token arbitration,
// unchanged) and records the arrival (last_heartbeat_at, server clock)
// — the ordinal input the wedge predicate reads: a heartbeat arriving
// AFTER nudged_at is the strong ALIVE signal OR'd in at every rung of
// the ladder, and the ONLY wedge detector when no liveness source can
// answer (deliver-a-message + call-a-tool is the harness-agnostic
// floor).
//
// A heartbeat on a NUDGED row (nudge_id set) is the response the nudge
// prose demands ("First RESPOND (call board_heartbeat once) to confirm
// you are active"): the lease renews, the nudge slot clears, and the
// wedge clock is dead — the worker may then run long commands safely.
// Requires the matching claim token; #1847 future-clamping applies to
// the carried stamp in every shape.
func (s *Service) Heartbeat(ctx context.Context, actor Actor, taskID, shortIDOrID string, observedProgressAt time.Time) (HeartbeatResult, error) {
	t, err := s.resolveTaskForActor(ctx, actor, pick(taskID, shortIDOrID))
	if err != nil {
		return HeartbeatResult{}, err
	}
	// A live claim exists in in_progress (the worker grant/re-claim)
	// and in in_review (the stage-persona claim — a persona worker
	// holds the same token+lease). An UNCLAIMED in_review row has a
	// NULL token and rejects (empty token never matches) — today's
	// semantics, unchanged. 000050: a flagged row (the old
	// pending_input stage) rejects the same way — the input wait
	// suspends the claim lifecycle, and the resume lane re-arms the
	// lease itself.
	claimStage := (t.Stage == StageInProgress || (t.Stage == StageInReview && t.ClaimToken != "")) && !t.PendingInput
	if !claimStage || actor.ClaimToken == "" || actor.ClaimToken != t.ClaimToken {
		return HeartbeatResult{}, &GuardError{Code: GuardClaimToken, Detail: "heartbeat requires a live claim (stage + token)"}
	}
	serverNow := time.Now().UTC()
	switch {
	case observedProgressAt.After(serverNow.Add(heartbeatFutureTolerance)):
		return HeartbeatResult{}, &GuardError{Code: GuardInput, Detail: fmt.Sprintf(
			"observedProgressAt %s is stamped in the future (server clock %s) — future stamps are not progress evidence; send the timestamp of your LAST observed progress, never an estimate",
			observedProgressAt.Format(time.RFC3339), serverNow.Format(time.RFC3339))}
	case observedProgressAt.After(serverNow):
		observedProgressAt = serverNow // clamp: a future stamp never stores — the server clock is the authority over the poison direction
	}
	// The response arm: ONE guarded write decides atomically whether a
	// live nudge was answered. The nudge_id predicate makes the race
	// safe — a heartbeat landing between the reconciler's nudge send
	// and its row write either sees the slot (and clears it) or the
	// next sweep's predicate sees last_heartbeat_at > nudged_at. The
	// stillborn consultation is GONE from this path: a zero-turn
	// session cannot call tools, so it can never answer a nudge — the
	// wedge verdict owns corpses (the turnEvidence seam survives for
	// the #1959c bounce re-dispatch).
	var lease, progress time.Time
	responded := false
	err = s.q().QueryRow(ctx, `
		UPDATE tasks SET last_heartbeat_at=now(),
			last_progress_at=$2, lease_expires_at=now()+make_interval(secs=>$3::float8),
			nudge_id=NULL, nudged_at=NULL, updated_at=now()
		WHERE id=$1 AND stage IN ('in_progress','in_review') AND claim_token=$4
		  AND NOT pending_input AND nudge_id IS NOT NULL
		RETURNING lease_expires_at`,
		t.ID, observedProgressAt, s.cfg.LeaseDuration.Seconds(), actor.ClaimToken).Scan(&lease)
	switch {
	case err == nil:
		responded = true
		progress = observedProgressAt
		s.writeAudit(ctx, t.ProjectID, "heartbeat", "task", t.ID, actor)
	case errors.Is(err, pgx.ErrNoRows):
		// No live nudge (or the claim raced away): record the arrival
		// — the advisory shape. The lease is NOT extended (the
		// standing discipline stays dead); a raced claim falls through
		// the token re-check below.
		lease = derefTime(t.LeaseExpiresAt)
		progress = derefTime(t.LastProgressAt)
		tag, terr := s.q().Exec(ctx, `
			UPDATE tasks SET last_heartbeat_at=now(), updated_at=now()
			WHERE id=$1 AND stage IN ('in_progress','in_review') AND claim_token=$2 AND NOT pending_input`,
			t.ID, actor.ClaimToken)
		if terr != nil {
			return HeartbeatResult{}, terr
		}
		if tag.RowsAffected() == 0 {
			return HeartbeatResult{}, &GuardError{Code: GuardClaimToken, Detail: "the claim moved during the heartbeat — re-read the task (the token no longer arbitrates)"}
		}
	default:
		return HeartbeatResult{}, err
	}
	res := HeartbeatResult{Extended: responded, LeaseExpiresAt: lease, LastProgressAt: progress}
	if !responded {
		res.Reason = "no active lease nudge — periodic heartbeats do not extend the lease (no standing renewal discipline); respond when nudged"
	}
	return res, nil
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
	if t.Stage != StageInProgress || t.PendingInput || actor.ClaimToken == "" || actor.ClaimToken != t.ClaimToken {
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
				claim_token=NULL, lease_expires_at=NULL, assignee_session=NULL, implementer_session=NULL,
				claim_incremented_attempts=false, claim_persona=NULL,
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
	out, err := s.store.resolveTask(ctx, s.q(), p.ID, t.ID)
	if err != nil {
		return nil, err
	}
	// #1922: the throttle echo rides the read-edge phrase translation.
	s.translateTaskReads(ctx, out)
	return out, nil
}

// resolveTaskForActor resolves a task through the actor's scope: the
// agent surfaces read workspace-scoped (r1-S1 — an unscoped read leaked
// cross-tenant rows and claim tokens; #1832 WS2 extends the scoping to
// the manager-grade agent surface — same pod, same binding); manager
// surfaces resolve by id (ownership follows via requireOwner).
func (s *Service) resolveTaskForActor(ctx context.Context, actor Actor, ref string) (*Task, error) {
	if isAgentSurface(actor.Surface) && actor.WorkspaceID != "" {
		return s.store.resolveTaskInWorkspace(ctx, s.q(), actor.WorkspaceID, ref)
	}
	return s.store.resolveTask(ctx, s.q(), "", ref)
}

// isAgentSurface reports whether the surface is one of the in-pod
// agent projections (worker or manager-grade) — the two surfaces the
// caller-workspace binding governs.
func isAgentSurface(surface string) bool {
	return surface == SurfaceAgent || surface == SurfaceAgentManager
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
