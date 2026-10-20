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
// identical outcomes per class. The board_read projections this file
// once omitted (epic listing, search, health) landed with the #1699
// seam closings; board-wide search + health ride /api/v1/me/board.

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
	PauseProject(ctx context.Context, actor board.Actor, projectID, shortIDOrID, requestID string) (*board.Project, error)
	ResumeProject(ctx context.Context, actor board.Actor, projectID, shortIDOrID, requestID string) (*board.Project, error)

	GetEpic(ctx context.Context, epicID, shortIDOrID string) (*board.Epic, error)
	CreateEpic(ctx context.Context, actor board.Actor, projectID, shortIDOrID string, in board.EpicInput, requestID string) (*board.Epic, error)
	UpdateEpic(ctx context.Context, actor board.Actor, epicID, shortIDOrID string, in board.EpicInput, requestID string) (*board.Epic, error)
	EpicActivate(ctx context.Context, actor board.Actor, epicID, shortIDOrID string, ackInactiveDeps bool, requestID string) (*board.Epic, error)
	EpicPause(ctx context.Context, actor board.Actor, epicID, shortIDOrID, requestID string) (*board.Epic, error)
	EpicArchive(ctx context.Context, actor board.Actor, epicID, shortIDOrID, requestID string) (*board.Epic, error)
	EpicUnarchive(ctx context.Context, actor board.Actor, epicID, shortIDOrID, requestID string) (*board.Epic, error)
	EpicInitialReadySet(ctx context.Context, epicID string) ([]string, error)
	DeleteEpic(ctx context.Context, actor board.Actor, epicID, shortIDOrID string, force bool, requestID string) (*board.DeleteResult, error)

	GetTask(ctx context.Context, taskID, shortIDOrID string) (*board.Task, error)
	ListTasks(ctx context.Context, projectID string, stage string) ([]board.Task, error)
	ListEpics(ctx context.Context, projectID string) ([]board.Epic, error)
	SearchTasks(ctx context.Context, userID, q string, filters board.SearchFilters) (*board.SearchResult, error)
	BoardHealth(ctx context.Context, userID string) (*board.Health, error)
	CreateTask(ctx context.Context, actor board.Actor, epicID, shortIDOrID string, in board.TaskInput, requestID string) (*board.Task, error)
	UpdateTask(ctx context.Context, actor board.Actor, taskID, shortIDOrID string, in board.TaskInput, requestID string) (*board.Task, error)
	MoveStage(ctx context.Context, actor board.Actor, taskID, shortIDOrID, toStage, evidenceURL, requestID string) (*board.Task, error)
	TaskStart(ctx context.Context, actor board.Actor, taskID, shortIDOrID, requestID string) (*board.Claim, error)
	DeleteTask(ctx context.Context, actor board.Actor, taskID, shortIDOrID string, reconnect bool, requestID string) (*board.DeleteResult, error)
	PauseTask(ctx context.Context, actor board.Actor, taskID, shortIDOrID, requestID string) (*board.Task, error)
	ResumeTask(ctx context.Context, actor board.Actor, taskID, shortIDOrID, requestID string) (*board.Task, error)
	AddDependency(ctx context.Context, actor board.Actor, taskID, shortIDOrID, dependsOnID, requestID string) error
	RemoveDependency(ctx context.Context, actor board.Actor, taskID, shortIDOrID, dependsOnID, requestID string) error

	ImportPlan(ctx context.Context, actor board.Actor, projectID, shortIDOrID string, in board.PlanInput, dryRun bool, requestID string) (*board.ImportResult, error)
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
// vocabulary, #1747-amended: five stages — cancel is eliminated).
func boardStages() map[string]bool {
	return map[string]bool{
		board.StageBacklog: true, board.StageInProgress: true, board.StageInReview: true,
		board.StageDone: true, board.StagePendingInput: true,
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

// boardLifecycleRequest is the pause/resume family's optional body
// (requestId only; an absent body is fine).
type boardLifecycleRequest struct {
	RequestID string `json:"requestId"`
}

func bindBoardLifecycleBody(c *gin.Context) string {
	var req boardLifecycleRequest
	if c.Request.Body == nil {
		return ""
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		return "" // absent/empty body: both fields optional (the StartTask discipline)
	}
	return req.RequestID
}

// PauseProject parks the project (#1747): claim candidacy off for every
// task under it, halt directives to in-flight workers, leases untouched.
func (h *BoardHandler) PauseProject(c *gin.Context) {
	p, err := h.board.PauseProject(c.Request.Context(), h.boardActor(c), "", c.Param("id"), bindBoardLifecycleBody(c))
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

// ResumeProject clears the pause and cascades resume directives.
func (h *BoardHandler) ResumeProject(c *gin.Context) {
	p, err := h.board.ResumeProject(c.Request.Context(), h.boardActor(c), "", c.Param("id"), bindBoardLifecycleBody(c))
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
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

// boardOptionalRequestID binds the optional {requestId} body on actions
// whose payloads are otherwise empty (lifecycle posts): an absent body,
// an empty body, and a chunked ContentLength −1 are all fine — only
// malformed JSON is a 400 (the StartTask r1-4 discipline).
func boardOptionalRequestID(c *gin.Context) (string, bool) {
	var req struct {
		RequestID string `json:"requestId"`
	}
	if c.Request.Body == nil {
		return "", true
	}
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return "", false
	}
	return req.RequestID, true
}

// ArchiveProject is reversible storage; it stops all claims into the
// project (the engine's p CTE predicate).
func (h *BoardHandler) ArchiveProject(c *gin.Context) {
	requestID, ok := boardOptionalRequestID(c)
	if !ok {
		return
	}
	p, err := h.board.ArchiveProject(c.Request.Context(), h.boardActor(c), c.Param("id"), requestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

// UnarchiveProject restores active.
func (h *BoardHandler) UnarchiveProject(c *gin.Context) {
	requestID, ok := boardOptionalRequestID(c)
	if !ok {
		return
	}
	p, err := h.board.UnarchiveProject(c.Request.Context(), h.boardActor(c), c.Param("id"), requestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

// ListProjectTasks lists a project's tasks, optionally filtered by one
// of the five stages (the adapter validates the filter; the engine owns
// everything else).
func (h *BoardHandler) ListProjectTasks(c *gin.Context) {
	p, ok := h.ownerProject(c, c.Param("id"))
	if !ok {
		return
	}
	stage := c.Query("stage")
	if stage != "" && !boardStages()[stage] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "stage filter must be one of backlog, in_progress, in_review, done, pending_input"})
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

// ListProjectEpics lists a project's epics with derived progress (the
// #1699 engine seam).
func (h *BoardHandler) ListProjectEpics(c *gin.Context) {
	p, ok := h.ownerProject(c, c.Param("id"))
	if !ok {
		return
	}
	epics, err := h.board.ListEpics(c.Request.Context(), p.ID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	if epics == nil {
		epics = []board.Epic{}
	}
	c.JSON(http.StatusOK, gin.H{"epics": epics})
}

// BoardSearch searches the caller's board (short ID exact or title
// match, §4.8's search box); zero-hit near-misses carry did-you-mean
// suggestions. The perimeter lives in the seam (foreign rows never
// match, never suggest). Query-shape validation is the seam's (§4.2);
// presence is the adapter's (the search box never sends an empty q).
func (h *BoardHandler) BoardSearch(c *gin.Context) {
	q := c.Query("q")
	if q == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "q is required"})
		return
	}
	stage := c.Query("stage")
	if stage != "" && !boardStages()[stage] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "stage filter must be one of the five stages: backlog, in_progress, in_review, done, pending_input"})
		return
	}
	res, err := h.board.SearchTasks(c.Request.Context(), c.GetString("userID"), q, board.SearchFilters{
		Project: c.Query("project"),
		Epic:    c.Query("epic"),
		Stage:   stage,
		Tags:    c.QueryArray("tag"),
	})
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, res)
}

