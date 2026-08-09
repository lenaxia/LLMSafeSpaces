// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// Epic 64: Workflow CRUD handlers (user | org scope).
//
// One file covers both scopes because the logic differs only in
// (ownerType, ownerID) resolution. Authz is enforced by the router's
// middleware chain (OrgAdminGuard / auth).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// workflowStore is the narrow DB interface the handler depends on.
type workflowStore interface {
	CreateWorkflow(ctx context.Context, row *wf.WorkflowRow) error
	ListWorkflows(ctx context.Context, ownerType, ownerID string) ([]*wf.WorkflowRow, error)
	GetWorkflow(ctx context.Context, ownerType, ownerID, workflowID string) (*wf.WorkflowRow, error)
	UpdateWorkflow(ctx context.Context, ownerType, ownerID, workflowID string, upd *wf.WorkflowUpdate) (*wf.WorkflowRow, error)
	DeleteWorkflow(ctx context.Context, ownerType, ownerID, workflowID string) error
	CountWorkflowsByOwner(ctx context.Context, ownerType, ownerID string) (int, error)
	CreateWorkflowRun(ctx context.Context, row *wf.WorkflowRunRow) error
	GetWorkflowRun(ctx context.Context, runID string) (*wf.WorkflowRunRow, error)
	UpdateWorkflowRunStatus(ctx context.Context, runID, status string, errorCode *string, errMsg json.RawMessage, output json.RawMessage) error
	ListWorkflowRuns(ctx context.Context, workflowID string, limit, offset int) ([]*wf.WorkflowRunRow, error)
	ListNodeRuns(ctx context.Context, workflowRunID string) ([]*wf.WorkflowNodeRunRow, error)
	ListWorkflowRunsByWorkspace(ctx context.Context, workspaceID string) ([]*wf.WorkflowRunRow, error)
	ListSessionOrigins(ctx context.Context, workspaceID string) ([]*wf.SessionOriginRow, error)
}

// workflowQuotaChecker reads instance settings for quota enforcement.
type workflowQuotaChecker interface {
	GetInt(ctx context.Context, key string) (int, error)
}

// workflowAuditLogger is the audit interface for workflow CRUD events.
type workflowAuditLogger interface {
	LogAuditEvent(ctx context.Context, domain, actorID, action, targetID string, orgID *string, metadata map[string]any) error
	LogOrgEvent(ctx context.Context, orgID, actorID, action, targetID string, metadata map[string]any) error
}

// WorkflowsHandler handles workflow CRUD for both user and org scopes.
type WorkflowsHandler struct {
	store workflowStore
	quota workflowQuotaChecker
	audit workflowAuditLogger
}

// NewUserWorkflowsHandler constructs a handler for user-scope workflows.
func NewUserWorkflowsHandler(store workflowStore, quota workflowQuotaChecker) *WorkflowsHandler {
	return &WorkflowsHandler{store: store, quota: quota}
}

// NewOrgWorkflowsHandler constructs a handler for org-scope workflows.
func NewOrgWorkflowsHandler(store workflowStore, quota workflowQuotaChecker) *WorkflowsHandler {
	return &WorkflowsHandler{store: store, quota: quota}
}

// SetAudit wires the audit logger (deferred injection — may be nil at construction).
func (h *WorkflowsHandler) SetAudit(a workflowAuditLogger) { h.audit = a }

// GetStore returns the underlying workflow store (for session origin queries).
func (h *WorkflowsHandler) GetStore() workflowStore { return h.store }

// --- User (personal) endpoints ---

func (h *WorkflowsHandler) UserList(c *gin.Context) {
	userID := c.GetString("userID")
	h.list(c, types.WorkflowOwnerUser, userID)
}

func (h *WorkflowsHandler) UserCreate(c *gin.Context) {
	userID := c.GetString("userID")

	if err := h.checkQuota(c, types.WorkflowOwnerUser, userID, "workflows.maxPerUser"); err != nil {
		return
	}

	h.create(c, types.WorkflowOwnerUser, userID)
}

func (h *WorkflowsHandler) UserGet(c *gin.Context) {
	userID := c.GetString("userID")
	h.get(c, types.WorkflowOwnerUser, userID)
}

func (h *WorkflowsHandler) UserUpdate(c *gin.Context) {
	userID := c.GetString("userID")
	h.update(c, types.WorkflowOwnerUser, userID)
}

