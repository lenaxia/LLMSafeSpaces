// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// wrapup.go — the epic wrap-up review engine seam (low-level design §§2-3,
// §7; high-level decisions D1-D3, D9). One interception point replaces
// epicCompletionCheck: every path that lands a story in done (the stage
// machine's doneCascade, the review settle, the lifecycle recheck, the RC2
// drift-done, the RC4 net) funnels through wrapUpAwareEpicCompletion, so
// PR-drift completions, review-settle completions, manager moves and the
// reconciler's sweeps all inherit identical wrap-up behavior with no
// per-site patching (D1). The mint decision is transactionally inside the
// same →done write that reached drain — no window between "drained" and
// "wrap-up minted" in which a concurrent writer could complete the epic.
//
// The wrap-up lifecycle state is EXPLICIT COLUMNS on epics (D2) — never
// derived from "does a wrap-up row exist" (racy) and never implied by row
// counting (the self-blocking paradox): wrapup_task_id is the in-flight
// pointer, wrapup_count the consecutive-close ledger, wrapup_ack_pending
// the human circuit-breaker (armed here; the ack verb itself is the
// wrapup-gap-recursion story's), last_wrapped_at the coverage watermark
// (advanced at close by the verdict story).

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Task row classes (§1b): 'story' is the default and the only value
// user-created rows can hold — the service layer never writes 'wrapup'
// outside the engine mint. Self-exclusion (drain + completion predicates)
// and the dispatch carve-outs key off this column.
const (
	TaskKindStory  = "story"
	TaskKindWrapup = "wrapup"
)

// WrapUpStoryRef is one covered story of a minted charter's window (the
// §5 delta list's read shape; the delta-query correctness itself is the
// wrapup-watermark-diff story's).
type WrapUpStoryRef struct {
	ShortID  string
	Title    string
	DoneAt   time.Time
	Evidence string
}

// WrapUpCharterInput is the deterministic input set of RenderWrapUpCharter
// — a pure function of (epic, window, stories, cap state), rendered ONCE
// at mint and frozen on the row (a re-claimed reviewer reads the same
// window; convergence, not drift).
type WrapUpCharterInput struct {
	Epic           *Epic
	WindowFrom     time.Time
	WindowTo       time.Time
	Stories        []WrapUpStoryRef
	WrapUpCount    int // consecutive closes BEFORE this wrap-up
	ConsecutiveCap int
}

