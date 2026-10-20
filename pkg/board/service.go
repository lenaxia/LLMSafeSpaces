// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Option configures a Service (functional options, constructed at boot).
type Option func(*Service)

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

// WithWorkerAborter installs the abort seam (delete of an in-flight
// claim, superseded-claim cleanup).
func WithWorkerAborter(ab WorkerAborter) Option {
	return func(s *Service) { s.aborter = ab }
}

// WithWorkerMessenger installs the engine→session directive seam
// (#1747 pause-must-pause): halt/resume delivery to in-flight workers
// rides the SAME orchestrator↔worker message machinery the platform
// already uses (the spawner's transport, existing-session spec) — no
// new transport.
func WithWorkerMessenger(m WorkerMessenger) Option {
	return func(s *Service) { s.messenger = m }
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
	// messenger delivers halt/resume directives to in-flight workers
	// (#1747 pause-must-pause). Nil = no delivery (the flags stay
	// authoritative; tests pin the nil-seam behavior).
	messenger WorkerMessenger
	// pressureMu guards the last resource-pressure sample per workspace
	// (#1747 round 3): agentd pushes over the seam; the claim choke
	// point skips pressured workspaces; reads surface the note.
	pressureMu sync.RWMutex
	pressure   map[string]PressureSample
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
	if cfg.UserClaimCap <= 0 {
		cfg.UserClaimCap = 8
	}
	if cfg.GlobalClaimCap <= 0 {
		cfg.GlobalClaimCap = 64
	}
	if cfg.MaxEdgesPerNode <= 0 {
		cfg.MaxEdgesPerNode = 16
	}
	if cfg.MaxNodesPerEpic <= 0 {
		cfg.MaxNodesPerEpic = 64
	}
	if cfg.PressureSampleTTL <= 0 {
		cfg.PressureSampleTTL = 2 * time.Minute
	}
	s := &Service{store: NewStore(pool), cfg: cfg, pressure: map[string]PressureSample{}}
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
	Title   string
	Outcome string
	Tags    []string
}

