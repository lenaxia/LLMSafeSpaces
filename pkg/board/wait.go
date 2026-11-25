// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// wait.go — the typed-wait entry verb (claim-wait lifecycle, low-level
// design §2; migration 000060): board_wait, the WORKER surface's wait
// declaration. A wait is a claim-holder's statement, never a manager
// imposition — the claim token is the guard on every arm.
//
// waitOn='user' is today's 000050 flag-op enter, byte-identically:
// pending_input=true, pending_origin='worker', wait_on='user',
// wait_reason — the ladder freezes (the victims scan's NOT
// t.pending_input predicate is untouched), the exits stay the
// enumerated manager ones. move_stage(toStage=pending_input) keeps
// working as the alias (implicit wait_on='user').
//
// waitOn='event' is the NEW state: pending_input stays FALSE, the row
// stays in_progress with its claim, wait_on='event' + wait_reason go
// on the row, and the declarative condition is validated HERE (the
// closed grammar below) and recorded ONLY in the wait_declared event
// payload — the row stores the human prose; nothing executable ever
// crosses the worker surface. The ladder does NOT freeze an event-wait
// (§3: event-waits are full ladder citizens). No broker dependency:
// event-waits declare and sit; resolution is the wait-event-broker
// story's machinery consuming the wait_declared events minted here.

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// MaxWaitReasonBytes caps wait_reason prose (the textguard seam's field
// cap; the design's 512 — a reason is a pointer for humans, never a
// charter).
const MaxWaitReasonBytes = 512

// WaitOn vocabulary (migration 000060's CHECK constraint).
const (
	WaitOnUser  = "user"
	WaitOnEvent = "event"
)

// WaitCondition is a parsed declarative condition — DATA, never code
// (the broker compiles it into a gate script; the worker surface only
// ever sees this grammar). Primitive ∈ {pr_merged, ci_green,
// story_done}; Arg is a PR number (1..9 digits) or a tsk-… story short
// id per the primitive's argument class.
type WaitCondition struct {
	Primitive string `json:"primitive"`
	Arg       string `json:"arg"`
}

// String renders the canonical wire form (the grammar's spelling — the
// form the broker re-parses and the audit payload carries).
func (c WaitCondition) String() string {
	return c.Primitive + "(" + c.Arg + ")"
}

// waitConditionGrammar is the accepted-vocabulary error suffix every
// parse refusal carries (the textguard discipline: refuse loudly, name
// what IS accepted).
const waitConditionGrammar = `condition must be primitive "(" arg ")" with primitive ∈ {pr_merged, ci_green, story_done}; arg is a 1-9 digit PR number (pr_merged/ci_green) or a tsk-… story short id (story_done) — e.g. pr_merged(2101), ci_green(2101), story_done(tsk-ebony-javelin-tramps)`

// WaitRepoGrammar is the accepted-vocabulary error suffix every repo
// refusal carries: the declarative repo identity rides the wait_declared
// payload (the patched charter — the broker compiles GitHub-anchored
// templates from it and never guesses), so it validates at the verb
// edge with the same closed-grammar discipline as the condition.
const WaitRepoGrammar = `repo must be "<owner>/<repo>" — a GitHub owner (alphanumeric with single inner hyphens) and repository (alphanumeric with . _ -), e.g. "lenaxia/llmsafespaces"`

// ConditionNeedsRepo reports whether the primitive's sensor is
// GitHub-anchored and therefore requires the declaration-time repo
// identity (pr_merged/ci_green true; story_done reads the platform's
// own board and carries none).
func ConditionNeedsRepo(primitive string) bool {
	return primitive == "pr_merged" || primitive == "ci_green"
}

// ParseWaitCondition is parseWaitCondition exported for the app seam
// (low-level design §4.1: the grammar is SHARED by the verb edge and
// the broker — the broker re-parses the event payload's condition so
// nothing unparseable can ever reach a trigger even via a hand-edited
// event row).
func ParseWaitCondition(s string) (WaitCondition, error) { return parseWaitCondition(s) }

// validateWaitRepo runs the closed repo grammar ("owner/repo", the
// GitHub shape) over the declaration-time repo identity. The grammar
// pins shape, not existence — resolution of the PR against it is the
// sensor's job at poll time.
func validateWaitRepo(repo string) error {
	slash := strings.Count(repo, "/")
	if slash != 1 || len(repo) > 128 || len(repo) < 3 {
		return &GuardError{Code: GuardInput, Detail: "wait repo " + strconv.Quote(repo) + " does not parse: " + WaitRepoGrammar}
	}
	owner, name, _ := strings.Cut(repo, "/")
	if !isGitHubName(owner) || !isGitHubName(name) || strings.ContainsAny(owner, "._") {
		return &GuardError{Code: GuardInput, Detail: "wait repo " + strconv.Quote(repo) + " does not parse: " + WaitRepoGrammar}
	}
	return nil
}

