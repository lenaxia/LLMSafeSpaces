// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Option configures a Service (functional options, constructed at boot).
type Option func(*Service)

// WithEventPublisher installs the §4.6 post-commit event projection
// (SSE fan-out). See EventPublisher.
func WithEventPublisher(pub EventPublisher) Option {
	return func(s *Service) { s.publisher = pub }
}

// WithFailpoint installs the fault seam (kill-tests).
func WithFailpoint(fp Failpoint) Option {
	return func(s *Service) { s.failpoint = fp }
}

// WithWorkerSpawner installs the fresh-worker spawn seam (ruling R9:
// claim-then-spawn; spawn failure never revokes the claim — the lease
// lapse path heals).
func WithWorkerSpawner(sp WorkerSpawner) Option {
	return func(s *Service) { s.spawner = sp }
}

// WithWorkerAborter installs the abort seam (cancel mid-flight).
func WithWorkerAborter(ab WorkerAborter) Option {
	return func(s *Service) { s.aborter = ab }
}

// WithPRDriftSource installs the RC2 external-truth source.
func WithPRDriftSource(src PRDriftSource) Option {
	return func(s *Service) { s.drift = src }
}

// WithPendingInputSource installs the pending-input coupling source.
func WithPendingInputSource(src PendingInputSource) Option {
	return func(s *Service) { s.pending = src }
}

// Service is the ONE canonical action layer (design §4.2): guards,
// validation, idempotency, and audit live here; every projection is a thin
// adapter. Safe for concurrent use (pgxpool arbitrates; Postgres is the
// only scheduler state).
type Service struct {
	store      *Store
	cfg        EngineConfig
	failpoint  Failpoint
	spawner    WorkerSpawner
	asyncSpawn bool
	driveQueue chan<- string
	aborter    WorkerAborter
	drift      PRDriftSource
	pending    PendingInputSource
	publisher  EventPublisher
}

// SetEventPublisher installs or replaces the §4.6 event projection at
// wiring time (before first use); nil disables publishing.
func (s *Service) SetEventPublisher(pub EventPublisher) { s.publisher = pub }

// publishCommitted fans a committed board event to the installed
// publisher. Called strictly AFTER the writing transaction commits
// (persist-then-publish, §4.6 i). A zero Event (a replayed
// deterministic id — ON CONFLICT produced no row, no view) publishes
// nothing. Fail-open by construction: a nil publisher is a no-op and
// the write already succeeded.
func (s *Service) publishCommitted(evt Event) {
	if s.publisher != nil && evt.ID != "" {
		s.publisher(evt)
	}
}

