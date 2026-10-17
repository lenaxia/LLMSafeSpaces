// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

// board.go — Task Board service (#1699, design 0067 §4.3a): the owner's
// manager surface at /me/projects, /me/epics, /me/tasks. Row encoding is
// the engine's canonical JSON (snake_case short_id etc.), identical to
// the internal automation seam's rendering.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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

func (s *BoardService) ListProjects(ctx context.Context) ([]BoardProject, error) {
	var resp struct {
		Projects []BoardProject `json:"projects"`
	}
	if err := s.c.do(ctx, http.MethodGet, "/me/projects", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Projects, nil
}

func (s *BoardService) GetProject(ctx context.Context, id string) (*BoardProject, error) {
	var p BoardProject
	if err := s.c.do(ctx, http.MethodGet, fmt.Sprintf("/me/projects/%s", id), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *BoardService) CreateProject(ctx context.Context, req BoardProjectInput) (*BoardProject, error) {
	var p BoardProject
	if err := s.c.do(ctx, http.MethodPost, "/me/projects", req, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *BoardService) UpdateProject(ctx context.Context, id string, req BoardProjectInput) (*BoardProject, error) {
	var p BoardProject
	if err := s.c.do(ctx, http.MethodPatch, fmt.Sprintf("/me/projects/%s", id), req, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *BoardService) ArchiveProject(ctx context.Context, id string) (*BoardProject, error) {
	var p BoardProject
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/projects/%s/archive", id), struct{}{}, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *BoardService) UnarchiveProject(ctx context.Context, id string) (*BoardProject, error) {
	var p BoardProject
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/projects/%s/unarchive", id), struct{}{}, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *BoardService) ListProjectTasks(ctx context.Context, projectID, stage string) ([]BoardTask, error) {
	path := fmt.Sprintf("/me/projects/%s/tasks", projectID)
	if stage != "" {
		path += "?stage=" + url.QueryEscape(stage)
	}
	var resp struct {
		Tasks []BoardTask `json:"tasks"`
	}
	if err := s.c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Tasks, nil
}

func (s *BoardService) CreateEpic(ctx context.Context, projectID string, req BoardEpicInput) (*BoardEpic, error) {
	var e BoardEpic
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/projects/%s/epics", projectID), req, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *BoardService) GetEpic(ctx context.Context, id string) (*BoardEpic, error) {
	var e BoardEpic
	if err := s.c.do(ctx, http.MethodGet, fmt.Sprintf("/me/epics/%s", id), nil, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *BoardService) UpdateEpic(ctx context.Context, id string, req BoardEpicInput) (*BoardEpic, error) {
	var e BoardEpic
	if err := s.c.do(ctx, http.MethodPatch, fmt.Sprintf("/me/epics/%s", id), req, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *BoardService) epicLifecycle(ctx context.Context, id, verb string) (*BoardEpic, error) {
	var e BoardEpic
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/epics/%s/%s", id, verb), struct{}{}, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *BoardService) EpicActivate(ctx context.Context, id string) (*BoardEpic, error) {
	return s.epicLifecycle(ctx, id, "activate")
}

func (s *BoardService) EpicPause(ctx context.Context, id string) (*BoardEpic, error) {
	return s.epicLifecycle(ctx, id, "pause")
}

func (s *BoardService) EpicArchive(ctx context.Context, id string) (*BoardEpic, error) {
	return s.epicLifecycle(ctx, id, "archive")
}

func (s *BoardService) EpicUnarchive(ctx context.Context, id string) (*BoardEpic, error) {
	return s.epicLifecycle(ctx, id, "unarchive")
}

func (s *BoardService) CancelEpic(ctx context.Context, id string, req BoardCancelInput) (*BoardEpic, error) {
	var e BoardEpic
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/epics/%s/cancel", id), req, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *BoardService) CreateTask(ctx context.Context, epicID string, req BoardTaskInput) (*BoardTask, error) {
	var t BoardTask
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/epics/%s/tasks", epicID), req, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *BoardService) GetTask(ctx context.Context, id string) (*BoardTask, error) {
	var t BoardTask
	if err := s.c.do(ctx, http.MethodGet, fmt.Sprintf("/me/tasks/%s", id), nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *BoardService) UpdateTask(ctx context.Context, id string, req BoardTaskInput) (*BoardTask, error) {
	var t BoardTask
	if err := s.c.do(ctx, http.MethodPatch, fmt.Sprintf("/me/tasks/%s", id), req, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *BoardService) MoveStage(ctx context.Context, id string, req BoardMoveStageInput) (*BoardTask, error) {
	var t BoardTask
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/tasks/%s/move-stage", id), req, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *BoardService) StartTask(ctx context.Context, id string, req BoardCancelInput) (*BoardClaim, error) {
	var cl BoardClaim
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/tasks/%s/start", id), req, &cl); err != nil {
		return nil, err
	}
	return &cl, nil
}

func (s *BoardService) CancelTask(ctx context.Context, id string, req BoardCancelInput) (*BoardTask, error) {
	var t BoardTask
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/tasks/%s/cancel", id), req, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *BoardService) AddDependency(ctx context.Context, id string, req BoardDependencyInput) (*BoardDependencyMutation, error) {
	var m BoardDependencyMutation
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/tasks/%s/dependencies", id), req, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *BoardService) RemoveDependency(ctx context.Context, id, dependsOn string) (*BoardDependencyMutation, error) {
	var m BoardDependencyMutation
	if err := s.c.do(ctx, http.MethodDelete, fmt.Sprintf("/me/tasks/%s/dependencies/%s", id, dependsOn), nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
