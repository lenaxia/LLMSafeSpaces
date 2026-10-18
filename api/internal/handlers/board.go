// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board.go — the Task Board's REST projection (design 0067 §4.3a,
// issue #1699 Lane 2): the owner's manager surface at /api/v1/me/*,
// user-level only (ruling R7 — no org routes, no scope switcher).
//
// A thin adapter in the §4.2 sense: authenticate (AuthMiddleware, mounted
// by the router), resolve the actor (JWT user, surface=rest), translate
// transport to one BoardService call, render the engine's rows verbatim
// (snake_case short_id — the same canonical encoding the internal
// automation seam ships). No business rules live here — guards,
// validation, idempotency, and audit are the service's (pkg/board).
// Error classes map to HTTP through the same renderBoardError the
// internal automation seam uses, so the parity matrix (§4.3) sees
// identical outcomes per class.
//
// Deliberately NOT routed (engine-seam gaps, flagged on the PR): epic
// listing (no ListEpics seam), board search, and board health — the
// board_read projections the engine does not expose yet.

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// boardManager is the narrow seam the REST adapter needs (§4.2: a
// projection is a thin adapter). Satisfied by *board.Service; narrowed
// for handler testability.
type boardManager interface {
	ListProjects(ctx context.Context, userID string) ([]board.Project, error)
	GetProject(ctx context.Context, projectID, shortIDOrID string) (*board.Project, error)
	CreateProject(ctx context.Context, actor board.Actor, in board.ProjectInput, requestID string) (*board.Project, error)
	UpdateProject(ctx context.Context, actor board.Actor, projectID, shortIDOrID string, in board.ProjectInput, requestID string) (*board.Project, error)
	ArchiveProject(ctx context.Context, actor board.Actor, projectID, requestID string) (*board.Project, error)
	UnarchiveProject(ctx context.Context, actor board.Actor, projectID, requestID string) (*board.Project, error)

	GetEpic(ctx context.Context, epicID, shortIDOrID string) (*board.Epic, error)
	CreateEpic(ctx context.Context, actor board.Actor, projectID, shortIDOrID string, in board.EpicInput, requestID string) (*board.Epic, error)
	UpdateEpic(ctx context.Context, actor board.Actor, epicID, shortIDOrID string, in board.EpicInput, requestID string) (*board.Epic, error)
	EpicActivate(ctx context.Context, actor board.Actor, epicID, shortIDOrID, requestID string) (*board.Epic, error)
	EpicPause(ctx context.Context, actor board.Actor, epicID, shortIDOrID, requestID string) (*board.Epic, error)
	EpicArchive(ctx context.Context, actor board.Actor, epicID, shortIDOrID, requestID string) (*board.Epic, error)
	EpicUnarchive(ctx context.Context, actor board.Actor, epicID, shortIDOrID, requestID string) (*board.Epic, error)
	CancelEpic(ctx context.Context, actor board.Actor, epicID, shortIDOrID, reason, requestID string) (*board.Epic, error)

	GetTask(ctx context.Context, taskID, shortIDOrID string) (*board.Task, error)
	ListTasks(ctx context.Context, projectID string, stage string) ([]board.Task, error)
	CreateTask(ctx context.Context, actor board.Actor, epicID, shortIDOrID string, in board.TaskInput, requestID string) (*board.Task, error)
	UpdateTask(ctx context.Context, actor board.Actor, taskID, shortIDOrID string, in board.TaskInput, requestID string) (*board.Task, error)
	MoveStage(ctx context.Context, actor board.Actor, taskID, shortIDOrID, toStage, evidenceURL, requestID string) (*board.Task, error)
	TaskStart(ctx context.Context, actor board.Actor, taskID, shortIDOrID, requestID string) (*board.Claim, error)
	CancelTask(ctx context.Context, actor board.Actor, taskID, shortIDOrID, reason, requestID string) (*board.Task, error)
	AddDependency(ctx context.Context, actor board.Actor, taskID, shortIDOrID, dependsOnID, requestID string) error
	RemoveDependency(ctx context.Context, actor board.Actor, taskID, shortIDOrID, dependsOnID, requestID string) error
}

