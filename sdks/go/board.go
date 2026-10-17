// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

// board.go — Task Board service (#1699, design 0067 §4.3a): the owner's
// manager surface at /me/projects, /me/epics, /me/tasks. Row encoding is
// the engine's canonical JSON (snake_case short_id etc.), identical to
// the internal automation seam's rendering.

import (
	"context"
	"errors"
)

// Board lifecycle / stage vocabulary (rulings R2/R1).
const (
	BoardProjectActive   = "active"
	BoardProjectArchived = "archived"

	BoardEpicDraft     = "draft"
	BoardEpicActive    = "active"
	BoardEpicPaused    = "paused"
	BoardEpicCompleted = "completed"
	BoardEpicArchived  = "archived"

	BoardStageBacklog      = "backlog"
	BoardStageInProgress   = "in_progress"
	BoardStageInReview     = "in_review"
	BoardStageDone         = "done"
	BoardStagePendingInput = "pending_input"
	BoardStageCancelled    = "cancelled"
)

// ErrBoardNotImplemented is the red-first lane stub (filled by the
// projection impl commit).
var ErrBoardNotImplemented = errors.New("board: not implemented")

// BoardProject is a board project row (swimlane; user-level per R7,
// single target workspace per R8).
type BoardProject struct {
	ID                string   `json:"id"`
	ShortID           string   `json:"short_id"`
	UserID            string   `json:"user_id"`
	Name              string   `json:"name"`
	TargetWorkspaceID string   `json:"target_workspace_id"`
	WorkerSlots       int      `json:"worker_slots"`
	Tags              []string `json:"tags"`
	Status            string   `json:"status"`
	CreatedAt         string   `json:"created_at"`
	UpdatedAt         string   `json:"updated_at"`
}

// BoardEpic is an epic row. Progress is DERIVED (ruling R2 — never
// stored): Done over (total − Cancelled); ProgressDerived false means
// no-progress ("—"), never 0/0.
type BoardEpic struct {
	ID        string   `json:"id"`
	ProjectID string   `json:"project_id"`
	ShortID   string   `json:"short_id"`
	Title     string   `json:"title"`
	Outcome   string   `json:"outcome"`
	Status    string   `json:"status"`
	Priority  string   `json:"priority"`
	Tags      []string `json:"tags"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`

	ProgressDone    int  `json:"progress_done"`
	ProgressTotal   int  `json:"progress_total"`
	ProgressDerived bool `json:"progress_derived"`
}

// BoardAcceptanceCriterion is one checkable criterion (ruling R2:
// Done = ALL criteria met + evidence; criteria never block).
type BoardAcceptanceCriterion struct {
	Text string `json:"text"`
	Met  bool   `json:"met"`
}

// BoardTask is a task row, including the dispatch state the health
// panel surfaces (attempts, lease, blocked-by).
type BoardTask struct {
	ID                 string                     `json:"id"`
	ProjectID          string                     `json:"project_id"`
	EpicID             string                     `json:"epic_id"`
	ShortID            string                     `json:"short_id"`
	Title              string                     `json:"title"`
	Description        string                     `json:"description"`
	Stage              string                     `json:"stage"`
	PendingOrigin      string                     `json:"pending_origin,omitempty"`
	Priority           string                     `json:"priority"`
	Tags               []string                   `json:"tags"`
	AcceptanceCriteria []BoardAcceptanceCriterion `json:"acceptance_criteria"`
	IsBlocked          bool                       `json:"is_blocked"`
	BlockedBy          []string                   `json:"blocked_by,omitempty"`
	Attempts           int                        `json:"attempts"`
	StageEnteredAt     string                     `json:"stage_entered_at"`
	CreatedAt          string                     `json:"created_at"`
	UpdatedAt          string                     `json:"updated_at"`
}