// BoardHealth renders the caller's board health snapshot (§4.8's health
// panel).
func (h *BoardHandler) BoardHealth(c *gin.Context) {
	h_, err := h.board.BoardHealth(c.Request.Context(), c.GetString("userID"))
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, h_)
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
		board.EpicInput{Title: req.Title, Outcome: req.Outcome, Tags: req.Tags},
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
		board.EpicInput{Title: req.Title, Outcome: req.Outcome, Tags: req.Tags},
		req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, e)
}

func (h *BoardHandler) epicLifecycle(c *gin.Context, call func(ctx context.Context, actor board.Actor, epicID, shortIDOrID, requestID string) (*board.Epic, error)) {
	requestID, ok := boardOptionalRequestID(c)
	if !ok {
		return
	}
	e, err := call(c.Request.Context(), h.boardActor(c), c.Param("id"), "", requestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, e)
}

type boardActivateRequest struct {
	AcknowledgeInactiveDeps bool   `json:"acknowledgeInactiveDeps"`
	RequestID               string `json:"requestId"`
}

// ActivateEpic starts feeding work to fresh workers (§4.4 dispatch
// drive). #1747: refuses cross-epic deps into inactive epics unless
// acknowledged, and reports the initial ready set.
func (h *BoardHandler) ActivateEpic(c *gin.Context) {
	var req boardActivateRequest
	if c.Request.Body != nil {
		if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
			return
		}
	}
	e, err := h.board.EpicActivate(c.Request.Context(), h.boardActor(c), c.Param("id"), "",
		req.AcknowledgeInactiveDeps, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	ready, rerr := h.board.EpicInitialReadySet(c.Request.Context(), e.ID)
	if rerr != nil {
		ready = nil // informational — activation already succeeded
	}
	c.JSON(http.StatusOK, gin.H{"epic": e, "initial_ready_set": ready})
}