// isGitHubName: the [-.\w]+ class (repo names additionally allow dots;
// owners do not — the asymmetry is pinned above by the caller).
func isGitHubName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.':
		default:
			return false
		}
	}
	return true
}

// parseWaitCondition parses the closed v1 grammar (low-level design
// §4.1 — shared by the verb edge and the broker):
//
//	condition := primitive "(" arg ")"
//	primitive := "pr_merged" | "ci_green" | "story_done"
//	arg       := 1..9 digit run (PR number) | tsk-[a-z-]+ (story short id)
//
// pr_merged/ci_green take a PR number; story_done takes a story short
// id. The grammar is CLOSED: anything else refuses with the accepted
// vocabulary in the error (never a silent best-effort parse — nothing
// executable may cross the worker surface on a guess).
func parseWaitCondition(s string) (WaitCondition, error) {
	open := strings.IndexByte(s, '(')
	if open < 0 || !strings.HasSuffix(s, ")") || len(s) < open+3 {
		return WaitCondition{}, &GuardError{Code: GuardInput, Detail: "wait condition " + strconv.Quote(s) + " does not parse: " + waitConditionGrammar}
	}
	primitive := s[:open]
	arg := s[open+1 : len(s)-1]
	switch primitive {
	case "pr_merged", "ci_green":
		if !isPRNumber(arg) {
			return WaitCondition{}, &GuardError{Code: GuardInput, Detail: primitive + " takes a PR number (1-9 digits), got " + strconv.Quote(arg) + ": " + waitConditionGrammar}
		}
	case "story_done":
		if !isStoryShortID(arg) {
			return WaitCondition{}, &GuardError{Code: GuardInput, Detail: "story_done takes a tsk-… story short id (lowercase words + hyphens), got " + strconv.Quote(arg) + ": " + waitConditionGrammar}
		}
	default:
		return WaitCondition{}, &GuardError{Code: GuardInput, Detail: "unknown wait-condition primitive " + strconv.Quote(primitive) + ": " + waitConditionGrammar}
	}
	return WaitCondition{Primitive: primitive, Arg: arg}, nil
}