// BoardClaim is the task_start dispatch outcome (the fresh worker's
// claim envelope; Escalated true means the grant was withheld —
// attempts cap reached, staged to pending_input instead).
type BoardClaim struct {
	TaskID         string     `json:"task_id"`
	ShortID        string     `json:"short_id"`
	ProjectID      string     `json:"project_id"`
	EpicID         string     `json:"epic_id"`
	ClaimToken     string     `json:"claim_token,omitempty"`
	LeaseExpiresAt string     `json:"lease_expires_at,omitempty"`
	Escalated      bool       `json:"escalated"`
	Task           *BoardTask `json:"task,omitempty"`
}

// BoardProjectInput is the create/patch body for projects.
type BoardProjectInput struct {
	Name              string   `json:"name"`
	TargetWorkspaceID string   `json:"targetWorkspaceId,omitempty"`
	WorkerSlots       int      `json:"workerSlots,omitempty"`
	Tags              []string `json:"tags,omitempty"`
	RequestID         string   `json:"requestId,omitempty"`
}

// BoardEpicInput is the create/patch body for epics.
type BoardEpicInput struct {
	Title     string   `json:"title,omitempty"`
	Outcome   string   `json:"outcome,omitempty"`
	Priority  string   `json:"priority,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	RequestID string   `json:"requestId,omitempty"`
}

// BoardTaskInput is the create/patch body for tasks.
type BoardTaskInput struct {
	Title              string                     `json:"title,omitempty"`
	Description        string                     `json:"description,omitempty"`
	Priority           string                     `json:"priority,omitempty"`
	Tags               []string                   `json:"tags,omitempty"`
	AcceptanceCriteria []BoardAcceptanceCriterion `json:"acceptanceCriteria,omitempty"`
	SetCriteriaMet     *int                       `json:"setCriteriaMet,omitempty"`
	RequestID          string                     `json:"requestId,omitempty"`
}

// BoardMoveStageInput is the move-stage body.
type BoardMoveStageInput struct {
	ToStage     string `json:"toStage"`
	EvidenceURL string `json:"evidenceUrl,omitempty"`
	RequestID   string `json:"requestId,omitempty"`
}

// BoardCancelInput is the cancel body (task or epic).
type BoardCancelInput struct {
	Reason    string `json:"reason,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// BoardDependencyInput is the add-dependency body (structural edges are
// the only blocking mechanism, rulings R3/R4).
type BoardDependencyInput struct {
	DependsOn string `json:"dependsOn"`
	RequestID string `json:"requestId,omitempty"`
}

// BoardDependencyMutation renders the re-derived task after an edge
// add/remove (is_blocked converges in the same read).
type BoardDependencyMutation struct {
	Task *BoardTask `json:"task"`
}

// BoardService manages the owner's boards.
type BoardService struct{ c *Client }

// ListProjects lists the caller's boards.
func (s *BoardService) ListProjects(ctx context.Context) ([]BoardProject, error) {
	return nil, ErrBoardNotImplemented
}

// GetProject resolves a project by UUID or short id (case-insensitive).
func (s *BoardService) GetProject(ctx context.Context, id string) (*BoardProject, error) {
	return nil, ErrBoardNotImplemented
}

// CreateProject creates a board (name + target workspace, R7/R8).
func (s *BoardService) CreateProject(ctx context.Context, req BoardProjectInput) (*BoardProject, error) {
	return nil, ErrBoardNotImplemented
}

// UpdateProject patches a project (tags replace — send the full set).
func (s *BoardService) UpdateProject(ctx context.Context, id string, req BoardProjectInput) (*BoardProject, error) {
	return nil, ErrBoardNotImplemented
}

// ArchiveProject is reversible storage (stops all claims into the project).
func (s *BoardService) ArchiveProject(ctx context.Context, id string) (*BoardProject, error) {
	return nil, ErrBoardNotImplemented
}

// UnarchiveProject restores active.
func (s *BoardService) UnarchiveProject(ctx context.Context, id string) (*BoardProject, error) {
	return nil, ErrBoardNotImplemented
}

// ListProjectTasks lists a project's tasks, optionally filtered by one
// of the six stages.
func (s *BoardService) ListProjectTasks(ctx context.Context, projectID, stage string) ([]BoardTask, error) {
	return nil, ErrBoardNotImplemented
}

// CreateEpic creates a draft epic in a project.
func (s *BoardService) CreateEpic(ctx context.Context, projectID string, req BoardEpicInput) (*BoardEpic, error) {
	return nil, ErrBoardNotImplemented
}

// GetEpic resolves an epic with derived progress.
func (s *BoardService) GetEpic(ctx context.Context, id string) (*BoardEpic, error) {
	return nil, ErrBoardNotImplemented
}

// UpdateEpic patches an epic (outcome is the steering wheel).
func (s *BoardService) UpdateEpic(ctx context.Context, id string, req BoardEpicInput) (*BoardEpic, error) {
	return nil, ErrBoardNotImplemented
}

// EpicActivate starts feeding work to workers.
func (s *BoardService) EpicActivate(ctx context.Context, id string) (*BoardEpic, error) {
	return nil, ErrBoardNotImplemented
}

// EpicPause stops new claims, never interrupts in-flight leases.
func (s *BoardService) EpicPause(ctx context.Context, id string) (*BoardEpic, error) {
	return nil, ErrBoardNotImplemented
}

// EpicArchive is reversible storage.
func (s *BoardService) EpicArchive(ctx context.Context, id string) (*BoardEpic, error) {
	return nil, ErrBoardNotImplemented
}

// EpicUnarchive always restores active (the resumable default).
func (s *BoardService) EpicUnarchive(ctx context.Context, id string) (*BoardEpic, error) {
	return nil, ErrBoardNotImplemented
}

// CancelEpic cascades to tasks; the epic lands paused (no cancelled
// epic value exists, ruling R2).
func (s *BoardService) CancelEpic(ctx context.Context, id string, req BoardCancelInput) (*BoardEpic, error) {
	return nil, ErrBoardNotImplemented
}

// CreateTask creates a backlog task in an epic.
func (s *BoardService) CreateTask(ctx context.Context, epicID string, req BoardTaskInput) (*BoardTask, error) {
	return nil, ErrBoardNotImplemented
}

// GetTask resolves a task (with blocked-by derivation).
func (s *BoardService) GetTask(ctx context.Context, id string) (*BoardTask, error) {
	return nil, ErrBoardNotImplemented
}

// UpdateTask patches a task (criteria check-off rides here).
func (s *BoardService) UpdateTask(ctx context.Context, id string, req BoardTaskInput) (*BoardTask, error) {
	return nil, ErrBoardNotImplemented
}

// MoveStage is the server-arbitrated stage write.
func (s *BoardService) MoveStage(ctx context.Context, id string, req BoardMoveStageInput) (*BoardTask, error) {
	return nil, ErrBoardNotImplemented
}

// StartTask is the manual one-off dispatch (task_start).
func (s *BoardService) StartTask(ctx context.Context, id string, req BoardCancelInput) (*BoardClaim, error) {
	return nil, ErrBoardNotImplemented
}

// CancelTask is the terminal write (revokes lease, stages dependents).
func (s *BoardService) CancelTask(ctx context.Context, id string, req BoardCancelInput) (*BoardTask, error) {
	return nil, ErrBoardNotImplemented
}

// AddDependency adds a structural edge, rendering the re-derived task.
func (s *BoardService) AddDependency(ctx context.Context, id string, req BoardDependencyInput) (*BoardDependencyMutation, error) {
	return nil, ErrBoardNotImplemented
}

// RemoveDependency removes an edge, rendering the re-derived task.
func (s *BoardService) RemoveDependency(ctx context.Context, id, dependsOn string) (*BoardDependencyMutation, error) {
	return nil, ErrBoardNotImplemented
}