// TaskInput is the create/update payload for tasks. DependsOn (create
// only, #1747) names prerequisite tasks by short id or UUID — the edges
// are added in the SAME transaction (a bad ref rolls the create back).
type TaskInput struct {
	Title              string
	Description        string
	Tags               []string
	AcceptanceCriteria []AcceptanceCriterion
	// SetCriteriaMet checks off the criterion at this 0-based index.
	SetCriteriaMet *int
	DependsOn      []string
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

// requireManagerAction enforces the #1747 manager-action scope on the
// agent surface: the acting pod's resolved workspace must be the
// project's target workspace (§4.4 caller-workspace binding extended to
// manager rows — a pod manages ITS OWN workspace's board only). Manager
// surfaces (REST/external MCP) scope by ownership alone (requireOwner,
// R7); the engine never routes here.
func requireManagerAction(actor Actor, p *Project) error {
	if actor.Surface != SurfaceAgent {
		return nil
	}
	if actor.WorkspaceID == "" || actor.WorkspaceID != p.TargetWorkspaceID {
		return &GuardError{Code: GuardWorkspaceBinding, Detail: "agent manager actions are workspace-scoped: the project must target the caller's workspace (ruling #1747)"}
	}
	return nil
}

// requireAgentWorkspaceTarget enforces the binding on CREATE (no project
// row exists yet): on the agent surface the payload's target workspace
// must be the caller's resolved workspace.
func requireAgentWorkspaceTarget(actor Actor, targetWorkspaceID string) error {
	if actor.Surface != SurfaceAgent {
		return nil
	}
	if actor.WorkspaceID == "" || actor.WorkspaceID != targetWorkspaceID {
		return &GuardError{Code: GuardWorkspaceBinding, Detail: "agent manager actions are workspace-scoped: a project created here targets the caller's workspace only (ruling #1747)"}
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

// --- Projects -------------------------------------------------------------------

// CreateProject creates a user-level board project (ruling R7). On the
// agent surface the target workspace is bound to the caller's resolved
// workspace (#1747 — structural, no pod creates boards for another
// tenant's workspace).
func (s *Service) CreateProject(ctx context.Context, actor Actor, in ProjectInput, requestID string) (*Project, error) {
	if err := requireAgentWorkspaceTarget(actor, in.TargetWorkspaceID); err != nil {
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
			// #1747: the agent surface never retargets — a pod's board
			// targets its own workspace structurally.
			if actor.Surface == SurfaceAgent {
				return nil, &GuardError{Code: GuardWorkspaceBinding, Detail: "agent manager actions cannot retarget a project's workspace (ruling #1747)"}
			}
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
	return s.projectLifecycle(ctx, actor, projectID, "", ProjectArchived, "archive_project")
}

// UnarchiveProject restores active.
func (s *Service) UnarchiveProject(ctx context.Context, actor Actor, projectID, requestID string) (*Project, error) {
	return s.projectLifecycle(ctx, actor, projectID, "", ProjectActive, "unarchive_project")
}

func (s *Service) projectLifecycle(ctx context.Context, actor Actor, projectID, shortIDOrID, to, action string) (*Project, error) {
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
	if p.Status == to {
		return p, nil
	}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: "", action: action, itemKind: "project", itemID: p.ID, actor: actor}
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
	if err := requireManagerAction(actor, p); err != nil {
		return nil, err
	}
	if in.Title == "" {
		return nil, &GuardError{Code: "validation", Detail: "epic title is required"}
	}
	epicID := uuid.NewString()
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "create_epic", itemKind: "epic", itemID: epicID, actor: actor}
	outcome, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		shortID, err := mintUnique(ctx, txQ{tx}, "epics", PrefixEpic, ` AND project_id=$2`, p.ID)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO epics (id, project_id, short_id, title, outcome, status, project_name, tags, quantum)
			VALUES ($1, $2, $3, $4, $5, 'draft', $6, $7, $8)`,
			epicID, p.ID, shortID, in.Title, in.Outcome, p.Name, tagsOrEmpty(in.Tags), s.cfg.DRRQuantum); err != nil {
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

// UpdateEpic updates title/outcome/tags (outcome is the human's
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
	if err := requireManagerAction(actor, p); err != nil {
		return nil, err
	}
	title := in.Title
	if title == "" {
		title = e.Title
	}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "update_epic", itemKind: "epic", itemID: e.ID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		if _, err := tx.Exec(ctx, `
			UPDATE epics SET title=$2, outcome=$3, tags=$4 WHERE id=$1`,
			e.ID, title, in.Outcome, tagsOrEmpty(in.Tags)); err != nil {
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
// #1747: activation refuses cross-epic edges into INACTIVE epics unless
// acknowledged (structurally-broken subgraph guard) and resumes any
// in-flight workers paused under the epic (the pause-must-pause lever).
func (s *Service) EpicActivate(ctx context.Context, actor Actor, epicID, shortIDOrID string, ackInactiveDeps bool, requestID string) (*Epic, error) {
	e, err := s.store.resolveEpic(ctx, s.q(), "", pick(epicID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	if !ackInactiveDeps {
		n, err := s.countInactiveEpicDeps(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			return nil, &GuardError{Code: GuardEpicState, Detail: fmt.Sprintf("%d cross-epic dependenc(ies) enter inactive epics — acknowledge (acknowledgeInactiveDeps) or resolve them first", n)}
		}
	}
	e, err = s.epicLifecycle(ctx, actor, epicID, shortIDOrID, EpicActive, "epic_activate", true)
	if err != nil {
		return nil, err
	}
	// Resume directives: in-flight workers held under the epic's pause
	// continue (post-commit, best-effort — the flag is authoritative).
	s.deliverToEpicWorkers(ctx, e.ID, DirectiveResume)
	return e, nil
}

// countInactiveEpicDeps counts the epic's dependency edges whose
// prerequisite task lives in an inactive (draft/paused/archived) epic —
// the structurally-broken-subgraph predicate for epic_activate (#1747).
func (s *Service) countInactiveEpicDeps(ctx context.Context, epicID string) (int, error) {
	var n int
	err := s.q().QueryRow(ctx, `
		SELECT count(*) FROM board_dependencies d
		JOIN tasks t ON t.id = d.task_id AND t.epic_id = $1
		JOIN tasks dep ON dep.id = d.depends_on_task_id
		JOIN epics de ON de.id = dep.epic_id
		WHERE de.status IN ('draft','paused','archived')`, epicID).Scan(&n)
	return n, err
}

// EpicInitialReadySet renders the epic's initial ready set (backlog,
// unblocked, unpaused, not cooling down) — epic_activate's report and
// import_plan's plan-approval render share it (#1747).
func (s *Service) EpicInitialReadySet(ctx context.Context, epicID string) ([]string, error) {
	rows, err := s.q().Query(ctx, `
		SELECT t.short_id FROM tasks t
		WHERE t.epic_id = $1 AND t.stage = 'backlog'
		  AND NOT t.is_blocked AND NOT t.paused
		  AND (t.cooldown_until IS NULL OR t.cooldown_until < now())
		ORDER BY t.created_at, t.id`, epicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// EpicPause stops new claims, never interrupts in-flight leases — and,
// per #1747's pause-must-pause ruling, delivers HALT directives to every
// in-flight worker under the epic (they land at the current turn's
// boundary; forward writes refuse while the epic is paused).
func (s *Service) EpicPause(ctx context.Context, actor Actor, epicID, shortIDOrID, requestID string) (*Epic, error) {
	e, err := s.epicLifecycle(ctx, actor, epicID, shortIDOrID, EpicPaused, "epic_pause", false)
	if err != nil {
		return nil, err
	}
	s.deliverToEpicWorkers(ctx, e.ID, DirectiveHalt)
	return e, nil
}

// EpicArchive is reversible storage.
func (s *Service) EpicArchive(ctx context.Context, actor Actor, epicID, shortIDOrID, requestID string) (*Epic, error) {
	return s.epicLifecycle(ctx, actor, epicID, shortIDOrID, EpicArchived, "epic_archive", false)
}

// EpicUnarchive always restores active (the resumable default) — and, as
// an archived-origin activation, runs the immediate re-check (r13 pin).
func (s *Service) EpicUnarchive(ctx context.Context, actor Actor, epicID, shortIDOrID, requestID string) (*Epic, error) {
	return s.epicLifecycle(ctx, actor, epicID, shortIDOrID, EpicActive, "epic_unarchive", true)
}

func (s *Service) epicLifecycle(ctx context.Context, actor Actor, epicID, shortIDOrID, to, action string, recheck bool) (*Epic, error) {
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
	if err := requireManagerAction(actor, p); err != nil {
		return nil, err
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
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: "", action: action, itemKind: "epic", itemID: e.ID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
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
	if err := requireManagerAction(actor, p); err != nil {
		return nil, err
	}
	if in.Title == "" {
		return nil, &GuardError{Code: "validation", Detail: "task title is required"}
	}
	// #1747 DAG blast-radius bound: nodes per epic.
	var nodes int
	if err := s.q().QueryRow(ctx, `SELECT count(*) FROM tasks WHERE epic_id=$1`, e.ID).Scan(&nodes); err != nil {
		return nil, err
	}
	if nodes >= s.cfg.MaxNodesPerEpic {
		return nil, &GuardError{Code: GuardGraphLimit, Detail: fmt.Sprintf("epic holds %d tasks (max %d) — the blast-radius bound refuses more", nodes, s.cfg.MaxNodesPerEpic)}
	}
	// Inline dependsOn (#1747): the edges are added in the create's OWN
	// transaction (a bad ref or guard rolls the create back).
	deps, err := s.resolveDepRefs(ctx, p, in.DependsOn)
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
			INSERT INTO tasks (id, project_id, epic_id, short_id, title, description,
			                   project_name, epic_name, tags, acceptance_criteria)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			taskID, p.ID, e.ID, shortID, in.Title, in.Description,
			p.Name, e.Title, tagsOrEmpty(in.Tags), criteriaJSON(in.AcceptanceCriteria)); err != nil {
			return nil, err
		}
		for _, dep := range deps {
			if err := s.insertDependencyTx(ctx, tx, p, taskID, dep); err != nil {
				return nil, err
			}
		}
		if err := rederiveBlocked(ctx, txQ{tx}, taskID); err != nil {
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

// UpdateTask patches fields; criteria check-off rides here (the
// reviewer's Done preparation).
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
	if err := requireManagerAction(actor, p); err != nil {
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
			UPDATE tasks SET title=$2, description=$3, tags=$4, acceptance_criteria=$5
			WHERE id=$1`,
			t.ID, title, desc, tagsOrEmpty(tags), criteriaJSON(criteria)); err != nil {
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
	if err := requireManagerAction(actor, p); err != nil {
		return err
	}
	// The target resolves UNSCOPED: a cross-workspace target must surface
	// as the trigger's 409-class (R4a), not a scoped 404 — the guard is
	// the authority, authz rides the SOURCE task's project.
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
	// #1747 DAG spec: edges may cross epics (and projects) WITHIN a
	// workspace, never across workspaces. Pre-check in Go (r1-S2): the DB
	// trigger's RAISE carries identifiers a 409 has no business
	// disclosing; the trigger remains the race belt via mapDepErr.
	if err := s.checkEdgeWorkspace(ctx, p, dep); err != nil {
		return err
	}
	// #1747 blast-radius bound: max edges per node.
	if err := s.checkEdgeCaps(ctx, t.ID, dep.ID); err != nil {
		return err
	}
	ew := &eventWriter{svc: s, projectID: p.ID, requestID: requestID, action: "add_dependency", itemKind: "task", itemID: t.ID, actor: actor}
	_, dup, err := ew.run(ctx, func(tx pgx.Tx) (map[string]any, error) {
		if err := s.insertDependencyTx(ctx, tx, p, t.ID, dep); err != nil {
			return nil, err
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

// insertDependencyTx adds one edge inside an open transaction (the
// shared writer for add_dependency, create_task's inline dependsOn, and
// delete_task's reconnect) — duplicate edges are an idempotent no-op
// (ON CONFLICT DO NOTHING, #1747), the DB trigger is the cycle/shape
// belt, mapDepErr sanitizes its violations.
func (s *Service) insertDependencyTx(ctx context.Context, tx pgx.Tx, p *Project, taskID string, dep *Task) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO board_dependencies (task_id, depends_on_task_id, created_by)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, taskID, dep.ID, p.UserID); err != nil {
		return mapDepErr(err, p.ID)
	}
	return nil
}

// checkEdgeWorkspace enforces "cross-epic within a workspace, never
// cross-workspace" (#1747): the dep task's project must target the SAME
// workspace as the source's.
func (s *Service) checkEdgeWorkspace(ctx context.Context, source *Project, dep *Task) error {
	var depWS string
	if err := s.q().QueryRow(ctx, `SELECT target_workspace_id::text FROM projects WHERE id=$1`, dep.ProjectID).Scan(&depWS); err != nil {
		return err
	}
	if depWS != source.TargetWorkspaceID {
		return &DependencyError{Code: "cross_workspace",
			Detail: "cross-workspace dependency rejected (edges live within a workspace, #1747)"}
	}
	return nil
}

// checkEdgeCaps enforces the per-node edge bound (#1747: 16 default) —
// counted over BOTH directions of each node.
func (s *Service) checkEdgeCaps(ctx context.Context, sourceID, depID string) error {
	var srcEdges, depEdges int
	if err := s.q().QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM board_dependencies WHERE task_id=$1 OR depends_on_task_id=$1),
		  (SELECT count(*) FROM board_dependencies WHERE task_id=$2 OR depends_on_task_id=$2)`,
		sourceID, depID).Scan(&srcEdges, &depEdges); err != nil {
		return err
	}
	if srcEdges >= s.cfg.MaxEdgesPerNode || depEdges >= s.cfg.MaxEdgesPerNode {
		return &GuardError{Code: GuardGraphLimit, Detail: fmt.Sprintf("edge cap reached (max %d per node) — the blast-radius bound refuses more", s.cfg.MaxEdgesPerNode)}
	}
	return nil
}

// resolveDepRefs resolves inline dependsOn references (short id or UUID)
// for create_task/import_plan: every ref must resolve to a task in the
// same workspace; self/cycle guards ride the writers.
func (s *Service) resolveDepRefs(ctx context.Context, p *Project, refs []string) ([]*Task, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	out := make([]*Task, 0, len(refs))
	for _, ref := range refs {
		dep, err := s.store.resolveTask(ctx, s.q(), "", ref)
		if err != nil {
			return nil, err
		}
		if err := s.checkEdgeWorkspace(ctx, p, dep); err != nil {
			return nil, err
		}
		out = append(out, dep)
	}
	return out, nil
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
		case strings.Contains(msg, "cross-workspace dependency"):
			return &DependencyError{Code: "cross_workspace",
				Detail: "cross-workspace dependency rejected (edges live within a workspace, #1747)"}
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
		WHERE t.id = $1`, taskID)
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
	if err := requireManagerAction(actor, p); err != nil {
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
	t, err := s.store.resolveTaskInWorkspace(ctx, s.q(), workspaceID, pick(taskID, shortIDOrID))
	if err != nil {
		return nil, err
	}
	s.decorateTask(ctx, t, nil)
	return t, nil
}

// inFlightCountsForProject feeds the #1747 concurrency caps at the
// claim choke point: the project's owner (R7 — the per-USER cap is
// per-board-owner), the owner's and the instance's in_progress counts,
// and the project's target workspace (the pressure gate's key). It
// lives in service.go, NOT engine.go/reconciler.go: the engine and
// reconciler are ownership-agnostic by construction (§4.7 forward-compat
// note, source-pinned by TestKill_SourceLevelOwnershipPins) — the
// owner column is this helper's business alone.
func (s *Service) inFlightCountsForProject(ctx context.Context, tx pgx.Tx, projectID string) (ConcurrencyCounts, string, error) {
	var targetWS string
	var c ConcurrencyCounts
	err := tx.QueryRow(ctx, `
		SELECT p.target_workspace_id::text,
		  (SELECT count(*) FROM tasks ti JOIN projects pi ON pi.id = ti.project_id
		   WHERE ti.stage='in_progress' AND pi.user_id = p.user_id),
		  (SELECT count(*) FROM tasks WHERE stage='in_progress')
		FROM projects p WHERE p.id = $1`, projectID).Scan(&targetWS, &c.UserInFlight, &c.GlobalInFlight)
	return c, targetWS, err
}