// BoardHandler renders the owner's board over REST (user-level, R7).
type BoardHandler struct {
	board boardManager
}

// NewBoardHandler constructs the REST projection adapter. The seam is
// satisfied by *board.Service (app wiring); tests install a fake.
func NewBoardHandler(svc boardManager) *BoardHandler {
	return &BoardHandler{board: svc}
}

// boardActor stamps the REST projection: the JWT user (AuthMiddleware),
// user kind, REST surface, no claim token (manager surface — the two
// named token overrides, cancel and pending_input exits, carry none by
// design; in-flight writes fail the token guard and snap back).
func (h *BoardHandler) boardActor(c *gin.Context) board.Actor {
	return board.Actor{Kind: board.ActorUser, UserID: c.GetString("userID"), Surface: board.SurfaceREST}
}

// boardStages is the enumerated stage filter set (ruling R2's binding
// vocabulary) — the one validation the adapter owns on listings.
func boardStages() map[string]bool {
	return map[string]bool{
		board.StageBacklog: true, board.StageInProgress: true, board.StageInReview: true,
		board.StageDone: true, board.StagePendingInput: true, board.StageCancelled: true,
	}
}

// ownerProject is the read perimeter: resolves a project and enforces
// R7 ownership at the edge — a foreign row renders the same plain 404
// a miss renders (never disclosed, never suggested across the boundary).
func (h *BoardHandler) ownerProject(c *gin.Context, idOrShort string) (*board.Project, bool) {
	p, err := h.board.GetProject(c.Request.Context(), "", idOrShort)
	if err != nil {
		renderBoardError(c, err)
		return nil, false
	}
	if p.UserID != c.GetString("userID") {
		c.JSON(http.StatusNotFound, gin.H{"error": "board: project not found"})
		return nil, false
	}
	return p, true
}

// ownerEpic resolves an epic and enforces ownership through its project.
func (h *BoardHandler) ownerEpic(c *gin.Context, idOrShort string) (*board.Epic, bool) {
	e, err := h.board.GetEpic(c.Request.Context(), "", idOrShort)
	if err != nil {
		renderBoardError(c, err)
		return nil, false
	}
	if _, ok := h.ownerProject(c, e.ProjectID); !ok {
		return nil, false
	}
	// Re-render the row: GetEpic carries the derived progress.
	c.JSON(http.StatusOK, e)
	return e, true
}

// ownerTask resolves a task and enforces ownership through its project.
// Renders nothing (silent mode) when render=false — the mutation paths
// delegate to the service, whose own guards own the outcomes.
func (h *BoardHandler) ownerTask(c *gin.Context, idOrShort string, render bool) (*board.Task, bool) {
	t, err := h.board.GetTask(c.Request.Context(), "", idOrShort)
	if err != nil {
		renderBoardError(c, err)
		return nil, false
	}
	if _, ok := h.ownerProject(c, t.ProjectID); !ok {
		return nil, false
	}
	if render {
		c.JSON(http.StatusOK, t)
	}
	return t, true
}

// --- Projects -----------------------------------------------------------------

// GetProject resolves a project by UUID or short id (case-insensitive,
// design 0066 resolution) — the read perimeter applies.
func (h *BoardHandler) GetProject(c *gin.Context) {
	if p, ok := h.ownerProject(c, c.Param("id")); ok {
		c.JSON(http.StatusOK, p)
	}
}

// ListProjects renders the caller's boards (user-level, R7).
func (h *BoardHandler) ListProjects(c *gin.Context) {
	projects, err := h.board.ListProjects(c.Request.Context(), c.GetString("userID"))
	if err != nil {
		renderBoardError(c, err)
		return
	}
	if projects == nil {
		projects = []board.Project{}
	}
	c.JSON(http.StatusOK, gin.H{"projects": projects})
}

