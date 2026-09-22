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
	"strings"
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
	// GetWorkflow is the owner-scoped workflow lookup the input-mapping
	// rules (0059 V3/V4/V6) validate the target's inputSchema against —
	// the same store method pod automation's gateWorkflowTarget uses.
	GetWorkflow(ctx context.Context, ownerType, ownerID, workflowID string) (*wf.WorkflowRow, error)
}

// triggerEncryptor encrypts/decrypts webhook HMAC secrets using the server KEK.
type triggerEncryptor interface {
	Encrypt(ctx context.Context, plaintext []byte) ([]byte, error)
}

// errTriggerMemoryCaptureConstraint is the create- and update-path
// rejection for memoryMode 'last_result' without captureMode 'full' —
// the invariant guaranteeing every delivered routine result row stores
// the agentd envelope the memory read path expects (#1453, #1467).
// One constant: the two paths must never diverge.
const errTriggerMemoryCaptureConstraint = "memoryMode 'last_result' requires captureMode 'full'"

// TriggersHandler handles trigger CRUD for both user and org scopes.
type TriggersHandler struct {
	store        triggerStore
	quota        workflowQuotaChecker
	audit        workflowAuditLogger
	encrypt      triggerEncryptor
	wsExistencer workspaceExistencer
}

// SetWorkspaceExistencer wires the target-workspace existence check for
// the parent-id contract (deferred injection; nil skips).
func (h *TriggersHandler) SetWorkspaceExistencer(w workspaceExistencer) { h.wsExistencer = w }

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
	h.get(c, types.WorkflowOwnerUser, userID, c.Param("id"))
}

func (h *TriggersHandler) UserUpdate(c *gin.Context) {
	userID := c.GetString("userID")
	h.update(c, types.WorkflowOwnerUser, userID, c.Param("id"))
}

func (h *TriggersHandler) UserDelete(c *gin.Context) {
	userID := c.GetString("userID")
	h.del(c, types.WorkflowOwnerUser, userID, c.Param("id"))
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
	h.get(c, types.WorkflowOwnerOrg, orgID, c.Param("triggerId"))
}

func (h *TriggersHandler) OrgUpdate(c *gin.Context) {
	orgID := c.Param("id")
	h.update(c, types.WorkflowOwnerOrg, orgID, c.Param("triggerId"))
}

func (h *TriggersHandler) OrgDelete(c *gin.Context) {
	orgID := c.Param("id")
	h.del(c, types.WorkflowOwnerOrg, orgID, c.Param("triggerId"))
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

	// Input mapping (0059): V1–V6 on the create view.
	inputFrom := wf.NormalizeTriggerInputFrom(req.InputFrom)
	if !h.validateTriggerInputMapping(c, ownerType, ownerID, req.SourceType, req.WorkflowID, inputFrom, req.Input) {
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
		c.JSON(http.StatusBadRequest, gin.H{"error": errTriggerMemoryCaptureConstraint})
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
		InputFrom: inputFrom, Input: req.Input,
		Prompt: req.Prompt, Agent: req.Agent,
		ScriptPath: req.ScriptPath, ScriptArgs: req.ScriptArgs, ScriptEnv: req.ScriptEnv,
		MemoryMode: memoryMode, MemoryMaxRuns: memoryMaxRuns,
		CaptureMode: captureMode, PreserveSession: preserveSession,
		AutoDisableAfter: autoDisable,
		CreatedAt:        now, UpdatedAt: now,
	}

	if req.SourceType == types.TriggerSourceCron {
		// Validate the schedule up front and start at the first real
		// occurrence (#1411): initializing next_fire_at to "now" made
		// every newly created enabled trigger fire immediately, and
		// unparseable exprs fell into the engine's hourly retry loop.
		_, nextFire, err := wf.NextCronFireFromConfig(req.SourceConfig, now)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		row.NextFireAt = &nextFire
	}

	// A user-supplied workflowId must resolve at create (the 35597973572
	// contract ruling): un-opted ghost wiring previously fell through to
	// the store insert and surfaced as an opaque 500 (FK shape). The
	// V-matrix pre-fetch above has already answered opted-in ghosts (400)
	// and ANY non-NotFound lookup fault (500) — the only error reachable
	// HERE is NotFound on the un-opted path, so this arm carries just the
	// contract 400 (a hypothetical non-NotFound error falls through to
	// the insert, where the FK answers identically to the pre-fix 500).
	// Ordered AFTER the cron validation so an invalid expr answers the
	// cron error; the FK stays the integrity anchor (ON DELETE SET NULL).
	if req.WorkflowID != "" {
		if _, err := h.store.GetWorkflow(c.Request.Context(), ownerType, ownerID, req.WorkflowID); errors.Is(err, wf.ErrNotFound) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "target workflow not found"})
			return
		}
	}

	// The parent-id audit: a user-supplied workspaceId must EXIST (the
	// triggers.workspace_id FK answered nonexistent targets with the same
	// opaque-500 class). Existence only — cross-owner targets remain the
	// fire-time loud class by design (#1440); nil existencer (legacy
	// construction) skips.
	if req.WorkspaceID != "" && h.wsExistencer != nil {
		exists, err := h.wsExistencer.WorkspaceExistsByID(c.Request.Context(), req.WorkspaceID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check target workspace"})
			return
		}
		if !exists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "target workspace not found"})
			return
		}
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