// NewService constructs the BoardService.
func NewService(pool *pgxpool.Pool, cfg EngineConfig, opts ...Option) *Service {
	if cfg.AttemptsCap <= 0 {
		cfg.AttemptsCap = 3
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = 15 * time.Minute
	}
	if cfg.ThrottleBackoff <= 0 {
		cfg.ThrottleBackoff = 5 * time.Minute
	}
	if cfg.DRRQuantum <= 0 {
		cfg.DRRQuantum = 1
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = 30 * time.Second
	}
	s := &Service{store: NewStore(pool), cfg: cfg}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Config returns the effective engine config.
func (s *Service) Config() EngineConfig { return s.cfg }

// --- Inputs ------------------------------------------------------------------

// ProjectInput is the create/update payload for projects.
type ProjectInput struct {
	Name              string
	TargetWorkspaceID string
	WorkerSlots       int
	Tags              []string
}

// EpicInput is the create/update payload for epics.
type EpicInput struct {
	Title    string
	Outcome  string
	Priority string
	Tags     []string
}

// TaskInput is the create/update payload for tasks.
type TaskInput struct {
	Title              string
	Description        string
	Priority           string
	Tags               []string
	AcceptanceCriteria []AcceptanceCriterion
	// SetCriteriaMet checks off the criterion at this 0-based index.
	SetCriteriaMet *int
}

// --- Shared guard helpers ------------------------------------------------------

func (s *Service) q() Queryer { return poolQ{s.store.pool} }

// requireOwner enforces R7 (user-level boards): manager actions belong to
// the owning user. The engine paths (claim/lease/reconcile) never call
// this — they are ownership-agnostic by construction (§4.7).
func requireOwner(actor Actor, userID string) error {
	if actor.UserID == "" || actor.UserID != userID {
		return &GuardError{Code: GuardAuthz, Detail: "board actions belong to the owning user (ruling R7)"}
	}
	return nil
}

func requireManagerSurface(actor Actor) error {
	if actor.Surface == SurfaceAgent {
		return &GuardError{Code: GuardManagerSurface, Detail: "manager-only action: agents are workers, not managers (§4.3c)"}
	}
	return nil
}

// taskForUpdate loads a task under row lock inside tx.
func (st *Store) taskForUpdate(ctx context.Context, tx pgx.Tx, taskID string) (*Task, error) {
	return scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks t WHERE t.id=$1 FOR UPDATE`, taskID))
}

func criteriaJSON(c []AcceptanceCriterion) []byte {
	if len(c) == 0 {
		return []byte("[]")
	}
	b, _ := json.Marshal(c)
	return b
}

func tagsOrEmpty(t []string) []string {
	if t == nil {
		return []string{}
	}
	return t
}

// validatePriority normalizes a P0-P3 (default P2).
func validatePriority(p string) (string, error) {
	switch p {
	case "":
		return PriorityP2, nil
	case PriorityP0, PriorityP1, PriorityP2, PriorityP3:
		return p, nil
	default:
		return "", &GuardError{Code: "validation", Detail: "priority must be P0-P3"}
	}
}

// --- Projects -------------------------------------------------------------------

// CreateProject creates a user-level board project (ruling R7).
func (s *Service) CreateProject(ctx context.Context, actor Actor, in ProjectInput, requestID string) (*Project, error) {
	if err := requireManagerSurface(actor); err != nil {
		return nil, err
	}
	if in.Name == "" {
		return nil, &GuardError{Code: "validation", Detail: "project name is required"}
	}
	// R7/R8: the target must be a workspace the owning user can access
	// (v1: the workspace's owning user).
	var wsOwner string
	if err := s.q().QueryRow(ctx, `SELECT user_id FROM workspaces WHERE id=$1`, in.TargetWorkspaceID).Scan(&wsOwner); err != nil {
		return nil, &NotFoundError{Item: "workspace", Identifier: in.TargetWorkspaceID}
	}
	if wsOwner != actor.UserID {
		return nil, &GuardError{Code: GuardWorkspaceBinding, Detail: "target workspace must be accessible to the owning user (rulings R7/R8)"}
	}
	slots := in.WorkerSlots
	if slots <= 0 {
		slots = 2
	}

	// The project id is generated up front so the audit event can carry
	// its (project_id, id) scope key in the same transaction.
	projID := uuid.NewString()
	ew := &eventWriter{svc: s, projectID: projID, requestID: requestID, action: "create_project", itemKind: "project", itemID: projID, actor: actor}
	outcome, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		shortID, err := mintUnique(ctx, txQ{tx}, "projects", PrefixProject, ` AND user_id=$2`, actor.UserID)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO projects (id, short_id, user_id, name, target_workspace_id, worker_slots, tags)
			VALUES ($1, $2, $3, $4, $5::uuid, $6, $7)`,
			projID, shortID, actor.UserID, in.Name, in.TargetWorkspaceID, slots, tagsOrEmpty(in.Tags)); err != nil {
			return nil, err
		}
		return map[string]any{"id": projID, "short_id": shortID}, nil
	})
	if err != nil {
		return nil, err
	}
	_ = dup
	pid, _ := outcome["id"].(string)
	return s.store.resolveProject(ctx, s.q(), actor.UserID, pid)
}

