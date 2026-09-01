// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// Epic 64: Trigger CRUD handlers (user | org scope).
//
// One file covers both scopes because the logic differs only in
// (ownerType, ownerID) resolution. For webhook triggers, an accompanying
// webhooks row (with encrypted HMAC secret) is created in the same handler.
// Authz is enforced by the router's middleware chain.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// triggerStore is the narrow DB interface the trigger handler depends on.
type triggerStore interface {
	CreateTrigger(ctx context.Context, row *wf.TriggerRow) error
	ListTriggers(ctx context.Context, ownerType, ownerID string) ([]*wf.TriggerRow, error)
	GetTrigger(ctx context.Context, ownerType, ownerID, triggerID string) (*wf.TriggerRow, error)
	UpdateTrigger(ctx context.Context, ownerType, ownerID, triggerID string, upd *wf.TriggerUpdate) (*wf.TriggerRow, error)
	DeleteTrigger(ctx context.Context, ownerType, ownerID, triggerID string) error
	CountTriggersByOwner(ctx context.Context, ownerType, ownerID string) (int, error)
	CreateWebhook(ctx context.Context, row *wf.WebhookRow) error
	GetWebhookByTriggerID(ctx context.Context, triggerID string) (*wf.WebhookRow, error)
	UpdateWebhookSecret(ctx context.Context, triggerID string, secretCipher []byte, keyVersion int) error
	ListTriggerFires(ctx context.Context, triggerID string, limit, offset int) ([]*wf.TriggerFireRow, error)
}

// triggerEncryptor encrypts/decrypts webhook HMAC secrets using the server KEK.
type triggerEncryptor interface {
	Encrypt(ctx context.Context, plaintext []byte) ([]byte, error)
}

// TriggersHandler handles trigger CRUD for both user and org scopes.
type TriggersHandler struct {
	store   triggerStore
	quota   workflowQuotaChecker
	audit   workflowAuditLogger
	encrypt triggerEncryptor
}

// NewUserTriggersHandler constructs a handler for user-scope triggers.
func NewUserTriggersHandler(store triggerStore, quota workflowQuotaChecker, encrypt triggerEncryptor) *TriggersHandler {
	return &TriggersHandler{store: store, quota: quota, encrypt: encrypt}
}

// NewOrgTriggersHandler constructs a handler for org-scope triggers.
func NewOrgTriggersHandler(store triggerStore, quota workflowQuotaChecker, encrypt triggerEncryptor) *TriggersHandler {
	return &TriggersHandler{store: store, quota: quota, encrypt: encrypt}
}

// SetAudit wires the audit logger.
func (h *TriggersHandler) SetAudit(a workflowAuditLogger) { h.audit = a }

// --- User endpoints ---

func (h *TriggersHandler) UserList(c *gin.Context) {
	userID := c.GetString("userID")
	h.list(c, types.WorkflowOwnerUser, userID)
}

func (h *TriggersHandler) UserCreate(c *gin.Context) {
	userID := c.GetString("userID")
	if err := h.checkQuota(c, types.WorkflowOwnerUser, userID); err != nil {
		return
	}
	h.create(c, types.WorkflowOwnerUser, userID)
}

func (h *TriggersHandler) UserGet(c *gin.Context) {
	userID := c.GetString("userID")
	h.get(c, types.WorkflowOwnerUser, userID)
}

func (h *TriggersHandler) UserUpdate(c *gin.Context) {
	userID := c.GetString("userID")
	h.update(c, types.WorkflowOwnerUser, userID)
}

func (h *TriggersHandler) UserDelete(c *gin.Context) {
	userID := c.GetString("userID")
	h.del(c, types.WorkflowOwnerUser, userID)
}

// --- Org endpoints ---

func (h *TriggersHandler) OrgList(c *gin.Context) {
	orgID := c.Param("id")
	h.list(c, types.WorkflowOwnerOrg, orgID)
}