func (h *TriggersHandler) get(c *gin.Context, ownerType, ownerID, triggerID string) {
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

// validateUpdateMapping runs the 0059 V7 wiring/mapping rules for
// patches that touch the mapping surface. Extracted from update() —
// the function exceeded the repo's gocyclo ceiling.
func (h *TriggersHandler) validateUpdateMapping(c *gin.Context, ownerType, ownerID string, existing *wf.TriggerRow, req *types.UpdateTriggerRequest, mergedWorkflowID string) bool {
	mergedInputFrom := wf.NormalizeTriggerInputFrom(existing.InputFrom)
	if req.InputFrom != nil {
		mergedInputFrom = wf.NormalizeTriggerInputFrom(*req.InputFrom)
	}
	mergedInput := existing.Input
	if req.Input != nil {
		// Present key (including JSON null, which clears the static
		// document) replaces; absent key keeps — discriminated here so
		// the store keeps its plain CASE WHEN NULL THEN keep shape.
		mergedInput = req.Input
	}
	return h.validateTriggerInputMapping(c, ownerType, ownerID, existing.SourceType, mergedWorkflowID, mergedInputFrom, mergedInput)
}

func (h *TriggersHandler) update(c *gin.Context, ownerType, ownerID, triggerID string) {
	var req types.UpdateTriggerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	upd := &wf.TriggerUpdate{
		Name: req.Name, Description: req.Description, Enabled: req.Enabled,
		SourceConfig: req.SourceConfig,
		WorkspaceID:  req.WorkspaceID, WorkflowID: req.WorkflowID,
		InputFrom: req.InputFrom, Input: req.Input,
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
	if (req.WorkflowID != nil && *req.WorkflowID != "") && (req.WorkspaceID != nil && *req.WorkspaceID != "") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot set both workflowId and workspaceId"})
		return
	}

	// Cron triggers: a changed schedule must take effect immediately, not
	// at the previously stored slot (#1410) — recompute next_fire_at from
	// the new config. Also recompute on re-enable when the stored slot is
	// already in the past, so a long-disabled trigger resumes on the next
	// future occurrence instead of emitting a backdated skip.
	//
	// The same stored row feeds the input-mapping re-validation below
	// (V7 needs the post-patch merged view).
	touchesMapping := req.WorkflowID != nil || req.InputFrom != nil || req.Input != nil
	touchesTarget := touchesMapping || req.WorkspaceID != nil
	// #1467: the last_result ⇒ full cross-constraint below needs the
	// stored row whenever a patch could flip one side without the other.
	touchesMemoryCapture := req.MemoryMode != nil || req.CaptureMode != nil
	targetlessAfterPatch := false
	var existing *wf.TriggerRow
	now := time.Now().UTC()
	if req.SourceConfig != nil || req.Enabled != nil || touchesTarget || touchesMemoryCapture {
		var err error
		existing, err = h.store.GetTrigger(c.Request.Context(), ownerType, ownerID, triggerID)
		if err != nil {
			if errors.Is(err, wf.ErrNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "trigger not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch trigger"})
			return
		}
		if req.WorkflowID != nil || req.WorkspaceID != nil {
			// Evaluated after the V-matrix below — a targetless post-patch
			// view that survives V1 (the opted-in case) is a plain
			// de-targeting patch and gets the generic #1442 rejection.
			workflowSet := (req.WorkflowID != nil && *req.WorkflowID != "") || (req.WorkflowID == nil && existing.WorkflowID != nil)
			workspaceSet := (req.WorkspaceID != nil && *req.WorkspaceID != "") || (req.WorkspaceID == nil && existing.WorkspaceID != nil)
			targetlessAfterPatch = !workflowSet && !workspaceSet
		}
		if existing.SourceType == types.TriggerSourceCron {
			cfg := existing.SourceConfig
			if req.SourceConfig != nil {
				cfg = req.SourceConfig
			}
			_, next, verr := wf.NextCronFireFromConfig(cfg, now)
			switch {
			case req.SourceConfig != nil:
				// An explicitly supplied config must validate.
				if verr != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": verr.Error()})
					return
				}
				upd.NextFireAt = &next
			case verr == nil && req.Enabled != nil && *req.Enabled && !existing.Enabled &&
				(existing.NextFireAt == nil || existing.NextFireAt.Before(now)):
				// Re-enable (disabled→enabled) with a stale slot; legacy
				// unparseable configs (verr != nil) keep the engine's
				// fallback behavior. The !existing.Enabled guard matters:
				// a no-op enabled:true on an ALREADY-enabled trigger must
				// never touch the slot — an imminent-but-unclaimed fire
				// (the ≤tick-window race) would otherwise be pushed to the
				// next occurrence.
				upd.NextFireAt = &next
			}
		}
	}

	// #1467: create enforces memoryMode 'last_result' ⇒ captureMode
	// 'full' (triggers.go create arm — the invariant guaranteeing every
	// delivered routine result row stores the agentd envelope the memory
	// read path expects, per #1453). The update path enforces the same
	// constraint on the POST-PATCH MERGED view: a patch may flip one
	// side while leaving the other stored. Rows whose stored state is
	// untouched keep whatever they had — legacy-invalid rows stay
	// editable so a repair patch can reach the store.
	if touchesMemoryCapture {
		mergedMemoryMode := existing.MemoryMode
		if req.MemoryMode != nil {
			mergedMemoryMode = *req.MemoryMode
		}
		mergedCaptureMode := existing.CaptureMode
		if req.CaptureMode != nil {
			mergedCaptureMode = *req.CaptureMode
		}
		if mergedMemoryMode == types.MemoryLastResult && mergedCaptureMode != types.CaptureFull {
			c.JSON(http.StatusBadRequest, gin.H{"error": errTriggerMemoryCaptureConstraint})
			return
		}
	}

	// The post-patch MERGED workflow target — feeds the V7 mapping rules
	// below. existing is nil only for patches touching none of
	// sourceConfig/enabled/target/memory-capture (then the merge is
	// empty). The parent-id existence checks further down read the
	// PATCHED req values directly, not this merge.
	mergedWorkflowID := ""
	if existing != nil {
		if existing.WorkflowID != nil {
			mergedWorkflowID = *existing.WorkflowID
		}
		if req.WorkflowID != nil {
			mergedWorkflowID = *req.WorkflowID
		}
	}

	// Input mapping (0059 V7): only patches that touch workflowId, input,
	// or inputFrom re-run the wiring/mapping rules — against the POST-PATCH
	// merged view. Patches that remove an opt-in (input → null, inputFrom →
	// envelope) re-expose the wiring and re-trip V6; a rename, enable flip,
	// or schedule change re-runs nothing.
	if touchesMapping {
		if !h.validateUpdateMapping(c, ownerType, ownerID, existing, &req, mergedWorkflowID) {
			return
		}
		// #1519: workflow-existence check on the POST-PATCH MERGED view —
		// create's contract (triggers.go create arm), mirrored. The
		// V-matrix's un-opted NotFound arm deliberately skips (V6's ghost
		// tolerance); this check closes the hole: a retarget to a
		// nonexistent workflow or a stored ghost (legacy cross-owner /
		// pre-#1517 wiring) surfaced by ANY mapping-touching patch answers
		// the named 400 here, never the store FK's opaque 500 or a silent
		// persist that fires trigger_has_no_target at drain. As on the
		// create arm, the V-matrix pre-fetch has already answered
		// non-NotFound lookup faults (500) — the only error reachable
		// HERE is NotFound, so this arm carries just the contract 400 (a
		// hypothetical non-NotFound error falls through to the update,
		// where the FK answers identically to the pre-fix 500).
		if mergedWorkflowID != "" {
			if _, err := h.store.GetWorkflow(c.Request.Context(), ownerType, ownerID, mergedWorkflowID); errors.Is(err, wf.ErrNotFound) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "target workflow not found"})
				return
			}
		}
	}

	// #1442: the post-patch row must keep an execution target. The 0059
	// V-matrix above already rejected opted-in de-targeting patches (V1,
	// the specific message); anything still targetless here is a plain
	// de-targeting patch — the user-API route to a zombie tick, now a 400.
	if targetlessAfterPatch {
		c.JSON(http.StatusBadRequest, gin.H{"error": "update would leave the trigger without an execution target — set workflowId or workspaceId"})
		return
	}

	// The parent-id audit on the UPDATE view (#1519/#1522): a PATCHED
	// target must resolve — nonexistent workflowId/workspaceId patches
	// previously reached the FK as an opaque 500 ("failed to update
	// trigger"). The workflowId half is answered by the #1519 merged-view
	// check inside the mapping block above (every workflowId patch
	// touches mapping, so that check always runs first and additionally
	// surfaces stored ghosts on de-opt patches); this arm carries the
	// workspaceId half via the existencer. STORED targets outside
	// mapping-touching patches stay FK-anchored and untouched (a legacy
	// row with a cross-owner workflowId remains patchable for mitigation
	// — the #1440 loud-fire zombie tolerates enabled:false).
	if req.WorkspaceID != nil && *req.WorkspaceID != "" && h.wsExistencer != nil {
		exists, err := h.wsExistencer.WorkspaceExistsByID(c.Request.Context(), *req.WorkspaceID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check target workspace"})
			return
		}
		if !exists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "target workspace not found"})
			return
		}
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