// PauseEpic stops new claims, never interrupts in-flight leases —
// and cascades halt directives to the epic's in-flight workers (#1747).
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

// DeleteEpic destructively removes the epic and its tasks (#1747: cancel
// is eliminated — delete replaces it; every removed row keeps its
// snapshot in the audit log). force is required while claims are
// in-flight (the delete commits, then in-flight workers are aborted —
// post-commit).
func (h *BoardHandler) DeleteEpic(c *gin.Context) {
	force := c.Query("force") == "true"
	out, err := h.board.DeleteEpic(c.Request.Context(), h.boardActor(c), c.Param("id"), "", force, "")
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// ImportPlan materializes (or, with dryRun=true, validates) an entire
// plan — one epic + tasks + edges, all-or-nothing (#1747: the user's
// plan-approval moment).
func (h *BoardHandler) ImportPlan(c *gin.Context) {
	var req boardPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	in := board.PlanInput{EpicTitle: req.EpicTitle, Outcome: req.Outcome, Tags: req.Tags}
	for _, t := range req.Tasks {
		in.Tasks = append(in.Tasks, board.PlanTaskInput{
			Key: t.Key, Title: t.Title, Description: t.Description, Tags: t.Tags,
			AcceptanceCriteria: t.AcceptanceCriteria, DependsOn: t.DependsOn,
		})
	}
	out, err := h.board.ImportPlan(c.Request.Context(), h.boardActor(c), c.Param("id"), "",
		in, req.DryRun, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

type boardPlanTaskRequest struct {
	Key                string                      `json:"key"`
	Title              string                      `json:"title"`
	Description        string                      `json:"description"`
	Tags               []string                    `json:"tags"`
	AcceptanceCriteria []board.AcceptanceCriterion `json:"acceptanceCriteria"`
	DependsOn          []string                    `json:"dependsOn"`
}

type boardPlanRequest struct {
	EpicTitle string                 `json:"epicTitle"`
	Outcome   string                 `json:"outcome"`
	Tags      []string               `json:"tags"`
	DryRun    bool                   `json:"dryRun"`
	Tasks     []boardPlanTaskRequest `json:"tasks"`
	RequestID string                 `json:"requestId"`
}

// --- Tasks ---------------------------------------------------------------------

type boardTaskRequest struct {
	Title              string                      `json:"title"`
	Description        string                      `json:"description"`
	Tags               []string                    `json:"tags"`
	AcceptanceCriteria []board.AcceptanceCriterion `json:"acceptanceCriteria"`
	SetCriteriaMet     *int                        `json:"setCriteriaMet"`
	// DependsOn (create only, #1747): prerequisite task refs (tsk-… or
	// UUID); the edges are added in the create's own transaction.
	DependsOn []string `json:"dependsOn"`
	RequestID string   `json:"requestId"`
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
			Title: req.Title, Description: req.Description,
			Tags: req.Tags, AcceptanceCriteria: req.AcceptanceCriteria, DependsOn: req.DependsOn,
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

// UpdateTask patches a task: title/description/tags, criteria
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
			Title: req.Title, Description: req.Description,
			Tags: req.Tags, AcceptanceCriteria: req.AcceptanceCriteria, SetCriteriaMet: req.SetCriteriaMet,
		}, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, t)
}

// PauseTask parks the task (#1747): claim candidacy off, forward writes
// refused, halt directive to the in-flight worker, lease untouched.
func (h *BoardHandler) PauseTask(c *gin.Context) {
	t, err := h.board.PauseTask(c.Request.Context(), h.boardActor(c), "", c.Param("id"), bindBoardLifecycleBody(c))
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, t)
}