type boardProjectRequest struct {
	Name              string   `json:"name"`
	TargetWorkspaceID string   `json:"targetWorkspaceId"`
	WorkerSlots       int      `json:"workerSlots"`
	Tags              []string `json:"tags"`
	RequestID         string   `json:"requestId"`
}

// CreateProject creates a board (name + one target workspace, R7/R8).
func (h *BoardHandler) CreateProject(c *gin.Context) {
	var req boardProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "project name is required"})
		return
	}
	if req.TargetWorkspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "targetWorkspaceId is required (single target workspace per project, ruling R8)"})
		return
	}
	p, err := h.board.CreateProject(c.Request.Context(), h.boardActor(c),
		board.ProjectInput{Name: req.Name, TargetWorkspaceID: req.TargetWorkspaceID, WorkerSlots: req.WorkerSlots, Tags: req.Tags},
		req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusCreated, p)
}

// UpdateProject patches a project (rename cascades the materialized
// display inheritance inside the service; tags REPLACE — send the full
// set, the engine writes them wholesale).
func (h *BoardHandler) UpdateProject(c *gin.Context) {
	var req boardProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	p, err := h.board.UpdateProject(c.Request.Context(), h.boardActor(c), "", c.Param("id"),
		board.ProjectInput{Name: req.Name, TargetWorkspaceID: req.TargetWorkspaceID, WorkerSlots: req.WorkerSlots, Tags: req.Tags},
		req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

// ArchiveProject is reversible storage; it stops all claims into the
// project (the engine's p CTE predicate).
func (h *BoardHandler) ArchiveProject(c *gin.Context) {
	p, err := h.board.ArchiveProject(c.Request.Context(), h.boardActor(c), c.Param("id"), "")
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

// UnarchiveProject restores active.
func (h *BoardHandler) UnarchiveProject(c *gin.Context) {
	p, err := h.board.UnarchiveProject(c.Request.Context(), h.boardActor(c), c.Param("id"), "")
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

// ListProjectTasks lists a project's tasks, optionally filtered by one
// of the six stages (the adapter validates the filter; the engine owns
// everything else).
func (h *BoardHandler) ListProjectTasks(c *gin.Context) {
	p, ok := h.ownerProject(c, c.Param("id"))
	if !ok {
		return
	}
	stage := c.Query("stage")
	if stage != "" && !boardStages()[stage] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "stage filter must be one of backlog, in_progress, in_review, done, pending_input, " + board.StageCancelled})
		return
	}
	tasks, err := h.board.ListTasks(c.Request.Context(), p.ID, stage)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	if tasks == nil {
		tasks = []board.Task{}
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks})
}

// CreateEpic creates a draft epic in a project (ruling R2's default).
// Resolution + ownership ride the service (scoped to the actor; a
// foreign or missing project is the engine's 404 class).
func (h *BoardHandler) CreateEpic(c *gin.Context) {
	var req boardEpicRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	if req.Title == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "epic title is required"})
		return
	}
	e, err := h.board.CreateEpic(c.Request.Context(), h.boardActor(c), c.Param("id"), "",
		board.EpicInput{Title: req.Title, Outcome: req.Outcome, Priority: req.Priority, Tags: req.Tags},
		req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusCreated, e)
}

type boardEpicRequest struct {
	Title     string   `json:"title"`
	Outcome   string   `json:"outcome"`
	Priority  string   `json:"priority"`
	Tags      []string `json:"tags"`
	RequestID string   `json:"requestId"`
}

// GetEpic resolves an epic with its DERIVED progress (ruling R2 —
// never stored; a zero denominator renders no-progress).
func (h *BoardHandler) GetEpic(c *gin.Context) {
	h.ownerEpic(c, c.Param("id"))
}