// RenderWrapUpCharter renders the wrap-up story's DESCRIPTION (engine-owned
// procedure; the role owns judgment — D8's split), byte-exact per §5's block
// structure: header, epic identity, the outcome VERBATIM, the RFC 3339
// coverage window pair, the covered story list with the rendered evidence
// fallback ("(none recorded)"), the aggregate-diff instruction, THE REVIEW
// procedure, and the lease-nudge trailer. The engine never runs git (D5) —
// the window + story list are DATA; the reviewer computes the diff.
func RenderWrapUpCharter(in WrapUpCharterInput) string {
	var b strings.Builder
	b.WriteString("Epic wrap-up review — holistic, delta-scoped, against the epic's outcome.\n\n")
	fmt.Fprintf(&b, "Epic: %s %q (project %s)\n", in.Epic.ShortID, in.Epic.Title, in.Epic.ProjectName)
	b.WriteString("Outcome (the bar this epic exists to achieve):\n  ")
	b.WriteString(in.Epic.Outcome)
	b.WriteString("\n\n")
	b.WriteString("COVERAGE WINDOW (UTC, both ends inclusive-bound):\n")
	fmt.Fprintf(&b, "  from %s\n", in.WindowFrom.Format(time.RFC3339))
	fmt.Fprintf(&b, "  to   %s\n", in.WindowTo.Format(time.RFC3339))
	fmt.Fprintf(&b, "Stories in this window (%d):\n", len(in.Stories))
	for _, st := range in.Stories {
		ev := st.Evidence
		if ev == "" {
			ev = "(none recorded)"
		}
		fmt.Fprintf(&b, "  - %s %q — done %s; evidence: %s\n",
			st.ShortID, st.Title, st.DoneAt.UTC().Format(time.RFC3339), ev)
	}
	if len(in.Stories) == 0 {
		b.WriteString("  (none)\n")
	}
	b.WriteString(`Aggregate diff: in the project's target workspace run, after fetching origin,
  git log --no-merges --since='`)
	b.WriteString(in.WindowFrom.Format(time.RFC3339))
	b.WriteString("' --until='")
	b.WriteString(in.WindowTo.Format(time.RFC3339))
	b.WriteString(`' origin/main
  (plus each story's PR head where its evidence URL points at an unmerged PR).
  The window bounds the review; the story list is the union to read.

THE REVIEW (engine-owned procedure — the role owns judgment, never the reverse):
1. Read the aggregate diff as ONE change against the outcome above — coherence
   across stories, integration seams between them, dead/orphaned code,
   built-but-never-wired features.
2. LENSES, not checklists: evidence verification (PRs merged, issues closed)
   and wiring (registered/mounted/reachable; flag states are decisions, not
   defaults) run INSIDE the holistic read.
3. Close ONLY via board_record_wrapup_verdict (claim token from your envelope):
   - pass  {verdict:"pass",  reportUrl:"<the wrap-up report>"}   — the epic completes.
   - gaps  {verdict:"gaps",  reportUrl:"<report>", gapRefs:["tsk-…",…]} — the epic
     stays open. File gap stories FIRST via board_create_story (tag "gap"; DEDUPE
     against existing rows — board_list the epic before creating; a gap is a real
     story with real acceptance criteria, never a note). Then record the verdict
     referencing them.
4. Check off this row's criteria with board_update_story {setCriteriaMet} as you
   verify them (#1946: your own claimed row); the verdict requires ALL met +
   reportUrl (the Done guard applies).
5. Recursion awareness: this is consecutive wrap-up `)
	fmt.Fprintf(&b, "%d of %d for this epic; at the cap a human ack is required before another mints.\n",
		in.WrapUpCount+1, in.ConsecutiveCap)
	b.WriteString(`
If a BOARD LEASE NUDGE arrives, call board_heartbeat ONCE immediately, then continue.
The claim token in your envelope authorizes in-flight writes for this task only.
`)
	return b.String()
}

// wrapUpCriteria is the engine-seeded acceptance criteria born on every
// wrap-up row (§5) — reviewer-checkable via board_update_story
// {setCriteriaMet}; the verdict's Done-guard parity rides them.
func wrapUpCriteria() []AcceptanceCriterion {
	return []AcceptanceCriterion{
		{Text: "The aggregate diff for the coverage window was read as a whole, against the epic's outcome, with the evidence and wiring lenses applied"},
		{Text: "A wrap-up report exists at a reachable URL and is referenced by reportUrl in the recorded verdict"},
		{Text: "The verdict was recorded via board_record_wrapup_verdict; on gaps, every gapRef resolves to a deduped gap-tagged story in this epic"},
	}
}

