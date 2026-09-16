// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/client-go/kubernetes"

	"github.com/lenaxia/llmsafespaces/pkg/interfaces"
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

// resolve authenticates the pod (bearer SA token → TokenReview → SA
// principal bound to this workspace+namespace) and returns the owner
// userID. Writes the auth-failure response itself; returns ok=false.
func (h *PodAutomationHandler) resolve(c *gin.Context) (workspaceID, ownerID string, ok bool) {
	token := extractBearerToken(c.GetHeader("Authorization"))
	if token == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authorization"})
		return "", "", false
	}
	username, err := h.tokenReviewer.Review(c.Request.Context(), token)
	if err != nil {
		if errors.Is(err, errTokenNotAuthenticated) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token not authenticated"})
			return "", "", false
		}
		if h.logger != nil {
			h.logger.Error("automation: token review failed", err)
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "token review failed"})
		return "", "", false
	}
	if exp, hasExp := unverifiedJWTExp(token); hasExp && time.Now().After(exp.Add(jwtExpiryLeeway)) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "token expired"})
		return "", "", false
	}

	// The workspace arrives in-query on reads/deletes (no body) or
	// in-body on creates (the seam stamps it). Patches and run-inputs
	// stay clean — they carry only their own fields.
	resolved := c.Query("workspaceID")
	if workspaceID == "" {
		var body struct {
			WorkspaceID string `json:"workspaceID"`
		}
		if c.Request.Body != nil && c.Request.ContentLength > 0 {
			_ = c.ShouldBindJSON(&body)
			resolved = body.WorkspaceID
		}
	}
	if resolved == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "workspaceID required (query or body)"})
		return "", "", false
	}
	workspaceID = resolved

	saNamespace, saWorkspaceID, ok := parseSAPrincipal(username)
	if !ok || saWorkspaceID != workspaceID || saNamespace != h.expectedNamespace {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "workspace identity mismatch"})
		return "", "", false
	}

	ws, err := h.lookup.GetWorkspace(c.Request.Context(), workspaceID)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("automation: lookup failed", err, "workspaceID", workspaceID)
		}
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "workspace lookup failed"})
		return "", "", false
	}
	if ws == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return "", "", false
	}
	return workspaceID, ws.UserID, true
}

// delegate re-binds the request as the resolved owner and invokes fn.
// The body was consumed by resolve's optional bind — replay it.
func (h *PodAutomationHandler) delegate(c *gin.Context, ownerID string, replay []byte, fn func(*gin.Context)) {
	if replay != nil {
		c.Request.Body = io.NopCloser(newReusableBody(replay))
		c.Request.ContentLength = int64(len(replay))
	}
	c.Set("userID", ownerID)
	fn(c)
}

// forceTriggerWorkspace rewrites the create body's workspaceId to this
// pod's workspace (the automation scoping rule).
func forceTriggerWorkspace(replay []byte, workspaceID string) []byte {
	var body map[string]any
	if json.Unmarshal(replay, &body) != nil {
		return replay
	}
	body["workspaceId"] = workspaceID
	out, err := json.Marshal(body)
	if err != nil {
		return replay
	}
	return out
}

// --- Trigger routes -------------------------------------------------------

func (h *PodAutomationHandler) TriggerList(c *gin.Context) {
	if _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, nil, h.triggers.UserList)
	}
}

func (h *PodAutomationHandler) TriggerCreate(c *gin.Context) {
	ws, owner, ok := h.resolve(c)
	if !ok {
		return
	}
	replay := readBodyForReplay(c)
	replay = forceTriggerWorkspace(replay, ws)
	h.delegate(c, owner, replay, h.triggers.UserCreate)
}

func (h *PodAutomationHandler) TriggerGet(c *gin.Context) {
	if _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, nil, h.triggers.UserGet)
	}
}

func (h *PodAutomationHandler) TriggerUpdate(c *gin.Context) {
	if _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, readBodyForReplay(c), h.triggers.UserUpdate)
	}
}

func (h *PodAutomationHandler) TriggerDelete(c *gin.Context) {
	if _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, nil, h.triggers.UserDelete)
	}
}

func (h *PodAutomationHandler) TriggerFires(c *gin.Context) {
	if _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, nil, h.triggers.UserListFires)
	}
}

// --- Workflow routes ------------------------------------------------------

func (h *PodAutomationHandler) WorkflowList(c *gin.Context) {
	if _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, nil, h.workflows.UserList)
	}
}

func (h *PodAutomationHandler) WorkflowCreate(c *gin.Context) {
	if _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, readBodyForReplay(c), h.workflows.UserCreate)
	}
}

func (h *PodAutomationHandler) WorkflowGet(c *gin.Context) {
	if _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, nil, h.workflows.UserGet)
	}
}

func (h *PodAutomationHandler) WorkflowUpdate(c *gin.Context) {
	if _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, readBodyForReplay(c), h.workflows.UserUpdate)
	}
}

func (h *PodAutomationHandler) WorkflowDelete(c *gin.Context) {
	if _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, nil, h.workflows.UserDelete)
	}
}

func (h *PodAutomationHandler) WorkflowRun(c *gin.Context) {
	if _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, readBodyForReplay(c), h.workflows.UserRunWorkflow)
	}
}

func (h *PodAutomationHandler) WorkflowRuns(c *gin.Context) {
	if _, owner, ok := h.resolve(c); ok {
		h.delegate(c, owner, nil, h.workflows.UserListRuns)
	}
}

// --- body replay helpers --------------------------------------------------

// readBodyForReplay drains the request body so resolve's optional bind
// and the delegated handler can both read it.
func readBodyForReplay(c *gin.Context) []byte {
	if c.Request.Body == nil || c.Request.ContentLength == 0 {
		return nil
	}
	buf := make([]byte, c.Request.ContentLength)
	n, _ := c.Request.Body.Read(buf)
	return buf[:n]
}

func newReusableBody(b []byte) *strings.Reader {
	return strings.NewReader(string(b))
}
