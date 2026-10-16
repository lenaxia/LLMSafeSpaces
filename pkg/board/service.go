// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Option configures a Service (functional options, constructed at boot).
type Option func(*Service)

// WithFailpoint installs the fault seam (kill-tests).
func WithFailpoint(fp Failpoint) Option {
	return func(s *Service) { s.failpoint = fp }
}

// WithWorkerSpawner installs the fresh-worker spawn seam (ruling R9).
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
// adapter. Safe for concurrent use (pgxpool arbitrates).
type Service struct {
	store     *Store
	cfg       EngineConfig
	failpoint Failpoint
	spawner   WorkerSpawner
	aborter   WorkerAborter
	drift     PRDriftSource
	pending   PendingInputSource
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
	// Ephemeral control for update_task (check-off).
	SetCriteriaMet *int
}

// --- Actions (design §4.2 action set) ----------------------------------------

// CreateProject creates a user-level board project (ruling R7).
func (s *Service) CreateProject(ctx context.Context, actor Actor, in ProjectInput, requestID string) (*Project, error) {
	return nil, ErrNotImplemented
}

// UpdateProject updates fields; a name rename cascades materialized
// display inheritance (one UPDATE per carrying table, §4.1).
func (s *Service) UpdateProject(ctx context.Context, actor Actor, projectID, shortIDOrID string, in ProjectInput, requestID string) (*Project, error) {
	return nil, ErrNotImplemented
}

// ArchiveProject is reversible storage.
func (s *Service) ArchiveProject(ctx context.Context, actor Actor, projectID string, requestID string) (*Project, error) {
	return nil, ErrNotImplemented
}

// UnarchiveProject restores active.
func (s *Service) UnarchiveProject(ctx context.Context, actor Actor, projectID string, requestID string) (*Project, error) {
	return nil, ErrNotImplemented
}

// CreateEpic creates a draft epic (default status per ruling).
func (s *Service) CreateEpic(ctx context.Context, actor Actor, projectID, shortIDOrID string, in EpicInput, requestID string) (*Epic, error) {
	return nil, ErrNotImplemented
}

// UpdateEpic updates title/outcome/priority/tags (outcome is the human's
// steering wheel, ruling R2).
func (s *Service) UpdateEpic(ctx context.Context, actor Actor, epicID, shortIDOrID string, in EpicInput, requestID string) (*Epic, error) {
	return nil, ErrNotImplemented
}

// EpicActivate starts feeding. The immediate RC4 re-check applies to
// draft/paused/archived-origin activations; the completed→active re-open is
// exempt (r13 pin).
func (s *Service) EpicActivate(ctx context.Context, actor Actor, epicID, shortIDOrID string, requestID string) (*Epic, error) {
	return nil, ErrNotImplemented
}

// EpicPause stops new claims, never interrupts in-flight leases.
func (s *Service) EpicPause(ctx context.Context, actor Actor, epicID, shortIDOrID string, requestID string) (*Epic, error) {
	return nil, ErrNotImplemented
}

// EpicArchive is reversible storage.
func (s *Service) EpicArchive(ctx context.Context, actor Actor, epicID, shortIDOrID string, requestID string) (*Epic, error) {
	return nil, ErrNotImplemented
}

// EpicUnarchive always restores active (the resumable default).
func (s *Service) EpicUnarchive(ctx context.Context, actor Actor, epicID, shortIDOrID string, requestID string) (*Epic, error) {
	return nil, ErrNotImplemented
}

// CreateTask creates a backlog task in an epic.
func (s *Service) CreateTask(ctx context.Context, actor Actor, epicID, shortIDOrID string, in TaskInput, requestID string) (*Task, error) {
	return nil, ErrNotImplemented
}

// UpdateTask patches fields; priority changes are ordinary writes (the
// dispatcher reads priority fresh each claim); criteria check-off rides here.
func (s *Service) UpdateTask(ctx context.Context, actor Actor, taskID, shortIDOrID string, in TaskInput, requestID string) (*Task, error) {
	return nil, ErrNotImplemented
}