func (h *WorkflowsHandler) UserDelete(c *gin.Context) {
	userID := c.GetString("userID")
	h.del(c, types.WorkflowOwnerUser, userID)
}

// --- Org endpoints ---

func (h *WorkflowsHandler) OrgList(c *gin.Context) {
	orgID := c.Param("id")
	h.list(c, types.WorkflowOwnerOrg, orgID)
}

func (h *WorkflowsHandler) OrgCreate(c *gin.Context) {
	orgID := c.Param("id")

	if err := h.checkQuota(c, types.WorkflowOwnerOrg, orgID, "workflows.maxPerOrg"); err != nil {
		return
	}

	actorID := c.GetString("userID")
	h.createWithAudit(c, types.WorkflowOwnerOrg, orgID, actorID)
}

func (h *WorkflowsHandler) OrgGet(c *gin.Context) {
	orgID := c.Param("id")
	h.get(c, types.WorkflowOwnerOrg, orgID)
}

func (h *WorkflowsHandler) OrgUpdate(c *gin.Context) {
	orgID := c.Param("id")
	h.update(c, types.WorkflowOwnerOrg, orgID)
}

func (h *WorkflowsHandler) OrgDelete(c *gin.Context) {
	orgID := c.Param("id")
	h.del(c, types.WorkflowOwnerOrg, orgID)
}

// --- shared CRUD ---

func (h *WorkflowsHandler) list(c *gin.Context, ownerType, ownerID string) {
	rows, err := h.store.ListWorkflows(c.Request.Context(), ownerType, ownerID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list workflows"})
		return
	}
	out := make([]types.WorkflowResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, workflowRowToResponse(r))
	}
	c.JSON(http.StatusOK, gin.H{"workflows": out})
}

func (h *WorkflowsHandler) create(c *gin.Context, ownerType, ownerID string) {
	h.createWithAudit(c, ownerType, ownerID, c.GetString("userID"))
}

func (h *WorkflowsHandler) createWithAudit(c *gin.Context, ownerType, ownerID, actorID string) {
	var req types.CreateWorkflowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if !types.ValidWorkflowName(req.Name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workflow name"})
		return
	}

	slug := req.Slug
	if slug == "" {
		slug = slugify(req.Name)
	}
	if !types.ValidWorkflowSlug(slug) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workflow slug"})
		return
	}

	status := req.Status
	if status == "" {
		status = types.WorkflowStatusDraft
	}
	if !types.ValidWorkflowStatus(status) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workflow status"})
		return
	}

	spec, err := wf.ParseSpec(json.RawMessage(extractSpecJSON(req.SpecYAML)))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid workflow spec: %v", err)})
		return
	}

	var defaults wf.DefaultsBlock
	if len(req.Defaults) > 0 {
		if err := json.Unmarshal(req.Defaults, &defaults); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid defaults block: %v", err)})
			return
		}
	}

	valErrs := wf.ValidateSpec(spec, nil, defaults)
	if len(valErrs) > 0 {
		details := make([]string, len(valErrs))
		for i, e := range valErrs {
			details[i] = e.Error()
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "workflow spec validation failed", "details": details})
		return
	}

	specJSON, err := json.Marshal(spec)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to marshal validated spec"})
		return
	}

	var targetWS *string
	if req.TargetWorkspaceID != "" {
		targetWS = &req.TargetWorkspaceID
	}

	onMissing := req.OnMissingWorkspace
	if onMissing == "" {
		onMissing = types.OnMissingAbort
	}
	if !types.ValidOnMissingWorkspace(onMissing) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid onMissingWorkspace (must be 'abort' or 'create')"})
		return
	}

	now := time.Now().UTC()
	row := &wf.WorkflowRow{
		ID: uuid.New().String(), OwnerType: ownerType, OwnerID: ownerID,
		Name: req.Name, Slug: slug, Description: req.Description,
		SpecYAML: req.SpecYAML, SpecJSON: specJSON,
		InputSchema: req.InputSchema, TargetWorkspaceID: targetWS,
		OnMissingWorkspace: onMissing,
		Status:             status, Defaults: req.Defaults,
		CreatedAt: now, UpdatedAt: now,
	}

	if err := h.store.CreateWorkflow(c.Request.Context(), row); err != nil {
		if isWorkflowUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "a workflow with this name or slug already exists"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create workflow"})
		return
	}

	h.logCreate(c, ownerType, ownerID, actorID, row.ID, req.Name)
	c.JSON(http.StatusCreated, workflowRowToResponse(row))
}

