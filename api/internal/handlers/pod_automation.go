// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/client-go/kubernetes"

	"github.com/lenaxia/llmsafespaces/pkg/interfaces"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// PodAutomationHandler exposes the owner's trigger + workflow CRUD to
// THIS workspace's agent (the agentd automation MCP tools) over the
// pod-identity auth contract — identical to PodWorkspaceRenameHandler:
// TokenReview, SA name/namespace bound to the workspace, owner resolved
// server-side. The pod is the user's own agent acting on the user's own
// automation; a compromised pod gains nothing it didn't already have as
// its owner's agent.
//
// Delegation, not duplication: every request injects the resolved owner
// as `userID` into the gin context and calls the EXISTING
// TriggersHandler/WorkflowsHandler user methods — all validation, quota
// checks, encryption, and audit flow through unchanged. The one
// automation-specific rule lives here: trigger creation forces
// `workspaceId` to this pod's workspace (a pod schedules routines that
// run in ITS workspace, not arbitrary others).
type PodAutomationHandler struct {
	tokenReviewer TokenReviewer
	lookup        bootstrapWorkspaceLookup
	triggers      *TriggersHandler
	workflows     *WorkflowsHandler
	// wfStore resolves workflow targets for trigger-create scoping (#1412):
	// a workflow-firing trigger is only valid when the workflow exists,
	// belongs to the resolved owner, and targets THIS pod's workspace.
	wfStore           automationWorkflowGetter
	expectedNamespace string
	logger            interfaces.LoggerInterface
}

// automationWorkflowGetter is the narrow workflow lookup the create path
// needs for target validation.
type automationWorkflowGetter interface {
	GetWorkflow(ctx context.Context, ownerType, ownerID, workflowID string) (*wf.WorkflowRow, error)
}

// NewPodAutomationHandlerFromClientset is the production constructor
// (shares the TokenReview clientset with the pod-bootstrap family).
func NewPodAutomationHandlerFromClientset(clientset kubernetes.Interface, lookup bootstrapWorkspaceLookup, triggers *TriggersHandler, workflows *WorkflowsHandler, wfStore automationWorkflowGetter, expectedNamespace string) *PodAutomationHandler {
	return &PodAutomationHandler{
		tokenReviewer:     &k8sTokenReviewer{clientset: clientset},
		lookup:            lookup,
		triggers:          triggers,
		workflows:         workflows,
		wfStore:           wfStore,
		expectedNamespace: expectedNamespace,
	}
}

// SetLogger installs the failure-path logger (the pod-bootstrap #407
// precedent; app wiring is test-enforced).
func (h *PodAutomationHandler) SetLogger(l interfaces.LoggerInterface) { h.logger = l }

// HasLogger reports logger wiring (app-level guard).
func (h *PodAutomationHandler) HasLogger() bool { return h.logger != nil }

