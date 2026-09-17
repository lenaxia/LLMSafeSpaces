// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	tokenReviewer     TokenReviewer
	lookup            bootstrapWorkspaceLookup
	triggers          *TriggersHandler
	workflows         *WorkflowsHandler
	expectedNamespace string
	logger            interfaces.LoggerInterface
}

// NewPodAutomationHandlerFromClientset is the production constructor
// (shares the TokenReview clientset with the pod-bootstrap family).
func NewPodAutomationHandlerFromClientset(clientset kubernetes.Interface, lookup bootstrapWorkspaceLookup, triggers *TriggersHandler, workflows *WorkflowsHandler, expectedNamespace string) *PodAutomationHandler {
	return &PodAutomationHandler{
		tokenReviewer:     &k8sTokenReviewer{clientset: clientset},
		lookup:            lookup,
		triggers:          triggers,
		workflows:         workflows,
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

// scopeTriggerCreateBody applies the automation scoping rule to a trigger
// create body (#1412). Routine triggers get `workspaceId` force-stamped to
// this pod's workspace (unchanged behavior). Workflow-targeted triggers
// (workflowId present — the snake_case workflow_id alias is normalized)
// pass through WITHOUT the stamp: the create DTO rejects
// workflowId+workspaceId together, which previously made DAG triggers
// impossible to create from this surface. The returned workflowID lets the
// caller enforce that the referenced workflow targets this workspace.
// All other fields pass through as raw JSON — no re-formatting, no
// float64 round-trips. Non-object bodies are an error (the seam always
// sends an object).
func scopeTriggerCreateBody(replay []byte, workspaceID string) (body []byte, workflowID string, err error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(replay, &fields); err != nil {
		return nil, "", fmt.Errorf("trigger body must be a JSON object: %w", err)
	}
	// A JSON `null` body unmarshals into a nil map WITHOUT error — guard
	// before the stamp below writes into it (nil-map assignment panics,
	// and Gin recovery would turn it into an opaque 500).
	if fields == nil {
		return nil, "", fmt.Errorf("trigger body must be a JSON object")
	}
	// Normalize the snake_case alias onto the DTO spelling — the json
	// decoder ignores unknown fields, so a caller using workflow_id would
	// otherwise lose the linkage silently. Contradictory duplicates are
	// an explicit error, never a silent pick.
	if alias, ok := fields["workflow_id"]; ok {
		if _, exists := fields["workflowId"]; exists {
			var aliasVal, canonicalVal string
			_ = json.Unmarshal(alias, &aliasVal)
			_ = json.Unmarshal(fields["workflowId"], &canonicalVal)
			if aliasVal != canonicalVal {
				return nil, "", fmt.Errorf("workflow_id and workflowId both present with different values")
			}
		} else {
			fields["workflowId"] = alias
		}
		delete(fields, "workflow_id")
	}
	if raw, ok := fields["workflowId"]; ok {
		if err := json.Unmarshal(raw, &workflowID); err != nil {
			return nil, "", fmt.Errorf("workflowId must be a string")
		}
	}
	// Strip EVERY caller-supplied workspace key, case-insensitively —
	// Go's decoder binds DTO fields case-insensitively and map keys
	// marshal in sorted order, so any case variant of workspaceId (e.g.
	// "workspaceid") left in the body would deterministically override
	// the forced stamp below (review finding on #1412).
	for k := range fields {
		if strings.EqualFold(k, "workspaceId") {
			delete(fields, k)
		}
	}
	if workflowID != "" {
		out, err := json.Marshal(fields)
		return out, workflowID, err
	}
	stamped, err := json.Marshal(workspaceID)
	if err != nil {
		return nil, "", err
	}
	fields["workspaceId"] = stamped
	out, err := json.Marshal(fields)
	return out, "", err
}

// --- Trigger routes -------------------------------------------------------

func (h *PodAutomationHandler) TriggerList(c *gin.Context) {
	if _, _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, nil, h.triggers.UserList)
	}
}

// TriggerCreate delegates trigger creation as the resolved owner. Routine
// triggers are workspace-stamped; workflow-targeted triggers additionally
// require the referenced workflow to exist, belong to the owner, and
// target THIS pod's workspace (the DAG equivalent of the routine scoping
// rule — a pod must not schedule DAGs that run in other workspaces).
func (h *PodAutomationHandler) TriggerCreate(c *gin.Context) {
	raw, ws, owner, ok := h.resolve(c)
	if !ok {
		return
	}
	forced, workflowID, err := scopeTriggerCreateBody(raw, ws)
	if err != nil {
		// Surface the specific scoping error (non-string workflowId,
		// contradictory aliases, non-object body) — a generic message
		// here misleads exactly the agents this surface serves.
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if workflowID != "" {
		wfRow, gerr := h.workflows.GetWorkflowRow(c.Request.Context(), "user", owner, workflowID)
		if gerr != nil {
			if errors.Is(gerr, wf.ErrNotFound) {
				c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "workflow not found"})
				return
			}
			if h.logger != nil {
				h.logger.Error("automation: workflow lookup failed", gerr, "workflowID", workflowID)
			}
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "workflow lookup failed"})
			return
		}
		if wfRow.TargetWorkspaceID == nil || *wfRow.TargetWorkspaceID != ws {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "workflow does not target this workspace"})
			return
		}
	}
	h.delegate(c, owner, forced, h.triggers.UserCreate)
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