func (h *TriggersHandler) del(c *gin.Context, ownerType, ownerID, triggerID string) {
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

// --- Input mapping validation (design 0059 §3.3, rules V1–V7) ---

// validateTriggerInputMapping enforces the input-mapping rules on ONE
// (sourceType, workflowID, inputFrom, input) view: the create request
// directly, or the post-patch merged view on update (V7). It runs inside
// create/update and therefore also covers the pod-automation delegation,
// which forwards through these handlers. Writes the failure response
// itself; returns true when the view passes.
//
//	V1  input mapping (static input, or inputFrom beyond the envelope
//	    default) requires a workflow target — routine triggers keep the
//	    envelope-fed {{.input}} prompt contract.
//	V2  inputFrom "body" requires a webhook source (cron envelopes carry
//	    no body key).
//	V3  an opted-in wiring must be able to reach its schema: a missing
//	    target workflow cannot be validated (distinct from #1412's
//	    fire-time ghost — still loud only for legacy rows never
//	    re-touched after this check; every create and every
//	    mapping-touching update now 400s at the #1519 existence check
//	    before the store write).
//	V4  inputFrom "mapped": the static document must satisfy the
//	    workflow's inputSchema (absent schema accepts everything).
//	V5  envelope/body + static input: the static document must be a JSON
//	    object (it overlays an object base).
//	V6  un-opted NEW wiring against a schema whose top-level `required`
//	    names properties beyond the source's envelope key set is rejected
//	    with the three remedies — the narrowed O1 guard (D4). Legacy
//	    triggers are never re-scanned; a missing workflow skips the guard
//	    (nothing to require). Cross-owner references no longer reach any
//	    arm: both #1519 checks (create and update) are owner-scoped
//	    against the same view and 400 them before the store write; only
//	    pre-check legacy rows ever persisted a ghost, and the engine
//	    remains loud for those at fire time.
func (h *TriggersHandler) validateTriggerInputMapping(c *gin.Context, ownerType, ownerID, sourceType, workflowID, inputFrom string, input json.RawMessage) bool {
	if !types.ValidTriggerInputFrom(inputFrom) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid inputFrom (want envelope, body, or mapped)"})
		return false
	}
	staticPresent := wf.StaticInputPresent(input)
	if staticPresent && len(input) > types.MaxTriggerStaticInputBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("input exceeds the %d-byte static-input cap", types.MaxTriggerStaticInputBytes)})
		return false
	}
	// V5: the overlay merges into an object base, so it must be one.
	if staticPresent && inputFrom != types.TriggerInputFromMapped && !isJSONDocumentObject(input) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "static input must be a JSON object in envelope/body modes"})
		return false
	}
	optedIn := staticPresent || inputFrom != types.TriggerInputFromEnvelope

	// V1.
	if optedIn && workflowID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "input mapping requires a workflow target"})
		return false
	}
	// V2.
	if inputFrom == types.TriggerInputFromBody && sourceType != types.TriggerSourceWebhook {
		c.JSON(http.StatusBadRequest, gin.H{"error": `inputFrom "body" requires a webhook source`})
		return false
	}
	if workflowID == "" {
		return true // routine target — no schema to check against
	}

	// V3/V6: owner-scoped fetch, exactly as pod automation's
	// gateWorkflowTarget does. Opted-in wiring must reach its schema;
	// un-opted wiring only consults it for the V6 guard, so a missing
	// (ghost) workflow skips the guard.
	wfRow, err := h.store.GetWorkflow(c.Request.Context(), ownerType, ownerID, workflowID)
	if err != nil {
		if optedIn {
			if errors.Is(err, wf.ErrNotFound) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "target workflow not found"})
				return false
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch workflow"})
			return false
		}
		if errors.Is(err, wf.ErrNotFound) {
			// V3/V6 guard skipped for un-opted wiring. Both callers now
			// close the hole with their own #1519 existence check on the
			// same merged view (the create arm at the pre-insert check,
			// the update arm inside the touchesMapping block) — a ghost
			// target 400s there before the store write.
			return true
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch workflow"})
		return false
	}

	// V4: the static document IS the run input in mapped mode — it must
	// satisfy the schema now (fire-time validation is the steady-state
	// backstop for later drift).
	if inputFrom == types.TriggerInputFromMapped {
		validateDoc := input
		if !staticPresent {
			validateDoc = nil
		}
		if verr := wf.ValidateRunInput(wfRow.InputSchema, validateDoc); verr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("input does not satisfy the workflow's inputSchema: %v", verr)})
			return false
		}
		return true
	}

	// V6: new un-opted wiring whose schema requires properties the
	// envelope never provides is the #1425 mis-wiring — make it loud at
	// create with the three remedies (D4).
	if !optedIn {
		if missing := wf.RequiredNonEnvelopeProperties(wfRow.InputSchema, sourceType); len(missing) > 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf(
				"workflow inputSchema requires %s, which the %s envelope never provides: set \"input\", use inputFrom \"body\" (webhook), or relax the workflow's inputSchema",
				strings.Join(missing, ", "), sourceType)})
			return false
		}
	}
	return true
}