func (h *WorkflowsHandler) get(c *gin.Context, ownerType, ownerID string) {
	workflowID := c.Param("id")
	row, err := h.store.GetWorkflow(c.Request.Context(), ownerType, ownerID, workflowID)
	if err != nil {
		if errors.Is(err, wf.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get workflow"})
		return
	}
	c.JSON(http.StatusOK, workflowRowToResponse(row))
}

func (h *WorkflowsHandler) update(c *gin.Context, ownerType, ownerID string) {
	workflowID := c.Param("id")
	var req types.UpdateWorkflowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	upd := &wf.WorkflowUpdate{
		Name: req.Name, Slug: req.Slug, Description: req.Description,
		SpecYAML: req.SpecYAML, InputSchema: req.InputSchema,
		TargetWorkspaceID: req.TargetWorkspaceID, OnMissingWorkspace: req.OnMissingWorkspace,
		Status: req.Status, Defaults: req.Defaults,
	}

	if req.Name != nil {
		if !types.ValidWorkflowName(*req.Name) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workflow name"})
			return
		}
	}
	if req.Slug != nil {
		if !types.ValidWorkflowSlug(*req.Slug) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workflow slug"})
			return
		}
	}
	if req.Status != nil {
		if !types.ValidWorkflowStatus(*req.Status) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workflow status"})
			return
		}
	}
	if req.OnMissingWorkspace != nil {
		if !types.ValidOnMissingWorkspace(*req.OnMissingWorkspace) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid onMissingWorkspace (must be 'abort' or 'create')"})
			return
		}
	}

	// Re-validate spec if it changed.
	if req.SpecYAML != nil {
		existing, err := h.store.GetWorkflow(c.Request.Context(), ownerType, ownerID, workflowID)
		if err != nil {
			if errors.Is(err, wf.ErrNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch existing workflow"})
			return
		}
		spec, err := wf.ParseSpec(json.RawMessage(extractSpecJSON(*req.SpecYAML)))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid workflow spec: %v", err)})
			return
		}
		var defaults wf.DefaultsBlock
		if len(req.Defaults) > 0 {
			_ = json.Unmarshal(req.Defaults, &defaults)
		} else if len(existing.Defaults) > 0 {
			_ = json.Unmarshal(existing.Defaults, &defaults)
		}
		valErrs := wf.ValidateSpec(spec, nil, defaults)
		if len(valErrs) > 0 {
			details := make([]string, len(valErrs))
			for i, e := range valErrs {
				details[i] = e.Error()
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": "workflow spec validation failed", "details": details})
			return
		}
		specJSON, err := json.Marshal(spec)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to marshal validated spec"})
			return
		}
		upd.SpecJSON = specJSON
	}

	row, err := h.store.UpdateWorkflow(c.Request.Context(), ownerType, ownerID, workflowID, upd)
	if err != nil {
		if errors.Is(err, wf.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
			return
		}
		if isWorkflowUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "a workflow with this name or slug already exists"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update workflow"})
		return
	}

	c.JSON(http.StatusOK, workflowRowToResponse(row))
}

func (h *WorkflowsHandler) del(c *gin.Context, ownerType, ownerID string) {
	workflowID := c.Param("id")
	if err := h.store.DeleteWorkflow(c.Request.Context(), ownerType, ownerID, workflowID); err != nil {
		if errors.Is(err, wf.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete workflow"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// --- quota ---

func (h *WorkflowsHandler) checkQuota(c *gin.Context, ownerType, ownerID, settingKey string) error {
	if h.quota == nil {
		return nil
	}
	maxCount, err := h.quota.GetInt(c.Request.Context(), settingKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check workflow quota"})
		return err
	}
	if maxCount == 0 {
		return nil
	}
	count, err := h.store.CountWorkflowsByOwner(c.Request.Context(), ownerType, ownerID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count workflows"})
		return err
	}
	if count >= maxCount {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("workflow limit reached (%d/%d)", count, maxCount)})
		return fmt.Errorf("quota exceeded")
	}
	return nil
}

// --- audit ---

func (h *WorkflowsHandler) logCreate(c *gin.Context, ownerType, ownerID, actorID, workflowID, name string) {
	if h.audit == nil {
		return
	}
	meta := map[string]any{"name": name, "ownerType": ownerType}
	if ownerType == types.WorkflowOwnerOrg {
		_ = h.audit.LogOrgEvent(c.Request.Context(), ownerID, actorID, "workflow.create", workflowID, meta)
	} else {
		_ = h.audit.LogAuditEvent(c.Request.Context(), "workflows", actorID, "workflow.create", workflowID, nil, meta)
	}
}

// --- helpers ---

func workflowRowToResponse(r *wf.WorkflowRow) types.WorkflowResponse {
	resp := types.WorkflowResponse{
		ID:                 r.ID,
		OwnerType:          r.OwnerType,
		Name:               r.Name,
		Slug:               r.Slug,
		Description:        r.Description,
		SpecYAML:           r.SpecYAML,
		InputSchema:        r.InputSchema,
		OnMissingWorkspace: r.OnMissingWorkspace,
		Status:             r.Status,
		Defaults:           r.Defaults,
		CreatedAt:          r.CreatedAt,
		UpdatedAt:          r.UpdatedAt,
	}
	if r.OwnerID != "" && r.OwnerID != "_platform" {
		resp.OwnerID = r.OwnerID
	}
	if r.TargetWorkspaceID != nil {
		resp.TargetWorkspaceID = *r.TargetWorkspaceID
	}
	return resp
}

var slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = slugNonAlnum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "workflow"
	}
	return s
}