// UpdateProject updates fields; a name rename cascades materialized
// display inheritance (one UPDATE per carrying table, §4.1).
func (s *Service) UpdateProject(ctx context.Context, actor Actor, projectID, shortIDOrID string, in ProjectInput, requestID string) (*Project, error) {
	p, err := s.store.resolveProject(ctx, s.q(), actor.UserID, pick(projectID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	if err := requireOwner(actor, p.UserID); err != nil {
		return nil, err
	}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "update_project", itemKind: "project", itemID: p.ID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		slots := in.WorkerSlots
		if slots <= 0 {
			slots = p.WorkerSlots
		}
		ws := in.TargetWorkspaceID
		if ws == "" {
			ws = p.TargetWorkspaceID
		}
		if ws != p.TargetWorkspaceID {
			var wsOwner string
			if err := tx.QueryRow(ctx, `SELECT user_id FROM workspaces WHERE id=$1`, ws).Scan(&wsOwner); err != nil || wsOwner != actor.UserID {
				return nil, &GuardError{Code: GuardWorkspaceBinding, Detail: "retargeting requires a workspace the owning user can access"}
			}
		}
		name := in.Name
		if name == "" {
			name = p.Name
		}
		if _, err := tx.Exec(ctx, `
			UPDATE projects SET name=$2, target_workspace_id=$3::uuid, worker_slots=$4, tags=$5 WHERE id=$1`,
			p.ID, name, ws, slots, tagsOrEmpty(in.Tags)); err != nil {
			return nil, err
		}
		// Rename cascade: one statement per carrying table (§4.1).
		if name != p.Name {
			if _, err := tx.Exec(ctx, `UPDATE epics SET project_name=$2 WHERE project_id=$1`, p.ID, name); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE tasks SET project_name=$2 WHERE project_id=$1`, p.ID, name); err != nil {
				return nil, err
			}
		}
		return map[string]any{"id": p.ID, "name": name}, nil
	})
	if err != nil {
		return nil, err
	}
	_ = dup
	return s.store.resolveProject(ctx, s.q(), actor.UserID, p.ID)
}

func projectStatusTx(ctx context.Context, tx pgx.Tx, actor Actor, p *Project, to string, action string) error {
	if _, err := tx.Exec(ctx, `UPDATE projects SET status=$2 WHERE id=$1`, p.ID, to); err != nil {
		return err
	}
	return nil
}

// ArchiveProject is reversible storage; it stops all claims into the
// project (the p CTE's status predicate).
func (s *Service) ArchiveProject(ctx context.Context, actor Actor, projectID, requestID string) (*Project, error) {
	return s.projectLifecycle(ctx, actor, projectID, "", ProjectArchived, "archive_project", requestID)
}

// UnarchiveProject restores active.
func (s *Service) UnarchiveProject(ctx context.Context, actor Actor, projectID, requestID string) (*Project, error) {
	return s.projectLifecycle(ctx, actor, projectID, "", ProjectActive, "unarchive_project", requestID)
}

func (s *Service) projectLifecycle(ctx context.Context, actor Actor, projectID, shortIDOrID, to, action, requestID string) (*Project, error) {
	p, err := s.store.resolveProject(ctx, s.q(), actor.UserID, pick(projectID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	if err := requireOwner(actor, p.UserID); err != nil {
		return nil, err
	}
	if err := requireManagerSurface(actor); err != nil {
		return nil, err
	}
	// Replay-first (a caller key that already landed renders the
	// original outcome — the status the original write left behind must
	// not turn a replay into a guard error).
	dup, err := s.replayOf(ctx, p.ID, requestID, action)
	if err != nil {
		return nil, err
	}
	if dup != nil {
		return nil, dup
	}
	if p.Status == to {
		return p, nil
	}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: action, itemKind: "project", itemID: p.ID, actor: actor}
	_, _, err = ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		if err := projectStatusTx(ctx, tx, actor, p, to, action); err != nil {
			return nil, err
		}
		return map[string]any{"id": p.ID, "status": to}, nil
	})
	if err != nil {
		return nil, err
	}
	return s.store.resolveProject(ctx, s.q(), actor.UserID, p.ID)
}

// --- Epics ----------------------------------------------------------------------

// CreateEpic creates a draft epic (the default status per ruling R2).
func (s *Service) CreateEpic(ctx context.Context, actor Actor, projectID, shortIDOrID string, in EpicInput, requestID string) (*Epic, error) {
	p, err := s.store.resolveProject(ctx, s.q(), actor.UserID, pick(projectID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	if err := requireOwner(actor, p.UserID); err != nil {
		return nil, err
	}
	if in.Title == "" {
		return nil, &GuardError{Code: "validation", Detail: "epic title is required"}
	}
	prio, err := validatePriority(in.Priority)
	if err != nil {
		return nil, err
	}
	epicID := uuid.NewString()
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "create_epic", itemKind: "epic", itemID: epicID, actor: actor}
	outcome, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		shortID, err := mintUnique(ctx, txQ{tx}, "epics", PrefixEpic, ` AND project_id=$2`, p.ID)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO epics (id, project_id, short_id, title, outcome, status, priority, project_name, tags, quantum)
			VALUES ($1, $2, $3, $4, $5, 'draft', $6, $7, $8, $9)`,
			epicID, p.ID, shortID, in.Title, in.Outcome, prio, p.Name, tagsOrEmpty(in.Tags), s.cfg.DRRQuantum); err != nil {
			return nil, err
		}
		return map[string]any{"id": epicID, "short_id": shortID}, nil
	})
	if err != nil {
		return nil, err
	}
	_ = dup
	// On replay the outcome's id is the ORIGINAL epic (a fresh uuid was
	// pre-generated this call): resolve from the recorded outcome.
	if oid, _ := outcome["id"].(string); oid != "" {
		return s.GetEpic(ctx, oid, "")
	}
	return s.GetEpic(ctx, epicID, "")
}