// UpdateEpic patches an epic (outcome is the human's steering wheel;
// tags REPLACE — send the full set).
func (h *BoardHandler) UpdateEpic(c *gin.Context) {
	var req boardEpicRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	e, err := h.board.UpdateEpic(c.Request.Context(), h.boardActor(c), "", c.Param("id"),
		board.EpicInput{Title: req.Title, Outcome: req.Outcome, Priority: req.Priority, Tags: req.Tags},
		req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, e)
}

func (h *BoardHandler) epicLifecycle(c *gin.Context, call func(ctx context.Context, actor board.Actor, epicID, shortIDOrID, requestID string) (*board.Epic, error)) {
	e, err := call(c.Request.Context(), h.boardActor(c), c.Param("id"), "", "")
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, e)
}

// ActivateEpic starts feeding work to fresh workers (§4.4 dispatch drive).
func (h *BoardHandler) ActivateEpic(c *gin.Context) {
	h.epicLifecycle(c, h.board.EpicActivate)
}

// PauseEpic stops new claims, never interrupts in-flight leases.
func (h *BoardHandler) PauseEpic(c *gin.Context) {
	h.epicLifecycle(c, h.board.EpicPause)
}

// ArchiveEpic is reversible storage.
func (h *BoardHandler) ArchiveEpic(c *gin.Context) {
	h.epicLifecycle(c, h.board.EpicArchive)
}

// UnarchiveEpic always restores active (the resumable default).
func (h *BoardHandler) UnarchiveEpic(c *gin.Context) {
	h.epicLifecycle(c, h.board.EpicUnarchive)
}

type boardCancelRequest struct {
	Reason    string `json:"reason"`
	RequestID string `json:"requestId"`
}

// CancelEpic cascades to its tasks (non-terminal ones land the cancel
// stage); the epic itself lands paused — the epic enum has no cancel
// value (ruling R2's set is binding; a cancel-landed epic is
// paused-with-a-canceled-task-set).
func (h *BoardHandler) CancelEpic(c *gin.Context) {
	var req boardCancelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	e, err := h.board.CancelEpic(c.Request.Context(), h.boardActor(c), c.Param("id"), "", req.Reason, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, e)
}

// --- Tasks ---------------------------------------------------------------------

type boardTaskRequest struct {
	Title              string                      `json:"title"`
	Description        string                      `json:"description"`
	Priority           string                      `json:"priority"`
	Tags               []string                    `json:"tags"`
	AcceptanceCriteria []board.AcceptanceCriterion `json:"acceptanceCriteria"`
	SetCriteriaMet     *int                        `json:"setCriteriaMet"`
	RequestID          string                      `json:"requestId"`
}

// CreateEpicTask creates a backlog task in an epic. Task tags may carry
// the explicit `pr:N` anchor (owner ruling OR4 — pattern ^pr:[1-9][0-9]*$,
// the only PR-drift form the reconciler consumes; never free-text).
func (h *BoardHandler) CreateEpicTask(c *gin.Context) {
	var req boardTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	if req.Title == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "task title is required"})
		return
	}
	t, err := h.board.CreateTask(c.Request.Context(), h.boardActor(c), c.Param("id"), "",
		board.TaskInput{
			Title: req.Title, Description: req.Description, Priority: req.Priority,
			Tags: req.Tags, AcceptanceCriteria: req.AcceptanceCriteria,
		}, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusCreated, t)
}

// GetTask resolves a task with its blocked-by derivation.
func (h *BoardHandler) GetTask(c *gin.Context) {
	h.ownerTask(c, c.Param("id"), true)
}