// isJSONDocumentObject reports whether raw is a JSON object.
func isJSONDocumentObject(raw json.RawMessage) bool {
	var head map[string]json.RawMessage
	return json.Unmarshal(raw, &head) == nil
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

// logRotateWebhookSecret records a webhook-secret rotation (never the
// secret itself) — the credential-issuance audit counterpart of
// logCreate.
func (h *TriggersHandler) logRotateWebhookSecret(c *gin.Context, ownerType, ownerID, actorID, triggerID string) {
	if h.audit == nil {
		return
	}
	meta := map[string]any{"ownerType": ownerType}
	if ownerType == types.WorkflowOwnerOrg {
		_ = h.audit.LogOrgEvent(c.Request.Context(), ownerID, actorID, "trigger.rotate_webhook_secret", triggerID, meta)
	} else {
		_ = h.audit.LogAuditEvent(c.Request.Context(), "triggers", actorID, "trigger.rotate_webhook_secret", triggerID, nil, meta)
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
	// Input mapping (0059): always report the effective mode — legacy
	// rows read as envelope via the column default (and pre-migration
	// in-memory rows normalize the same way).
	resp.InputFrom = wf.NormalizeTriggerInputFrom(r.InputFrom)
	resp.Input = r.Input
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
	h.listFires(c, types.WorkflowOwnerUser, userID, c.Param("id"))
}

// OrgListFires returns recent trigger fire audit rows for an org-scope trigger.
func (h *TriggersHandler) OrgListFires(c *gin.Context) {
	orgID := c.Param("id")
	h.listFires(c, types.WorkflowOwnerOrg, orgID, c.Param("triggerId"))
}

func (h *TriggersHandler) listFires(c *gin.Context, ownerType, ownerID, triggerID string) {

	// Owner-scoped guard, mirroring every sibling (get/update/del/
	// rotate): without it, any authenticated user could read any
	// trigger's fires by UUID — and trigger UUIDs travel in
	// externally-shared webhook URLs. Fire rows carry captured agent
	// outputs (result) and input envelopes; that exposure must be
	// owner-bound (404 on mismatch, same as the siblings).
	if _, err := h.store.GetTrigger(c.Request.Context(), ownerType, ownerID, triggerID); err != nil {
		if errors.Is(err, wf.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "trigger not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch trigger"})
		return
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
		Result:        f.Result,
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
	h.rotateWebhookSecret(c, types.WorkflowOwnerUser, userID, c.Param("id"))
}

// OrgRotateWebhookSecret does the same for org-scope triggers.
func (h *TriggersHandler) OrgRotateWebhookSecret(c *gin.Context) {
	orgID := c.Param("id")
	h.rotateWebhookSecret(c, types.WorkflowOwnerOrg, orgID, c.Param("triggerId"))
}

func (h *TriggersHandler) rotateWebhookSecret(c *gin.Context, ownerType, ownerID, triggerID string) {

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

	// Credential issuance is auditable: every rotation leaves an event
	// (actor + target), including rotations driven by the workspace pod
	// via the automation surface. The secret itself is NEVER in the
	// audit row.
	h.logRotateWebhookSecret(c, ownerType, ownerID, c.GetString("userID"), triggerID)

	c.JSON(http.StatusOK, gin.H{
		"webhookSecret": secret,
		"webhookUrl":    fmt.Sprintf("/api/v1/hooks/%s", triggerID),
	})
}