// UpdateEpic updates title/outcome/priority/tags (outcome is the human's
// steering wheel, ruling R2). A title rename cascades tasks.epic_name.
func (s *Service) UpdateEpic(ctx context.Context, actor Actor, epicID, shortIDOrID string, in EpicInput, requestID string) (*Epic, error) {
	e, err := s.store.resolveEpic(ctx, s.q(), "", pick(epicID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	p, err := s.store.resolveProject(ctx, s.q(), "", e.ProjectID)
	if err != nil {
		return nil, err
	}
	if err := requireOwner(actor, p.UserID); err != nil {
		return nil, err
	}
	prio, err := validatePriority(in.Priority)
	if err != nil {
		return nil, err
	}
	title := in.Title
	if title == "" {
		title = e.Title
	}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "update_epic", itemKind: "epic", itemID: e.ID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		if _, err := tx.Exec(ctx, `
			UPDATE epics SET title=$2, outcome=$3, priority=$4, tags=$5 WHERE id=$1`,
			e.ID, title, in.Outcome, prio, tagsOrEmpty(in.Tags)); err != nil {
			return nil, err
		}
		if title != e.Title {
			if _, err := tx.Exec(ctx, `UPDATE tasks SET epic_name=$2 WHERE epic_id=$1`, e.ID, title); err != nil {
				return nil, err
			}
		}
		return map[string]any{"id": e.ID}, nil
	})
	if err != nil {
		return nil, err
	}
	_ = dup
	return s.GetEpic(ctx, e.ID, "")
}

// EpicActivate starts feeding. The immediate RC4 re-check applies to
// draft/paused/archived-origin activations (a finished epic completes at
// activation); the completed→active re-open is exempt (r13 pin).
func (s *Service) EpicActivate(ctx context.Context, actor Actor, epicID, shortIDOrID, requestID string) (*Epic, error) {
	return s.epicLifecycle(ctx, actor, epicID, shortIDOrID, EpicActive, "epic_activate", true, requestID)
}

// EpicPause stops new claims, never interrupts in-flight leases.
func (s *Service) EpicPause(ctx context.Context, actor Actor, epicID, shortIDOrID, requestID string) (*Epic, error) {
	return s.epicLifecycle(ctx, actor, epicID, shortIDOrID, EpicPaused, "epic_pause", false, requestID)
}

// EpicArchive is reversible storage.
func (s *Service) EpicArchive(ctx context.Context, actor Actor, epicID, shortIDOrID, requestID string) (*Epic, error) {
	return s.epicLifecycle(ctx, actor, epicID, shortIDOrID, EpicArchived, "epic_archive", false, requestID)
}

// EpicUnarchive always restores active (the resumable default) — and, as
// an archived-origin activation, runs the immediate re-check (r13 pin).
func (s *Service) EpicUnarchive(ctx context.Context, actor Actor, epicID, shortIDOrID, requestID string) (*Epic, error) {
	return s.epicLifecycle(ctx, actor, epicID, shortIDOrID, EpicActive, "epic_unarchive", true, requestID)
}

