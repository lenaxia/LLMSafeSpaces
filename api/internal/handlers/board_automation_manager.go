// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_automation_manager.go — the manager half of the Task Board's
// internal automation seam: the same pod-identity resolve (TokenReview
// → SA principal → workspace pin) the worker routes ride, carrying the
// FULL manager set at project, epic, and task level. Design 0067
// §4.3c's agent/manager surface split is REPEALED in the grant
// direction (ruling 2026-10-27 / tsk-copper-refectory-generates): a
// workspace pod is single-tenant and every session shares one
// credential, so the #1747 §5 manager-grant gate (session-identity
// requirement + live-assignee worker refusal) is gone — the manager
// set is open to ANY in-workspace session, still scoped structurally
// by the caller-workspace binding the engine enforces
// (requireManagerAction). The session id (the harness-attested
// injected identity, the send_message pattern) rides the body purely
// as AUDIT ATTRIBUTION — absent or unknown, the call proceeds with an
// unattributed actor.

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// boardSessionID extracts the calling session's identity (audit
// attribution only, never a gate): the query first (body-less
// DELETEs), then the exact-key body probe (the resolver-spelling
// discipline: exact key, no case folding).
func boardSessionID(c *gin.Context, raw []byte) string {
	if v := c.Query("sessionId"); v != "" {
		return v
	}
	if len(raw) == 0 {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ""
	}
	v, ok := fields["sessionId"]
	if !ok {
		return ""
	}
	var s string
	_ = json.Unmarshal(v, &s)
	return s
}

// managerActor stamps the manager delegation: the pod's resolved owner
// (R7), the MANAGER-GRADE agent surface (#1832 WS2 — a distinct audit
// vocabulary from the worker projection: the pending_input exits read
// it as a manager surface), the resolved workspace (the structural
// binding), and the calling SESSION (audit attribution — empty when
// the caller supplied no resolvable identity; never a refusal, ruling
// 2026-10-27 / tsk-copper-refectory-generates).
func (h *PodAutomationHandler) managerActor(ownerID, workspaceID, sessionID string) board.Actor {
	return board.Actor{
		Kind: board.ActorUser, UserID: ownerID, Surface: board.SurfaceAgentManager,
		WorkspaceID: workspaceID, SessionID: sessionID,
	}
}

// resolveManager runs the pod resolve. The #1747 §5 grant belt (the
// required-session 403 and the live-assignee worker refusal) is
// REPEALED (ruling 2026-10-27 / tsk-copper-refectory-generates): the
// manager set is open to any in-workspace session, and the resolved
// sessionId (query first — body-less DELETEs — then the exact-key body
// probe) is audit attribution only. ok=false only on the pod-identity
// resolve failure.
func (h *PodAutomationHandler) resolveManager(c *gin.Context) (raw []byte, workspaceID, ownerID, sessionID string, ok bool) {
	raw, workspaceID, ownerID, ok = h.resolve(c)
	if !ok {
		return nil, "", "", "", false
	}
	sessionID = boardSessionID(c, raw)
	return raw, workspaceID, ownerID, sessionID, true
}

// --- Projects -----------------------------------------------------------------

// BoardCreateProject creates a board targeting the caller's OWN
// workspace (the engine binds targetWorkspaceId to the pod identity).
func (h *PodAutomationHandler) BoardCreateProject(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		Name              string   `json:"name"`
		TargetWorkspaceID string   `json:"targetWorkspaceId"`
		WorkerSlots       int      `json:"workerSlots"`
		Tags              []string `json:"tags"`
		RequestID         string   `json:"requestId"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	// Structural scope: the pod's own workspace, stamped over any caller
	// spelling (the scopeTriggerCreateBody discipline).
	req.TargetWorkspaceID = ws
	p, err := h.boardMgr.CreateProject(c.Request.Context(), h.managerActor(owner, ws, session),
		board.ProjectInput{Name: req.Name, TargetWorkspaceID: req.TargetWorkspaceID, WorkerSlots: req.WorkerSlots, Tags: req.Tags},
		req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusCreated, projectEchoResponse{Project: *p, ContentEchoDiff: projectEchoDiff(req.Name, p)})
}

// BoardUpdateProject patches a project (the engine refuses agent-side
// retargeting by construction).
func (h *PodAutomationHandler) BoardUpdateProject(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		Name        string   `json:"name"`
		WorkerSlots int      `json:"workerSlots"`
		Tags        []string `json:"tags"`
		RequestID   string   `json:"requestId"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	p, err := h.boardMgr.UpdateProject(c.Request.Context(), h.managerActor(owner, ws, session), "", c.Param("id"),
		board.ProjectInput{Name: req.Name, WorkerSlots: req.WorkerSlots, Tags: req.Tags}, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, projectEchoResponse{Project: *p, ContentEchoDiff: projectEchoDiff(req.Name, p)})
}