// AddDependency adds the only blocking edge (rulings R3/R4). Violations
// (cycle/cross-project/self) return DependencyError with suggestions.
func (s *Service) AddDependency(ctx context.Context, actor Actor, taskID, shortIDOrID string, dependsOnID string, requestID string) error {
	return ErrNotImplemented
}

// RemoveDependency removes an edge and re-derives is_blocked.
func (s *Service) RemoveDependency(ctx context.Context, actor Actor, taskID, shortIDOrID string, dependsOnID string, requestID string) error {
	return ErrNotImplemented
}

// MoveStage is the server-arbitrated stage machine write. Every legal
// transition records its board_transitions row; done requires ALL
// acceptance criteria met + evidence_url (ruling R2). The pending_input
// exit is manager-surface-only; the exit resets attempts (the ONE write
// that does).
func (s *Service) MoveStage(ctx context.Context, actor Actor, taskID, shortIDOrID string, toStage, evidenceURL string, requestID string) (*Task, error) {
	return nil, ErrNotImplemented
}

// CancelTask is terminal: revokes lease + assignee, aborts the worker,
// stages non-terminal dependents to pending_input/dependency_abandoned.
// Stage terminal-immutable (RC5: cancel is the only writer; nothing resurrects).
func (s *Service) CancelTask(ctx context.Context, actor Actor, taskID, shortIDOrID string, reason string, requestID string) (*Task, error) {
	return nil, ErrNotImplemented
}

// CancelEpic pauses the epic and cancels its non-terminal tasks (done tasks
// stay done), cascading dependency abandonment.
func (s *Service) CancelEpic(ctx context.Context, actor Actor, epicID, shortIDOrID string, reason string, requestID string) (*Epic, error) {
	return nil, ErrNotImplemented
}

// TaskStart is the manual one-off dispatch (allowed in draft∪active epics;
// paused/completed/archived epics and archived projects refuse). Mirrors
// claim_next exactly (atomic statement, slots cap, workspace binding,
// token minted, attempts incremented, DRR counters advance).
func (s *Service) TaskStart(ctx context.Context, actor Actor, taskID, shortIDOrID string, workerWorkspaceID string, requestID string) (*Claim, error) {
	return nil, ErrNotImplemented
}

// Heartbeat refreshes the lease ONLY when it carries new progress evidence
// (the worker's message-level watermark moved, #1685 semantics). Requires
// the matching claim token.
func (s *Service) Heartbeat(ctx context.Context, actor Actor, taskID, shortIDOrID string, observedProgressAt time.Time) (HeartbeatResult, error) {
	return HeartbeatResult{}, ErrNotImplemented
}

// ReportThrottle is the 429 path: requeue to backlog WITHOUT attempt
// penalty (the increment rolls back iff claim_incremented_attempts) +
// cooldown on both the task and the project's slots.
func (s *Service) ReportThrottle(ctx context.Context, actor Actor, taskID, shortIDOrID string, retryAfter time.Duration, requestID string) (*Task, error) {
	return nil, ErrNotImplemented
}

// ClaimNext is the dispatch-engine entry (worker surface). See engine.go.
func (s *Service) ClaimNext(ctx context.Context, workerID, workspaceID string) (*Claim, error) {
	return nil, ErrNotImplemented
}

// --- Reads (board_read projection) -------------------------------------------

// GetTask resolves a task (case-insensitive short ID or UUID).
func (s *Service) GetTask(ctx context.Context, taskID, shortIDOrID string) (*Task, error) {
	return nil, ErrNotImplemented
}

// ListTasks lists a project's tasks (optionally by stage).
func (s *Service) ListTasks(ctx context.Context, projectID string, stage string) ([]Task, error) {
	return nil, ErrNotImplemented
}

// GetEpic resolves an epic with derived progress.
func (s *Service) GetEpic(ctx context.Context, epicID, shortIDOrID string) (*Epic, error) {
	return nil, ErrNotImplemented
}

// GetProject resolves a project.
func (s *Service) GetProject(ctx context.Context, projectID, shortIDOrID string) (*Project, error) {
	return nil, ErrNotImplemented
}

// ListProjects lists the owner's projects.
func (s *Service) ListProjects(ctx context.Context, userID string) ([]Project, error) {
	return nil, ErrNotImplemented
}