// epicCompletionCheckPlain is TODAY's completion statement, byte-identical
// (D9): flips an active epic to completed iff it holds at least one done
// task and no non-terminal tasks (RC4, §4.5). The disabled branch of the
// wrap-up seam runs this VERBATIM — the regression pin's subject.
func epicCompletionCheckPlain(ctx context.Context, q Queryer, epicID string) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE epics e SET status='completed'
		WHERE e.id = $1 AND e.status='active'
		  AND EXISTS(SELECT 1 FROM tasks t WHERE t.epic_id = e.id AND t.stage='done')
		  AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.epic_id = e.id
		                 AND t.stage <> 'done')`, epicID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// wrapUpAwareEpicCompletion is the single completion seam (§2). Runs
// inside the caller's transaction. Branch order (all anchors in the
// low-level design):
//
//  1. FOR UPDATE the epic row — the drain serialization point (D1):
//     concurrent drains serialize here; the second sees the in-flight
//     wrap-up and defers. Absent row → no-op (deleted epic).
//  2. status <> 'active' → no-op.
//  3. wrap_up_review = false → today's statement VERBATIM
//     (epicCompletionCheckPlain) and return — the seam's ONLY completion
//     path; an enabled epic never completes through the drain seam (the
//     PASS verdict's transaction owns its completion, §4).
//  4. drain check counting kind='story' rows ONLY (self-exclusion, D2).
//  5. in-flight (wrapup_task_id NOT NULL) → defer.
//  6. ack_pending → defer (the human holds the gate).
//  7. wrapup_count >= WrapUpConsecutiveCap → arm ack-wait (event, no mint).
//  8. else MINT (§3) and set epics.wrapup_task_id; the post-commit spawn
//     rides the caller's nudge (the stage.go entry-mint pattern).
//
// Returns (completed, mintedTaskID, err). n may be nil (the mint still
// lands; the spawn converges through the reconciler's re-claim pass).
func (s *Service) wrapUpAwareEpicCompletion(ctx context.Context, tx pgx.Tx, epicID string, n *nudge) (bool, string, error) {
	var projectID, shortID, projectName, title, outcome, status string
	var wrapUpReview, ackPending bool
	var wrapupCount int
	var createdAt time.Time
	var lastWrappedAt *time.Time
	var inFlight *string
	err := tx.QueryRow(ctx, `
		SELECT e.project_id::text, e.short_id, e.project_name, e.title, e.outcome, e.status, e.wrap_up_review,
		       e.created_at, e.last_wrapped_at, e.wrapup_count, e.wrapup_ack_pending, e.wrapup_task_id::text
		FROM epics e WHERE e.id = $1 FOR UPDATE OF e`, epicID).
		Scan(&projectID, &shortID, &projectName, &title, &outcome, &status, &wrapUpReview,
			&createdAt, &lastWrappedAt, &wrapupCount, &ackPending, &inFlight)
	if err == pgx.ErrNoRows {
		return false, "", nil // deleted epic: no-op
	}
	if err != nil {
		return false, "", err
	}
	if status != EpicActive {
		return false, "", nil
	}
	if !wrapUpReview {
		// D9: the disabled branch is byte-identical to today.
		flipped, err := epicCompletionCheckPlain(ctx, txQ{tx}, epicID)
		return flipped, "", err
	}

	// Enabled: the drain predicate counts story rows ONLY (D2 — a
	// wrap-up row never counts, no self-blocking paradox; a wrap-up epic
	// with zero stories never completes, today's EXISTS(done) rule
	// preserved).
	var hasDone, noneOpen bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM tasks t WHERE t.epic_id=$1 AND t.kind='story' AND t.stage='done'),
		       NOT EXISTS(SELECT 1 FROM tasks t WHERE t.epic_id=$1 AND t.kind='story' AND t.stage<>'done')`,
		epicID).Scan(&hasDone, &noneOpen); err != nil {
		return false, "", err
	}
	if !hasDone || !noneOpen {
		return false, "", nil // not drained
	}
	if inFlight != nil {
		return false, "", nil // in-flight wrap-up owns the epic: defer
	}
	if ackPending {
		return false, "", nil // the human holds the gate
	}
	if wrapupCount >= s.cfg.WrapUpConsecutiveCap {
		// Ack-wait arm (§2 step 7): loud event, no mint. The cap bounds
		// the mint loop — a pedantic gap-filer cannot loop forever. The
		// ack verb itself (reset + re-drain) is wrapup-gap-recursion's.
		tag, err := tx.Exec(ctx,
			`UPDATE epics SET wrapup_ack_pending=true WHERE id=$1 AND NOT wrapup_ack_pending`, epicID)
		if err != nil {
			return false, "", err
		}
		if tag.RowsAffected() > 0 {
			sys := Actor{Kind: ActorSystem, Surface: "engine"}
			if _, err := recordEventTx(ctx, tx, projectID,
				"wrapup:ackreq:"+epicID+":"+itoa(wrapupCount), "wrapup_ack_required",
				"epic", epicID, sys,
				map[string]any{"epic_id": epicID, "wrapup_count": wrapupCount,
					"cap": s.cfg.WrapUpConsecutiveCap}); err != nil {
				return false, "", err
			}
			slog.WarnContext(ctx, "board: epic wrap-up review at the consecutive cap — human ack required before another mints",
				"epic_id", epicID, "wrapup_count", wrapupCount, "cap", s.cfg.WrapUpConsecutiveCap)
		}
		return false, "", nil
	}

	// Drained, enabled, no in-flight row, no pending ack, under the cap:
	// MINT (§3).
	e := &Epic{ID: epicID, ProjectID: projectID, ShortID: shortID, ProjectName: projectName,
		Title: title, Outcome: outcome,
		CreatedAt: createdAt, WrapUpCount: wrapupCount, LastWrappedAt: lastWrappedAt}
	minted, err := s.mintWrapUpTx(ctx, tx, e, n)
	if err != nil {
		return false, "", err
	}
	return false, minted, nil
}

