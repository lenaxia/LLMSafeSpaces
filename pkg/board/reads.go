// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// reads.go — the board_read projection (§4.2): a read model over the
// materialized columns. Epic progress is DERIVED here, never stored
// (ruling R2, #1747-amended): Done over ALL tasks; a zero denominator
// renders no-progress, never 0/0.

// GetTask resolves a task (case-insensitive short ID or UUID) and
// attaches the derived informational fields (waiting_on, backpressure).
// The assignee session id is translated to the #1756 phrase at this
// read edge (#1922) — raw + assignee_session_deprecated when cold.
func (s *Service) GetTask(ctx context.Context, taskID, shortIDOrID string) (*Task, error) {
	t, err := s.store.resolveTask(ctx, s.q(), "", pick(taskID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	s.decorateTask(ctx, t, nil)
	s.translateTaskReads(ctx, t)
	// RC-C S2: badge honesty rides the same read edge — busy is
	// re-derived per read under the lease TTL (busy.go).
	s.decorateAssigneeBusy(ctx, t)
	// #2047: …and the quiet/busy-seconds surface (quiet.go) — the
	// GetTask wire's net-new fields, same re-derived-per-read posture.
	s.decorateAssigneeQuiet(ctx, t)
	return t, nil
}

// ListTasks lists a project's tasks (optionally by stage and by the
// #1813 failed-state overlay) in creation order — the FIFO claim
// order's read-side mirror (#1747). failed is tri-state: "" (no
// filter), "true" (only failed-attempt rows), "false" (only clean).
func (s *Service) ListTasks(ctx context.Context, projectID string, stage, failed string) ([]Task, error) {
	q := `SELECT ` + taskColumns + ` FROM tasks t WHERE t.project_id=$1`
	args := []any{projectID}
	if stage != "" {
		// 000050: pending_input is a flag — the stage filter literal
		// translates to the flag predicate (rows keep their real stage).
		if stage == StagePendingInput {
			q += ` AND t.pending_input`
		} else {
			q += ` AND t.stage=$` + itoa(len(args)+1)
			args = append(args, stage)
		}
	}
	if failed == "true" || failed == "false" {
		q += ` AND t.last_attempt_failed=$` + itoa(len(args)+1)
		args = append(args, failed == "true")
	}
	q += ` ORDER BY t.created_at, t.id`
	rows, err := s.q().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// r2 N+1 fix: the project (and its cap census context) resolves ONCE
	// per list call — every row shares the same waiting_on/backpressure
	// decoration inputs.
	var proj *Project
	var out []Task
	var scanned []*Task
	for rows.Next() {
		t, err := scanTask(rowScan{rows})
		if err != nil {
			return nil, err
		}
		scanned = append(scanned, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// #1922: one bounded translation pass for the whole batch (the
	// public read edge — canonical rows above stay untouched).
	s.translateTaskReads(ctx, scanned...)
	// RC-C S2: one batched busy question for the whole list (the
	// board's primary feed — the badge poll).
	s.decorateAssigneeBusy(ctx, scanned...)
	// #2047: …and one batched quiet question (the feed's quiet/busy
	// seconds ride the same single pass).
	s.decorateAssigneeQuiet(ctx, scanned...)
	for _, t := range scanned {
		if proj == nil || proj.ID != t.ProjectID {
			p, perr := s.store.resolveProject(ctx, s.q(), "", t.ProjectID)
			if perr != nil {
				slog.WarnContext(ctx, "board: project resolve failed (decoration suppressed)",
					"projectID", t.ProjectID, "error", perr.Error())
			}
			proj = p
		}
		s.decorateTask(ctx, t, proj)
		out = append(out, *t)
	}
	return out, nil
}

// GetEpic resolves an epic with derived progress + the backpressure note.
func (s *Service) GetEpic(ctx context.Context, epicID, shortIDOrID string) (*Epic, error) {
	e, err := s.store.resolveEpic(ctx, s.q(), "", pick(epicID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	if err := s.deriveProgress(ctx, e); err != nil {
		return nil, err
	}
	s.decorateEpic(ctx, e)
	// The in-flight wrap-up pointer renders as the live row's short id
	// (§1b — join at read time, never stored denormalized).
	s.decorateWrapUpTaskIDs(ctx, e)
	return e, nil
}

// deriveProgress carries the SAME denominator as ListEpics (the #1747
// owner ruling: Done over ALL live tasks — deleted rows leave the
// board entirely; the one-epic and list reads must agree).
func (s *Service) deriveProgress(ctx context.Context, e *Epic) error {
	err := s.q().QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE t.stage = 'done'),
			count(*)
		FROM tasks t WHERE t.epic_id = $1`, e.ID).Scan(&e.ProgressDone, &e.ProgressTotal)
	if err != nil {
		return err
	}
	e.ProgressDerived = e.ProgressTotal > 0
	return nil
}

// GetProject resolves a project (+ the backpressure note).
func (s *Service) GetProject(ctx context.Context, projectID, shortIDOrID string) (*Project, error) {
	p, err := s.store.resolveProject(ctx, s.q(), "", pick(projectID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	s.decorateProject(p)
	return p, nil
}

// TaskEvents-window knobs: the audit timeline is a display read over
// board_events, so it is bounded (response-channel audits write a row per call — a
// long-lived claim would otherwise render unbounded). The window keeps
// the NEWEST rows; order is always chronological (oldest→newest).
const (
	TaskEventsDefaultLimit = 200
	TaskEventsMaxLimit     = 500
)

// TaskEvents returns the task's FULL audit timeline as persisted in
// board_events (#1832 item 4): stage transitions, claims (who/when via
// the payload), review verdicts when the payload carries one,
// dependency-graph changes, and orphan/unblock events — every row the
// write path commits with item_kind='task', item_id=<uuid>, including
// the reconciler's deterministic-id system rows (reclaim, drift, …).
// The SSE projection only carries post-subscribe commits (§4.6); this
// read is the history seam the task-detail drawer renders. Ownership
// follows the caller's read perimeter (the adapter resolves + checks
// the row, exactly like GetTask).
//
// The GONE-ROW FALLBACK (the 2026-11 row-loss class, issue #2107):
// delete removes the object, not the history — the delete_task event
// embeds the row's full snapshot — yet the timeline 404'd the moment
// the row left the table, sealing the audit trail exactly when an
// investigation needs it most (the incident's fifth loss was the
// investigation story itself). When the row no longer resolves,
// TaskEvents anchors the trail through board_events instead: the
// item_id directly for UUID input, else any event payload carrying
// the short id (create_task, delete_task, dependency events all do).
// No anchor either ⇒ nothing ever existed OR the row vanished below
// the store with no event trail — the caller's 404 (with its
// did-you-mean contract) still distinguishes that fork, honestly.
func (s *Service) TaskEvents(ctx context.Context, taskID, shortIDOrID string, limit int) ([]Event, error) {
	t, err := s.store.resolveTask(ctx, s.q(), "", pick(taskID, shortIDOrID))
	if err != nil {
		var nf *NotFoundError
		if errors.As(err, &nf) {
			if events, _, found, ferr := s.taskEventsTombstone(ctx, pick(taskID, shortIDOrID), limit); ferr != nil {
				return nil, ferr
			} else if found {
				return events, nil
			}
		}
		return nil, err
	}
	if limit <= 0 {
		limit = TaskEventsDefaultLimit
	}
	if limit > TaskEventsMaxLimit {
		limit = TaskEventsMaxLimit
	}
	rows, err := s.q().Query(ctx, `
		SELECT id, item_kind, item_id, action, actor, payload, created_at
		FROM board_events
		WHERE project_id=$1 AND item_kind='task' AND item_id=$2
		ORDER BY created_at DESC, id DESC
		LIMIT $3`, t.ProjectID, t.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var newestFirst []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.ItemKind, &e.ItemID, &e.Action, &e.Actor, &e.Payload, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.ProjectID = t.ProjectID
		newestFirst = append(newestFirst, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reverse to chronological order — the timeline's render order.
	out := make([]Event, len(newestFirst))
	for i, e := range newestFirst {
		out[len(newestFirst)-1-i] = e
	}
	// #1922: board_events.actor is append-only — the session id is
	// translated at READ time only (phrase when known; raw + the #1821
	// session_id_deprecated flag when cold). The stored rows stay
	// byte-identical. The delete/cascade payloads embed the task row
	// snapshot verbatim (2026-10-29 ruling: implementer_session rides
	// it now) — the SAME read-time pass translates both snapshot
	// session fields, never-worse, off the one consult.
	s.translateEventReads(ctx, t.ProjectID, out)
	return out, nil
}

// TaskEventsTombstone is the adapter-facing arm of the gone-row
// fallback: the audit timeline for a task whose row no longer exists,
// plus the project the trail itself names — the ownership perimeter's
// anchor when there is no row to derive it from. Returns the original
// NotFoundError when no trail resolves (never-existed, or a below-store
// loss that left no events — the honest distinction the investigation
// fork needs, issue #2107).
func (s *Service) TaskEventsTombstone(ctx context.Context, idOrShort string, limit int) ([]Event, *Project, error) {
	nf := &NotFoundError{Item: "task", Identifier: idOrShort}
	events, p, found, err := s.taskEventsTombstone(ctx, idOrShort, limit)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		return nil, nil, nf
	}
	return events, p, nil
}

// taskEventsTombstone is the shared primitive: anchor the (project,
// item) pair from board_events without the tasks row, then serve the
// same timeline query the live path runs. found=false keeps the
// caller's 404 (the row AND its trail are both gone).
func (s *Service) taskEventsTombstone(ctx context.Context, idOrShort string, limit int) ([]Event, *Project, bool, error) {
	if idOrShort == "" {
		return nil, nil, false, nil
	}
	if limit <= 0 {
		limit = TaskEventsDefaultLimit
	}
	if limit > TaskEventsMaxLimit {
		limit = TaskEventsMaxLimit
	}
	// The anchor, split per arm so each stays sargable on its partial
	// index (migration 000061 — an OR of the two would sequential-scan
	// the append-only audit table on every probe, the r1 review's
	// blocking finding): UUID input matches item_id directly; otherwise
	// any task-scoped event payload carrying the short id (create_task,
	// delete_task, dependency events — a row that ever existed has at
	// least its create event). Case-insensitive on the phrase form,
	// matching the row-resolution contract.
	type anchor struct{ project, item string }
	var anchors []anchor
	var rows pgx.Rows
	var err error
	if isUUID(idOrShort) {
		rows, err = s.q().Query(ctx, `
			SELECT DISTINCT e.project_id::text, e.item_id::text
			FROM board_events e
			WHERE e.item_kind='task' AND e.item_id IS NOT NULL
			  AND e.item_id=$1::uuid`, idOrShort)
	} else {
		rows, err = s.q().Query(ctx, `
			SELECT DISTINCT e.project_id::text, e.item_id::text
			FROM board_events e
			WHERE e.item_kind='task' AND e.item_id IS NOT NULL
			  AND e.payload->'short_id' IS NOT NULL
			  AND lower(e.payload->>'short_id')=lower($1)`, idOrShort)
	}
	if err != nil {
		return nil, nil, false, err
	}
	var scanned []anchor
	for rows.Next() {
		var a anchor
		if err := rows.Scan(&a.project, &a.item); err != nil {
			rows.Close()
			return nil, nil, false, err
		}
		scanned = append(scanned, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, false, err
	}
	rows.Close()
	anchors = scanned
	if len(anchors) != 1 {
		if len(anchors) > 1 {
			// Short ids are minted user-scoped-unique; multiple anchors
			// mean a below-store anomaly. Refuse loudly, keep the 404 —
			// never silently merge trails across projects.
			slog.WarnContext(ctx, "board: task-events tombstone anchor ambiguous — refusing",
				"id", idOrShort, "anchors", len(anchors))
		}
		return nil, nil, false, nil
	}
	p, err := s.store.resolveProject(ctx, s.q(), "", anchors[0].project)
	if err != nil {
		return nil, nil, false, err
	}
	evRows, err := s.q().Query(ctx, `
		SELECT id, item_kind, item_id, action, actor, payload, created_at
		FROM board_events
		WHERE project_id=$1 AND item_kind='task' AND item_id=$2
		ORDER BY created_at DESC, id DESC
		LIMIT $3`, anchors[0].project, anchors[0].item, limit)
	if err != nil {
		return nil, nil, false, err
	}
	defer evRows.Close()
	var newestFirst []Event
	for evRows.Next() {
		var e Event
		if err := evRows.Scan(&e.ID, &e.ItemKind, &e.ItemID, &e.Action, &e.Actor, &e.Payload, &e.CreatedAt); err != nil {
			return nil, nil, false, err
		}
		e.ProjectID = anchors[0].project
		newestFirst = append(newestFirst, e)
	}
	if err := evRows.Err(); err != nil {
		return nil, nil, false, err
	}
	out := make([]Event, len(newestFirst))
	for i, e := range newestFirst {
		out[len(newestFirst)-1-i] = e
	}
	s.translateEventReads(ctx, anchors[0].project, out)
	return out, p, true, nil
}

// ListProjects lists the owner's boards (user-level, ruling R7).
func (s *Service) ListProjects(ctx context.Context, userID string) ([]Project, error) {
	rows, err := s.q().Query(ctx, `SELECT `+projectColumns+` FROM projects WHERE user_id=$1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rowScan{rows})
		if err != nil {
			return nil, err
		}
		s.decorateProject(p)
		out = append(out, *p)
	}
	return out, rows.Err()
}

// ListEpics lists a project's epics with derived progress (never
// stored; a zero denominator renders no-progress). One query: the
// per-epic counts ride a LEFT JOIN over the tasks aggregate (no N+1).
// Denominator semantics per the #1747 owner ruling: Done over ALL live
// tasks — deleted rows leave the board entirely.
func (s *Service) ListEpics(ctx context.Context, projectID string) ([]Epic, error) {
	rows, err := s.q().Query(ctx, `
		SELECT e.id, e.project_id, e.short_id, e.title, e.outcome, e.status, e.project_name,
		       e.tags, e.vft, e.deficit, e.quantum,
		       e.wrap_up_review, e.last_wrapped_at, e.wrapup_count, e.wrapup_task_id::text, e.wrapup_ack_pending,
		       e.created_at, e.updated_at,
		       COALESCE(c.done_count, 0), COALESCE(c.total_count, 0)
		FROM epics e
		LEFT JOIN (
			SELECT epic_id,
			       count(*) FILTER (WHERE stage = 'done') AS done_count,
			       count(*) AS total_count
			FROM tasks GROUP BY epic_id
		) c ON c.epic_id = e.id
		WHERE e.project_id = $1
		ORDER BY e.created_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Epic
	for rows.Next() {
		var e Epic
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.ShortID, &e.Title, &e.Outcome, &e.Status,
			&e.ProjectName, &e.Tags, &e.VFT, &e.Deficit, &e.Quantum,
			&e.WrapUpReview, &e.LastWrappedAt, &e.WrapUpCount, &e.WrapUpTaskID, &e.WrapUpAckPending,
			&e.CreatedAt, &e.UpdatedAt,
			&e.ProgressDone, &e.ProgressTotal); err != nil {
			return nil, err
		}
		e.ProgressDerived = e.ProgressTotal > 0
		e.ShortIDDeprecated = IsDeprecatedBoardID(e.ShortID)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// One batched decorate pass for the whole list (the cross-epic
	// dedupe rides the single call — one query per list, not per epic).
	if len(out) > 0 {
		ptrs := make([]*Epic, len(out))
		for i := range out {
			ptrs[i] = &out[i]
		}
		s.decorateWrapUpTaskIDs(ctx, ptrs...)
	}
	return out, nil
}

// decorateWrapUpTaskIDs renders each epic's in-flight wrap-up pointer as
// the LIVE wrap-up row's short id (§1b: join at read time, never stored
// denormalized — the stored column keeps the UUID identity). One bounded
// query; a dangling pointer (raced delete between read and join) renders
// the raw value. The §8 read surface's config-derived half rides along:
// wrap-up-enabled epics carry the engine's consecutive-close cap.
func (s *Service) decorateWrapUpTaskIDs(ctx context.Context, epics ...*Epic) {
	byTask := map[string]*Epic{}
	ids := make([]string, 0, len(epics))
	for _, e := range epics {
		if e.WrapUpReview {
			e.WrapUpCap = s.cfg.WrapUpConsecutiveCap
		}
		if p := e.WrapUpTaskID; p != nil && *p != "" {
			if _, dup := byTask[*p]; !dup {
				ids = append(ids, *p)
			}
			byTask[*p] = e
		}
	}
	if len(ids) == 0 {
		return
	}
	rows, err := s.q().Query(ctx, `
		SELECT id::text, short_id FROM tasks WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return // decoration is best-effort; the raw pointer still renders
	}
	defer rows.Close()
	for rows.Next() {
		var id, short string
		if err := rows.Scan(&id, &short); err == nil {
			if e, ok := byTask[id]; ok {
				e.WrapUpTaskID = &short
			}
		}
	}
}

// searchLimit bounds result and suggestion sets (a personal board's
// search is small by construction; the caps keep the worst case flat).
const (
	searchResultLimit    = 50
	searchCandidateLimit = 1000
	searchQueryMaxLength = 200
)

// searchStages is the stage-filter vocabulary (ruling R2's five-stage
// set, #1747-amended — the eliminated terminal stage is not searchable).
func searchStages() map[string]bool {
	return map[string]bool{
		StageBacklog: true, StageInProgress: true, StageInReview: true,
		StageDone: true, StagePendingInput: true,
	}
}

// SearchTasks searches the owner's board: short-ID (case-insensitive,
// exact) or title substring, scoped by the caller's authorization
// perimeter — foreign rows never match and never suggest. Zero-hit
// near-misses carry 0066-shape did-you-mean suggestions (top-3,
// bounded edit distance, suggest-only). Filters resolve INSIDE the
// user-scoped query (a foreign project/epic filter renders empty, no
// disclosure). Result order is creation order — the FIFO read-side
// mirror of the #1747 claim-order ruling (priority fields are gone in
// its schema).
func (s *Service) SearchTasks(ctx context.Context, userID, q string, filters SearchFilters) (*SearchResult, error) {
	trimmed := strings.TrimSpace(q)
	if trimmed == "" {
		return nil, &GuardError{Code: GuardInput, Detail: "search query is required"}
	}
	if len(trimmed) > searchQueryMaxLength {
		return nil, &GuardError{Code: GuardInput, Detail: "search query exceeds 200 characters"}
	}
	if filters.Stage != "" && !searchStages()[filters.Stage] {
		return nil, &GuardError{Code: GuardInput, Detail: "stage filter must be one of the five stages"}
	}

	args := []any{userID, trimmed, likeLiteral(trimmed)}
	q2 := `SELECT ` + taskColumns + ` FROM tasks t
		JOIN projects p ON p.id = t.project_id
		WHERE p.user_id = $1
		  AND (lower(t.short_id) = lower($2) OR t.title ILIKE $3 ESCAPE '\')`
	if filters.Stage != "" {
		if filters.Stage == StagePendingInput {
			q2 += ` AND t.pending_input` // 000050: flag filter alias
		} else {
			args = append(args, filters.Stage)
			q2 += fmt.Sprintf(" AND t.stage = $%d", len(args))
		}
	}
	if filters.Project != "" {
		args = append(args, filters.Project)
		q2 += fmt.Sprintf(` AND t.project_id IN (
			SELECT p2.id FROM projects p2 WHERE p2.user_id = $1 AND (lower(p2.short_id) = lower($%d) OR p2.id::text = $%d))`,
			len(args), len(args))
	}
	if filters.Epic != "" {
		args = append(args, filters.Epic)
		q2 += fmt.Sprintf(` AND t.epic_id IN (
			SELECT e2.id FROM epics e2 JOIN projects p2 ON p2.id = e2.project_id
			WHERE p2.user_id = $1 AND (lower(e2.short_id) = lower($%d) OR e2.id::text = $%d))`,
			len(args), len(args))
	}
	if len(filters.Tags) > 0 {
		args = append(args, filters.Tags)
		q2 += fmt.Sprintf(" AND t.tags @> $%d", len(args))
	}
	q2 += ` ORDER BY t.created_at, t.id LIMIT ` + strconv.Itoa(searchResultLimit)

	rows, err := s.q().Query(ctx, q2, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := &SearchResult{Results: []Task{}, Suggestions: []string{}}
	var scanned []*Task
	for rows.Next() {
		t, err := scanTask(rowScan{rows})
		if err != nil {
			return nil, err
		}
		scanned = append(scanned, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// #1922: the search projection rides the same read-edge translation.
	s.translateTaskReads(ctx, scanned...)
	// RC-C S2: …and the same read-edge busy pass (a search hit's badge
	// tells the same truth a list row's does).
	s.decorateAssigneeBusy(ctx, scanned...)
	// #2047: …and the same quiet pass (a search hit's quiet/busy
	// seconds tell the same truth a list row's do).
	s.decorateAssigneeQuiet(ctx, scanned...)
	for _, t := range scanned {
		res.Results = append(res.Results, *t)
	}
	if len(res.Results) > 0 {
		return res, nil
	}

	// Zero hits: suggest near-misses over the caller's own short IDs and
	// titles (top-3, 0066 shape). Far misses render no suggestions.
	sres, err := s.q().Query(ctx, `
		SELECT t.short_id, t.title FROM tasks t
		JOIN projects p ON p.id = t.project_id
		WHERE p.user_id = $1
		ORDER BY t.created_at DESC LIMIT `+strconv.Itoa(searchCandidateLimit), userID)
	if err != nil {
		return nil, err
	}
	defer sres.Close()
	var candidates []string
	for sres.Next() {
		var shortID, title string
		if err := sres.Scan(&shortID, &title); err != nil {
			return nil, err
		}
		candidates = append(candidates, shortID, title)
	}
	if err := sres.Err(); err != nil {
		return nil, err
	}
	res.Suggestions = suggestTop3(trimmed, candidates)
	return res, nil
}

// likeLiteral escapes ILIKE wildcards so a user query matches a literal
// substring (predictable search, no wildcard injection semantics).
func likeLiteral(s string) string {
	repl := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + repl.Replace(s) + "%"
}

// BoardHealth renders the owner's board health snapshot (§4.2
// board_read(health) / §4.8's health panel).
func (s *Service) BoardHealth(ctx context.Context, userID string) (*Health, error) {
	h := &Health{
		ActiveLeases:         []HealthLease{},
		TaskCooldowns:        []HealthTaskCooldown{},
		ProjectCooldowns:     []HealthProjectCooldown{},
		PendingInputByOrigin: map[string]int{},
		RecentReclaims:       []HealthReclaim{},
		ProjectSlots:         []HealthProjectSlots{},
		AckWaits:             []HealthAckWait{},
		Waiting:              []HealthWait{},
	}
	if err := s.healthActiveLeases(ctx, userID, h); err != nil {
		return nil, err
	}
	if err := s.healthCooldowns(ctx, userID, h); err != nil {
		return nil, err
	}
	if err := s.healthPendingInput(ctx, userID, h); err != nil {
		return nil, err
	}
	if err := s.healthRecentReclaims(ctx, userID, h); err != nil {
		return nil, err
	}
	if err := s.healthAckWaits(ctx, &userID, nil, h); err != nil {
		return nil, err
	}
	if err := s.healthWaiting(ctx, &userID, nil, h); err != nil {
		return nil, err
	}
	return h, s.healthProjectSlots(ctx, userID, h)
}

// healthWaiting renders the typed-wait section (claim-wait lifecycle
// §2.3): every waiting claimed row — user-waits (flagged; wait_on
// renders 'user' by the COALESCE read for pre-verb rows) AND
// event-waits — with {short_id, wait_on, wait_reason, since}. since is
// the declaration event's timestamp: the verb's wait_declared event,
// or the flag-op's pending_input_flag event for alias-entered
// user-waits (the latest flag=true one; zero when history has aged out
// of board_events — the facts stay, the timestamp degrades honestly).
// A nil bound widens the perimeter (the agent surface passes only its
// workspace, the healthAckWaits pattern).
func (s *Service) healthWaiting(ctx context.Context, userID, workspaceID *string, h *Health) error {
	rows, err := s.q().Query(ctx, `
		SELECT t.id, t.short_id, t.title, t.project_id,
		       COALESCE(t.wait_on, CASE WHEN t.pending_input THEN 'user' END),
		       COALESCE(t.wait_reason, ''),
		       (SELECT be.created_at FROM board_events be
		         WHERE be.project_id = t.project_id AND be.item_kind = 'task'
		           AND be.item_id = t.id
		           AND (be.action = 'wait_declared'
		                OR (be.action = 'pending_input_flag' AND (be.payload->>'flag')::boolean))
		         ORDER BY be.created_at DESC LIMIT 1)
		FROM tasks t JOIN projects p ON p.id = t.project_id
		WHERE ($1::text IS NULL OR p.user_id = $1)
		  AND ($2::uuid IS NULL OR p.target_workspace_id = $2)
		  AND (t.wait_on IS NOT NULL OR t.pending_input)
		ORDER BY t.created_at, t.id`, userID, workspaceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var w HealthWait
		var since *time.Time
		if err := rows.Scan(&w.TaskID, &w.ShortID, &w.Title, &w.ProjectID, &w.WaitOn, &w.WaitReason, &since); err != nil {
			return err
		}
		if since != nil {
			w.Since = *since
		}
		h.Waiting = append(h.Waiting, w)
	}
	return rows.Err()
}

// healthAckWaits renders the wrap-up circuit-breaker's surface (§8): the
// owner's epics parked in ack-wait (wrapup_ack_pending — the human holds
// the gate) or carrying a PARKED in-flight pointer (the pointed row is
// NOT in_review: a manager-override close, §4's fail-visible tail — a
// healthy in-flight review never rides this list). A nil bound widens
// the perimeter (the agent surface passes no user, only its workspace).
func (s *Service) healthAckWaits(ctx context.Context, userID, workspaceID *string, h *Health) error {
	rows, err := s.q().Query(ctx, `
		SELECT e.id, e.short_id, e.project_id, e.title, e.wrapup_count, e.wrapup_ack_pending,
		       COALESCE(t.short_id, '')
		FROM epics e
		JOIN projects p ON p.id = e.project_id
		LEFT JOIN tasks t ON t.id = e.wrapup_task_id AND t.stage <> 'in_review'
		WHERE ($1::text IS NULL OR p.user_id = $1)
		  AND ($2::uuid IS NULL OR p.target_workspace_id = $2::uuid)
		  AND (e.wrapup_ack_pending OR t.id IS NOT NULL)
		ORDER BY e.created_at`, userID, workspaceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var aw HealthAckWait
		if err := rows.Scan(&aw.EpicID, &aw.EpicShortID, &aw.ProjectID, &aw.EpicTitle,
			&aw.WrapUpCount, &aw.AckPending, &aw.InFlightShortID); err != nil {
			return err
		}
		aw.Cap = s.cfg.WrapUpConsecutiveCap
		h.AckWaits = append(h.AckWaits, aw)
	}
	return rows.Err()
}

func (s *Service) healthActiveLeases(ctx context.Context, userID string, h *Health) error {
	rows, err := s.q().Query(ctx, `
		SELECT t.id, t.short_id, t.title, t.project_id, coalesce(t.assignee_session, ''), t.lease_expires_at,
		       coalesce(p.target_workspace_id::text, '')
		FROM tasks t JOIN projects p ON p.id = t.project_id
		WHERE p.user_id = $1 AND t.stage = 'in_progress' AND t.lease_expires_at IS NOT NULL
		ORDER BY t.lease_expires_at`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	wsByProject := map[string]string{}
	for rows.Next() {
		var l HealthLease
		var ws string
		if err := rows.Scan(&l.TaskID, &l.ShortID, &l.Title, &l.ProjectID, &l.AssigneeSession, &l.LeaseExpiresAt, &ws); err != nil {
			return err
		}
		wsByProject[l.ProjectID] = ws
		h.ActiveLeases = append(h.ActiveLeases, l)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// #1922: phrase-translate the lease assignees at the read edge
	// (one bounded consult per DISTINCT target workspace).
	s.translateHealthLeases(ctx, h.ActiveLeases, func(projectID string) string { return wsByProject[projectID] })
	return nil
}

func (s *Service) healthCooldowns(ctx context.Context, userID string, h *Health) error {
	rows, err := s.q().Query(ctx, `
		SELECT t.id, t.short_id, t.project_id, t.cooldown_until
		FROM tasks t JOIN projects p ON p.id = t.project_id
		WHERE p.user_id = $1 AND t.cooldown_until > now()
		ORDER BY t.cooldown_until`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c HealthTaskCooldown
		if err := rows.Scan(&c.TaskID, &c.ShortID, &c.ProjectID, &c.CooldownUntil); err != nil {
			return err
		}
		h.TaskCooldowns = append(h.TaskCooldowns, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	prows, err := s.q().Query(ctx, `
		SELECT id, short_id, slot_cooldown_until FROM projects
		WHERE user_id = $1 AND slot_cooldown_until > now()
		ORDER BY slot_cooldown_until`, userID)
	if err != nil {
		return err
	}
	defer prows.Close()
	for prows.Next() {
		var c HealthProjectCooldown
		if err := prows.Scan(&c.ProjectID, &c.ShortID, &c.CooldownUntil); err != nil {
			return err
		}
		h.ProjectCooldowns = append(h.ProjectCooldowns, c)
	}
	return prows.Err()
}

func (s *Service) healthPendingInput(ctx context.Context, userID string, h *Health) error {
	// 000050: the panel's question is "what awaits the owner" — the
	// flagged parks AND the failed/escalated terminus (rearm's queue)
	// both answer it; the stage swap must not hide the escalation
	// backlog the panel has always shown.
	rows, err := s.q().Query(ctx, `
		SELECT coalesce(t.pending_origin, 'unknown'), count(*)
		FROM tasks t JOIN projects p ON p.id = t.project_id
		WHERE p.user_id = $1
		  AND (t.pending_input OR (t.stage='failed' AND t.pending_origin='escalated'))
		GROUP BY 1`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var origin string
		var n int
		if err := rows.Scan(&origin, &n); err != nil {
			return err
		}
		h.PendingInputByOrigin[origin] = n
	}
	return rows.Err()
}

func (s *Service) healthRecentReclaims(ctx context.Context, userID string, h *Health) error {
	rows, err := s.q().Query(ctx, `
		SELECT be.item_id, be.created_at
		FROM board_events be JOIN projects p ON p.id = be.project_id
		WHERE p.user_id = $1 AND be.action = 'reclaim' AND be.item_id IS NOT NULL
		ORDER BY be.created_at DESC LIMIT 20`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r HealthReclaim
		if err := rows.Scan(&r.TaskID, &r.At); err != nil {
			return err
		}
		h.RecentReclaims = append(h.RecentReclaims, r)
	}
	return rows.Err()
}

func (s *Service) healthProjectSlots(ctx context.Context, userID string, h *Health) error {
	rows, err := s.q().Query(ctx, `
		SELECT p.id, p.short_id, p.worker_slots,
		       (SELECT count(*) FROM tasks t WHERE t.project_id = p.id AND t.stage = 'in_progress')
		FROM projects p WHERE p.user_id = $1 AND p.status = 'active'
		ORDER BY p.created_at`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ps HealthProjectSlots
		if err := rows.Scan(&ps.ProjectID, &ps.ShortID, &ps.WorkerSlots, &ps.InUse); err != nil {
			return err
		}
		h.ProjectSlots = append(h.ProjectSlots, ps)
	}
	return rows.Err()
}

// rowScan adapts pgx.Rows to the pgx.Row interface (single-row scans over
// iteration cursors).
type rowScan struct{ rows pgx.Rows }

func (r rowScan) Scan(dest ...any) error { return r.rows.Scan(dest...) }

// --- Workspace-scoped agent reads (#1837 P2a) -----------------------------------
//
// The agent surface's list/health projections: the caller's resolved
// WORKSPACE is the perimeter (a pod sees its own workspace's boards
// only — narrower than the owner perimeter the REST/external surfaces
// use, per the #1747 caller-workspace binding).

// ListTasksInWorkspace lists a project's tasks for the agent surface:
// the project resolves WORKSPACE-scoped (a foreign workspace's project
// is a plain 404), then the projection is ListTasks' (stage/claim/
// lease/paused/blocked columns, FIFO creation order).
func (s *Service) ListTasksInWorkspace(ctx context.Context, workspaceID, projectID, shortIDOrID, stage, failed string) ([]Task, *Project, error) {
	p, err := s.resolveProjectInWorkspace(ctx, workspaceID, pick(projectID, shortIDOrID))
	if err != nil {
		return nil, nil, err
	}
	tasks, err := s.ListTasks(ctx, p.ID, stage, failed)
	if err != nil {
		return nil, nil, err
	}
	return tasks, p, nil
}

// resolveProjectInWorkspace resolves a project by UUID or
// case-insensitive short id within one target workspace.
func (s *Service) resolveProjectInWorkspace(ctx context.Context, workspaceID, idOrShort string) (*Project, error) {
	var row pgx.Row
	base := `SELECT ` + projectColumns + ` FROM projects WHERE target_workspace_id = $1::uuid AND `
	if isUUID(idOrShort) {
		row = s.q().QueryRow(ctx, base+`id=$2`, workspaceID, idOrShort)
	} else {
		row = s.q().QueryRow(ctx, base+`lower(short_id)=lower($2)`, workspaceID, idOrShort)
	}
	p, err := scanProject(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, s.store.notFoundProject(ctx, s.q(), "", workspaceID, idOrShort)
		}
		return nil, err
	}
	s.decorateProject(p)
	return p, nil
}

// BoardHealthInWorkspace renders the health snapshot for the agent
// surface: the same projection as BoardHealth, scoped to the caller's
// workspace (the owner's OTHER workspaces' boards are invisible here).
func (s *Service) BoardHealthInWorkspace(ctx context.Context, workspaceID string) (*Health, error) {
	h := &Health{
		ActiveLeases:         []HealthLease{},
		TaskCooldowns:        []HealthTaskCooldown{},
		ProjectCooldowns:     []HealthProjectCooldown{},
		PendingInputByOrigin: map[string]int{},
		RecentReclaims:       []HealthReclaim{},
		ProjectSlots:         []HealthProjectSlots{},
		AckWaits:             []HealthAckWait{},
		Waiting:              []HealthWait{},
	}
	rows, err := s.q().Query(ctx, `
		SELECT t.id, t.short_id, t.title, t.project_id, coalesce(t.assignee_session, ''), t.lease_expires_at
		FROM tasks t JOIN projects p ON p.id = t.project_id
		WHERE p.target_workspace_id = $1::uuid AND t.stage = 'in_progress' AND t.lease_expires_at IS NOT NULL
		ORDER BY t.lease_expires_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l HealthLease
		if err := rows.Scan(&l.TaskID, &l.ShortID, &l.Title, &l.ProjectID, &l.AssigneeSession, &l.LeaseExpiresAt); err != nil {
			rows.Close()
			return nil, err
		}
		h.ActiveLeases = append(h.ActiveLeases, l)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	// #1922: the workspace-scoped health rides the same read-edge
	// phrase translation (the caller's workspace resolves every lease).
	s.translateHealthLeases(ctx, h.ActiveLeases, func(string) string { return workspaceID })

	crows, err := s.q().Query(ctx, `
		SELECT t.id, t.short_id, t.project_id, t.cooldown_until
		FROM tasks t JOIN projects p ON p.id = t.project_id
		WHERE p.target_workspace_id = $1::uuid AND t.cooldown_until > now()
		ORDER BY t.cooldown_until`, workspaceID)
	if err != nil {
		return nil, err
	}
	for crows.Next() {
		var c HealthTaskCooldown
		if err := crows.Scan(&c.TaskID, &c.ShortID, &c.ProjectID, &c.CooldownUntil); err != nil {
			crows.Close()
			return nil, err
		}
		h.TaskCooldowns = append(h.TaskCooldowns, c)
	}
	if err := crows.Err(); err != nil {
		crows.Close()
		return nil, err
	}
	crows.Close()

	prows, err := s.q().Query(ctx, `
		SELECT id, short_id, slot_cooldown_until FROM projects
		WHERE target_workspace_id = $1::uuid AND slot_cooldown_until > now()
		ORDER BY slot_cooldown_until`, workspaceID)
	if err != nil {
		return nil, err
	}
	for prows.Next() {
		var c HealthProjectCooldown
		if err := prows.Scan(&c.ProjectID, &c.ShortID, &c.CooldownUntil); err != nil {
			prows.Close()
			return nil, err
		}
		h.ProjectCooldowns = append(h.ProjectCooldowns, c)
	}
	if err := prows.Err(); err != nil {
		prows.Close()
		return nil, err
	}
	prows.Close()

	orows, err := s.q().Query(ctx, `
		SELECT coalesce(t.pending_origin, 'unknown'), count(*)
		FROM tasks t JOIN projects p ON p.id = t.project_id
		WHERE p.target_workspace_id = $1::uuid
		  AND (t.pending_input OR (t.stage='failed' AND t.pending_origin='escalated'))
		GROUP BY 1`, workspaceID)
	if err != nil {
		return nil, err
	}
	for orows.Next() {
		var origin string
		var n int
		if err := orows.Scan(&origin, &n); err != nil {
			orows.Close()
			return nil, err
		}
		h.PendingInputByOrigin[origin] = n
	}
	if err := orows.Err(); err != nil {
		orows.Close()
		return nil, err
	}
	orows.Close()

	rrows, err := s.q().Query(ctx, `
		SELECT be.item_id, be.created_at
		FROM board_events be JOIN projects p ON p.id = be.project_id
		WHERE p.target_workspace_id = $1::uuid AND be.action = 'reclaim' AND be.item_id IS NOT NULL
		ORDER BY be.created_at DESC LIMIT 20`, workspaceID)
	if err != nil {
		return nil, err
	}
	for rrows.Next() {
		var r HealthReclaim
		if err := rrows.Scan(&r.TaskID, &r.At); err != nil {
			rrows.Close()
			return nil, err
		}
		h.RecentReclaims = append(h.RecentReclaims, r)
	}
	if err := rrows.Err(); err != nil {
		rrows.Close()
		return nil, err
	}
	rrows.Close()

	srows, err := s.q().Query(ctx, `
		SELECT p.id, p.short_id, p.worker_slots,
		       (SELECT count(*) FROM tasks t WHERE t.project_id = p.id AND t.stage = 'in_progress')
		FROM projects p WHERE p.target_workspace_id = $1::uuid AND p.status = 'active'
		ORDER BY p.created_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	for srows.Next() {
		var ps HealthProjectSlots
		if err := srows.Scan(&ps.ProjectID, &ps.ShortID, &ps.WorkerSlots, &ps.InUse); err != nil {
			srows.Close()
			return nil, err
		}
		h.ProjectSlots = append(h.ProjectSlots, ps)
	}
	if err := srows.Err(); err != nil {
		srows.Close()
		return nil, err
	}
	srows.Close()
	if err := s.healthAckWaits(ctx, nil, &workspaceID, h); err != nil {
		return nil, err
	}
	if err := s.healthWaiting(ctx, nil, &workspaceID, h); err != nil {
		return nil, err
	}
	return h, nil
}
