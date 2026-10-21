// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Queryer abstracts pool vs tx (pgx's own common surface).
type Queryer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// poolQ adapts *pgxpool.Pool; txQ adapts pgx.Tx — both to Queryer.
type poolQ struct{ p *pgxpool.Pool }

func (q poolQ) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return q.p.Exec(ctx, sql, args...)
}

func (q poolQ) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return q.p.Query(ctx, sql, args...)
}

func (q poolQ) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return q.p.QueryRow(ctx, sql, args...)
}

type txQ struct{ t pgx.Tx }

func (q txQ) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return q.t.Exec(ctx, sql, args...)
}

func (q txQ) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return q.t.Query(ctx, sql, args...)
}

func (q txQ) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return q.t.QueryRow(ctx, sql, args...)
}

const taskColumns = `t.id, t.project_id, t.epic_id, t.short_id, t.title, t.description,
	t.stage, t.pending_origin, t.project_name, t.epic_name, t.tags,
	t.acceptance_criteria, t.is_blocked, t.paused, t.assignee_session, t.claim_token::text,
	t.lease_expires_at, t.last_progress_at, t.attempts, t.claim_incremented_attempts,
	t.cooldown_until, t.stage_entered_at, t.created_at, t.updated_at`

func scanTask(row pgx.Row) (*Task, error) {
	var t Task
	var criteria []byte
	var token, assignee *string
	err := row.Scan(&t.ID, &t.ProjectID, &t.EpicID, &t.ShortID, &t.Title, &t.Description,
		&t.Stage, &t.PendingOrigin, &t.ProjectName, &t.EpicName, &t.Tags,
		&criteria, &t.IsBlocked, &t.Paused, &assignee, &token,
		&t.LeaseExpiresAt, &t.LastProgressAt, &t.Attempts, &t.ClaimIncremented,
		&t.CooldownUntil, &t.StageEnteredAt, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if token != nil {
		t.ClaimToken = *token
	}
	if assignee != nil {
		t.AssigneeSession = *assignee
	}
	if len(criteria) > 0 {
		if err := json.Unmarshal(criteria, &t.AcceptanceCriteria); err != nil {
			return nil, fmt.Errorf("board: criteria decode: %w", err)
		}
	}
	return &t, nil
}

const projectColumns = `id, short_id, user_id, name, target_workspace_id::text, worker_slots,
	slot_cooldown_until, tags, status, paused, created_at, updated_at`

func scanProject(row pgx.Row) (*Project, error) {
	var p Project
	var ws *string
	err := row.Scan(&p.ID, &p.ShortID, &p.UserID, &p.Name, &ws, &p.WorkerSlots,
		&p.SlotCooldownUntil, &p.Tags, &p.Status, &p.Paused, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if ws != nil {
		p.TargetWorkspaceID = *ws
	}
	return &p, nil
}

const epicColumns = `id, project_id, short_id, title, outcome, status, project_name,
	tags, vft, deficit, quantum, created_at, updated_at`

func scanEpic(row pgx.Row) (*Epic, error) {
	var e Epic
	err := row.Scan(&e.ID, &e.ProjectID, &e.ShortID, &e.Title, &e.Outcome, &e.Status,
		&e.ProjectName, &e.Tags, &e.VFT, &e.Deficit, &e.Quantum, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// isUUID reports whether s is a canonical UUID (identity vs display
// resolution: FKs are identity, names/short IDs are display data).
func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

// resolveProject resolves a project by UUID or case-insensitive short ID.
// userID scopes manager-surface resolution (R7 ownership); pass "" for
// engine scope. A miss returns NotFoundError with in-scope suggestions.
func (st *Store) resolveProject(ctx context.Context, q Queryer, userID, idOrShort string) (*Project, error) {
	var row pgx.Row
	if isUUID(idOrShort) {
		if userID != "" {
			row = q.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE id=$1 AND user_id=$2`, idOrShort, userID)
		} else {
			row = q.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE id=$1`, idOrShort)
		}
	} else {
		if userID != "" {
			row = q.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE lower(short_id)=lower($1) AND user_id=$2`, idOrShort, userID)
		} else {
			row = q.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE lower(short_id)=lower($1)`, idOrShort)
		}
	}
	p, err := scanProject(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			_ = userID // ownership scoping is the caller's requireOwner; suggestions scope below
			return nil, st.notFoundProject(ctx, q, "", "", idOrShort)
		}
		return nil, err
	}
	return p, nil
}

func (st *Store) notFoundProject(ctx context.Context, q Queryer, projectID, workspaceID, idOrShort string) error {
	cands := st.scopeCandidates(ctx, q, "project", projectID, workspaceID)
	return &NotFoundError{Item: "project", Identifier: idOrShort, Suggestions: suggestTop3(idOrShort, cands)}
}

// scopeCandidates returns in-scope short IDs for did-you-mean. A scope is
// REQUIRED (project or workspace): suggestions never cross authorization
// boundaries (design §5 — the r1 S1 finding; an unscoped suggestion list
// would leak other tenants' short IDs and make discovery feasible).
func (st *Store) scopeCandidates(ctx context.Context, q Queryer, kind, projectID, workspaceID string) []string {
	var rows pgx.Rows
	var err error
	switch kind {
	case "project":
		if workspaceID != "" {
			rows, err = q.Query(ctx,
				`SELECT short_id FROM projects WHERE target_workspace_id=$1::uuid`, workspaceID)
		} else if projectID != "" {
			rows, err = q.Query(ctx, `SELECT short_id FROM projects WHERE id=$1::uuid`, projectID)
		} else {
			return nil // fail-closed: no scope, no suggestions
		}
	case "epic":
		if projectID == "" {
			return nil
		}
		rows, err = q.Query(ctx, `SELECT short_id FROM epics WHERE project_id=$1`, projectID)
	case "task":
		switch {
		case projectID != "":
			rows, err = q.Query(ctx, `SELECT short_id FROM tasks WHERE project_id=$1`, projectID)
		case workspaceID != "":
			rows, err = q.Query(ctx,
				`SELECT t.short_id FROM tasks t JOIN projects p ON p.id=t.project_id
				 WHERE p.target_workspace_id=$1::uuid`, workspaceID)
		default:
			return nil // fail-closed
		}
	default:
		return nil
	}
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// resolveTask resolves a task by UUID or case-insensitive short ID, with
// optional project scope.
func (st *Store) resolveTask(ctx context.Context, q Queryer, projectID, idOrShort string) (*Task, error) {
	resolve := func() pgx.Row {
		base := `SELECT ` + taskColumns + ` FROM tasks t WHERE `
		if isUUID(idOrShort) {
			if projectID != "" {
				return q.QueryRow(ctx, base+`t.id=$1 AND t.project_id=$2`, idOrShort, projectID)
			}
			return q.QueryRow(ctx, base+`t.id=$1`, idOrShort)
		}
		if projectID != "" {
			return q.QueryRow(ctx, base+`lower(t.short_id)=lower($1) AND t.project_id=$2`, idOrShort, projectID)
		}
		return q.QueryRow(ctx, base+`lower(t.short_id)=lower($1)`, idOrShort)
	}
	t, err := scanTask(resolve())
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, st.notFoundTask(ctx, q, projectID, "", idOrShort)
		}
		return nil, err
	}
	if err := st.attachBlockedBy(ctx, q, t); err != nil {
		return nil, err
	}
	return t, nil
}

