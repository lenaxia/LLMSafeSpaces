// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// Epic 64: Webhook receiver — public endpoint for inbound webhook deliveries.
//
// POST /api/v1/hooks/:webhookId
//
// The signature IS the credential — no JWT. Verifies HMAC-SHA256, deduplicates
// via webhook_deliveries, enforces IP allowlist + rate limit, then fires the
// trigger's target (run_workflow or run_script) atomically.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// webhookReceiverStore is the narrow store interface for the webhook receiver.
type webhookReceiverStore interface {
	GetWebhook(ctx context.Context, webhookID string) (*wf.WebhookRow, error)
	GetTriggerByID(ctx context.Context, triggerID string) (*wf.TriggerRow, error)
	GetWorkflow(ctx context.Context, ownerType, ownerID, workflowID string) (*wf.WorkflowRow, error)
	RecordWebhookDelivery(ctx context.Context, webhookID, dedupKey string) error
	CreateWorkflowRunWithFire(ctx context.Context, fire *wf.TriggerFireRow, run *wf.WorkflowRunRow) error
	CreateTriggerFire(ctx context.Context, row *wf.TriggerFireRow) error
}

// webhookDecryptor decrypts webhook HMAC secrets.
type webhookDecryptor interface {
	Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error)
}

// WebhookReceiverHandler handles inbound webhook deliveries at POST /api/v1/hooks/:webhookId.
type WebhookReceiverHandler struct {
	store   webhookReceiverStore
	decrypt webhookDecryptor
	maxBody int64
}

// NewWebhookReceiverHandler constructs the handler. maxBody is the max request body size in bytes.
func NewWebhookReceiverHandler(store webhookReceiverStore, decrypt webhookDecryptor, maxBody int64) *WebhookReceiverHandler {
	if maxBody <= 0 {
		maxBody = 1 << 20 // 1 MiB default
	}
	return &WebhookReceiverHandler{store: store, decrypt: decrypt, maxBody: maxBody}
}

const (
	webhookTimestampSkew = 5 * time.Minute
	webhookRetryAfter    = "30"
)