// BoardProjectLifecycle serves archive/unarchive/pause/resume (the
// adapter folds the short-id-optional variants onto one helper).
func (h *PodAutomationHandler) BoardProjectLifecycle(c *gin.Context, call func(ctx context.Context, actor board.Actor, projectID, requestID string) (*board.Project, error)) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		RequestID string `json:"requestId"`
	}
	if raw != nil {
		_ = bindBoardBody(c, raw, &req)
	}
	p, err := call(c.Request.Context(), h.managerActor(owner, ws, session), c.Param("id"), req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, p)
}

// BoardArchiveProject is reversible storage.
func (h *PodAutomationHandler) BoardArchiveProject(c *gin.Context) {
	h.BoardProjectLifecycle(c, h.boardMgr.ArchiveProject)
}

// BoardUnarchiveProject restores active.
func (h *PodAutomationHandler) BoardUnarchiveProject(c *gin.Context) {
	h.BoardProjectLifecycle(c, h.boardMgr.UnarchiveProject)
}

// BoardPauseProject parks the project (halt cascade to its workers).
func (h *PodAutomationHandler) BoardPauseProject(c *gin.Context) {
	h.BoardProjectLifecycle(c, func(ctx context.Context, a board.Actor, id, rid string) (*board.Project, error) {
		return h.boardMgr.PauseProject(ctx, a, id, "", rid)
	})
}

// BoardResumeProject clears the pause (resume cascade).
func (h *PodAutomationHandler) BoardResumeProject(c *gin.Context) {
	h.BoardProjectLifecycle(c, func(ctx context.Context, a board.Actor, id, rid string) (*board.Project, error) {
		return h.boardMgr.ResumeProject(ctx, a, id, "", rid)
	})
}

// --- Epics --------------------------------------------------------------------

// BoardCreateEpic creates a draft epic.
func (h *PodAutomationHandler) BoardCreateEpic(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		Title     string   `json:"title"`
		Outcome   string   `json:"outcome"`
		Tags      []string `json:"tags"`
		RequestID string   `json:"requestId"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	e, err := h.boardMgr.CreateEpic(c.Request.Context(), h.managerActor(owner, ws, session), c.Param("id"), "",
		board.EpicInput{Title: req.Title, Outcome: req.Outcome, Tags: req.Tags}, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusCreated, epicEchoResponse{Epic: *e, ContentEchoDiff: epicEchoDiff(req.Title, req.Outcome, e)})
}

// BoardUpdateEpic patches an epic.
func (h *PodAutomationHandler) BoardUpdateEpic(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		Title     string   `json:"title"`
		Outcome   string   `json:"outcome"`
		Tags      []string `json:"tags"`
		RequestID string   `json:"requestId"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	e, err := h.boardMgr.UpdateEpic(c.Request.Context(), h.managerActor(owner, ws, session), "", c.Param("id"),
		board.EpicInput{Title: req.Title, Outcome: req.Outcome, Tags: req.Tags}, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, epicEchoResponse{Epic: *e, ContentEchoDiff: epicEchoDiff(req.Title, req.Outcome, e)})
}

// BoardEpicLifecycle serves activate/pause/archive/unarchive.
func (h *PodAutomationHandler) BoardEpicLifecycle(c *gin.Context, call func(ctx context.Context, actor board.Actor, epicID, shortIDOrID, requestID string) (*board.Epic, error)) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		RequestID string `json:"requestId"`
	}
	if raw != nil {
		_ = bindBoardBody(c, raw, &req)
	}
	e, err := call(c.Request.Context(), h.managerActor(owner, ws, session), c.Param("id"), "", req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, e)
}

// BoardEpicPause stops new claims + cascades halts.
func (h *PodAutomationHandler) BoardEpicPause(c *gin.Context) {
	h.BoardEpicLifecycle(c, h.boardMgr.EpicPause)
}