// ResumeTask clears the pause and delivers the resume directive; a
// lapsed lease re-enters the FIFO pool.
func (h *BoardHandler) ResumeTask(c *gin.Context) {
	t, err := h.board.ResumeTask(c.Request.Context(), h.boardActor(c), "", c.Param("id"), bindBoardLifecycleBody(c))
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

type boardStartRequest struct {
	RequestID string `json:"requestId"`
}

// StartTask is the manual one-off dispatch (task_start): claim + fresh
// spawn into the project's target workspace (R9), explicitly allowed in
// draft epics; paused/completed/archived epics and archived projects
// refuse it. Renders the engine's Claim (escalated: true when the
// attempts cap withheld the grant).
func (h *BoardHandler) StartTask(c *gin.Context) {
	var req boardStartRequest
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

// DeleteTask is the destructive replacement for the eliminated cancel
// (#1747): an idle row goes straight; an in-flight row's worker is
// aborted AFTER the delete commits (post-commit). Edges cascade;
// orphaned dependents are listed and each
// gets an unblocked_by_delete audit event. reconnect=true lets orphans
// inherit the deleted node's direct upstream edges (deduplicated,
// cycle-guarded) — default off (no implicit graph rewrites).
func (h *BoardHandler) DeleteTask(c *gin.Context) {
	reconnect := c.Query("reconnect") == "true"
	out, err := h.board.DeleteTask(c.Request.Context(), h.boardActor(c), c.Param("id"), "", reconnect, "")
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

type boardDependencyRequest struct {
	DependsOn string `json:"dependsOn"`
	RequestID string `json:"requestId"`
}

// AddDependency adds a structural edge (rulings R3/R4 as amended by
// #1747 — the ONLY blocking mechanism): N-ary task→task within a
// WORKSPACE (cross-epic allowed, never cross-workspace), cycle-free by
// construction, duplicate edges idempotent no-ops, edges-per-node capped;
// violations render the 409 class with did-you-mean suggestions (R4a).
// Renders the re-derived task (is_blocked converges in the same read).
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
// row; the human re-queue stays a move-stage write). DELETE carries no
// body: the optional replay key rides a query parameter.
func (h *BoardHandler) RemoveDependency(c *gin.Context) {
	if err := h.board.RemoveDependency(c.Request.Context(), h.boardActor(c), c.Param("id"), "", c.Param("dependsOnId"), c.Query("requestId")); err != nil {
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