// UpdateTask patches a task: title/description/priority/tags, criteria
// edits, and criteria check-off (setCriteriaMet — the reviewer's Done
// preparation; the engine's Done guard consumes the flags). Tags and
// criteria REPLACE when present; omitted fields keep existing values.
func (h *BoardHandler) UpdateTask(c *gin.Context) {
	var req boardTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	t, err := h.board.UpdateTask(c.Request.Context(), h.boardActor(c), "", c.Param("id"),
		board.TaskInput{
			Title: req.Title, Description: req.Description, Priority: req.Priority,
			Tags: req.Tags, AcceptanceCriteria: req.AcceptanceCriteria, SetCriteriaMet: req.SetCriteriaMet,
		}, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, t)
}

type boardMoveStageRequest struct {
	ToStage     string `json:"toStage"`
	EvidenceURL string `json:"evidenceUrl"`
	RequestID   string `json:"requestId"`
}

// MoveStageTask is the server-arbitrated stage write (optimistic UI,
// authoritative server): a drag that loses a race — item claimed,
// terminal-cancel, or lease-held — returns the guard error and the UI
// snaps back. done requires ALL acceptance criteria met + evidenceUrl (R2).
func (h *BoardHandler) MoveStageTask(c *gin.Context) {
	var req boardMoveStageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	if req.ToStage == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "toStage is required"})
		return
	}
	t, err := h.board.MoveStage(c.Request.Context(), h.boardActor(c), "", c.Param("id"), req.ToStage, req.EvidenceURL, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, t)
}

// StartTask is the manual one-off dispatch (task_start): claim + fresh
// spawn into the project's target workspace (R9), explicitly allowed in
// draft epics; paused/completed/archived epics and archived projects
// refuse it. Renders the engine's Claim (escalated: true when the
// attempts cap withheld the grant).
func (h *BoardHandler) StartTask(c *gin.Context) {
	var req boardCancelRequest
	// An absent body is fine (both requestId fields optional); a
	// present-but-empty one (chunked transfer, ContentLength -1, or a
	// bare 0-length body) is equally fine — only malformed JSON is a
	// 400. Binding EOF on an empty body must never 400 (r1-4).
	if c.Request.Body != nil {
		if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
			return
		}
	}
	claim, err := h.board.TaskStart(c.Request.Context(), h.boardActor(c), c.Param("id"), "", req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, claim)
}

// CancelTask is the terminal write: revokes the lease + claim token,
// aborts the worker, and stages non-terminal dependents to
// pending_input ("dependency abandoned"). Idempotent on terminal rows.
func (h *BoardHandler) CancelTask(c *gin.Context) {
	var req boardCancelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	t, err := h.board.CancelTask(c.Request.Context(), h.boardActor(c), c.Param("id"), "", req.Reason, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, t)
}

type boardDependencyRequest struct {
	DependsOn string `json:"dependsOn"`
	RequestID string `json:"requestId"`
}

// AddDependency adds a structural edge (rulings R3/R4 — the ONLY
// blocking mechanism): N-ary task→task within a project, cycle-free by
// construction; violations render the 409 class with did-you-mean
// suggestions (R4a). Renders the re-derived task (is_blocked converges
// in the same read).
func (h *BoardHandler) AddDependency(c *gin.Context) {
	var req boardDependencyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	if req.DependsOn == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "dependsOn is required"})
		return
	}
	if err := h.board.AddDependency(c.Request.Context(), h.boardActor(c), c.Param("id"), "", req.DependsOn, req.RequestID); err != nil {
		renderBoardError(c, err)
		return
	}
	t, err := h.board.GetTask(c.Request.Context(), "", c.Param("id"))
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"task": t})
}

// RemoveDependency removes an edge — one of the pending_input
// resolutions (removing an abandoned prerequisite's edge unblocks the
// row; the human re-queue stays a move-stage write).
func (h *BoardHandler) RemoveDependency(c *gin.Context) {
	if err := h.board.RemoveDependency(c.Request.Context(), h.boardActor(c), c.Param("id"), "", c.Param("dependsOnId"), ""); err != nil {
		renderBoardError(c, err)
		return
	}
	t, err := h.board.GetTask(c.Request.Context(), "", c.Param("id"))
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"task": t})
}