func (h *TriggersHandler) OrgCreate(c *gin.Context) {
	orgID := c.Param("id")
	if err := h.checkQuota(c, types.WorkflowOwnerOrg, orgID); err != nil {
		return
	}
	h.create(c, types.WorkflowOwnerOrg, orgID)
}

func (h *TriggersHandler) OrgGet(c *gin.Context) {
	orgID := c.Param("id")
	h.get(c, types.WorkflowOwnerOrg, orgID)
}

func (h *TriggersHandler) OrgUpdate(c *gin.Context) {
	orgID := c.Param("id")
	h.update(c, types.WorkflowOwnerOrg, orgID)
}

func (h *TriggersHandler) OrgDelete(c *gin.Context) {
	orgID := c.Param("id")
	h.del(c, types.WorkflowOwnerOrg, orgID)
}

// --- shared CRUD ---

func (h *TriggersHandler) list(c *gin.Context, ownerType, ownerID string) {
	rows, err := h.store.ListTriggers(c.Request.Context(), ownerType, ownerID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list triggers"})
		return
	}
	out := make([]types.TriggerResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, triggerRowToResponse(r))
	}
	c.JSON(http.StatusOK, gin.H{"triggers": out})
}

func (h *TriggersHandler) create(c *gin.Context, ownerType, ownerID string) {
	var req types.CreateTriggerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if !types.ValidWorkflowName(req.Name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid trigger name"})
		return
	}
	if !types.ValidTriggerSourceType(req.SourceType) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid source type"})
		return
	}

	memoryMode := req.MemoryMode
	if memoryMode == "" {
		memoryMode = types.MemoryNone
	}
	if !types.ValidMemoryMode(memoryMode) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid memoryMode"})
		return
	}

	captureMode := req.CaptureMode
	if captureMode == "" {
		captureMode = types.CaptureErrorsOnly
	}
	if !types.ValidCaptureMode(captureMode) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid captureMode"})
		return
	}

	preserveSession := req.PreserveSession
	if preserveSession == "" {
		preserveSession = types.PreserveNever
	}
	if !types.ValidPreserveSession(preserveSession) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid preserveSession"})
		return
	}

	if req.WorkflowID == "" && req.WorkspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "either workflowId or workspaceId is required"})
		return
	}
	if req.WorkflowID != "" && req.WorkspaceID != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot set both workflowId and workspaceId"})
		return
	}

	effectiveMemoryMode := req.MemoryMode
	if effectiveMemoryMode == "" {
		effectiveMemoryMode = types.MemoryNone
	}
	effectiveCaptureMode := req.CaptureMode
	if effectiveCaptureMode == "" {
		effectiveCaptureMode = types.CaptureErrorsOnly
	}
	if effectiveMemoryMode == types.MemoryLastResult && effectiveCaptureMode != types.CaptureFull {
		c.JSON(http.StatusBadRequest, gin.H{"error": "memoryMode 'last_result' requires captureMode 'full'"})
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	autoDisable := 10
	if req.AutoDisableAfter != nil {
		autoDisable = *req.AutoDisableAfter
	}

	memoryMaxRuns := 1
	if req.MemoryMaxRuns != nil {
		memoryMaxRuns = *req.MemoryMaxRuns
	}

	now := time.Now().UTC()
	triggerID := uuid.New().String()

	var wsID *string
	if req.WorkspaceID != "" {
		wsID = &req.WorkspaceID
	}
	var wfID *string
	if req.WorkflowID != "" {
		wfID = &req.WorkflowID
	}

	row := &wf.TriggerRow{
		ID: triggerID, OwnerType: ownerType, OwnerID: ownerID,
		Name: req.Name, Description: req.Description, Enabled: enabled,
		SourceType: req.SourceType, SourceConfig: req.SourceConfig,
		WorkspaceID: wsID, WorkflowID: wfID,
		Prompt: req.Prompt, Agent: req.Agent,
		ScriptPath: req.ScriptPath, ScriptArgs: req.ScriptArgs, ScriptEnv: req.ScriptEnv,
		MemoryMode: memoryMode, MemoryMaxRuns: memoryMaxRuns,
		CaptureMode: captureMode, PreserveSession: preserveSession,
		AutoDisableAfter: autoDisable,
		CreatedAt:        now, UpdatedAt: now,
	}

	if req.SourceType == types.TriggerSourceCron {
		var cronCfg types.CronSourceConfig
		if err := json.Unmarshal(req.SourceConfig, &cronCfg); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid cron source config"})
			return
		}
		if cronCfg.Expr == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cron source requires 'expr'"})
			return
		}
		nextFire := now
		row.NextFireAt = &nextFire
	}

	if err := h.store.CreateTrigger(c.Request.Context(), row); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create trigger"})
		return
	}

	if req.SourceType == types.TriggerSourceWebhook {
		if h.encrypt == nil {
			_ = h.store.DeleteTrigger(c.Request.Context(), ownerType, ownerID, triggerID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encryption provider not configured"})
			return
		}
		secret := generateWebhookSecret()
		ciphertext, err := h.encrypt.Encrypt(c.Request.Context(), []byte(secret))
		if err != nil {
			_ = h.store.DeleteTrigger(c.Request.Context(), ownerType, ownerID, triggerID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt webhook secret"})
			return
		}
		keyVersion := 1
		if vp, ok := h.encrypt.(secrets.VersionedProvider); ok {
			keyVersion = vp.ActiveVersion()
		}

		idemMode := req.WebhookIdempotencyMode
		if idemMode == "" {
			idemMode = types.WebhookIdempotencyHeader
		}
		if !types.ValidWebhookIdempotencyMode(idemMode) {
			_ = h.store.DeleteTrigger(c.Request.Context(), ownerType, ownerID, triggerID)
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid idempotency mode"})
			return
		}

		hookRow := &wf.WebhookRow{
			ID: uuid.New().String(), TriggerID: triggerID,
			SecretCipher: ciphertext, KeyVersion: keyVersion,
			AllowedIPs:        req.WebhookAllowedIPs,
			IdempotencyMode:   idemMode,
			IdempotencyHeader: req.WebhookIdempotencyHeader,
			CreatedAt:         now,
		}
		if err := h.store.CreateWebhook(c.Request.Context(), hookRow); err != nil {
			_ = h.store.DeleteTrigger(c.Request.Context(), ownerType, ownerID, triggerID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create webhook config"})
			return
		}
	}

	resp := triggerRowToResponse(row)
	h.logCreate(c, ownerType, ownerID, c.GetString("userID"), triggerID, req.Name)
	if req.SourceType == types.TriggerSourceWebhook {
		c.JSON(http.StatusCreated, gin.H{
			"trigger":    resp,
			"webhookUrl": fmt.Sprintf("/api/v1/hooks/%s", triggerID),
		})
		return
	}
	c.JSON(http.StatusCreated, resp)
}

func (h *TriggersHandler) get(c *gin.Context, ownerType, ownerID string) {
	triggerID := c.Param("id")
	row, err := h.store.GetTrigger(c.Request.Context(), ownerType, ownerID, triggerID)
	if err != nil {
		if errors.Is(err, wf.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "trigger not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get trigger"})
		return
	}
	c.JSON(http.StatusOK, triggerRowToResponse(row))
}

func (h *TriggersHandler) update(c *gin.Context, ownerType, ownerID string) {
	triggerID := c.Param("id")
	var req types.UpdateTriggerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	upd := &wf.TriggerUpdate{
		Name: req.Name, Description: req.Description, Enabled: req.Enabled,
		SourceConfig: req.SourceConfig,
		WorkspaceID:  req.WorkspaceID, WorkflowID: req.WorkflowID,
		Prompt: req.Prompt, Agent: req.Agent,
		ScriptPath: req.ScriptPath, ScriptArgs: req.ScriptArgs, ScriptEnv: req.ScriptEnv,
		MemoryMode: req.MemoryMode, MemoryMaxRuns: req.MemoryMaxRuns,
		CaptureMode: req.CaptureMode, PreserveSession: req.PreserveSession,
		AutoDisableAfter: req.AutoDisableAfter,
	}

	if req.MemoryMode != nil && !types.ValidMemoryMode(*req.MemoryMode) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid memoryMode"})
		return
	}
	if req.CaptureMode != nil && !types.ValidCaptureMode(*req.CaptureMode) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid captureMode"})
		return
	}
	if req.PreserveSession != nil && !types.ValidPreserveSession(*req.PreserveSession) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid preserveSession"})
		return
	}
	if req.AutoDisableAfter != nil && *req.AutoDisableAfter < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auto_disable_after must be >= 1"})
		return
	}

	row, err := h.store.UpdateTrigger(c.Request.Context(), ownerType, ownerID, triggerID, upd)
	if err != nil {
		if errors.Is(err, wf.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "trigger not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update trigger"})
		return
	}
	c.JSON(http.StatusOK, triggerRowToResponse(row))
}

