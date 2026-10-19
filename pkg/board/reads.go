// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// reads.go — the board_read projection (§4.2): a read model over the
// materialized columns. Epic progress is DERIVED here, never stored
// (ruling R2): Done over total; a zero denominator renders no-progress,
// never 0/0.

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
	var out []Task
	for rows.Next() {
		t, err := scanTask(rowScan{rows})
		if err != nil {
			return nil, err
		}
		s.decorateTask(ctx, t, nil)
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

// rowScan adapts pgx.Rows to the pgx.Row interface (single-row scans over
// iteration cursors).
type rowScan struct{ rows pgx.Rows }

func (r rowScan) Scan(dest ...any) error { return r.rows.Scan(dest...) }