func (s *Service) epicLifecycle(ctx context.Context, actor Actor, epicID, shortIDOrID, to, action string, recheck bool, requestID string) (*Epic, error) {
	e, err := s.store.resolveEpic(ctx, s.q(), "", pick(epicID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	p, err := s.store.resolveProject(ctx, s.q(), "", e.ProjectID)
	if err != nil {
		return nil, err
	}
	if err := requireOwner(actor, p.UserID); err != nil {
		return nil, err
	}
	if err := requireManagerSurface(actor); err != nil {
		return nil, err
	}
	// Replay-first: the allowed-map below reads the status the ORIGINAL
	// write may have changed (activate's replay sees active→active) — a
	// landed caller key renders the original outcome instead of a guard
	// error.
	dup, err := s.replayOf(ctx, p.ID, requestID, action)
	if err != nil {
		return nil, err
	}
	if dup != nil {
		return nil, dup
	}
	allowed := map[string][]string{
		EpicActive:   {EpicDraft, EpicPaused, EpicCompleted, EpicArchived},
		EpicPaused:   {EpicActive},
		EpicArchived: {EpicDraft, EpicActive, EpicPaused, EpicCompleted},
	}
	ok := false
	for _, from := range allowed[to] {
		if from == e.Status {
			ok = true
		}
	}
	if !ok {
		return nil, &GuardError{Code: GuardEpicState, Detail: fmt.Sprintf("epic %s→%s is not a lifecycle transition (from %s)", e.Status, to, e.Status)}
	}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: action, itemKind: "epic", itemID: e.ID, actor: actor}
	_, dup, err = ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		if _, err := tx.Exec(ctx, `UPDATE epics SET status=$2 WHERE id=$1`, e.ID, to); err != nil {
			return nil, err
		}
		// The immediate re-check (RC4): draft/paused/archived-origin
		// activations of a finished epic complete immediately; the
		// completed→active re-open is exempt (stays active until follow-up
		// work lands and finishes).
		if recheck && e.Status != EpicCompleted {
			if _, err := epicCompletionCheck(ctx, tx, e.ID); err != nil {
				return nil, err
			}
		}
		return map[string]any{"id": e.ID, "status": to}, nil
	})
	if err != nil {
		return nil, err
	}
	_ = dup
	if to == EpicActive {
		// Event-driven drive: activation starts feeding (§4.4).
		s.pokeDrive(p.ID)
	}
	return s.GetEpic(ctx, e.ID, "")
}

// --- Tasks ----------------------------------------------------------------------

// CreateTask creates a backlog task in an epic.
func (s *Service) CreateTask(ctx context.Context, actor Actor, epicID, shortIDOrID string, in TaskInput, requestID string) (*Task, error) {
	e, err := s.store.resolveEpic(ctx, s.q(), "", pick(epicID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	p, err := s.store.resolveProject(ctx, s.q(), "", e.ProjectID)
	if err != nil {
		return nil, err
	}
	if err := requireOwner(actor, p.UserID); err != nil {
		return nil, err
	}
	if in.Title == "" {
		return nil, &GuardError{Code: "validation", Detail: "task title is required"}
	}
	prio, err := validatePriority(in.Priority)
	if err != nil {
		return nil, err
	}
	taskID := uuid.NewString()
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "create_task", itemKind: "task", itemID: taskID, actor: actor}
	outcome, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		shortID, err := mintUnique(ctx, txQ{tx}, "tasks", PrefixTask, ` AND project_id=$2`, p.ID)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO tasks (id, project_id, epic_id, short_id, title, description, priority,
			                   project_name, epic_name, tags, acceptance_criteria)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			taskID, p.ID, e.ID, shortID, in.Title, in.Description, prio,
			p.Name, e.Title, tagsOrEmpty(in.Tags), criteriaJSON(in.AcceptanceCriteria)); err != nil {
			return nil, err
		}
		return map[string]any{"id": taskID, "short_id": shortID}, nil
	})
	if err != nil {
		return nil, err
	}
	_ = dup
	// Event-driven drive: a created backlog task is potential work (the
	// project's epics gate it; the drive's predicates resolve the rest).
	s.pokeDrive(p.ID)
	// On replay the outcome's id is the ORIGINAL task: resolve from the
	// recorded outcome (200-with-original).
	if oid, _ := outcome["id"].(string); oid != "" {
		return s.store.resolveTask(ctx, s.q(), p.ID, oid)
	}
	return s.store.resolveTask(ctx, s.q(), p.ID, taskID)
}