func (st *Store) notFoundTask(ctx context.Context, q Queryer, projectID, workspaceID, idOrShort string) error {
	cands := st.scopeCandidates(ctx, q, "task", projectID, workspaceID)
	return &NotFoundError{Item: "task", Identifier: idOrShort, Suggestions: suggestTop3(idOrShort, cands)}
}

func (st *Store) attachBlockedBy(ctx context.Context, q Queryer, t *Task) error {
	rows, err := q.Query(ctx, `
		SELECT dep.short_id FROM board_dependencies d
		JOIN tasks dep ON dep.id = d.depends_on_task_id
		WHERE d.task_id = $1 AND dep.stage <> 'done'
		ORDER BY dep.short_id`, t.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		t.BlockedBy = append(t.BlockedBy, id)
	}
	return nil
}

// resolveTaskInWorkspace resolves a task SCOPED to the caller's
// workspace (the agent-surface reader, design §4.4 caller-workspace
// binding): only tasks whose project targets workspaceID resolve; a miss
// is a 404 whose suggestions stay inside the same tenant. This is the
// r1-S1 fix — the unscoped reader leaked cross-tenant rows and ids.
func (st *Store) resolveTaskInWorkspace(ctx context.Context, q Queryer, workspaceID, idOrShort string) (*Task, error) {
	var row pgx.Row
	base := `SELECT ` + taskColumns + ` FROM tasks t
		JOIN projects p ON p.id = t.project_id
		WHERE p.target_workspace_id = $1::uuid AND `
	if isUUID(idOrShort) {
		row = q.QueryRow(ctx, base+`t.id=$2`, workspaceID, idOrShort)
	} else {
		row = q.QueryRow(ctx, base+`lower(t.short_id)=lower($2)`, workspaceID, idOrShort)
	}
	t, err := scanTask(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, st.notFoundTask(ctx, q, "", workspaceID, idOrShort)
		}
		return nil, err
	}
	if err := st.attachBlockedBy(ctx, q, t); err != nil {
		return nil, err
	}
	return t, nil
}

