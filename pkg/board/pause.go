// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

// pause.go — the #1747 scheduling lever. PAUSE MUST PAUSE (owner ruling
// 6261172515): pause_task/project/epic sets a FLAG (never a stage — the
// five-stage vocabulary is binding) AND delivers a HALT directive to the
// in-flight worker's session over the existing message machinery; the
// directive lands at the current turn's boundary (finish the thought,
// then stop — never a mid-generation cut). Pause gates claiming, claim
// MINTS, and FORWARD stage-writes; it never revokes a lease or token.
//
// Lease accounting during pause is LAPSE, not freeze (worker proposal,
// adopted): the lease clock runs; heartbeats keep it alive only while
// the halted worker still produces evidence (the #1685 gate needs no
// paused special-case). A long pause lapses naturally — the reconciler
// requeues the row to backlog where the paused flag parks it. Resume
// clears the flag AND delivers a RESUME directive: the held session
// continues if its lease survived; otherwise the row re-enters the FIFO
// pool and the next claim spawns a fresh worker (the stale session's
// next write 409s and it re-reads).

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PauseTask parks one task: paused=true, claim candidacy off, forward
// stage-writes refused, HALT delivered to a live in-flight worker.
// Terminal rows refuse (done is immutable; cancel is eliminated —
// #1747 round 3).
func (s *Service) PauseTask(ctx context.Context, actor Actor, taskID, shortIDOrID, requestID string) (*Task, error) {
	t, p, err := s.taskProjectForActor(ctx, actor, taskID, shortIDOrID)
	if err != nil {
		return nil, err
	}
	if t.Stage == StageDone {
		return nil, &GuardError{Code: GuardPaused, Detail: "terminal rows refuse pause (done is immutable)"}
	}
	if t.Paused {
		return t, nil // idempotent
	}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "pause_task", itemKind: "task", itemID: t.ID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		tag, err := tx.Exec(ctx, `UPDATE tasks SET paused=true, updated_at=now() WHERE id=$1`, t.ID)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, &NotFoundError{Item: "task", Identifier: t.ShortID}
		}
		return map[string]any{"task_id": t.ID, "paused": true}, nil
	})
	if err != nil {
		return nil, err
	}
	_ = dup
	// The halt directive: post-commit, best-effort — the flag is the
	// authority; a lost directive self-heals at lease lapse (parked).
	s.deliverToWorker(ctx, t, DirectiveHalt)
	out, err := s.store.resolveTask(ctx, s.q(), p.ID, t.ID)
	if err != nil {
		return nil, err
	}
	s.decorateTask(ctx, out, p)
	return out, nil
}

// ResumeTask clears the flag and delivers the RESUME directive to the
// held session. A survived lease simply continues; a lapsed one parked
// the row in backlog — it re-enters the FIFO pool now.
func (s *Service) ResumeTask(ctx context.Context, actor Actor, taskID, shortIDOrID, requestID string) (*Task, error) {
	t, p, err := s.taskProjectForActor(ctx, actor, taskID, shortIDOrID)
	if err != nil {
		return nil, err
	}
	if !t.Paused {
		return nil, &GuardError{Code: GuardPaused, Detail: "the task is not paused"}
	}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "resume_task", itemKind: "task", itemID: t.ID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		tag, err := tx.Exec(ctx, `UPDATE tasks SET paused=false, updated_at=now() WHERE id=$1`, t.ID)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, &NotFoundError{Item: "task", Identifier: t.ShortID}
		}
		return map[string]any{"task_id": t.ID, "paused": false}, nil
	})
	if err != nil {
		return nil, err
	}
	_ = dup
	s.deliverToWorker(ctx, t, DirectiveResume)
	s.pokeDrive(p.ID)
	out, err := s.store.resolveTask(ctx, s.q(), p.ID, t.ID)
	if err != nil {
		return nil, err
	}
	s.decorateTask(ctx, out, p)
	return out, nil
}

// PauseProject pauses the project ("pause this project so another moves
// forward"): claim candidacy off for every task under it (query
// scoping — no duplicated flags), HALT cascades to all in-flight
// workers. Archived projects refuse.
func (s *Service) PauseProject(ctx context.Context, actor Actor, projectID, shortIDOrID, requestID string) (*Project, error) {
	return s.projectPause(ctx, actor, projectID, shortIDOrID, true, "pause_project")
}