// UpdateTask patches fields; priority changes are ordinary writes (the
// dispatcher reads priority fresh each claim); criteria check-off rides
// here (the reviewer's Done preparation).
func (s *Service) UpdateTask(ctx context.Context, actor Actor, taskID, shortIDOrID string, in TaskInput, requestID string) (*Task, error) {
	t, err := s.store.resolveTask(ctx, s.q(), "", pick(taskID, shortIDOrID))
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
	prio, err := validatePriority(in.Priority)
	if err != nil {
		return nil, err
	}
	title, desc := t.Title, t.Description
	if in.Title != "" {
		title = in.Title
	}
	if in.Description != "" {
		desc = in.Description
	}
	criteria := t.AcceptanceCriteria
	if in.AcceptanceCriteria != nil {
		criteria = in.AcceptanceCriteria
	}
	if in.SetCriteriaMet != nil && *in.SetCriteriaMet >= 0 && *in.SetCriteriaMet < len(criteria) {
		criteria[*in.SetCriteriaMet].Met = true
	}
	tags := t.Tags
	if in.Tags != nil {
		tags = in.Tags
	}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "update_task", itemKind: "task", itemID: t.ID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		if _, err := tx.Exec(ctx, `
			UPDATE tasks SET title=$2, description=$3, priority=$4, tags=$5, acceptance_criteria=$6
			WHERE id=$1`,
			t.ID, title, desc, prio, tagsOrEmpty(tags), criteriaJSON(criteria)); err != nil {
			return nil, err
		}
		return map[string]any{"id": t.ID}, nil
	})
	if err != nil {
		return nil, err
	}
	_ = dup
	return s.store.resolveTask(ctx, s.q(), p.ID, t.ID)
}

// --- Dependencies (rulings R3/R4: the ONLY blocking mechanism) -------------------

// AddDependency adds a structural edge. The DB trigger enforces
// same-project / no-self / acyclic (full DFS); violations map to 409 +
// did-you-mean (R4a). is_blocked is re-derived in-transaction.
func (s *Service) AddDependency(ctx context.Context, actor Actor, taskID, shortIDOrID, dependsOnID, requestID string) error {
	return s.addDependency(ctx, actor, pick(taskID, shortIDOrID), dependsOnID, requestID)
}