// captureBody drains the request body ONCE, up front, so every later
// reader (the workspaceID sniff in resolve, the delegated handler's
// bind) works from the same bytes instead of a consumed stream. Returns
// nil for body-less requests.
func captureBody(c *gin.Context) []byte {
	if c.Request == nil || c.Request.Body == nil || c.Request.ContentLength == 0 {
		return nil
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil && len(raw) == 0 {
		return nil // delegated bind (if any) reports; resolve falls back to query
	}
	return raw
}

// resolve authenticates the pod (bearer SA token → TokenReview → SA
// principal bound to this workspace+namespace) and returns the captured
// request body plus the owner userID. The workspace arrives in-query on
// reads/updates/deletes/runs or in-body on creates (the seam stamps
// it); patches and run-inputs stay clean — they carry only their own
// fields. Writes the auth-failure response itself; returns ok=false.
func (h *PodAutomationHandler) resolve(c *gin.Context) (raw []byte, workspaceID, ownerID string, ok bool) {
	token := extractBearerToken(c.GetHeader("Authorization"))
	if token == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authorization"})
		return nil, "", "", false
	}
	username, err := h.tokenReviewer.Review(c.Request.Context(), token)
	if err != nil {
		if errors.Is(err, errTokenNotAuthenticated) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token not authenticated"})
			return nil, "", "", false
		}
		if h.logger != nil {
			h.logger.Error("automation: token review failed", err)
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "token review failed"})
		return nil, "", "", false
	}
	if exp, hasExp := unverifiedJWTExp(token); hasExp && time.Now().After(exp.Add(jwtExpiryLeeway)) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token expired"})
		return nil, "", "", false
	}

	raw = captureBody(c)
	resolved := c.Query("workspaceID")
	if resolved == "" && len(raw) > 0 {
		// Exact-key read: the resolver spelling must not be folded with
		// the DTO's "workspaceId" by the decoder's case-insensitive
		// matching — a caller-supplied DTO value must never satisfy
		// (or trip) the identity check.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err == nil {
			if v, ok := fields["workspaceID"]; ok {
				_ = json.Unmarshal(v, &resolved)
			}
		}
	}
	if resolved == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "workspaceID required (query or body)"})
		return nil, "", "", false
	}

	saNamespace, saWorkspaceID, principalOK := parseSAPrincipal(username)
	if !principalOK || saWorkspaceID != resolved || saNamespace != h.expectedNamespace {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "workspace identity mismatch"})
		return nil, "", "", false
	}

	ws, err := h.lookup.GetWorkspace(c.Request.Context(), resolved)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("automation: lookup failed", err, "workspaceID", resolved)
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "workspace lookup failed"})
		return nil, "", "", false
	}
	if ws == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return nil, "", "", false
	}
	return raw, resolved, ws.UserID, true
}

// delegate re-binds the request as the resolved owner and invokes fn,
// replaying the captured body (if any) onto a fresh reader.
func (h *PodAutomationHandler) delegate(c *gin.Context, ownerID string, replay []byte, fn func(*gin.Context)) {
	if replay != nil {
		c.Request.Body = io.NopCloser(bytes.NewReader(replay))
		c.Request.ContentLength = int64(len(replay))
	}
	c.Set("userID", ownerID)
	fn(c)
}

// forceTriggerWorkspace rewrites the create body's workspaceId to this
// pod's workspace (the automation scoping rule). All other fields pass
// through as raw JSON — no re-formatting, no float64 round-trips.
// Non-object bodies are an error (the seam always sends an object).
func forceTriggerWorkspace(replay []byte, workspaceID string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(replay, &fields); err != nil {
		return nil, err
	}
	stamped, err := json.Marshal(workspaceID)
	if err != nil {
		return nil, err
	}
	fields["workspaceId"] = stamped
	return json.Marshal(fields)
}

// --- Trigger routes -------------------------------------------------------

func (h *PodAutomationHandler) TriggerList(c *gin.Context) {
	if _, _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, nil, h.triggers.UserList)
	}
}

func (h *PodAutomationHandler) TriggerCreate(c *gin.Context) {
	raw, ws, owner, ok := h.resolve(c)
	if !ok {
		return
	}
	body, err := normalizeTriggerCreateBody(raw)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// #1412: a workflow-firing trigger carries workflowId — no routine
	// workspace stamp (the user API rejects both set), but the workflow
	// MUST exist, belong to the resolved owner, and target THIS pod's
	// workspace (the same pod-scoping rule routines get by forcing).
	if workflowID := workflowIDOf(body); workflowID != "" {
		if err := h.authorizeWorkflowTarget(c, owner, ws, workflowID); err != nil {
			return // response written
		}
	} else {
		if body, err = forceTriggerWorkspace(body, ws); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "trigger body must be a JSON object"})
			return
		}
	}
	h.delegate(c, owner, body, h.triggers.UserCreate)
}