// ResumeProject clears the project pause; RESUME cascades to in-flight
// workers under it.
func (s *Service) ResumeProject(ctx context.Context, actor Actor, projectID, shortIDOrID, requestID string) (*Project, error) {
	return s.projectPause(ctx, actor, projectID, shortIDOrID, false, "resume_project")
}

func (s *Service) projectPause(ctx context.Context, actor Actor, projectID, shortIDOrID string, to bool, action string) (*Project, error) {
	p, err := s.store.resolveProject(ctx, s.q(), actor.UserID, pick(projectID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	if err := requireOwner(actor, p.UserID); err != nil {
		return nil, err
	}
	if err := requireManagerAction(actor, p); err != nil {
		return nil, err
	}
	if to && p.Status == ProjectArchived {
		return nil, &GuardError{Code: GuardProjectState, Detail: "archived projects refuse pause (unarchive first)"}
	}
	if p.Paused == to {
		s.decorateProject(p)
		return p, nil // idempotent
	}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: "", action: action, itemKind: "project", itemID: p.ID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		if _, err := tx.Exec(ctx, `UPDATE projects SET paused=$2, updated_at=now() WHERE id=$1`, p.ID, to); err != nil {
			return nil, err
		}
		return map[string]any{"id": p.ID, "paused": to}, nil
	})
	if err != nil {
		return nil, err
	}
	_ = dup
	directive := DirectiveResume
	if to {
		directive = DirectiveHalt
	}
	s.deliverToProjectWorkers(ctx, p.ID, directive)
	out, err := s.store.resolveProject(ctx, s.q(), actor.UserID, p.ID)
	if err != nil {
		return nil, err
	}
	s.decorateProject(out)
	return out, nil
}

// --- directive delivery ---------------------------------------------------------

// deliverToWorker sends one directive to a task's live in-flight
// session (halt/resume), if any — best-effort, never blocking the write.
func (s *Service) deliverToWorker(ctx context.Context, t *Task, directive string) {
	if s.messenger == nil {
		return
	}
	if t.AssigneeSession == "" || t.Stage != StageInProgress {
		return
	}
	_ = s.messenger(ctx, t.AssigneeSession, directive, t.ShortID)
}

// deliverToEpicWorkers cascades a directive to every in-flight worker
// under an epic.
func (s *Service) deliverToEpicWorkers(ctx context.Context, epicID, directive string) {
	if s.messenger == nil {
		return
	}
	for _, w := range s.inFlightUnder(ctx, `t.epic_id = $1`, epicID) {
		_ = s.messenger(ctx, w.session, directive, w.shortID)
	}
}

// deliverToProjectWorkers cascades a directive to every in-flight
// worker under a project.
func (s *Service) deliverToProjectWorkers(ctx context.Context, projectID, directive string) {
	if s.messenger == nil {
		return
	}
	for _, w := range s.inFlightUnder(ctx, `t.project_id = $1`, projectID) {
		_ = s.messenger(ctx, w.session, directive, w.shortID)
	}
}

type inFlightWorker struct {
	session, shortID string
}

func (s *Service) inFlightUnder(ctx context.Context, predicate, id string) []inFlightWorker {
	rows, err := s.q().Query(ctx, fmt.Sprintf(`
		SELECT assignee_session, short_id FROM tasks t
		WHERE %s AND t.stage='in_progress' AND t.assignee_session IS NOT NULL`, predicate), id)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []inFlightWorker
	for rows.Next() {
		var w inFlightWorker
		if err := rows.Scan(&w.session, &w.shortID); err != nil {
			return nil
		}
		out = append(out, w)
	}
	return out
}

// --- resource pressure (#1747 round 3) -------------------------------------------

// ReportResourcePressure records a workspace-pod's latest pressure
// sample (agentd pushes over the seam; thresholds + hysteresis are the
// reporter's). The claim choke point skips pressured workspaces; reads
// surface the informational note.
func (s *Service) ReportResourcePressure(ctx context.Context, workspaceID string, sample PressureSample) error {
	sample.At = time.Now().UTC()
	s.pressureMu.Lock()
	s.pressure[workspaceID] = sample
	s.pressureMu.Unlock()
	return nil
}

// ActivePressure returns the workspace's live sample (nil when absent
// or stale past the TTL — silence heals).
func (s *Service) ActivePressure(workspaceID string) *PressureSample {
	s.pressureMu.RLock()
	sample, ok := s.pressure[workspaceID]
	s.pressureMu.RUnlock()
	if !ok || time.Since(sample.At) > s.cfg.PressureSampleTTL {
		return nil
	}
	return &sample
}