func (s *Service) addDependency(ctx context.Context, actor Actor, taskRef, dependsOnID, requestID string) error {
	t, err := s.store.resolveTask(ctx, s.q(), "", taskRef)
	if err != nil {
		return err
	}
	p, err := s.store.resolveProject(ctx, s.q(), "", t.ProjectID)
	if err != nil {
		return err
	}
	if err := requireOwner(actor, p.UserID); err != nil {
		return err
	}
	// The target resolves UNSCOPED: a cross-project target must surface as
	// the trigger's 409-class (R4a), not a scoped 404 — the guard is the
	// authority, authz rides the SOURCE task's project.
	dep, err := s.store.resolveTask(ctx, s.q(), "", dependsOnID)
	if err != nil {
		var nf *NotFoundError
		if errors.As(err, &nf) {
			nf.Suggestions = append(nf.Suggestions, suggestTop3(dependsOnID, s.store.scopeCandidates(ctx, s.q(), "task", p.ID, ""))...)
		}
		return err
	}
	if t.ID == dep.ID {
		return &DependencyError{Code: "self", Detail: "a task cannot depend on itself",
			Suggestions: s.store.scopeCandidates(ctx, q2(s), "task", p.ID, "")}
	}
	// Pre-check cross-project in Go (r1-S2): the DB trigger's RAISE
	// carries both tasks' and projects' UUIDs — identifier disclosure a
	// 409 has no business making. Clean detail here; the trigger remains
	// the race belt via mapDepErr's sanitized mapping.
	if dep.ProjectID != p.ID {
		return &DependencyError{Code: "cross_project",
			Detail: "cross-project dependency rejected (edges live within a project, ruling R4)"}
	}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "add_dependency", itemKind: "task", itemID: t.ID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		if _, err := tx.Exec(ctx, `
			INSERT INTO board_dependencies (task_id, depends_on_task_id, created_by)
			VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, t.ID, dep.ID, actor.UserID); err != nil {
			return nil, mapDepErr(err, p.ID)
		}
		if err := rederiveBlocked(ctx, tx, t.ID); err != nil {
			return nil, err
		}
		return map[string]any{"task_id": t.ID, "depends_on": dep.ID}, nil
	})
	if err != nil {
		return err
	}
	_ = dup
	return nil
}

// mapDepErr translates the BEFORE INSERT trigger's violations into the
// R4a 409 shape with did-you-mean suggestions.
func mapDepErr(err error, projectID string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// Sanitized details only (r1-S2): the trigger's RAISE carries row
		// identifiers — a 409 never discloses them.
		msg := pgErr.Message
		switch {
		case strings.Contains(msg, "cannot depend on itself"):
			return &DependencyError{Code: "self", Detail: "a task cannot depend on itself"}
		case strings.Contains(msg, "cross-project dependency"):
			return &DependencyError{Code: "cross_project",
				Detail: "cross-project dependency rejected (edges live within a project, ruling R4)"}
		case strings.Contains(msg, "would create a cycle"):
			return &DependencyError{Code: "cycle",
				Detail: "dependency edge would create a cycle (ruling R4a)"}
		}
	}
	return err
}

// rederiveBlocked recomputes is_blocked for one task from the edge table
// (§4.1-edges maintenance; RC3 is the sweep-level belt).
func rederiveBlocked(ctx context.Context, q Queryer, taskID string) error {
	_, err := q.Exec(ctx, `
		UPDATE tasks t SET is_blocked = EXISTS(
			SELECT 1 FROM board_dependencies d
			JOIN tasks dep ON dep.id = d.depends_on_task_id
			WHERE d.task_id = t.id AND dep.stage <> 'done')
		WHERE t.id = $1 AND t.stage <> 'cancelled'`, taskID)
	return err
}

// RemoveDependency removes an edge and re-derives is_blocked (one of the
// pending_input resolutions: removing an abandoned prerequisite's edge
// unblocks the row — the human re-queue stays a move_stage write).
func (s *Service) RemoveDependency(ctx context.Context, actor Actor, taskID, shortIDOrID, dependsOnID, requestID string) error {
	t, err := s.store.resolveTask(ctx, s.q(), "", pick(taskID, shortIDOrID))
	if err != nil {
		return err
	}
	p, err := s.store.resolveProject(ctx, s.q(), "", t.ProjectID)
	if err != nil {
		return err
	}
	if err := requireOwner(actor, p.UserID); err != nil {
		return err
	}
	// Resolve the target like AddDependency does (r1-C1): every
	// addressing surface is short-ID-first, and a raw short id against
	// the uuid column would match zero rows and report success.
	dep, err := s.store.resolveTask(ctx, s.q(), p.ID, dependsOnID)
	if err != nil {
		return err
	}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "remove_dependency", itemKind: "task", itemID: t.ID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		if _, err := tx.Exec(ctx, `DELETE FROM board_dependencies WHERE task_id=$1 AND depends_on_task_id=$2`, t.ID, dep.ID); err != nil {
			return nil, err
		}
		if err := rederiveBlocked(ctx, txQ{tx}, t.ID); err != nil {
			return nil, err
		}
		return map[string]any{"task_id": t.ID, "removed": dep.ID}, nil
	})
	if err != nil {
		return err
	}
	_ = dup
	// Event-driven drive: removing an abandoned prerequisite's edge
	// unblocks the row (§4.4's unblocking events).
	s.pokeDrive(p.ID)
	return nil
}

func pick(id, shortIDOrID string) string {
	if id != "" {
		return id
	}
	return shortIDOrID
}

// q2 adapts the service to a Queryer for candidate scoping.
func q2(s *Service) Queryer { return s.q() }

// GetTaskInWorkspace is the agent surface's reader (r1-S1): resolution,
// row, and did-you-mean suggestions all scoped to the caller's workspace.
func (s *Service) GetTaskInWorkspace(ctx context.Context, workspaceID, taskID, shortIDOrID string) (*Task, error) {
	return s.store.resolveTaskInWorkspace(ctx, s.q(), workspaceID, pick(taskID, shortIDOrID))
}
