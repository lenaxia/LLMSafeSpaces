// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

// board.go — Task Board service (#1699, design 0067 §4.3a): the owner's
// manager surface at /me/projects, /me/epics, /me/tasks. Row encoding is
// the engine's canonical JSON (snake_case short_id etc.), identical to
// the internal automation seam's rendering.

import (
	"context"
	"encoding/json"
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
// stored): Done over (total − Canceled); ProgressDerived false means
// no-progress ("—"), never 0/0.
type BoardEpic struct {
	ID        string   `json:"id"`
	ProjectID string   `json:"project_id"`
	ShortID   string   `json:"short_id"`
	Title     string   `json:"title"`
	Outcome   string   `json:"outcome"`
	Status    string   `json:"status"`
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
	Paused             bool                       `json:"paused"`
	WaitingOn          string                     `json:"waiting_on,omitempty"`
	Backpressure       string                     `json:"backpressure,omitempty"`
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

// BoardLifecycleOptions carries the optional replay key on otherwise-
// empty lifecycle bodies (clientMessageID discipline: one id per logical
// operation, reused on retry).
type BoardLifecycleOptions struct {
	RequestID string
}

func lifecycleBody(opts []BoardLifecycleOptions) struct {
	RequestID string `json:"requestId,omitempty"`
} {
	var body struct {
		RequestID string `json:"requestId,omitempty"`
	}
	if len(opts) > 0 {
		body.RequestID = opts[0].RequestID
	}
	return body
}

// BoardEpicInput is the create/patch body for epics.
type BoardEpicInput struct {
	Title     string   `json:"title,omitempty"`
	Outcome   string   `json:"outcome,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	RequestID string   `json:"requestId,omitempty"`
}

// BoardTaskInput is the create/patch body for tasks.
type BoardTaskInput struct {
	Title              string                     `json:"title,omitempty"`
	Description        string                     `json:"description,omitempty"`
	Tags               []string                   `json:"tags,omitempty"`
	AcceptanceCriteria []BoardAcceptanceCriterion `json:"acceptanceCriteria,omitempty"`
	SetCriteriaMet     *int                       `json:"setCriteriaMet,omitempty"`
	DependsOn          []string                   `json:"dependsOn,omitempty"`
	RequestID          string                     `json:"requestId,omitempty"`
}

// BoardMoveStageInput is the move-stage body.
type BoardMoveStageInput struct {
	ToStage     string `json:"toStage"`
	EvidenceURL string `json:"evidenceUrl,omitempty"`
	RequestID   string `json:"requestId,omitempty"`
}

// BoardLifecycleInput is the pause/resume/start body (requestId only).
type BoardLifecycleInput struct {
	RequestID string `json:"requestId,omitempty"`
}

// BoardDeleteResult is the delete_task/delete_epic outcome (#1747):
// orphaned dependents are listed; every removal keeps a full snapshot
// in the audit log.
type BoardDeleteResult struct {
	ItemKind           string   `json:"item_kind"`
	ID                 string   `json:"id"`
	ShortID            string   `json:"short_id"`
	CascadeTasks       []string `json:"cascade_tasks,omitempty"`
	OrphanedDependents []string `json:"orphaned_dependents,omitempty"`
	Reconnected        []string `json:"reconnected,omitempty"`
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

func (s *BoardService) ArchiveProject(ctx context.Context, id string, opts ...BoardLifecycleOptions) (*BoardProject, error) {
	var p BoardProject
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/projects/%s/archive", id), lifecycleBody(opts), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *BoardService) UnarchiveProject(ctx context.Context, id string, opts ...BoardLifecycleOptions) (*BoardProject, error) {
	var p BoardProject
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/projects/%s/unarchive", id), lifecycleBody(opts), &p); err != nil {
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

func (s *BoardService) epicLifecycle(ctx context.Context, id, verb string, opts []BoardLifecycleOptions) (*BoardEpic, error) {
	var e BoardEpic
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/epics/%s/%s", id, verb), lifecycleBody(opts), &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// BoardActivateOptions carries epic-activation's acknowledge flag
// (the engine's 409 instructs it) plus the replay key.
type BoardActivateOptions struct {
	AcknowledgeInactiveDeps bool
	RequestID               string
}

// BoardEpicActivateResult is the activation response wrapper: the epic
// plus its initial ready set (handler shape; the bare epic the SDK
// once decoded was binding drift).
type BoardEpicActivateResult struct {
	BoardEpic
	InitialReadySet []string `json:"initial_ready_set"`
}

// UnmarshalJSON decodes the {"epic": {...}, "initial_ready_set": [...]}
// wrapper into the embedded epic (embedding alone cannot map it).
func (r *BoardEpicActivateResult) UnmarshalJSON(b []byte) error {
	var wire struct {
		Epic            BoardEpic `json:"epic"`
		InitialReadySet []string  `json:"initial_ready_set"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	r.BoardEpic = wire.Epic
	r.InitialReadySet = wire.InitialReadySet
	return nil
}

func (s *BoardService) EpicActivate(ctx context.Context, id string, opts ...BoardActivateOptions) (*BoardEpicActivateResult, error) {
	var body struct {
		AcknowledgeInactiveDeps bool   `json:"acknowledgeInactiveDeps"`
		RequestID               string `json:"requestId,omitempty"`
	}
	for _, o := range opts {
		body.AcknowledgeInactiveDeps = body.AcknowledgeInactiveDeps || o.AcknowledgeInactiveDeps
		if o.RequestID != "" {
			body.RequestID = o.RequestID
		}
	}
	var out BoardEpicActivateResult
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/epics/%s/activate", id), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *BoardService) EpicPause(ctx context.Context, id string, opts ...BoardLifecycleOptions) (*BoardEpic, error) {
	return s.epicLifecycle(ctx, id, "pause", opts)
}

func (s *BoardService) EpicArchive(ctx context.Context, id string, opts ...BoardLifecycleOptions) (*BoardEpic, error) {
	return s.epicLifecycle(ctx, id, "archive", opts)
}

func (s *BoardService) EpicUnarchive(ctx context.Context, id string, opts ...BoardLifecycleOptions) (*BoardEpic, error) {
	return s.epicLifecycle(ctx, id, "unarchive", opts)
}

// DeleteEpic destructively removes the epic and its tasks (#1747: cancel
// is eliminated); force is required while claims are in-flight.
func (s *BoardService) DeleteEpic(ctx context.Context, id string, force bool) (*BoardDeleteResult, error) {
	path := fmt.Sprintf("/me/epics/%s", id)
	if force {
		path += "?force=true"
	}
	var out BoardDeleteResult
	if err := s.c.do(ctx, http.MethodDelete, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
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

func (s *BoardService) StartTask(ctx context.Context, id string, req BoardLifecycleInput) (*BoardClaim, error) {
	var cl BoardClaim
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/tasks/%s/start", id), req, &cl); err != nil {
		return nil, err
	}
	return &cl, nil
}

// DeleteTask destructively removes a task (#1747); reconnect lets
// orphaned dependents inherit the deleted node's upstream edges.
func (s *BoardService) DeleteTask(ctx context.Context, id string, reconnect bool) (*BoardDeleteResult, error) {
	path := fmt.Sprintf("/me/tasks/%s", id)
	if reconnect {
		path += "?reconnect=true"
	}
	var out BoardDeleteResult
	if err := s.c.do(ctx, http.MethodDelete, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PauseTask parks a task (#1747): claims off, forward writes refused,
// HALT to the in-flight worker; the lease keeps running.
func (s *BoardService) PauseTask(ctx context.Context, id string) (*BoardTask, error) {
	var t BoardTask
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/tasks/%s/pause", id), struct{}{}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// ResumeTask clears the pause and delivers the RESUME directive.
func (s *BoardService) ResumeTask(ctx context.Context, id string) (*BoardTask, error) {
	var t BoardTask
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/tasks/%s/resume", id), struct{}{}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// PauseProject parks every task under the project (#1747).
func (s *BoardService) PauseProject(ctx context.Context, id string) (*BoardProject, error) {
	var p BoardProject
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/projects/%s/pause", id), struct{}{}, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ResumeProject clears the project pause.
func (s *BoardService) ResumeProject(ctx context.Context, id string) (*BoardProject, error) {
	var p BoardProject
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/projects/%s/resume", id), struct{}{}, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *BoardService) AddDependency(ctx context.Context, id string, req BoardDependencyInput) (*BoardDependencyMutation, error) {
	var m BoardDependencyMutation
	if err := s.c.do(ctx, http.MethodPost, fmt.Sprintf("/me/tasks/%s/dependencies", id), req, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *BoardService) RemoveDependency(ctx context.Context, id, dependsOn string, opts ...BoardLifecycleOptions) (*BoardDependencyMutation, error) {
	path := fmt.Sprintf("/me/tasks/%s/dependencies/%s", id, dependsOn)
	if len(opts) > 0 && opts[0].RequestID != "" {
		path += "?requestId=" + url.QueryEscape(opts[0].RequestID)
	}
	var m BoardDependencyMutation
	if err := s.c.do(ctx, http.MethodDelete, path, nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// ListBoardEpics lists a project's epics with derived progress — Done
// over ALL tasks per the #1747 ruling; a zero denominator renders
// no-progress.
func (s *BoardService) ListBoardEpics(ctx context.Context, projectID string) ([]BoardEpic, error) {
	var resp struct {
		Epics []BoardEpic `json:"epics"`
	}
	if err := s.c.do(ctx, http.MethodGet, fmt.Sprintf("/me/projects/%s/epics", projectID), nil, &resp); err != nil {
		return nil, err
	}
	return resp.Epics, nil
}

// BoardSearchFilters narrows a board search (all optional; q is the query).
type BoardSearchFilters struct {
	Project string
	Epic    string
	Stage   string
	Tags    []string
}

// BoardSearchResult is the search projection: matching tasks plus
// did-you-mean suggestions on zero-hit near-misses.
type BoardSearchResult struct {
	Results     []BoardTask `json:"results"`
	Suggestions []string    `json:"suggestions"`
}

// SearchBoard searches the caller's board — short ID exact
// (case-insensitive) or title substring, board-wide or filtered.
func (s *BoardService) SearchBoard(ctx context.Context, q string, f BoardSearchFilters) (*BoardSearchResult, error) {
	params := url.Values{}
	params.Set("q", q)
	if f.Project != "" {
		params.Set("project", f.Project)
	}
	if f.Epic != "" {
		params.Set("epic", f.Epic)
	}
	if f.Stage != "" {
		params.Set("stage", f.Stage)
	}
	for _, tag := range f.Tags {
		params.Add("tag", tag)
	}
	var res BoardSearchResult
	if err := s.c.do(ctx, http.MethodGet, "/me/board/search?"+params.Encode(), nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// BoardHealthLease is one in-flight claim on the health snapshot.
type BoardHealthLease struct {
	TaskID          string `json:"task_id"`
	ShortID         string `json:"short_id"`
	Title           string `json:"title"`
	ProjectID       string `json:"project_id"`
	AssigneeSession string `json:"assignee_session,omitempty"`
	LeaseExpiresAt  string `json:"lease_expires_at"`
}

// BoardHealthTaskCooldown is one throttled task's cooldown.
type BoardHealthTaskCooldown struct {
	TaskID        string `json:"task_id"`
	ShortID       string `json:"short_id"`
	ProjectID     string `json:"project_id"`
	CooldownUntil string `json:"cooldown_until"`
}

// BoardHealthProjectCooldown is one project's worker-slot cooldown.
type BoardHealthProjectCooldown struct {
	ProjectID     string `json:"project_id"`
	ShortID       string `json:"short_id"`
	CooldownUntil string `json:"cooldown_until"`
}

// BoardHealthReclaim is one recent lease reclaim.
type BoardHealthReclaim struct {
	TaskID string `json:"task_id"`
	At     string `json:"at"`
}

// BoardHealthProjectSlots is a project's worker budget in use.
type BoardHealthProjectSlots struct {
	ProjectID   string `json:"project_id"`
	ShortID     string `json:"short_id"`
	WorkerSlots int    `json:"worker_slots"`
	InUse       int    `json:"in_use"`
}

// BoardHealth is the §4.8 health panel snapshot.
type BoardHealth struct {
	ActiveLeases         []BoardHealthLease           `json:"active_leases"`
	TaskCooldowns        []BoardHealthTaskCooldown    `json:"task_cooldowns"`
	ProjectCooldowns     []BoardHealthProjectCooldown `json:"project_cooldowns"`
	PendingInputByOrigin map[string]int               `json:"pending_input_by_origin"`
	RecentReclaims       []BoardHealthReclaim         `json:"recent_reclaims"`
	ProjectSlots         []BoardHealthProjectSlots    `json:"project_slots"`
}

// GetBoardHealth renders the caller's board health snapshot.
func (s *BoardService) GetBoardHealth(ctx context.Context) (*BoardHealth, error) {
	var h BoardHealth
	if err := s.c.do(ctx, http.MethodGet, "/me/board/health", nil, &h); err != nil {
		return nil, err
	}
	return &h, nil
}