// authorizeWorkflowTarget enforces the pod-scoping rule for DAG-firing
// triggers: the target workflow must exist, be owned by the resolved
// owner, and run in THIS pod's workspace. Writes the failure response.
func (h *PodAutomationHandler) authorizeWorkflowTarget(c *gin.Context, owner, workspaceID, workflowID string) error {
	if h.wfStore == nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "workflow lookup not configured"})
		return errWorkflowLookupUnconfigured
	}
	row, err := h.wfStore.GetWorkflow(c.Request.Context(), "user", owner, workflowID)
	switch {
	case err != nil && errors.Is(err, wf.ErrNotFound):
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "workflow not found"})
		return err
	case err != nil:
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "workflow lookup failed"})
		return err
	case row.TargetWorkspaceID == nil || *row.TargetWorkspaceID != workspaceID:
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "workflow targets a different workspace — this pod cannot schedule it"})
		return errWorkflowTargetMismatch
	}
	return nil
}

var errWorkflowTargetMismatch = errors.New("workflow target workspace mismatch")
var errWorkflowLookupUnconfigured = errors.New("workflow lookup not configured")

// workflowIDOf reads the (normalized) workflowId from the create body.
func workflowIDOf(body []byte) string {
	var fields struct {
		WorkflowID string `json:"workflowId"`
	}
	_ = json.Unmarshal(body, &fields)
	return strings.TrimSpace(fields.WorkflowID)
}

// normalizeTriggerCreateBody prepares the create body for the delegated
// user handler (#1412): (a) strips the resolver-spelling workspaceID —
// it exists only for the identity sniff, and Go's case-insensitive
// decoder would otherwise fold it into the DTO's workspaceId and
// collide with workflowId ("cannot set both"); (b) aliases snake_case
// workflow_id onto the DTO's workflowId — the decoder silently drops
// unknown fields, so the caller's intent vanished without an error.
func normalizeTriggerCreateBody(raw []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	delete(fields, "workspaceID")
	if snake, hasSnake := fields["workflow_id"]; hasSnake {
		delete(fields, "workflow_id")
		if _, exists := fields["workflowId"]; !exists {
			fields["workflowId"] = snake
		}
	}
	return json.Marshal(fields)
}

func (h *PodAutomationHandler) TriggerGet(c *gin.Context) {
	if _, _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, nil, h.triggers.UserGet)
	}
}

func (h *PodAutomationHandler) TriggerUpdate(c *gin.Context) {
	if raw, _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, raw, h.triggers.UserUpdate)
	}
}

func (h *PodAutomationHandler) TriggerDelete(c *gin.Context) {
	if _, _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, nil, h.triggers.UserDelete)
	}
}

func (h *PodAutomationHandler) TriggerFires(c *gin.Context) {
	if _, _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, nil, h.triggers.UserListFires)
	}
}

// TriggerRotateWebhookSecret rotates a webhook trigger's HMAC secret and
// returns {webhookSecret, webhookUrl} to the pod's agent — the owner's
// own credential, delivered to the owner's own agent (the same secret
// the owner would see rotating via the user API).
func (h *PodAutomationHandler) TriggerRotateWebhookSecret(c *gin.Context) {
	if _, _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, nil, h.triggers.UserRotateWebhookSecret)
	}
}

// --- Workflow routes ------------------------------------------------------

func (h *PodAutomationHandler) WorkflowList(c *gin.Context) {
	if _, _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, nil, h.workflows.UserList)
	}
}

func (h *PodAutomationHandler) WorkflowCreate(c *gin.Context) {
	if raw, _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, raw, h.workflows.UserCreate)
	}
}

func (h *PodAutomationHandler) WorkflowGet(c *gin.Context) {
	if _, _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, nil, h.workflows.UserGet)
	}
}

func (h *PodAutomationHandler) WorkflowUpdate(c *gin.Context) {
	if raw, _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, raw, h.workflows.UserUpdate)
	}
}

func (h *PodAutomationHandler) WorkflowDelete(c *gin.Context) {
	if _, _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, nil, h.workflows.UserDelete)
	}
}

func (h *PodAutomationHandler) WorkflowRun(c *gin.Context) {
	if raw, _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, raw, h.workflows.UserRunWorkflow)
	}
}

func (h *PodAutomationHandler) WorkflowRuns(c *gin.Context) {
	if _, _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, nil, h.workflows.UserListRuns)
	}
}