// mintWrapUpTx mints the epic's wrap-up review row inside the caller's
// transaction, mirroring CreateTask's write shape without the owner-facing
// guards (§3): phrase id from the shared task space, born stage='in_review'
// kind='wrapup' tags '{wrapup}', engine-seeded criteria, the charter
// description rendered from the epic state, and the reviewer persona claim
// minted in the SAME transaction. The mint deliberately does NOT check
// MaxNodesPerEpic — engine state, not author graph growth (the pinned
// non-goal; capping it could make a drained epic unreviewable).
//
// An unresolvable reviewer persona DEFERS: no row, a warn line +
// wrapup_mint_deferred event, and the drain re-fires on the next
// qualifying transition or sweep — fail-closed, never a role-less spawn
// (the applyPersonaRole degradation discipline).
func (s *Service) mintWrapUpTx(ctx context.Context, tx pgx.Tx, e *Epic, n *nudge) (string, error) {
	p := s.projectFor(ctx, e.ProjectID)
	if p == nil {
		return s.deferWrapUpMintTx(ctx, tx, e, "project-unresolved")
	}
	persona := s.wrapUpReviewerForProject(ctx, p)
	if persona == nil {
		return s.deferWrapUpMintTx(ctx, tx, e, "reviewer-unresolved")
	}

	// The §5 delta list: done story rows whose LATEST →done transition
	// falls after the watermark; a NULL watermark (the first run) covers
	// ALL done stories ever ('-infinity'). The RENDERED window's from-bound
	// stays coalesce(last_wrapped_at, epic.created_at) (D5) — the diff
	// range the reviewer reads; the coverage predicate is the watermark's
	// alone, a strictly weaker bound (no skip, no double-cover).
	windowFrom := e.CreatedAt
	if e.LastWrappedAt != nil {
		windowFrom = *e.LastWrappedAt
	}
	stories, err := wrapUpWindowStories(ctx, tx, e.ID, e.LastWrappedAt)
	if err != nil {
		return "", err
	}

	shortID, err := mintUnique(ctx, txQ{tx}, "tasks", PrefixTask, taskMintOwnerScope, p.UserID)
	if err != nil {
		return "", err
	}
	taskID := uuidNew()
	title := "Epic wrap-up review: " + e.Title
	if e.WrapUpCount > 0 {
		title += fmt.Sprintf(" (delta %d)", e.WrapUpCount+1)
	}
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `
		INSERT INTO tasks (id, project_id, epic_id, short_id, title, description,
		                   stage, kind, project_name, epic_name, tags, acceptance_criteria)
		VALUES ($1, $2, $3, $4, $5, '', 'in_review', 'wrapup', $6, $7, '{wrapup}', $8)
		RETURNING created_at`,
		taskID, p.ID, e.ID, shortID, title, p.Name, e.Title, criteriaJSON(wrapUpCriteria())).
		Scan(&createdAt); err != nil {
		return "", err
	}
	// The charter is rendered against the row's OWN mint moment (the
	// window's `to` bound) and frozen on the row in the same transaction.
	desc := RenderWrapUpCharter(WrapUpCharterInput{
		Epic:           e,
		WindowFrom:     windowFrom,
		WindowTo:       createdAt,
		Stories:        stories,
		WrapUpCount:    e.WrapUpCount,
		ConsecutiveCap: s.cfg.WrapUpConsecutiveCap,
	})
	if _, err := tx.Exec(ctx, `UPDATE tasks SET description=$2 WHERE id=$1`, taskID, desc); err != nil {
		return "", err
	}

	// The mint guard (§3 step 5): exactly one mint wins under any
	// concurrency — the guarded pointer UPDATE. Unreachable given the
	// seam's row lock (both drains serialize on the epic FOR UPDATE);
	// a lost race aborts the caller's transaction, rolling the duplicate
	// INSERT back (converged on the winner).
	tag, err := tx.Exec(ctx,
		`UPDATE epics SET wrapup_task_id=$2 WHERE id=$1 AND wrapup_task_id IS NULL`, e.ID, taskID)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", fmt.Errorf("board: wrap-up mint lost the pointer race on epic %s (converged on the winner)", e.ID)
	}

	// The reviewer persona claim, minted in the same transaction (the
	// stage.go entry-mint pattern; origin 'wrapup-mint' on the audit
	// event). attempts untouched — the personaClaimSQL discipline.
	token, err := s.personaClaimMintTx(ctx, tx, taskID, StageInReview, persona.Label())
	if err != nil {
		return "", err
	}
	claimEvt, err := personaClaimEventTx(ctx, tx, p.ID, taskID, StageInReview, persona, "wrapup-mint", 0)
	if err != nil {
		return "", err
	}
	sys := Actor{Kind: ActorSystem, Surface: "engine"}
	mintEvt, err := recordEventTx(ctx, tx, p.ID,
		"wrapup:mint:"+taskID, "wrapup_mint", "task", taskID, sys,
		map[string]any{"epic_id": e.ID, "task_id": taskID, "short_id": shortID,
			"window_from": windowFrom, "window_to": createdAt,
			"story_count": len(stories), "wrapup_count": e.WrapUpCount})
	if err != nil {
		return "", err
	}

	// The post-commit spawn rides the caller's nudge (r1-C5: a token
	// handed out for a mint that rolled back would strand a worker).
	if n != nil {
		n.taskID = taskID
		n.token = token
		n.spawn = true
		n.events = append(n.events, claimEvt, mintEvt)
	}
	slog.InfoContext(ctx, "board: epic wrap-up review minted",
		"epic_id", e.ID, "task", shortID, "stories", len(stories), "wrapup_count", e.WrapUpCount)
	return taskID, nil
}