// extractSpecJSON wraps a YAML spec string as a JSON string for ParseSpec.
// In v1, spec_yaml is expected to be JSON (the YAML editor sends JSON via the
// API). If the content is already valid JSON, it passes through; if it's YAML,
// it will fail ParseSpec with a clear error (YAML parsing is a frontend concern).
func extractSpecJSON(specYAML string) string {
	trimmed := strings.TrimSpace(specYAML)
	if trimmed == "" {
		return "{}"
	}
	// If it starts with { or [, it's JSON — pass through.
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return trimmed
	}
	// Not JSON — wrap in a minimal object so ParseSpec produces a clear error.
	return trimmed
}

func isWorkflowUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate") || strings.Contains(msg, "violates unique constraint")
}

// --- Run management endpoints ---

// UserRunWorkflow starts a manual run for a user-scope workflow.
func (h *WorkflowsHandler) UserRunWorkflow(c *gin.Context) {
	userID := c.GetString("userID")
	h.runWorkflow(c, types.WorkflowOwnerUser, userID)
}

func (h *WorkflowsHandler) OrgRunWorkflow(c *gin.Context) {
	orgID := c.Param("id")
	h.runWorkflow(c, types.WorkflowOwnerOrg, orgID)
}

func (h *WorkflowsHandler) runWorkflow(c *gin.Context, ownerType, ownerID string) {
	workflowID := c.Param("id")
	var req types.CreateWorkflowRunRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		if err.Error() != "EOF" {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}

	wfRow, err := h.store.GetWorkflow(c.Request.Context(), ownerType, ownerID, workflowID)
	if err != nil {
		if errors.Is(err, wf.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get workflow"})
		return
	}

	workspaceID := ""
	if req.WorkspaceID != "" {
		workspaceID = req.WorkspaceID
	} else if wfRow.TargetWorkspaceID != nil {
		workspaceID = *wfRow.TargetWorkspaceID
	}
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace_id is required (no target_workspace_id set on workflow)"})
		return
	}

	now := time.Now().UTC()
	run := &wf.WorkflowRunRow{
		ID: uuid.New().String(), WorkflowID: workflowID,
		SpecSnapshot: wfRow.SpecJSON, Input: req.Input,
		Status: types.RunStatusQueued, WorkspaceID: workspaceID,
		CreatedAt: now, UpdatedAt: now,
	}

	if err := h.store.CreateWorkflowRun(c.Request.Context(), run); err != nil {
		if errors.Is(err, wf.ErrConcurrentRun) {
			c.Header("Retry-After", "30")
			c.JSON(http.StatusConflict, gin.H{"error": "already_running"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create run"})
		return
	}

	c.JSON(http.StatusAccepted, workflowRunRowToResponse(run))
}

// GetRun returns a single run by ID (GET /runs/:runId).
func (h *WorkflowsHandler) GetRun(c *gin.Context) {
	runID := c.Param("runId")
	run, err := h.store.GetWorkflowRun(c.Request.Context(), runID)
	if err != nil {
		if errors.Is(err, wf.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get run"})
		return
	}
	c.JSON(http.StatusOK, workflowRunRowToResponse(run))
}

// GetRunNodes returns per-node status for a run (GET /runs/:runId/nodes).
func (h *WorkflowsHandler) GetRunNodes(c *gin.Context) {
	runID := c.Param("runId")
	nodes, err := h.store.ListNodeRuns(c.Request.Context(), runID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list nodes"})
		return
	}
	out := make([]types.WorkflowNodeRunResponse, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, nodeRunRowToResponse(n))
	}
	c.JSON(http.StatusOK, gin.H{"nodes": out})
}

// CancelRun cancels a running workflow (POST /runs/:runId/cancel).
func (h *WorkflowsHandler) CancelRun(c *gin.Context) {
	runID := c.Param("runId")
	run, err := h.store.GetWorkflowRun(c.Request.Context(), runID)
	if err != nil {
		if errors.Is(err, wf.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get run"})
		return
	}
	if types.IsTerminalRunStatus(run.Status) {
		c.JSON(http.StatusConflict, gin.H{"error": "run is already in terminal state: " + run.Status})
		return
	}
	ec := types.RunErrorCodeCanceled
	if err := h.store.UpdateWorkflowRunStatus(c.Request.Context(), runID, types.RunStatusCanceled, &ec, nil, nil); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to cancel run"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"canceled": true})
}

// ListActiveRunsByWorkspace returns non-terminal runs for a workspace.
// GET /api/v1/workspaces/:workspaceId/runs/active
func (h *WorkflowsHandler) ListActiveRunsByWorkspace(c *gin.Context) {
	workspaceID := c.Param("workspaceId")
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspaceId required"})
		return
	}
	runs, err := h.store.ListWorkflowRunsByWorkspace(c.Request.Context(), workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list active runs"})
		return
	}
	out := make([]types.WorkflowRunResponse, 0, len(runs))
	for _, r := range runs {
		out = append(out, workflowRunRowToResponse(r))
	}
	c.JSON(http.StatusOK, gin.H{"runs": out})
}

// UserListRuns lists runs for a user-scope workflow.
func (h *WorkflowsHandler) UserListRuns(c *gin.Context) {
	userID := c.GetString("userID")
	h.listRuns(c, types.WorkflowOwnerUser, userID)
}

func (h *WorkflowsHandler) OrgListRuns(c *gin.Context) {
	orgID := c.Param("id")
	h.listRuns(c, types.WorkflowOwnerOrg, orgID)
}

func (h *WorkflowsHandler) listRuns(c *gin.Context, ownerType, ownerID string) {
	workflowID := c.Param("id")
	limit := 20
	offset := 0
	runs, err := h.store.ListWorkflowRuns(c.Request.Context(), workflowID, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list runs"})
		return
	}
	out := make([]types.WorkflowRunResponse, 0, len(runs))
	for _, r := range runs {
		out = append(out, workflowRunRowToResponse(r))
	}
	c.JSON(http.StatusOK, gin.H{"runs": out})
}

func workflowRunRowToResponse(r *wf.WorkflowRunRow) types.WorkflowRunResponse {
	resp := types.WorkflowRunResponse{
		ID: r.ID, WorkflowID: r.WorkflowID, SpecSnapshot: r.SpecSnapshot,
		Input: r.Input, Output: r.Output, Status: r.Status,
		WorkspaceID: r.WorkspaceID,
		StartedAt:   r.StartedAt, FinishedAt: r.FinishedAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.ErrorCode != nil {
		resp.ErrorCode = *r.ErrorCode
	}
	if r.Error != nil {
		resp.Error = r.Error
	}
	if r.TriggerID != nil {
		resp.TriggerID = *r.TriggerID
	}
	if r.TriggerFireID != nil {
		resp.TriggerFireID = *r.TriggerFireID
	}
	return resp
}

func nodeRunRowToResponse(n *wf.WorkflowNodeRunRow) types.WorkflowNodeRunResponse {
	resp := types.WorkflowNodeRunResponse{
		ID: n.ID, WorkflowRunID: n.WorkflowRunID, NodeID: n.NodeID,
		NodeType: n.NodeType, Status: n.Status, Attempt: n.Attempt,
		Input: n.Input, Output: n.Output,
		StartedAt: n.StartedAt, FinishedAt: n.FinishedAt,
	}
	if n.Branch != nil {
		resp.Branch = *n.Branch
	}
	if n.ErrorCode != nil {
		resp.ErrorCode = *n.ErrorCode
	}
	if n.Error != nil {
		resp.Error = n.Error
	}
	return resp
}