// BoardEpicArchive is reversible storage.
func (h *PodAutomationHandler) BoardEpicArchive(c *gin.Context) {
	h.BoardEpicLifecycle(c, h.boardMgr.EpicArchive)
}

// BoardEpicUnarchive restores active.
func (h *PodAutomationHandler) BoardEpicUnarchive(c *gin.Context) {
	h.BoardEpicLifecycle(c, h.boardMgr.EpicUnarchive)
}

// BoardEpicActivate is the publish/start lever: refuses cross-epic deps
// into inactive epics unless acknowledged, reports the initial ready
// set, cascades resume directives.
func (h *PodAutomationHandler) BoardEpicActivate(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		AcknowledgeInactiveDeps bool   `json:"acknowledgeInactiveDeps"`
		RequestID               string `json:"requestId"`
	}
	if raw != nil {
		_ = bindBoardBody(c, raw, &req)
	}
	e, err := h.boardMgr.EpicActivate(c.Request.Context(), h.managerActor(owner, ws, session), c.Param("id"), "",
		req.AcknowledgeInactiveDeps, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	ready, rerr := h.boardMgr.EpicInitialReadySet(c.Request.Context(), e.ID)
	if rerr != nil {
		ready = nil
	}
	c.JSON(http.StatusOK, gin.H{"epic": e, "initial_ready_set": ready})
}

// BoardDeleteEpic cascades destructively (force required while claims
// are live — snapshot events preserve every removed row).
func (h *PodAutomationHandler) BoardDeleteEpic(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	_ = raw
	out, err := h.boardMgr.DeleteEpic(c.Request.Context(), h.managerActor(owner, ws, session), c.Param("id"), "",
		c.Query("force") == "true", "")
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// --- Tasks + dependencies ------------------------------------------------------

// BoardManagerMoveStage is the manager-grade stage move (#1832 WS2 —
// the tsk-3do6n458 incident: an escalated pending_input row froze the
// dependency chain because the ONLY agent-surface move route is the
// worker's, and the worker surface is denied the pending_input exits).
// Rides resolveManager (audit attribution; any in-workspace session —
// the #1747 §5 gate is repealed, ruling 2026-10-27 /
// tsk-copper-refectory-generates) and the canonical MoveStage — the
// stage machine arbitrates exactly as it does for the owner on REST;
// the actor's manager-grade surface is what unlocks the exits, and the
// audit carries the calling session's identity when supplied.
func (h *PodAutomationHandler) BoardManagerMoveStage(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		ToStage     string `json:"toStage"`
		EvidenceURL string `json:"evidenceUrl"`
		RequestID   string `json:"requestId"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	t, err := h.boardMgr.MoveStage(c.Request.Context(), h.managerActor(owner, ws, session),
		"", c.Param("id"), req.ToStage, req.EvidenceURL, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, t)
}

// BoardManagerMoveStageBulk is #1836's bulk complete on the pod seam —
// the manager-grade batch stage move (BoardManagerMoveStage's bulk
// twin): one shared target stage + evidence across up to 50 tasks, one
// call. Per-item results (partial failures included — never
// all-or-nothing); an item already at the target returns done
// (idempotent retry); every landed item keeps its own single-task
// transition + audit. NO claim tokens render (#1864 ruling). The bulk
// CLAIM variant deliberately does NOT exist here: task_start is
// owner-surface only (round-3 #2), and the bulk form follows the
// single form's boundary.
func (h *PodAutomationHandler) BoardManagerMoveStageBulk(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		TaskIDs     []string `json:"taskIds"`
		ToStage     string   `json:"toStage"`
		EvidenceURL string   `json:"evidenceUrl"`
		RequestID   string   `json:"requestId"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	if len(req.TaskIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "taskIds is required (at least one task id)", "code": board.GuardInput})
		return
	}
	if req.ToStage == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "toStage is required", "code": board.GuardInput})
		return
	}
	// The wire stage vocabulary's shared helper (board.go's boardStages
	// — the same single source the REST bulk adapter and the engine's
	// StageInVocabulary render from; #1836 review r1 style finding).
	// 000050: pending_input stays in the set as the flag-op alias
	// (move = set the flag; failed is the new storable terminus).
	if !boardStages()[req.ToStage] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "toStage must be one of the wire stages: " + board.WireStageVocabulary(), "code": board.GuardInput})
		return
	}
	results, err := h.boardMgr.BulkMoveStage(c.Request.Context(), h.managerActor(owner, ws, session),
		req.TaskIDs, req.ToStage, req.EvidenceURL, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"results": results, "count": len(results)})
}