// isPRNumber: a canonical positive decimal, 1..9 digits (no zero PR,
// no leading zeros — GitHub PR numbers are positive integers in their
// canonical spelling; "07" is a different string, not a different PR).
func isPRNumber(s string) bool {
	if len(s) < 1 || len(s) > 9 || s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isStoryShortID: the tsk-[a-z-]+ class (design §4.1's literal — the
// phrase form's lowercase word run; resolution is the broker's job,
// the grammar only pins the shape).
func isStoryShortID(s string) bool {
	if len(s) < 5 || !strings.HasPrefix(s, "tsk-") {
		return false
	}
	for i := 4; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && c != '-' {
			return false
		}
	}
	return true
}

// composeWaitingOn renders the wire's composed prose (low-level design
// §2.3): the event-wait's reason bare ("PR 2083 merge"), the user-wait
// prefixed ("user: runtime bump scope" — the human is the blocker,
// the reason names what they hold). Empty wait_on composes nothing.
func composeWaitingOn(waitOn, waitReason string) string {
	switch waitOn {
	case WaitOnUser:
		if waitReason == "" {
			return WaitOnUser
		}
		return WaitOnUser + ": " + waitReason
	case WaitOnEvent:
		return waitReason
	}
	return ""
}

// waitEventID builds the wait lifecycle's deterministic event ids
// (low-level design §2.1): wait:<taskID>:<claimToken>:<unix-nanos> —
// the persona-escalation nanos precedent, so a within-episode
// re-declaration gets its OWN event (replays converge by the guards:
// the second event-wait refuses wait_already_declared; the flag-op
// refuses already-flagged). seq separates the (rare) cleared+declared
// pair a conversion writes inside one transaction.
func waitEventID(taskID, claimToken string, nanos int64, seq int64) string {
	return "wait:" + taskID + ":" + claimToken + ":" + strconv.FormatInt(nanos+seq, 10)
}

// validateWaitReason runs the textguard seam over wait_reason: the
// 512-byte cap, the corruption heuristics, and the whole-value
// placeholder-token scan (#1820 discipline — a placeholder reason is
// the emission-corruption substitute, never a wait statement).
func validateWaitReason(reason string) error {
	if err := validateTextField("wait_reason", reason, MaxWaitReasonBytes); err != nil {
		return err
	}
	if IsPlaceholderClass(reason) {
		return seedFieldPlaceholderError("wait_reason")
	}
	return nil
}

// DeclareWait is board_wait — the typed-wait entry verb (worker
// surface, claim token required on every arm). waitOn='user' enters
// today's pending_input flag-op byte-identically (plus the typed
// dress); waitOn='event' parks the claim on a declarative condition
// the platform (the wait-broker, a later story) resolves. Any
// worker-anchored advance clears the wait — the exits are the stage
// machine's, never this verb's (a wait has no self-exit; the worker
// either advances, or the enumerated exits move the row).
//
// repo is the declaration-time repo identity ("owner/repo", closed
// grammar): REQUIRED for pr-anchored conditions (pr_merged/ci_green),
// refused for story_done and user waits. It is DATA — it rides the
// wait_declared payload and the broker compiles the sensor from it;
// the broker never guesses (the patched charter's change).
func (s *Service) DeclareWait(ctx context.Context, actor Actor, taskID, shortIDOrID, waitOn, waitReason, condition, repo, requestID string) (*Task, error) {
	// Argument validation at the verb edge, loudly, before any row is
	// touched (the grammar is closed HERE — nothing unparseable ever
	// reaches the event the broker consumes).
	switch waitOn {
	case WaitOnUser:
		if condition != "" {
			return nil, &GuardError{Code: GuardInput, Detail: "waitOn='user' takes no condition — a user-wait's exit is the human's decision, not a declarative event (drop the condition, or declare waitOn='event')"}
		}
		if repo != "" {
			return nil, &GuardError{Code: GuardInput, Detail: "waitOn='user' takes no repo — the repo identity rides event-wait declarations only (the GitHub-anchored conditions)"}
		}
	case WaitOnEvent:
		cond, err := parseWaitCondition(condition)
		if err != nil {
			return nil, err
		}
		if ConditionNeedsRepo(cond.Primitive) {
			if repo == "" {
				return nil, &GuardError{Code: GuardInput, Detail: cond.Primitive + " requires the repo identity (repo: \"<owner>/<repo>\") — the sensor polls that repository; the broker never guesses it"}
			}
			if err := validateWaitRepo(repo); err != nil {
				return nil, err
			}
		} else if repo != "" {
			return nil, &GuardError{Code: GuardInput, Detail: "story_done carries no repo — the condition reads the platform's own board"}
		}
	default:
		return nil, &GuardError{Code: GuardInput, Detail: "waitOn must be 'user' or 'event' (the typed-wait vocabulary, migration 000060), got " + strconv.Quote(waitOn)}
	}
	if waitOn == WaitOnEvent && strings.TrimSpace(waitReason) == "" {
		return nil, &GuardError{Code: GuardInput, Detail: "wait_reason is required on an event-wait — the reason is what every surface (wire, health, UI badge) shows for what the story needs"}
	}
	if err := validateWaitReason(waitReason); err != nil {
		return nil, err
	}

	t, err := s.resolveTaskForActor(ctx, actor, pick(taskID, shortIDOrID))
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

	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "wait", itemKind: "task", itemID: t.ID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		return s.declareWaitTx(ctx, tx, actor, t.ID, waitOn, waitReason, condition, repo)
	})
	if err != nil {
		return nil, err
	}
	_ = dup // 200-with-original: the reloaded row IS the original outcome
	out, err := s.store.resolveTask(ctx, s.q(), p.ID, t.ID)
	if err != nil {
		return nil, err
	}
	// #1922: the wait echo rides the read-edge phrase translation.
	s.translateTaskReads(ctx, out)
	return out, nil
}

