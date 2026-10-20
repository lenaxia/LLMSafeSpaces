// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// reads.go — the board_read projection (§4.2): a read model over the
// materialized columns. Epic progress is DERIVED here, never stored
// (ruling R2, #1747-amended): Done over ALL tasks; a zero denominator
// renders no-progress, never 0/0.

// GetTask resolves a task (case-insensitive short ID or UUID) and
// attaches the derived informational fields (waiting_on, backpressure).
func (s *Service) GetTask(ctx context.Context, taskID, shortIDOrID string) (*Task, error) {
	t, err := s.store.resolveTask(ctx, s.q(), "", pick(taskID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	s.decorateTask(ctx, t, nil)
	return t, nil
}

// ListTasks lists a project's tasks (optionally by stage) in creation
// order — the FIFO claim order's read-side mirror (#1747).
func (s *Service) ListTasks(ctx context.Context, projectID string, stage string) ([]Task, error) {
	q := `SELECT ` + taskColumns + ` FROM tasks t WHERE t.project_id=$1`
	args := []any{projectID}
	if stage != "" {
		q += ` AND t.stage=$2`
		args = append(args, stage)
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
	for rows.Next() {
		t, err := scanTask(rowScan{rows})
		if err != nil {
			return nil, err
		}
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
	return out, rows.Err()
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
	return e, nil
}

// deriveProgress carries the SAME denominator as ListEpics (the #1747
// owner ruling: Done over ALL tasks — the canceled exclusion design
// 0067 R2 carried is superseded by PR #1763's semantics; the one-epic
// and list reads must agree).
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
// Denominator semantics per the #1747 owner ruling (design authority
// transferred to PR #1763): Done over ALL tasks — the canceled
// exclusion design 0067 R2 carried is superseded.
func (s *Service) ListEpics(ctx context.Context, projectID string) ([]Epic, error) {
	rows, err := s.q().Query(ctx, `
		SELECT e.id, e.project_id, e.short_id, e.title, e.outcome, e.status, e.project_name,
		       e.tags, e.vft, e.deficit, e.quantum, e.created_at, e.updated_at,
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
			&e.ProjectName, &e.Tags, &e.VFT, &e.Deficit, &e.Quantum, &e.CreatedAt, &e.UpdatedAt,
			&e.ProgressDone, &e.ProgressTotal); err != nil {
			return nil, err
		}
		e.ProgressDerived = e.ProgressTotal > 0
		out = append(out, e)
	}
	return out, rows.Err()
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
		return nil, &GuardError{Code: GuardInput, Detail: "stage filter must be one of the six stages"}
	}

	args := []any{userID, trimmed, likeLiteral(trimmed)}
	q2 := `SELECT ` + taskColumns + ` FROM tasks t
		JOIN projects p ON p.id = t.project_id
		WHERE p.user_id = $1
		  AND (lower(t.short_id) = lower($2) OR t.title ILIKE $3 ESCAPE '\')`
	if filters.Stage != "" {
		args = append(args, filters.Stage)
		q2 += fmt.Sprintf(" AND t.stage = $%d", len(args))
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
	for rows.Next() {
		t, err := scanTask(rowScan{rows})
		if err != nil {
			return nil, err
		}
		res.Results = append(res.Results, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
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
	return h, s.healthProjectSlots(ctx, userID, h)
}

func (s *Service) healthActiveLeases(ctx context.Context, userID string, h *Health) error {
	rows, err := s.q().Query(ctx, `
		SELECT t.id, t.short_id, t.title, t.project_id, coalesce(t.assignee_session, ''), t.lease_expires_at
		FROM tasks t JOIN projects p ON p.id = t.project_id
		WHERE p.user_id = $1 AND t.stage = 'in_progress' AND t.lease_expires_at IS NOT NULL
		ORDER BY t.lease_expires_at`, userID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var l HealthLease
		if err := rows.Scan(&l.TaskID, &l.ShortID, &l.Title, &l.ProjectID, &l.AssigneeSession, &l.LeaseExpiresAt); err != nil {
			return err
		}
		h.ActiveLeases = append(h.ActiveLeases, l)
	}
	return rows.Err()
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
	rows, err := s.q().Query(ctx, `
		SELECT coalesce(t.pending_origin, 'unknown'), count(*)
		FROM tasks t JOIN projects p ON p.id = t.project_id
		WHERE p.user_id = $1 AND t.stage = 'pending_input'
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