// resolveEpic resolves an epic by UUID or case-insensitive short ID.
func (st *Store) resolveEpic(ctx context.Context, q Queryer, projectID, idOrShort string) (*Epic, error) {
	var row pgx.Row
	if isUUID(idOrShort) {
		if projectID != "" {
			row = q.QueryRow(ctx, `SELECT `+epicColumns+` FROM epics WHERE id=$1 AND project_id=$2`, idOrShort, projectID)
		} else {
			row = q.QueryRow(ctx, `SELECT `+epicColumns+` FROM epics WHERE id=$1`, idOrShort)
		}
	} else {
		if projectID != "" {
			row = q.QueryRow(ctx, `SELECT `+epicColumns+` FROM epics WHERE lower(short_id)=lower($1) AND project_id=$2`, idOrShort, projectID)
		} else {
			row = q.QueryRow(ctx, `SELECT `+epicColumns+` FROM epics WHERE lower(short_id)=lower($1)`, idOrShort)
		}
	}
	e, err := scanEpic(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			cands := st.scopeCandidates(ctx, q, "epic", "", projectID)
			return nil, &NotFoundError{Item: "epic", Identifier: idOrShort, Suggestions: suggestTop3(idOrShort, cands)}
		}
		return nil, err
	}
	return e, nil
}

// mintUniqueTaskShortID mints with check-and-retry inside the caller's tx.
func mintUnique(ctx context.Context, q Queryer, table, prefix, scopeSQL string, scopeArgs ...any) (string, error) {
	for attempt := 0; attempt < 5; attempt++ {
		id, err := mintShortID(prefix)
		if err != nil {
			return "", err
		}
		var exists bool
		args := append([]any{id}, scopeArgs...)
		err = q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE short_id=$1`+scopeSQL+`)`, args...).Scan(&exists)
		if err != nil {
			return "", err
		}
		if !exists {
			return id, nil
		}
	}
	return "", fmt.Errorf("board: short-id mint exhausted for %s", prefix)
}

// writeTransition records one board_transitions row.
func writeTransition(ctx context.Context, q Queryer, taskID, from, to, evidence string, actor Actor) error {
	_, err := q.Exec(ctx, `
		INSERT INTO board_transitions (task_id, from_stage, to_stage, evidence_url, actor)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5)`, taskID, from, to, evidence, actorViewOf(actor))
	return err
}

// unblockDependents re-derives is_blocked for non-terminal dependents of
// the given task (the reverse-edge cascade, §4.1-edges). Called in-tx on
// every →done write and its inverse.
func unblockDependents(ctx context.Context, q Queryer, taskID string, targetDone bool) (int64, error) {
	sql := `
		UPDATE tasks t SET is_blocked = `
	if targetDone {
		sql += `EXISTS(SELECT 1 FROM board_dependencies d
			JOIN tasks dep ON dep.id = d.depends_on_task_id
			WHERE d.task_id = t.id AND dep.stage <> 'done')`
	} else {
		sql += `true`
	}
	sql += `
		WHERE t.stage <> 'done'
		  AND EXISTS(SELECT 1 FROM board_dependencies d WHERE d.depends_on_task_id = $1 AND d.task_id = t.id)`
	tag, err := q.Exec(ctx, sql, taskID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// epicCompletionCheck flips an active epic to completed iff it holds at
// least one done task and no non-terminal tasks (RC4, §4.5). Returns true
// when the flip fired.
func epicCompletionCheck(ctx context.Context, q Queryer, epicID string) (bool, error) {
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

// actorView is the persisted actor shape (board_events.actor,
// board_transitions.actor).
type actorView struct {
	Kind        string `json:"kind"`
	UserID      string `json:"user_id,omitempty"`
	Surface     string `json:"surface,omitempty"`
	ClaimToken  string `json:"claim_token,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	// SessionID attributes agent-surface actions to the creating session
	// (#1747 audit ruling). Empty on session-less surfaces.
	SessionID string `json:"session_id,omitempty"`
}

func actorViewOf(a Actor) []byte {
	v := actorView(a) //nolint:staticcheck // ST1016 opt-out: the struct exists for the JSON field tags/omitempty
	v.SessionID = a.SessionID
	b, _ := json.Marshal(v)
	return b
}

// criteriaAllMet reports whether every acceptance criterion is met (the
// Done guard's first half, ruling R2).
func criteriaAllMet(t *Task) bool {
	if len(t.AcceptanceCriteria) == 0 {
		return true
	}
	for _, c := range t.AcceptanceCriteria {
		if !c.Met {
			return false
		}
	}
	return true
}

// prAnchor extracts a conservative `pr:N` anchor from a task's tags (RC2;
// never free-text guessing).
func prAnchor(tags []string) (int, bool) {
	for _, tg := range tags {
		if strings.HasPrefix(tg, "pr:") {
			var n int
			if _, err := fmt.Sscanf(tg, "pr:%d", &n); err == nil && n > 0 {
				return n, true
			}
		}
	}
	return 0, false
}

func uuidNew() string { return uuid.NewString() }