// BoardManagerDeleteTasksBulk is #1836's sibling bulk delete on the
// pod seam (board_delete_story's batch twin — delete IS a manager
// action; only task_start/task_create carry the owner-only boundary,
// so unlike the bulk claim this form legitimately lives here): up to
// 50 destructive removals, one call, ONE shared reconnect flag.
// Per-item results (partial failures included — never
// all-or-nothing); each landed item keeps its own single-task
// transaction + snapshot audit event + orphan events + post-commit
// worker abort. The delete-variant's idempotent-retry rendering: a
// miss, a foreign row, and an already-deleted row all render the same
// bare not_found item (absence ≡ miss ≡ foreign — never disclosed,
// never a conflict on retry).
func (h *PodAutomationHandler) BoardManagerDeleteTasksBulk(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		TaskIDs   []string `json:"taskIds"`
		Reconnect bool     `json:"reconnect"`
		RequestID string   `json:"requestId"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	if len(req.TaskIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "taskIds is required (at least one task id)", "code": board.GuardInput})
		return
	}
	results, err := h.boardMgr.BulkDeleteTasks(c.Request.Context(), h.managerActor(owner, ws, session),
		req.TaskIDs, req.Reconnect, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"results": results, "count": len(results)})
}

// BoardCreateTask creates a backlog task (inline dependsOn allowed).
func (h *PodAutomationHandler) BoardCreateTask(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		Title              string                      `json:"title"`
		Description        string                      `json:"description"`
		Tags               []string                    `json:"tags"`
		AcceptanceCriteria []board.AcceptanceCriterion `json:"acceptanceCriteria"`
		DependsOn          []string                    `json:"dependsOn"`
		SetCriteriaMet     *board.CriteriaCheck        `json:"setCriteriaMet"`
		RequestID          string                      `json:"requestId"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	// #1837 P2b: create never had criteria check-off — the uniform
	// loud-refusal posture (REST + external MCP refuse the same way).
	if req.SetCriteriaMet != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "setCriteriaMet applies to task UPDATES only — create accepts criteria with explicit met flags", "code": board.GuardInput})
		return
	}
	t, err := h.boardMgr.CreateTask(c.Request.Context(), h.managerActor(owner, ws, session), c.Param("id"), "",
		board.TaskInput{Title: req.Title, Description: req.Description, Tags: req.Tags,
			AcceptanceCriteria: req.AcceptanceCriteria, DependsOn: req.DependsOn}, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusCreated, taskEchoResponse{Task: *t, ContentEchoDiff: taskEchoDiff(req.Title, req.Description, t)})
}