// --- concurrency caps + read decoration ------------------------------------------

// ConcurrencyCounts is the raw per-owner / per-instance in_progress
// census the #1747 caps compare against (service.go's helper owns the
// query — see inFlightCountsForProject).
type ConcurrencyCounts struct {
	UserInFlight   int
	GlobalInFlight int
}

// UserAtCap reports whether the owner's in_progress count reached the cap.
func (c ConcurrencyCounts) UserAtCap(cap int) bool { return c.UserInFlight >= cap }

// GlobalAtCap reports whether the instance's in_progress count reached the cap.
func (c ConcurrencyCounts) GlobalAtCap(cap int) bool { return c.GlobalInFlight >= cap }

// ConcurrencyState is the derived backpressure answer for one project's
// owner: whether the per-user or per-instance concurrent-claim cap is
// saturated (informational — never an error at the claim path).
type ConcurrencyState struct {
	UserAtCap   bool
	GlobalAtCap bool
}

func (s *Service) concurrencyFor(ctx context.Context, p *Project) ConcurrencyState {
	var user, global int
	if err := s.q().QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM tasks ti JOIN projects pi ON pi.id = ti.project_id
		   WHERE ti.stage='in_progress' AND pi.user_id = $1),
		  (SELECT count(*) FROM tasks WHERE stage='in_progress')`, p.UserID).Scan(&user, &global); err != nil {
		return ConcurrencyState{}
	}
	return ConcurrencyState{UserAtCap: user >= s.cfg.UserClaimCap, GlobalAtCap: global >= s.cfg.GlobalClaimCap}
}

// decorateTask attaches the derived informational fields (waiting_on,
// backpressure) to one task read. p may be nil (lookup skipped).
func (s *Service) decorateTask(ctx context.Context, t *Task, p *Project) {
	if t == nil {
		return
	}
	if p == nil {
		var err error
		p, err = s.store.resolveProject(ctx, s.q(), "", t.ProjectID)
		if err != nil {
			return
		}
	}
	if t.Stage == StageBacklog && !t.IsBlocked && !t.Paused &&
		(t.CooldownUntil == nil || t.CooldownUntil.After(time.Now())) {
		if cs := s.concurrencyFor(ctx, p); cs.UserAtCap || cs.GlobalAtCap {
			t.WaitingOn = "concurrency_limit"
		}
	}
	if sample := s.ActivePressure(p.TargetWorkspaceID); sample != nil {
		t.Backpressure = sample.Summary()
	}
}

// decorateEpic/decorateProject attach the backpressure note.
func (s *Service) decorateEpic(ctx context.Context, e *Epic) {
	if e == nil {
		return
	}
	p, err := s.store.resolveProject(ctx, s.q(), "", e.ProjectID)
	if err != nil {
		return
	}
	if sample := s.ActivePressure(p.TargetWorkspaceID); sample != nil {
		e.Backpressure = sample.Summary()
	}
}

func (s *Service) decorateProject(p *Project) {
	if p == nil {
		return
	}
	if sample := s.ActivePressure(p.TargetWorkspaceID); sample != nil {
		p.Backpressure = sample.Summary()
	}
}

// --- grant differentiation belt ---------------------------------------------------

// IsWorkerSession reports whether the session currently holds a board
// worker role (assignee of a live/pending row) — the seam's manager
// route belt (#1747 §5: spawned workers must not reach manager actions;
// agentd's spawn-time registry is the first gate, this is the
// engine-authoritative second).
func (s *Service) IsWorkerSession(ctx context.Context, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	var live bool
	err := s.q().QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM tasks
			WHERE assignee_session = $1 AND stage IN ('in_progress','pending_input'))`, sessionID).Scan(&live)
	return live, err
}

// taskProjectForActor resolves a task + its project through the actor's
// scope (agent surface: workspace-scoped resolution, r1-S1) and stamps
// the manager guards (R7 ownership + the #1747 workspace binding).
func (s *Service) taskProjectForActor(ctx context.Context, actor Actor, taskID, shortIDOrID string) (*Task, *Project, error) {
	t, err := s.resolveTaskForActor(ctx, actor, pick(taskID, shortIDOrID))
	if err != nil {
		return nil, nil, err
	}
	p, err := s.store.resolveProject(ctx, s.q(), "", t.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := requireOwner(actor, p.UserID); err != nil {
		return nil, nil, err
	}
	if err := requireManagerAction(actor, p); err != nil {
		return nil, nil, err
	}
	return t, p, nil
}
