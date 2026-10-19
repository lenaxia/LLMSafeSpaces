// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// board_automation_manager.go — the #1747 manager half of the Task
// Board's internal automation seam: the same pod-identity resolve
// (TokenReview → SA principal → workspace pin) the worker routes ride,
// carrying the FULL manager set at project, epic, and task level.
// Design 0067 §4.3c ("agents are workers, not managers") is superseded
// in the manager direction: an in-workspace agent manages its OWN
// workspace's board, scoped structurally by the caller-workspace
// binding the engine enforces (requireManagerAction).
//
// Grant differentiation (#1747 §5), belt tier: agentd's spawn-time
// worker-session registry is the first gate; here the engine-authoritative
// second — a manager route called BY a session that currently holds a
// board worker role (live assignee) is refused. The session id rides
// the body (sessionId — the harness-attested injected identity, the
// send_message pattern) and also stamps the audit actor.

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// boardGrantBelt is the engine-authoritative worker check (satisfied by
// *board.Service).
type boardGrantBelt interface {
	IsWorkerSession(ctx context.Context, sessionID string) (bool, error)
}

// boardSessionID extracts the calling session's identity: the query
// first (body-less DELETEs), then the exact-key body probe (the
// resolver-spelling discipline: exact key, no case folding).
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

// managerActor stamps the #1747 manager delegation: the pod's resolved
// owner (R7), the agent surface, the resolved workspace (the structural
// binding), and the creating SESSION (audit attribution).
func (h *PodAutomationHandler) managerActor(ownerID, workspaceID, sessionID string) board.Actor {
	return board.Actor{
		Kind: board.ActorUser, UserID: ownerID, Surface: board.SurfaceAgent,
		WorkspaceID: workspaceID, SessionID: sessionID,
	}
}

// resolveManager runs the pod resolve + the grant belt. Writes the
// refusal itself; ok=false on failure.
func (h *PodAutomationHandler) resolveManager(c *gin.Context) (raw []byte, workspaceID, ownerID, sessionID string, ok bool) {
	raw, workspaceID, ownerID, ok = h.resolve(c)
	if !ok {
		return nil, "", "", "", false
	}
	sessionID = boardSessionID(c, raw)
	if belt, hasBelt := h.boardMgr.(boardGrantBelt); hasBelt && sessionID != "" {
		if worker, err := belt.IsWorkerSession(c.Request.Context(), sessionID); err == nil && worker {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "board manager tools attach to user-collaborator sessions, not spawned workers (ruling #1747 §5)",
				"code":  board.GuardManagerSurface,
			})
			return nil, "", "", "", false
		}
	}
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
	c.JSON(http.StatusCreated, p)
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
	c.JSON(http.StatusOK, p)
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
	c.JSON(http.StatusCreated, e)
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
	c.JSON(http.StatusOK, e)
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
		RequestID          string                      `json:"requestId"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	t, err := h.boardMgr.CreateTask(c.Request.Context(), h.managerActor(owner, ws, session), c.Param("id"), "",
		board.TaskInput{Title: req.Title, Description: req.Description, Tags: req.Tags,
			AcceptanceCriteria: req.AcceptanceCriteria, DependsOn: req.DependsOn}, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusCreated, t)
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
		SetCriteriaMet     *int                        `json:"setCriteriaMet"`
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
	c.JSON(http.StatusOK, t)
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

// BoardImportPlan materializes (dryRun validates) an entire plan.
func (h *PodAutomationHandler) BoardImportPlan(c *gin.Context) {
	raw, ws, owner, session, ok := h.resolveManager(c)
	if !ok {
		return
	}
	var req struct {
		EpicTitle string `json:"epicTitle"`
		Outcome   string `json:"outcome"`
		DryRun    bool   `json:"dryRun"`
		Tasks     []struct {
			Key                string                      `json:"key"`
			Title              string                      `json:"title"`
			Description        string                      `json:"description"`
			Tags               []string                    `json:"tags"`
			AcceptanceCriteria []board.AcceptanceCriterion `json:"acceptanceCriteria"`
			DependsOn          []string                    `json:"dependsOn"`
		} `json:"tasks"`
		RequestID string `json:"requestId"`
	}
	if !bindBoardBody(c, raw, &req) {
		return
	}
	in := board.PlanInput{EpicTitle: req.EpicTitle, Outcome: req.Outcome}
	for _, t := range req.Tasks {
		in.Tasks = append(in.Tasks, board.PlanTaskInput{
			Key: t.Key, Title: t.Title, Description: t.Description, Tags: t.Tags,
			AcceptanceCriteria: t.AcceptanceCriteria, DependsOn: t.DependsOn,
		})
	}
	out, err := h.boardMgr.ImportPlan(c.Request.Context(), h.managerActor(owner, ws, session), c.Param("id"), "",
		in, req.DryRun, req.RequestID)
	if err != nil {
		renderBoardError(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
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