// BoardUpdateTask patches a task.
func (h *PodAutomationHandler) BoardUpdateTask(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		Title              string                      `json:"title"`
		Description        string                      `json:"description"`
		Tags               []string                    `json:"tags"`
		AcceptanceCriteria []board.AcceptanceCriterion `json:"acceptanceCriteria"`
		SetCriteriaMet     *board.CriteriaCheck        `json:"setCriteriaMet"`
		RequestID          string                      `json:"requestId"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	t, err := h.boardMgr.UpdateTask(c.Request.Context(), h.managerActor(owner, ws, session), "", c.Param("id"),
		board.TaskInput{Title: req.Title, Description: req.Description, Tags: req.Tags,
			AcceptanceCriteria: req.AcceptanceCriteria, SetCriteriaMet: req.SetCriteriaMet}, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, taskEchoResponse{Task: *t, ContentEchoDiff: taskEchoDiff(req.Title, req.Description, t), MutatedCriterion: mutatedCriterion(req.SetCriteriaMet, t)})
}

// BoardTaskLifecycle serves pause/resume.
func (h *PodAutomationHandler) BoardTaskLifecycle(c *gin.Context, call func(ctx context.Context, actor board.Actor, taskID, shortIDOrID, requestID string) (*board.Task, error)) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		RequestID string `json:"requestId"`
	}
	if raw != nil {
		_ = bindBoardBody(c, raw, &req)
	}
	t, err := call(c.Request.Context(), h.managerActor(owner, ws, session), "", c.Param("id"), req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, t)
}

// BoardPauseTask parks the task (halt to the in-flight worker).
func (h *PodAutomationHandler) BoardPauseTask(c *gin.Context) {
	h.BoardTaskLifecycle(c, h.boardMgr.PauseTask)
}

// BoardResumeTask clears the pause (resume directive).
func (h *PodAutomationHandler) BoardResumeTask(c *gin.Context) {
	h.BoardTaskLifecycle(c, h.boardMgr.ResumeTask)
}

// BoardRearmTask is the attempts-exhaustion re-pool (#1832 WS3) on the
// pod seam: manager-grade actor, canonical RearmTask.
func (h *PodAutomationHandler) BoardRearmTask(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		RequestID string `json:"requestId"`
	}
	if raw != nil {
		_ = bindBoardBody(c, raw, &req)
	}
	t, err := h.boardMgr.RearmTask(c.Request.Context(), h.managerActor(owner, ws, session), "", c.Param("id"), req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, t)
}

// BoardDeposeTask is S5's DEPOSED directive on the pod seam: the
// orchestrator (manager route) deposes a LAPSED claim and records the
// salvage pointer (branch/PR/last known state) on the row — the next
// claim's charter embeds it automatically. Manager-grade actor,
// canonical DeposeTask; a live healthy claim refuses (claim_live).
func (h *PodAutomationHandler) BoardDeposeTask(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		Branch    string `json:"branch"`
		PR        string `json:"pr"`
		Head      string `json:"head"`
		State     string `json:"state"`
		Note      string `json:"note"`
		RequestID string `json:"requestId"`
	}
	if raw != nil {
		_ = bindBoardBody(c, raw, &req)
	}
	ptr := board.SalvagePointer{Branch: req.Branch, PR: req.PR, Head: req.Head, State: req.State, Note: req.Note}
	t, err := h.boardMgr.DeposeTask(c.Request.Context(), h.managerActor(owner, ws, session), "", c.Param("id"), ptr, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, t)
}

// BoardDeleteTask destructively removes a task (clean abort first when
// in-flight; orphans surfaced + audited; optional reconnect).
func (h *PodAutomationHandler) BoardDeleteTask(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	_ = raw
	out, err := h.boardMgr.DeleteTask(c.Request.Context(), h.managerActor(owner, ws, session), "", c.Param("id"),
		c.Query("reconnect") == "true", "")
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// BoardAddDependency adds one edge (workspace-scoped, capped, cycle-free).
func (h *PodAutomationHandler) BoardAddDependency(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		DependsOn string `json:"dependsOn"`
		RequestID string `json:"requestId"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	if err := h.boardMgr.AddDependency(c.Request.Context(), h.managerActor(owner, ws, session), "", c.Param("id"), req.DependsOn, req.RequestID); err != nil {
		renderBoardError(c, err)
		return
	}
	t, err := h.boardMgr.GetTask(c.Request.Context(), "", c.Param("id"))
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"task": t})
}

// BoardRemoveDependency removes one edge.
func (h *PodAutomationHandler) BoardRemoveDependency(c *gin.Context) {
	_, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	if err := h.boardMgr.RemoveDependency(c.Request.Context(), h.managerActor(owner, ws, session), "", c.Param("id"), c.Param("dependsOnId"), ""); err != nil {
		renderBoardError(c, err)
		return
	}
	t, err := h.boardMgr.GetTask(c.Request.Context(), "", c.Param("id"))
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"task": t})
}