// deferWrapUpMintTx records the deferred mint (warn + event, no row) —
// the drain re-fires on the next qualifying transition or sweep.
func (s *Service) deferWrapUpMintTx(ctx context.Context, tx pgx.Tx, e *Epic, reason string) (string, error) {
	slog.WarnContext(ctx, "board: epic wrap-up mint deferred — no row minted, the drain re-fires",
		"epic_id", e.ID, "reason", reason)
	sys := Actor{Kind: ActorSystem, Surface: "engine"}
	_, err := recordEventTx(ctx, tx, e.ProjectID,
		"wrapup:mint_deferred:"+e.ID+":"+strconv.FormatInt(time.Now().UnixNano(), 10),
		"wrapup_mint_deferred", "epic", e.ID, sys,
		map[string]any{"epic_id": e.ID, "reason": reason})
	return "", err
}

// advanceWatermarkTx is the wrap-up settle SQL (§4; D4): last_wrapped_at
// takes the closing wrap-up row's MINT time (created_at — captured at mint,
// NOT close time: stories completing while the review was in flight fall
// AFTER the snapshot and land in the NEXT delta — no double-cover, no
// skip), the consecutive-close ledger increments, and the in-flight pointer
// clears — one guarded statement inside the CALLER's transaction (the
// verdict's), so the advance is one-transaction-atomic with the caller's
// commit: a crash between the row's →done and this advance is impossible
// to observe (both-or-neither). The pointer guard makes a replayed or raced
// close a CLEAN NO-OP (0 rows): only the close that still owns the
// in-flight row settles it. RecordWrapUpVerdict (wrapup-gap-recursion) is
// the only production caller; the primitive's direct pins live in
// wrapup_settle_internal_test.go.
func advanceWatermarkTx(ctx context.Context, tx pgx.Tx, epicID, wrapupTaskID string) (bool, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE epics SET
			last_wrapped_at = (SELECT created_at FROM tasks WHERE id = $2),
			wrapup_count    = wrapup_count + 1,
			wrapup_task_id  = NULL
		WHERE id = $1 AND wrapup_task_id = $2`, epicID, wrapupTaskID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// wrapUpWindowStories lists the covered stories for a minted charter (§5):
// the epic's done kind='story' rows whose LATEST →done transition falls
// after the watermark — re-done rows count at their latest done.
// board_transitions is append-only evidence (`at` is the done time;
// tasks.updated_at is not trustworthy for completion ordering). A NULL
// watermark (the FIRST run) covers ALL done stories ever: the predicate's
// bound is '-infinity', NOT epic.created_at — a legacy row whose evidence
// predates the epic itself is still covered (owner ruling "first run: all").
// Evidence per story is the done transition's evidence_url, falling back to
// the latest review verdict's evidenceUrl (the re-done shape: approved with
// evidence, later re-done on a transition that recorded none) — "(none
// recorded)" renders only when neither exists.
func wrapUpWindowStories(ctx context.Context, q Queryer, epicID string, watermark *time.Time) ([]WrapUpStoryRef, error) {
	rows, err := q.Query(ctx, `
		SELECT t.short_id, t.title, tr.at,
		       COALESCE(NULLIF(tr.evidence_url, ''),
		                (SELECT ev.payload->>'evidenceUrl'
		                 FROM board_events ev
		                 WHERE ev.project_id = t.project_id
		                   AND ev.item_kind = 'task' AND ev.item_id = t.id
		                   AND ev.action = 'review_verdict'
		                 ORDER BY ev.created_at DESC, ev.id DESC LIMIT 1),
		                '')
		FROM tasks t
		JOIN LATERAL (
			SELECT at, evidence_url FROM board_transitions
			WHERE task_id = t.id AND to_stage = 'done'
			ORDER BY at DESC, id DESC LIMIT 1
		) tr ON true
		WHERE t.epic_id = $1 AND t.kind = 'story' AND t.stage = 'done'
		  AND tr.at > COALESCE($2::timestamptz, '-infinity'::timestamptz)
		ORDER BY t.created_at, t.id`, epicID, watermark)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WrapUpStoryRef
	for rows.Next() {
		var r WrapUpStoryRef
		if err := rows.Scan(&r.ShortID, &r.Title, &r.DoneAt, &r.Evidence); err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// clearWrapUpPointerTx is the service-level belt behind the FK floor: a
// wrap-up row leaving the board (delete) or leaving the cycle (the
// manager backlog exit) clears the epic's in-flight pointer in the same
// transaction, audibly — the next drain re-mints fresh. Returns the
// cleared epic id, or "" when the row was not any epic's in-flight
// pointer (nothing to clear). Run BEFORE the row delete in delete paths
// (the FK's ON DELETE SET NULL then has nothing left to do).
func clearWrapUpPointerTx(ctx context.Context, tx pgx.Tx, projectID, taskID string, actor Actor, eventID, eventAction string) (string, error) {
	var epicID string
	err := tx.QueryRow(ctx, `
		UPDATE epics SET wrapup_task_id=NULL
		WHERE wrapup_task_id=$1
		RETURNING id`, taskID).Scan(&epicID)
	if err == pgx.ErrNoRows {
		return "", nil // not the in-flight pointer: nothing to clear
	}
	if err != nil {
		return "", err
	}
	if _, err := recordEventTx(ctx, tx, projectID, eventID, eventAction,
		"epic", epicID, actor, map[string]any{"epic_id": epicID, "task_id": taskID}); err != nil {
		return "", err
	}
	return epicID, nil
}