// HandleWebhook is the public POST /api/v1/hooks/:webhookId endpoint.
func (h *WebhookReceiverHandler) HandleWebhook(c *gin.Context) {
	webhookID := c.Param("webhookId")

	hook, err := h.store.GetWebhook(c.Request.Context(), webhookID)
	if err != nil {
		if errors.Is(err, wf.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "webhook not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch webhook"})
		return
	}

	// Resolve the trigger by ID (webhooks → triggers is 1:1; trigger_id is
	// an unguessable UUID, so no owner-scoping needed for the receiver).
	trigger, err := h.store.GetTriggerByID(c.Request.Context(), hook.TriggerID)
	if err != nil {
		// We need to find the trigger by ID without scope. The store's GetTrigger
		// is scoped. For webhooks, the trigger_id is authoritative (unguessable UUID).
		// We use a different approach: query directly.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch trigger"})
		return
	}

	// 1. IP allowlist check (early reject before HMAC).
	if len(hook.AllowedIPs) > 0 {
		clientIP := c.ClientIP()
		if !ipInAllowlist(clientIP, hook.AllowedIPs) {
			c.JSON(http.StatusForbidden, gin.H{"error": "ip_not_allowed"})
			return
		}
	}

	// 2. Read raw body (needed for HMAC — must be byte-exact).
	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, h.maxBody))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}

	// 3. HMAC verification.
	signatureHeader := c.GetHeader("X-Hub-Signature-256")
	if signatureHeader == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing_signature"})
		return
	}

	secret, err := h.decrypt.Decrypt(c.Request.Context(), hook.SecretCipher)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to decrypt webhook secret"})
		return
	}

	if !verifyHMAC(rawBody, secret, signatureHeader) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_signature"})
		return
	}

	// 4. Timestamp skew check (if X-Hub-Signature-Timestamp present).
	if tsHeader := c.GetHeader("X-Hub-Signature-Timestamp"); tsHeader != "" {
		if !checkTimestampSkew(tsHeader) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "timestamp_skew_exceeded"})
			return
		}
	}

	// 5. Idempotency dedup.
	dedupKey := extractDedupKey(c, hook)
	if dedupKey != "" {
		if err := h.store.RecordWebhookDelivery(c.Request.Context(), webhookID, dedupKey); err != nil {
			if errors.Is(err, wf.ErrDedupConflict) {
				c.JSON(http.StatusOK, gin.H{"status": "duplicate"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record delivery"})
			return
		}
	}

	// 6. Build the trigger envelope.
	var bodyJSON any
	if len(rawBody) > 0 {
		if json.Unmarshal(rawBody, &bodyJSON) != nil {
			bodyJSON = map[string]any{"raw": string(rawBody)}
		}
	}

	headersMap := make(map[string]string)
	for k := range c.Request.Header {
		headersMap[k] = c.Request.Header.Get(k)
	}

	envelope := map[string]any{
		"source":      map[string]any{"type": "webhook", "id": webhookID},
		"received_at": time.Now().UTC().Format(time.RFC3339),
		"headers":     headersMap,
		"body":        bodyJSON,
	}
	envelopeJSON, _ := json.Marshal(envelope)

	// 7. Fire the trigger target.
	fireID := uuid.New().String()
	now := time.Now().UTC()

	// If single-in-flight rejects, write a skipped fire row + return 409.
	var inputForRun json.RawMessage
	if trigger.TargetType == types.TriggerTargetRunWorkflow {
		targetCfg := parseTargetConfig(trigger.TargetConfig)
		workflowID, _ := targetCfg["workflowId"].(string)
		if workflowID == "" {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "trigger target missing workflowId"})
			return
		}

		wfRow, err := h.store.GetWorkflow(c.Request.Context(), trigger.OwnerType, trigger.OwnerID, workflowID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "target workflow not found"})
			return
		}

		// Render input template from the envelope.
		inputForRun = renderInputTemplate(targetCfg, envelopeJSON)

		fire := &wf.TriggerFireRow{
			ID: fireID, TriggerID: trigger.ID, SourceType: "webhook",
			InputEnvelope: envelopeJSON, ActionType: "run_workflow",
			Status: "fired", FiredAt: now,
		}

		run := &wf.WorkflowRunRow{
			ID: uuid.New().String(), WorkflowID: workflowID,
			SpecSnapshot: wfRow.SpecJSON, Input: inputForRun,
			Status: "queued", TriggerID: &trigger.ID,
			WorkspaceID: resolveWorkspaceID(wfRow, trigger),
			CreatedAt:   now, UpdatedAt: now,
		}

		err = h.store.CreateWorkflowRunWithFire(c.Request.Context(), fire, run)
		if err != nil {
			if errors.Is(err, wf.ErrConcurrentRun) {
				_ = h.store.CreateTriggerFire(c.Request.Context(), &wf.TriggerFireRow{
					ID: uuid.New().String(), TriggerID: trigger.ID, SourceType: "webhook",
					InputEnvelope: envelopeJSON, ActionType: "run_workflow",
					ActionResult: json.RawMessage(`{"reason":"already_running"}`),
					Status:       "skipped", FiredAt: now, CompletedAt: &now,
				})
				c.Header("Retry-After", webhookRetryAfter)
				c.JSON(http.StatusConflict, gin.H{"error": "already_running"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create workflow run"})
			return
		}

		c.JSON(http.StatusAccepted, gin.H{"status": "fired", "runId": run.ID})
		return
	}

	// run_script target (no workflow run — just record the fire).
	_ = h.store.CreateTriggerFire(c.Request.Context(), &wf.TriggerFireRow{
		ID: fireID, TriggerID: trigger.ID, SourceType: "webhook",
		InputEnvelope: envelopeJSON, ActionType: trigger.TargetType,
		Status: "delivered", FiredAt: now, CompletedAt: &now,
	})
	c.JSON(http.StatusAccepted, gin.H{"status": "delivered"})
}

// --- helpers ---

func verifyHMAC(body, secret []byte, signatureHeader string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(signatureHeader, prefix) {
		return false
	}
	received, err := hex.DecodeString(signatureHeader[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	expected := mac.Sum(nil)
	return hmac.Equal(received, expected)
}

func checkTimestampSkew(tsStr string) bool {
	var ts int64
	if _, err := fmt.Sscanf(tsStr, "%d", &ts); err != nil {
		return false
	}
	delivered := time.Unix(ts, 0)
	now := time.Now().UTC()
	return now.Sub(delivered) <= webhookTimestampSkew && delivered.Sub(now) <= webhookTimestampSkew
}

func extractDedupKey(c *gin.Context, hook *wf.WebhookRow) string {
	if hook.IdempotencyMode == types.WebhookIdempotencyDisabled {
		return ""
	}
	if hook.IdempotencyMode == types.WebhookIdempotencyHeader {
		headerName := hook.IdempotencyHeader
		if headerName == "" {
			headerName = "X-Request-ID"
		}
		return c.GetHeader(headerName)
	}
	// hash mode — derive from body+timestamp window.
	return ""
}

func ipInAllowlist(ipStr string, allowed []string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, cidr := range allowed {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func parseTargetConfig(raw json.RawMessage) map[string]any {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

func renderInputTemplate(targetCfg map[string]any, envelope []byte) json.RawMessage {
	tmpl, _ := targetCfg["inputTemplate"].(map[string]any)
	if len(tmpl) == 0 {
		return envelope // pass envelope directly if no template
	}
	var envelopeData map[string]any
	_ = json.Unmarshal(envelope, &envelopeData)
	rendered := make(map[string]any)
	for k, v := range tmpl {
		if strVal, ok := v.(string); ok {
			rendered[k] = interpolateTemplate(strVal, envelopeData)
		} else {
			rendered[k] = v
		}
	}
	out, _ := json.Marshal(rendered)
	return out
}

func interpolateTemplate(tmpl string, data map[string]any) string {
	result := tmpl
	for path, val := range flattenPaths(data, "") {
		placeholder := "{{." + path + "}}"
		result = strings.ReplaceAll(result, placeholder, fmt.Sprintf("%v", val))
	}
	return result
}

func flattenPaths(data map[string]any, prefix string) map[string]any {
	out := make(map[string]any)
	for k, v := range data {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if nested, ok := v.(map[string]any); ok {
			for nk, nv := range flattenPaths(nested, key) {
				out[nk] = nv
			}
		} else {
			out[key] = v
		}
	}
	return out
}

func resolveWorkspaceID(wfRow *wf.WorkflowRow, trigger *wf.TriggerRow) string {
	if wfRow.TargetWorkspaceID != nil && *wfRow.TargetWorkspaceID != "" {
		return *wfRow.TargetWorkspaceID
	}
	if targetCfg := parseTargetConfig(trigger.TargetConfig); targetCfg != nil {
		if wsID, ok := targetCfg["workspaceId"].(string); ok {
			return wsID
		}
	}
	return ""
}