// BoardImportPlan materializes (dryRun validates) an entire plan. The
// unified plan document (design 0070 §1a) carries one epic arm — the
// `epic` object (create a new epic) or `epicId` (attach to an existing
// epic, §3) — with the legacy epicTitle/outcome spelling still bound for
// the REST/external surfaces; exactly-one-of and all atomicity live in
// the canonical BoardService.
func (h *PodAutomationHandler) BoardImportPlan(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		Epic      *board.PlanEpicSpec   `json:"epic"`
		EpicID    string                `json:"epicId"`
		EpicTitle string                `json:"epicTitle"`
		Outcome   string                `json:"outcome"`
		Tags      []string              `json:"tags"`
		DryRun    bool                  `json:"dryRun"`
		Tasks     []board.PlanTaskInput `json:"tasks"`
		RequestID string                `json:"requestId"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	in := board.PlanInput{
		Epic: req.Epic, EpicID: req.EpicID,
		EpicTitle: req.EpicTitle, Outcome: req.Outcome, Tags: req.Tags,
		Tasks: req.Tasks,
	}
	out, err := h.boardMgr.ImportPlan(c.Request.Context(), h.managerActor(owner, ws, session), c.Param("id"), "",
		in, req.DryRun, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// --- Workspace-scoped manager reads (#1837 P2a) ---------------------------------

// readySetFromTasks derives the project's ready HINT from a fully
// UNFILTERED task list: backlog + unblocked + unpaused + not cooling —
// the TASK-LEVEL claim predicates. Necessary-not-sufficient: the
// engine's claim choke point additionally gates epic status/DRR quota
// and project-level pause/saturation at claim time. Derived, not a
// second engine read (the list already carries every task-level
// predicate column); rendered only for fully unfiltered lists (a
// stage OR failed filter would hide ready rows).
func readySetFromTasks(tasks []board.Task) []string {
	var ready []string
	for _, t := range tasks {
		if t.Stage == board.StageBacklog && !t.IsBlocked && !t.Paused &&
			(t.CooldownUntil == nil || t.CooldownUntil.Before(time.Now())) {
			ready = append(ready, t.ShortID)
		}
	}
	return ready
}

// BoardManagerListTasks is board_list: one project's every task in one
// call — stage, claim (assignee), lease expiry, paused, blocked — plus
// the derived ready set. Workspace-scoped (a foreign workspace's
// project is a plain 404); open to any in-workspace session (the
// #1747 §5 manager grant is repealed, ruling 2026-10-27 /
// tsk-copper-refectory-generates).
func (h *PodAutomationHandler) BoardManagerListTasks(c *gin.Context) {
	_, ws, _, _, ok := h.resolveManager(c)
	if !ok {
		return
	}
	stage := c.Query("stage")
	if stage != "" && !boardStages()[stage] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "stage filter must be one of the wire stages: " + board.WireStageVocabulary(), "code": board.GuardInput})
		return
	}
	failed := c.Query("failed")
	if failed != "" && failed != "true" && failed != "false" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed filter must be true or false", "code": board.GuardInput})
		return
	}
	tasks, p, err := h.boardMgr.ListTasksInWorkspace(c.Request.Context(), ws, c.Param("id"), "", stage, failed)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	// RULING (#1864 review round 2): the bulk projection never carries
	// live claim tokens — a manager reader gets assignee+lease columns
	// (scheduling truth); the token stays on the per-task read where
	// the claimant's own flow needs it (the #1832 dogfood logged a
	// manager lifting a token off a bulk read; bulk surface = no tokens).
	for i := range tasks {
		tasks[i].ClaimToken = ""
	}
	ready := []string{}
	if stage == "" && failed == "" {
		ready = readySetFromTasks(tasks)
	}
	c.JSON(http.StatusOK, gin.H{
		"project_id": p.ShortID,
		"project":    p,
		"tasks":      tasks,
		"ready_set":  ready,
		"count":      len(tasks),
	})
}

// BoardManagerHealth is board_health: the workspace's board health
// snapshot (active leases with expiry, cooldowns, pending origins,
// reclaims, per-project slots). Workspace-scoped; open to any
// in-workspace session (ruling 2026-10-27 /
// tsk-copper-refectory-generates).
func (h *PodAutomationHandler) BoardManagerHealth(c *gin.Context) {
	_, ws, _, _, ok := h.resolveManager(c)
	if !ok {
		return
	}
	health, err := h.boardMgr.BoardHealthInWorkspace(c.Request.Context(), ws)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"health": health})
}

// --- Resource pressure telemetry (#1747 round 3) --------------------------------

// boardPressureSink receives the pod's pressure samples.
type boardPressureSink interface {
	ReportResourcePressure(ctx context.Context, workspaceID string, sample board.PressureSample) error
}

// BoardResourcePressure is agentd's push endpoint: the pod reports its
// CPU/memory/disk state (thresholds + hysteresis computed reporter-side
// with static defaults); the engine stops claiming into pressured
// workspaces and surfaces the informational note on reads.
func (h *PodAutomationHandler) BoardResourcePressure(c *gin.Context) {
	raw, ws, _, ok := h.resolve(c)
	if !ok {
		return
	}
	sink, hasSink := h.boardMgr.(boardPressureSink)
	if !hasSink {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "board pressure reporting is not wired"})
		return
	}
	var sample board.PressureSample
	if !bindBoardBody(c, raw, &sample) {
		return
	}
	if err := sink.ReportResourcePressure(c.Request.Context(), ws, sample); err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "recorded"})
}