// declareWaitTx performs one guarded wait declaration inside tx (the
// row is locked and re-read — server arbitration, the SetCriteriaMet
// posture): stage, token, and the wait-state guards re-run against the
// fresh row, then the typed write + the deterministic wait events land
// in the same transaction.
func (s *Service) declareWaitTx(ctx context.Context, tx pgx.Tx, actor Actor, taskID, waitOn, waitReason, condition, repo string) (map[string]any, error) {
	cur, err := s.store.taskForUpdate(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	// Stage guard: the verb enters a wait from LIVE WORK only (the
	// flag-op's own stage discipline — in_progress; a reviewer's
	// in_review claim is not a waiting worker, and a parked row is
	// already waiting). Pause does NOT gate entry (the flag-op's own
	// rule: the question path stays open on a paused row).
	if cur.Stage != StageInProgress {
		return nil, &GuardError{Code: GuardStage, Detail: "board_wait enters a wait from in_progress only — the row is " + cur.Stage + " (the flag-op alias move_stage(toStage=pending_input) keeps its own stage set)"}
	}
	// The in-flight guard, every arm: a wait is a claim-holder's
	// declaration (empty or stale tokens never match).
	if actor.ClaimToken == "" || actor.ClaimToken != cur.ClaimToken {
		return nil, &GuardError{Code: GuardClaimToken, Detail: "wait declarations require the row's live claim token (the in-flight guard from your charter envelope)"}
	}

	outcome := map[string]any{"task_id": taskID, "wait_on": waitOn}
	nanos := time.Now().UnixNano()

	switch waitOn {
	case WaitOnUser:
		// Byte-identical flag-op enter (the move_stage alias's write +
		// its already-flagged refusal), plus the typed dress. An
		// event-waiting row CONVERTS here: the flag wins, wait_on='user'.
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET pending_input=true, pending_origin='worker', wait_on='user', wait_reason=$2, updated_at=now()
			WHERE id=$1 AND stage='in_progress' AND NOT pending_input`, taskID, waitReason)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, &GuardError{Code: GuardStage, Detail: "the row moved or was already flagged — server-arbitrated refusal"}
		}
		// Today's flag audit, byte-identical (same deterministic id,
		// same payload): ON CONFLICT DO NOTHING keeps it inert against
		// the alias's own write for this episode.
		if _, err := recordEventTx(ctx, tx, cur.ProjectID, engineEventID("flag", taskID, cur.Attempts), "pending_input_flag",
			"task", taskID, actor, map[string]any{"task_id": taskID, "flag": true, "origin": "worker"}); err != nil {
			return nil, err
		}
		if cur.WaitOn == WaitOnEvent {
			// Conversion: the event-wait ended at the flag — audited so
			// the broker's orphan-clean sees the transition in the trail.
			if _, err := recordEventTx(ctx, tx, cur.ProjectID, waitEventID(taskID, cur.ClaimToken, nanos, 0), "wait_cleared",
				"task", taskID, actor, map[string]any{"task_id": taskID, "cleared": "user_conversion"}); err != nil {
				return nil, err
			}
			outcome["converted_from"] = WaitOnEvent
		}
		outcome["wait_reason"] = waitReason

	case WaitOnEvent:
		// Cap: ONE active event-wait per story — a live event-wait
		// refuses re-declaration (the advance clears; the next
		// declaration is the next episode).
		if cur.WaitOn == WaitOnEvent {
			return nil, &GuardError{Code: GuardWaitDeclared, Detail: "this story already carries an active event-wait — one per story (advance the row or clear the wait before declaring another)"}
		}
		cond, err := parseWaitCondition(condition)
		if err != nil {
			return nil, err
		}
		// A flagged row CONVERTS: the flag clears, wait_on='event', the
		// reason replaces the user-wait's (pendingCoupling's auto-stage
		// machinery keys on the flag — it must not re-stage the row out
		// from under the event-wait).
		tag, err := tx.Exec(ctx, `
			UPDATE tasks SET wait_on='event', wait_reason=$2, pending_input=false, pending_origin=NULL, updated_at=now()
			WHERE id=$1 AND stage='in_progress'`, taskID, waitReason)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, &GuardError{Code: GuardStage, Detail: "the row moved — server-arbitrated refusal"}
		}
		if cur.PendingInput {
			if _, err := recordEventTx(ctx, tx, cur.ProjectID, waitEventID(taskID, cur.ClaimToken, nanos, 0), "wait_cleared",
				"task", taskID, actor, map[string]any{"task_id": taskID, "cleared": "event_conversion"}); err != nil {
				return nil, err
			}
			outcome["converted_from"] = WaitOnUser
		}
		// The declaration the broker consumes: condition DATA + the
		// declaration-time repo identity in the payload, prose on the
		// row — nothing executable crosses the worker surface, and the
		// broker never guesses the repository (the patched charter).
		if _, err := recordEventTx(ctx, tx, cur.ProjectID, waitEventID(taskID, cur.ClaimToken, nanos, 1), "wait_declared",
			"task", taskID, actor, map[string]any{
				"task_id": taskID, "wait_on": WaitOnEvent, "wait_reason": waitReason,
				"condition":   map[string]any{"primitive": cond.Primitive, "arg": cond.Arg, "text": cond.String()},
				"repo":        repo,
				"short_id":    cur.ShortID,
				"assignee":    cur.AssigneeSession,
				"claim_token": cur.ClaimToken,
			}); err != nil {
			return nil, err
		}
		outcome["wait_reason"] = waitReason
		outcome["condition"] = cond.String()
		if repo != "" {
			outcome["repo"] = repo
		}
	default:
		return nil, &GuardError{Code: GuardInput, Detail: "waitOn must be 'user' or 'event'"}
	}
	return outcome, nil
}
