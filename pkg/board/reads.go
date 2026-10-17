// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// reads.go — the board_read projection (§4.2): a read model over the
// materialized columns. Epic progress is DERIVED here, never stored
// (ruling R2): Done over (total − StageCancelled); a zero denominator
// renders no-progress, never 0/0.

// GetTask resolves a task (case-insensitive short ID or UUID).
func (s *Service) GetTask(ctx context.Context, taskID, shortIDOrID string) (*Task, error) {
	return s.store.resolveTask(ctx, s.q(), "", pick(taskID, shortIDOrID))
}

// ListTasks lists a project's tasks (optionally by stage).
func (s *Service) ListTasks(ctx context.Context, projectID string, stage string) ([]Task, error) {
	q := `SELECT ` + taskColumns + ` FROM tasks t WHERE t.project_id=$1`
	args := []any{projectID}
	if stage != "" {
		q += ` AND t.stage=$2`
		args = append(args, stage)
	}
	q += ` ORDER BY t.priority, t.created_at`
	rows, err := s.q().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rowScan{rows})
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// GetEpic resolves an epic with derived progress.
func (s *Service) GetEpic(ctx context.Context, epicID, shortIDOrID string) (*Epic, error) {
	e, err := s.store.resolveEpic(ctx, s.q(), "", pick(epicID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	if err := s.deriveProgress(ctx, e); err != nil {
		return nil, err
	}
	return e, nil
}

func (s *Service) deriveProgress(ctx context.Context, e *Epic) error {
	err := s.q().QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE t.stage = 'done'),
			count(*) FILTER (WHERE t.stage <> 'cancelled')
		FROM tasks t WHERE t.epic_id = $1`, e.ID).Scan(&e.ProgressDone, &e.ProgressTotal)
	if err != nil {
		return err
	}
	e.ProgressDerived = e.ProgressTotal > 0
	return nil
}

// GetProject resolves a project.
func (s *Service) GetProject(ctx context.Context, projectID, shortIDOrID string) (*Project, error) {
	return s.store.resolveProject(ctx, s.q(), "", pick(projectID, shortIDOrID))
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
		out = append(out, *p)
	}
	return out, rows.Err()
}

// rowScan adapts pgx.Rows to the pgx.Row interface (single-row scans over
// iteration cursors).
type rowScan struct{ rows pgx.Rows }

func (r rowScan) Scan(dest ...any) error { return r.rows.Scan(dest...) }