func (h *TriggersHandler) del(c *gin.Context, ownerType, ownerID string) {
	triggerID := c.Param("id")
	if err := h.store.DeleteTrigger(c.Request.Context(), ownerType, ownerID, triggerID); err != nil {
		if errors.Is(err, wf.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "trigger not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete trigger"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// --- quota ---

func (h *TriggersHandler) checkQuota(c *gin.Context, ownerType, ownerID string) error {
	if h.quota == nil {
		return nil
	}
	settingKey := "triggers.maxPerUser"
	if ownerType == types.WorkflowOwnerOrg {
		settingKey = "triggers.maxPerOrg"
	}
	maxCount, err := h.quota.GetInt(c.Request.Context(), settingKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check trigger quota"})
		return err
	}
	if maxCount == 0 {
		return nil
	}
	count, err := h.store.CountTriggersByOwner(c.Request.Context(), ownerType, ownerID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to count triggers"})
		return err
	}
	if count >= maxCount {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("trigger limit reached (%d/%d)", count, maxCount)})
		return fmt.Errorf("quota exceeded")
	}
	return nil
}

// --- audit ---

func (h *TriggersHandler) logCreate(c *gin.Context, ownerType, ownerID, actorID, triggerID, name string) {
	if h.audit == nil {
		return
	}
	meta := map[string]any{"name": name, "ownerType": ownerType}
	if ownerType == types.WorkflowOwnerOrg {
		_ = h.audit.LogOrgEvent(c.Request.Context(), ownerID, actorID, "trigger.create", triggerID, meta)
	} else {
		_ = h.audit.LogAuditEvent(c.Request.Context(), "triggers", actorID, "trigger.create", triggerID, nil, meta)
	}
}

// --- helpers ---

func triggerRowToResponse(r *wf.TriggerRow) types.TriggerResponse {
	resp := types.TriggerResponse{
		ID: r.ID, OwnerType: r.OwnerType, Name: r.Name, Description: r.Description,
		Enabled: r.Enabled, SourceType: r.SourceType, SourceConfig: r.SourceConfig,
		Prompt: r.Prompt, Agent: r.Agent,
		ScriptPath: r.ScriptPath, ScriptArgs: r.ScriptArgs, ScriptEnv: r.ScriptEnv,
		MemoryMode: r.MemoryMode, MemoryMaxRuns: r.MemoryMaxRuns,
		CaptureMode: r.CaptureMode, PreserveSession: r.PreserveSession,
		ConsecutiveFailures: r.ConsecutiveFailures, AutoDisableAfter: r.AutoDisableAfter,
		LastFiredAt: r.LastFiredAt, NextFireAt: r.NextFireAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.OwnerID != "" && r.OwnerID != "_platform" {
		resp.OwnerID = r.OwnerID
	}
	if r.WorkspaceID != nil {
		resp.WorkspaceID = *r.WorkspaceID
	}
	if r.WorkflowID != nil {
		resp.WorkflowID = *r.WorkflowID
	}
	return resp
}

// generateWebhookSecret produces a random 32-byte HMAC key with a whsec_ prefix.
// Uses crypto/rand — suitable for HMAC signing (not password hashing).
func generateWebhookSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return "whsec_" + hex.EncodeToString(b)
}

// --- Delivery log (observability) ---

// UserListFires returns recent trigger fire audit rows for a user-scope trigger.
func (h *TriggersHandler) UserListFires(c *gin.Context) {
	userID := c.GetString("userID")
	h.listFires(c, types.WorkflowOwnerUser, userID)
}

// OrgListFires returns recent trigger fire audit rows for an org-scope trigger.
func (h *TriggersHandler) OrgListFires(c *gin.Context) {
	orgID := c.Param("id")
	h.listFires(c, types.WorkflowOwnerOrg, orgID)
}

func (h *TriggersHandler) listFires(c *gin.Context, ownerType, ownerID string) {
	triggerID := c.Param("id")
	if triggerID == "" {
		triggerID = c.Param("triggerId")
	}

	limit := 50
	offset := 0

	fires, err := h.store.ListTriggerFires(c.Request.Context(), triggerID, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list trigger fires"})
		return
	}

	out := make([]types.TriggerFireResponse, 0, len(fires))
	for _, f := range fires {
		out = append(out, triggerFireRowToResponse(f))
	}
	c.JSON(http.StatusOK, gin.H{"fires": out})
}

func triggerFireRowToResponse(f *wf.TriggerFireRow) types.TriggerFireResponse {
	resp := types.TriggerFireResponse{
		ID:            f.ID,
		TriggerID:     f.TriggerID,
		SourceType:    f.SourceType,
		InputEnvelope: f.InputEnvelope,
		ActionType:    f.ActionType,
		ActionResult:  f.ActionResult,
		Status:        f.Status,
		FiredAt:       f.FiredAt,
	}
	if f.CompletedAt != nil {
		resp.CompletedAt = f.CompletedAt
	}
	return resp
}

// --- Webhook secret rotation ---

// UserRotateWebhookSecret generates a new HMAC secret for a webhook trigger.
// Returns the plaintext secret ONE TIME — the caller must store it; it cannot be recovered.
func (h *TriggersHandler) UserRotateWebhookSecret(c *gin.Context) {
	userID := c.GetString("userID")
	h.rotateWebhookSecret(c, types.WorkflowOwnerUser, userID)
}

// OrgRotateWebhookSecret does the same for org-scope triggers.
func (h *TriggersHandler) OrgRotateWebhookSecret(c *gin.Context) {
	orgID := c.Param("id")
	h.rotateWebhookSecret(c, types.WorkflowOwnerOrg, orgID)
}

func (h *TriggersHandler) rotateWebhookSecret(c *gin.Context, ownerType, ownerID string) {
	triggerID := c.Param("id")
	if triggerID == "" {
		triggerID = c.Param("triggerId")
	}

	trigger, err := h.store.GetTrigger(c.Request.Context(), ownerType, ownerID, triggerID)
	if err != nil {
		if errors.Is(err, wf.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "trigger not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch trigger"})
		return
	}
	if trigger.SourceType != types.TriggerSourceWebhook {
		c.JSON(http.StatusBadRequest, gin.H{"error": "trigger is not a webhook trigger"})
		return
	}
	if h.encrypt == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encryption provider not configured"})
		return
	}

	secret := generateWebhookSecret()
	ciphertext, err := h.encrypt.Encrypt(c.Request.Context(), []byte(secret))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encrypt webhook secret"})
		return
	}
	keyVersion := 1
	if vp, ok := h.encrypt.(secrets.VersionedProvider); ok {
		keyVersion = vp.ActiveVersion()
	}

	if err := h.store.UpdateWebhookSecret(c.Request.Context(), triggerID, ciphertext, keyVersion); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update webhook secret"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"webhookSecret": secret,
		"webhookUrl":    fmt.Sprintf("/api/v1/hooks/%s", triggerID),
	})
}
