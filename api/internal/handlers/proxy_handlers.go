// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	"github.com/lenaxia/llmsafespaces/pkg/agent/systemnotices"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/session/attachments"
)

func (h *ProxyHandler) CreateSession(c *gin.Context) {
	if h.adapter == nil {
		h.adapterUnavailable(c)
		return
	}
	wid := c.Param("id")
	_, ok := h.resolveWorkspaceForAdapter(c, wid)
	if !ok {
		return
	}
	defer h.releaseConnection(wid)

	s, err := h.adapter.CreateSession(c.Request.Context(), "", wid, "")
	if err != nil {
		h.logger.Error("CreateSession: adapter failed", err, "workspaceID", wid)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to create session"})
		return
	}
	// Index at creation (design 0054 R5): the session index is
	// otherwise event-fed, and V2-mode sessions don't emit the V1
	// session lifecycle events the indexer consumes — new sessions
	// never reached the sidebar (2026-08-29: agent listed 3, API
	// served 2). The platform OWNS session CRUD; index where it
	// happens instead of hoping an event follows.
	h.recordActivityIfTracked(wid)
	if s != nil && h.sessionIndex != nil {
		h.persistSessionMeta(context.Background(), wid, s.ID, s.Title, s.ParentID)
	}
	c.JSON(http.StatusOK, s)
}

func (h *ProxyHandler) ListSessions(c *gin.Context) {
	if h.adapter == nil {
		h.adapterUnavailable(c)
		return
	}
	wid := c.Param("id")
	_, ok := h.resolveWorkspaceForAdapter(c, wid)
	if !ok {
		return
	}
	defer h.releaseConnection(wid)
	h.adapterEnsureSSEWatch(wid)

	sessions, err := h.adapter.ListSessions(c.Request.Context(), "", wid)
	if err != nil {
		h.logger.Error("ListSessions: adapter failed", err, "workspaceID", wid)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to list sessions"})
		return
	}
	h.recordActivityIfTracked(wid)
	c.JSON(http.StatusOK, sessions)
}

func (h *ProxyHandler) SendMessage(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	wid := c.Param("id")

	if rejectMessageRouteFiles(c) {
		return
	}

	if h.adapter == nil {
		h.adapterUnavailable(c)
		return
	}

	// adapter.Send returns a typed session.Message with contract-shaped
	// parts. The response is contract JSON, not raw opencode bytes.
	text, bodyBytes, err := extractMessageText(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(text) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "text must not be empty"})
		return
	}
	if len(text) > 100_000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "text exceeds 100KB limit"})
		return
	}

	workspace, ok := h.resolveWorkspaceForAdapter(c, wid)
	if !ok {
		return
	}
	defer h.releaseConnection(wid)

	if !h.checkAdapterSessionLimit(c, workspace, wid, sid) {
		return
	}
	if !h.checkAdapterQuota(c, workspace) {
		if sid != "" {
			h.removeActiveSession(c.Request.Context(), wid, sid)
		}
		return
	}
	h.adapterEnsureSSEWatch(wid)

	// Per-prompt model selector: symmetric with SendPromptAsync
	// (PR #909 review round — /message is the SDK-documented
	// synchronous send path and must honor the same override).
	modelOverride := extractPromptModel(bodyBytes)
	if modelOverride != nil && !h.modelOverrideAllowed(c.Request.Context(), workspace, modelOverride) {
		// Release the slot checkAdapterSessionLimit reserved — the
		// quota and adapter-error paths do the same (#913 review
		// round 3, finding 4: a leaked slot would count against the
		// session limit without an active send).
		if sid != "" {
			h.removeActiveSession(c.Request.Context(), wid, sid)
		}
		c.JSON(http.StatusForbidden, gin.H{"error": "model not allowed by organization policy"})
		return
	}

	msg, err := h.adapter.Send(c.Request.Context(), "", wid, sid, text, session.SendOpts{
		Model: modelOverride,
	})
	if err != nil {
		// #817: log the underlying adapter error — without this the
		// 502 body says only "failed to send message" and the root
		// cause (context deadline, connection reset, decode failure)
		// is invisible in production.
		h.logger.Error("SendMessage: adapter failed", err,
			"workspaceID", wid, "sessionID", sid)
		if sid != "" {
			h.removeActiveSession(c.Request.Context(), wid, sid)
		}
		errBody := []byte(`{"error":"failed to send message"}`)
		if h.agentStateChecker != nil {
			changedAt, checkerErr := h.agentStateChecker.GetLastCredentialChangedAt(c.Request.Context(), wid)
			if checkerErr == nil && !changedAt.IsZero() {
				errBody = EnrichChatErrorBody(errBody, true, changedAt, wid)
			}
		}
		c.Data(http.StatusBadGateway, "application/json", errBody)
		return
	}
	h.postAdapterSuccess(c, workspace, wid, sid, true)
	c.JSON(http.StatusOK, msg)
	if h.sessionIndex != nil {
		go h.fetchAndPersistTitle(wid, sid)
	}
}

func (h *ProxyHandler) SendPromptAsync(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	wid := c.Param("id")

	// Extract text from the V1 parts body (files compose into the text
	// before this point). The body cap bounds allocation before the
	// 100KB text check below rejects oversized prompts.
	const maxPromptBodyBytes = 100_000 + 1024
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxPromptBodyBytes)
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}
	_ = c.Request.Body.Close()
	text, perr := extractPromptText(bodyBytes)
	if perr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": perr.Error()})
		return
	}
	if files := extractPromptFiles(bodyBytes); len(files) > 0 {
		composed, cerr := attachments.Compose(text, files)
		if cerr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": cerr.Error()})
			return
		}
		text = composed
	}
	if len(text) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "text must not be empty"})
		return
	}
	if len(text) > 100_000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "text exceeds 100KB limit"})
		return
	}

	if h.adapter == nil {
		h.adapterUnavailable(c)
		return
	}

	// D3 outbox path (design 0050 §D3, #907): accept into the Valkey
	// outbox and 202 immediately — delivery happens in a detached worker
	// so a client disconnect (iOS killing an in-flight POST, the
	// 2026-08-15/16 incident's 6× context-canceled loss class) can never
	// lose an accepted message.
	if h.outbox != nil {
		workspace, ok := h.resolveWorkspaceForAdapter(c, wid)
		if !ok {
			return
		}
		defer h.releaseConnection(wid)

		if !h.checkAdapterSessionLimit(c, workspace, wid, sid) {
			return
		}
		if !h.checkAdapterQuota(c, workspace) {
			if sid != "" {
				h.removeActiveSession(c.Request.Context(), wid, sid)
			}
			return
		}
		h.adapterEnsureSSEWatch(wid)

		var cmid string
		if req := struct {
			ClientMessageID string `json:"clientMessageID"`
		}{}; json.Unmarshal(bodyBytes, &req) == nil {
			cmid = req.ClientMessageID
		}
		userID, _ := extractAuth(c)
		modelOverride := extractPromptModel(bodyBytes)

		// Org model-policy enforcement on the explicit override — the
		// SAME check the sync path applies (r1 finding 1: the outbox must
		// not become the policy bypass).
		if !h.modelOverrideAllowed(c.Request.Context(), workspace, modelOverride) {
			if sid != "" {
				h.removeActiveSession(c.Request.Context(), wid, sid)
			}
			c.JSON(http.StatusForbidden, gin.H{"error": "model not allowed by organization policy"})
			return
		}

		var modelJSON json.RawMessage
		if modelOverride != nil {
			modelJSON, _ = json.Marshal(modelOverride)
		}

		e, err := h.outbox.Accept(c.Request.Context(), wid, sid, userID, cmid, text, modelJSON)
		if err != nil {
			var dup *outbox.Duplicate
			if errors.As(err, &dup) {
				// Retry of an accepted message: 200 with the ORIGINAL ID
				// (idempotent accept), not an error (r1 finding 7).
				c.JSON(http.StatusOK, gin.H{"messageID": dup.AcceptedID, "clientMessageID": cmid, "status": "duplicate"})
				return
			}
			if errors.Is(err, outbox.ErrCapped) {
				c.JSON(http.StatusTooManyRequests, gin.H{"error": "session queue is full", "retryAfter": 10})
				return
			}
			h.logger.Error("outbox accept failed", err, "workspaceID", wid, "sessionID", sid)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to accept message"})
			return
		}
		c.JSON(http.StatusAccepted, gin.H{
			"messageID":       e.ID,
			"clientMessageID": e.ClientMessageID,
			"status":          "queued",
		})
		return
	}

	// Synchronous fallback when the outbox is unset (dev/test): the
	// outbox arm above returns unconditionally, so reaching here means
	// no outbox. The per-prompt model selector (contract type
	// session.ModelRef) is forwarded — what keeps a workspace usable
	// when its persisted default model is unresolvable (incident
	// 2026-08-16).
	h.syncSend(c, wid, sid, text, extractPromptModel(bodyBytes))
}

// extractMessageText reads the request body and extracts the
// concatenated text from opencode's {parts:[{type:"text",text:"..."}]}
// shape. Caps at 100KB to match the SendPromptAsync body limit. Returns
// the raw body bytes alongside the text so callers can also extract the
// per-prompt model selector without a second read.
func extractMessageText(c *gin.Context) (string, []byte, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 100_000+1024)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return "", nil, fmt.Errorf("failed to read request body: %w", err)
	}
	_ = c.Request.Body.Close()
	text, perr := extractPromptText(body)
	if perr != nil {
		return "", nil, perr
	}
	return text, body, nil
}

// extractPromptText parses a prompt body and returns the
// concatenation of all text parts. Returns an error only if the body
// is not valid JSON. Empty/whitespace-only text is returned as "" so
// the caller can apply its own empty-check policy.
func extractPromptText(body []byte) (string, error) {
	var parsed struct {
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("invalid request body: %w", err)
	}
	var sb strings.Builder
	for _, p := range parsed.Parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String(), nil
}

func extractPromptFiles(body []byte) []string {
	var parsed struct {
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	return parsed.Files
}

func rejectMessageRouteFiles(c *gin.Context) bool {
	if c.Request.Body == nil || c.Request.ContentLength == 0 {
		return false
	}
	limited := http.MaxBytesReader(c.Writer, c.Request.Body, 10*1024*1024)
	bodyBytes, err := io.ReadAll(limited)
	_ = c.Request.Body.Close()
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body exceeds 10 MB limit"})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		}
		return true
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	var probe struct {
		Files []string `json:"files"`
	}
	if json.Unmarshal(bodyBytes, &probe) == nil && len(probe.Files) > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "files not supported on this route; use /prompt"})
		return true
	}
	return false
}

// modelOverrideAllowed enforces the org's allowed-models/allowed-providers
// policy on an explicit per-prompt model override. The workspace CRD already
// carries the org (Spec.Owner.OrgID), so no DB round-trip.
//
// Model axis: the client-sent modelID (the catalog-advertised form) or its
// bare tail — SetModel and ListModels accept the same two forms.
//
// Provider axis: m.Provider is AUTHORITATIVE when present — modelOverride
// (pkg/agent/opencode) uses it as the routing providerID, so it is the
// only provider-axis check. For provider-less slashed IDs, the embedded
// FIRST-segment prefix is the routing provider (opencode's split — the
// incident proved bare IDs parse as first-segment provider + empty
// modelID) and is checked instead. An
// empty provider axis (flat ID, no providerID) skips the axis — the
// adapter degrades such refs to the session default, which SetModel
// already screened.
//
// Fails open on policy-infra errors and for personal workspaces — same
// semantics as ListModels' filter and SetModel's check (governance filter,
// not an availability gate).
func (h *ProxyHandler) modelOverrideAllowed(ctx context.Context, workspace *v1.Workspace, m *session.ModelRef) bool {
	if h.modelPolicyChecker == nil || m == nil {
		return true
	}
	if workspace == nil || workspace.Spec.Owner.OrgID == "" {
		return true // personal workspace — no org policy dimension
	}
	pol, err := h.modelPolicyChecker.GetEffectivePolicy(ctx, workspace.Spec.Owner.OrgID)
	if err != nil || pol == nil {
		h.logger.Warn("prompt model-override policy check unavailable, failing open",
			"error", err, "orgID", workspace.Spec.Owner.OrgID)
		return true
	}
	if !pol.IsModelAllowed(m.ID) {
		// Slashed catalog IDs: an org may allowlist either the advertised
		// full ID ("anthropic/claude-sonnet-4.5") or the bare tail
		// ("claude-sonnet-4.5") — accept both, matching SetModel's axis.
		tailAllowed := false
		if idx := strings.Index(m.ID, "/"); idx >= 0 && idx < len(m.ID)-1 {
			tailAllowed = pol.IsModelAllowed(m.ID[idx+1:])
		}
		if !tailAllowed {
			return false
		}
	}
	// Provider axis: m.Provider is AUTHORITATIVE when present — the
	// adapter uses it as the routing providerID (modelOverride), so it is
	// the provider routing actually uses. The embedded first-segment
	// prefix is checked only for provider-less slashed IDs (round 3's
	// bypass shape); checking it when Provider is set would false-deny
	// the frontend's double form
	// ({modelID:"vendor/model", providerID:"openrouter"} — the first
	// segment is a vendor namespace, not the routing provider).
	if m.Provider != "" {
		return pol.IsProviderAllowed(m.Provider)
	}
	if idx := strings.Index(m.ID, "/"); idx > 0 && idx < len(m.ID)-1 {
		return pol.IsProviderAllowed(m.ID[:idx])
	}
	return true
}

// extractPromptModel parses the per-prompt model selector
// ({"model":{"modelID":...,"providerID":...}}) from a prompt body into the
// contract's ModelRef. Returns nil when absent, empty, or malformed — the
// agent's session default applies. Malformed bodies never fail the prompt:
// text extraction has already validated the JSON, and a bad selector should
// degrade to default-model routing, not reject the user's message.
func extractPromptModel(body []byte) *session.ModelRef {
	var parsed struct {
		Model *struct {
			ModelID    string `json:"modelID"`
			ProviderID string `json:"providerID"`
		} `json:"model"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil
	}
	if parsed.Model == nil || parsed.Model.ModelID == "" {
		return nil
	}
	return &session.ModelRef{ID: parsed.Model.ModelID, Provider: parsed.Model.ProviderID}
}

// historyPageDefaultLimit is the default page size when ?limit= is omitted.
// Mirrors the value the frontend (api/messages.ts) already requests.
const historyPageDefaultLimit = 50

// historyPageMaxLimit caps ?limit= so a misbehaving client cannot force
// the API to materialize an unbounded message slice in memory.
const historyPageMaxLimit = 200

// GetHistory returns a chronological page of contract messages for a
// session, served by the agent Adapter (the raw-proxy tail was deleted
// in #828 batch 2).
//
// Query parameters:
//   - limit: page size (default 50, max 200). Counts RAW messages as the
//     agent returns them — the adapter translates afterwards
//     (slice-then-translate): step-start/step-finish parts are dropped,
//     so a marker-only message can surface with empty parts, and system
//     roles are contract data. Rejecting invalid limits (<=0 or
//     non-numeric) surfaces client bugs early.
//   - before: opaque cursor — the message id of the OLDEST message in the
//     previously-rendered page. Returns messages strictly older than
//     this cursor. Absent => return the newest `limit` messages via the
//     agent's native pagination (#971); a full page optimistically emits
//     a cursor even when no older messages exist (one bounded spurious
//     back-page).
//
// Response:
//   - body: JSON array of contract session.Message values, oldest-first
//     within the page (the adapter translator's output — see the
//     slice-then-translate note under `limit` above).
//   - X-Next-Cursor header: the id of the OLDEST message in the returned
//     page. On the first-page native fetch a FULL page emits the cursor
//     optimistically (#971 — older messages may exist beyond what the
//     agent's page can see); a spurious cursor costs one back-page fetch
//     that returns empty and no further cursor. Absent means no more
//     messages to fetch.
func (h *ProxyHandler) GetHistory(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}

	// Parse + validate pagination params before touching the cluster.
	limit, err := parseHistoryLimit(c.Query("limit"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	before := c.Query("before")
	wid := c.Param("id")

	if h.adapter == nil {
		h.adapterUnavailable(c)
		return
	}

	// Typed session.Message[] from the Adapter, contract-shaped JSON to
	// the client. The Adapter translator already drops
	// step-start/step-finish and collects patch file paths, so the
	// response is clean contract data — no opencode-specific shapes.
	_, ok := h.resolveWorkspaceForAdapter(c, wid)
	if !ok {
		return
	}
	defer h.releaseConnection(wid)
	h.adapterEnsureSSEWatch(wid)

	// #971: the FIRST page (no before-cursor) is the session-load
	// hot path — fetch ONLY the newest `limit` messages via the
	// agent's native pagination (measured: ~26ms vs ~1.8s full
	// fetch+decode on a 452-message session). Older pages (user
	// scrolled back) keep the full-fetch path: opencode 1.18.10's
	// cursor params are unusable (before= rejects every shape
	// probed; cursor= is ignored — verified live), so slicing the
	// full list remains the only correct back-pagination.
	var msgs []session.Message
	if before == "" {
		msgs, err = h.adapter.GetHistoryPage(c.Request.Context(), "", wid, sid, limit)
	} else {
		msgs, err = h.adapter.GetHistory(c.Request.Context(), "", wid, sid)
	}
	if err != nil {
		h.logger.Error("GetHistory: adapter failed", err, "sessionID", sid)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch history"})
		return
	}
	page, nextCursor := paginateContractHistory(msgs, limit, before)
	if page == nil {
		page = []session.Message{}
	}
	// Paged-path gap (#971): paginateContractHistory sets the cursor
	// only when it can SEE older messages (start > 0 in the full
	// list) — but the native page hides everything older. A full page
	// (len == limit) means older messages MAY exist; optimistically
	// emit the page's oldest id. A spurious cursor costs one
	// back-page fetch that returns empty and no further cursor.
	if before == "" && nextCursor == "" && len(page) == limit && len(page) > 0 {
		nextCursor = page[0].ID
	}
	if nextCursor != "" {
		c.Header("X-Next-Cursor", nextCursor)
	}
	h.recordActivityIfTracked(wid)
	c.JSON(http.StatusOK, page)
}

// parseHistoryLimit normalises the ?limit query parameter. An empty
// string falls back to the default; any other value must parse to a
// strictly positive integer. The result is capped at historyPageMaxLimit.
func parseHistoryLimit(raw string) (int, error) {
	if raw == "" {
		return historyPageDefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid limit %q: must be a positive integer", raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("invalid limit %d: must be > 0", n)
	}
	if n > historyPageMaxLimit {
		n = historyPageMaxLimit
	}
	return n, nil
}

// paginateContractHistory applies cursor-based pagination on typed
// session.Message values from the Adapter. Simpler than the deleted raw
// path because the Adapter translator already dropped
// non-displayable messages (step-start/step-finish) and filtered parts.
// Returns the page + next cursor (empty if no older messages remain).
func paginateContractHistory(msgs []session.Message, limit int, before string) ([]session.Message, string) {
	// Determine the inclusive end (exclusive of the cursor itself).
	endExclusive := len(msgs)
	if before != "" {
		idx := -1
		for i, m := range msgs {
			if m.ID == before {
				idx = i
				break
			}
		}
		if idx < 0 {
			return []session.Message{}, ""
		}
		endExclusive = idx
	}

	start := endExclusive - limit
	if start < 0 {
		start = 0
	}
	page := msgs[start:endExclusive]

	nextCursor := ""
	if start > 0 && len(page) > 0 {
		nextCursor = page[0].ID
	}
	return page, nextCursor
}

func (h *ProxyHandler) GetSession(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	if h.adapter == nil {
		h.adapterUnavailable(c)
		return
	}
	wid := c.Param("id")
	_, ok := h.resolveWorkspaceForAdapter(c, wid)
	if !ok {
		return
	}
	defer h.releaseConnection(wid)
	h.adapterEnsureSSEWatch(wid)

	s, err := h.adapter.GetSession(c.Request.Context(), "", wid, sid)
	if err != nil {
		h.logger.Error("GetSession: adapter failed", err, "workspaceID", wid, "sessionID", sid)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to get session"})
		return
	}
	h.recordActivityIfTracked(wid)
	c.JSON(http.StatusOK, s)
}

func (h *ProxyHandler) AbortSession(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	wid := c.Param("id")

	// adapter.Abort: the V1 POST /session/:id/abort (the only interrupt
	// endpoint on opencode 1.18.10+) destructively stops the in-flight
	// turn — queued input is not preserved, unlike the old V2 interrupt
	// which was removed in 1.18.10. (A former "clear pending tracking"
	// step died with the V2 path; stranded-input recovery is the
	// outbox ledger's concern.)
	if h.adapter == nil {
		h.adapterUnavailable(c)
		return
	}
	if err := h.adapter.Abort(c.Request.Context(), "", wid, sid); err != nil {
		h.logger.Error("AbortSession: adapter abort failed", err, "workspaceID", wid, "sessionID", sid)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to abort session"})
		return
	}
	h.recordActivityIfTracked(wid)
	c.Status(http.StatusNoContent)
}

func (h *ProxyHandler) DeleteSession(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	workspaceID := c.Param("id")

	// Delegate to the adapter, then run the post-delete side effects
	// (tombstone, session index cleanup, SSE tombstone publish).
	if h.adapter == nil {
		h.adapterUnavailable(c)
		return
	}
	if err := h.adapter.DeleteSession(c.Request.Context(), "", workspaceID, sid); err != nil {
		// #817: same observability gap — log the underlying error.
		h.logger.Error("DeleteSession: adapter failed", err,
			"workspaceID", workspaceID, "sessionID", sid)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to delete session"})
		return
	}
	c.Status(http.StatusNoContent)
	h.recordActivityIfTracked(workspaceID)

	// Post-delete side effects run after a successful adapter delete.
	h.state().MarkSessionDeleted(context.Background(), workspaceID, sid) //nolint:contextcheck // tombstone must survive client disconnect

	if h.sessionIndex != nil {
		if err := h.sessionIndex.DeleteSession(context.Background(), workspaceID, sid); err != nil { //nolint:contextcheck
			h.logger.Error("failed to delete session from index", err, "workspaceID", workspaceID, "sessionID", sid)
		}
	}

	go func() {
		h.removeActiveSession(context.Background(), workspaceID, sid)
		if h.sessionParents != nil {
			h.sessionParents.invalidate(workspaceID)
		}
		if h.userBroker != nil {
			h.publishWorkspaceEvent(workspaceID, apitypes.WorkspaceSSEEvent{
				Type:      "session.status",
				SessionID: sid,
				Status:    "deleted",
			})
		}
	}()
}

// isSessionDeleted returns true if the session was recently deleted via the
// API and late events should be suppressed. Delegates to the state store —
// the store's in-memory implementation matches the prior ProxyHandler
// behavior exactly; a future Redis-backed implementation will move
// tombstones to a shared key so the suppression is cluster-wide.
func (h *ProxyHandler) isSessionDeleted(workspaceID, sessionID string) bool {
	return h.state().IsSessionDeleted(context.Background(), workspaceID, sessionID)
}

// RenameSessionInAgent sends a title update to the opencode agent running on
// the workspace pod so that the agent's in-memory session title matches the
// user-assigned title. Without this, the periodic title fetch (useSessionTitle
// hook in the frontend) retrieves the old agent-side title and overwrites the
// user's rename in PostgreSQL.
func (h *ProxyHandler) RenameSessionInAgent(ctx context.Context, workspaceID, sessionID, title string) error {
	if err := validateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid sessionId: %w", err)
	}

	if h.adapter == nil {
		return fmt.Errorf("agent adapter not configured")
	}
	return h.adapter.RenameSession(ctx, "", workspaceID, sessionID, title)
}

var sessionIDPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func validateSessionID(s string) error {
	if s == "" {
		return errors.New("sessionId must not be empty")
	}
	if len(s) > 128 {
		return errors.New("sessionId exceeds the 128-character limit")
	}
	if strings.Contains(s, "..") {
		return errors.New("sessionId contains forbidden '..' (path traversal)")
	}
	if !sessionIDPattern.MatchString(s) {
		return errors.New("sessionId contains characters outside [a-zA-Z0-9._-]")
	}
	return nil
}

type enqueueRequest struct {
	ClientMessageID string   `json:"clientMessageID,omitempty"`
	Text            string   `json:"text"`
	Files           []string `json:"files,omitempty"`
}

// queuedMessageResponse is the typed JSON shape for a queue list entry.
// Mirrors the queued-message wire shape so the frontend is unchanged.
type queuedMessageResponse struct {
	ID          string `json:"id"`
	Text        string `json:"text"`
	SessionID   string `json:"session_id"`
	WorkspaceID string `json:"workspace_id"`
	EnqueuedAt  string `json:"enqueued_at"`
	// D3 (#907) outbox fields: the UI's retry button keys on status=error;
	// attempts/lastError give the user retry context; clientMessageID is
	// the render-dedupe key. blockedByInFlight/inFlightFor (#1019 D)
	// distinguish "queued behind the current turn" from "frozen behind a
	// stale lock" — the incident's silent-no-send signal.
	Status          string `json:"status"`
	Attempts        int    `json:"attempts"`
	LastError       string `json:"lastError,omitempty"`
	ClientMessageID string `json:"clientMessageID,omitempty"`
	BlockedInFlight bool   `json:"blockedByInFlight,omitempty"`
	InFlightForMs   int64  `json:"inFlightForMs,omitempty"`
}

type queueListResponse struct {
	Messages []queuedMessageResponse `json:"messages"`
}

func (h *ProxyHandler) EnqueueMessage(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	wid := c.Param("id")

	// Cap the body before ShouldBindJSON reads it. Without this, a client
	// could force the API to allocate an arbitrarily large buffer in memory
	// before the 100KB text check below rejects it. Same pattern as
	// proxy.go:275. 100KB text limit + 1KB slack for JSON overhead.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 100_000+1024)
	var req enqueueRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if len(req.Files) > 0 {
		composed, cerr := attachments.Compose(req.Text, req.Files)
		if cerr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": cerr.Error()})
			return
		}
		req.Text = composed
	}
	if len(req.Text) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "text must not be empty"})
		return
	}
	if len(req.Text) > 100_000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "text exceeds 100KB limit"})
		return
	}

	// D3 (#907): with the outbox wired, enqueue and prompt are the SAME
	// accept (single path — client-decides routing is retired). The
	// clientMessageID field rides the same body.
	if h.adapter == nil {
		h.adapterUnavailable(c)
		return
	}
	if h.outbox != nil {
		workspace, ok := h.resolveWorkspaceForAdapter(c, wid)
		if !ok {
			return
		}
		defer h.releaseConnection(wid)
		if !h.checkAdapterSessionLimit(c, workspace, wid, sid) {
			return
		}
		if !h.checkAdapterQuota(c, workspace) {
			if sid != "" {
				h.removeActiveSession(c.Request.Context(), wid, sid)
			}
			return
		}
		h.adapterEnsureSSEWatch(wid)
		userID, _ := extractAuth(c)
		var modelJSON json.RawMessage
		e, err := h.outbox.Accept(c.Request.Context(), wid, sid, userID, req.ClientMessageID, req.Text, modelJSON)
		if err != nil {
			var dup *outbox.Duplicate
			if errors.As(err, &dup) {
				c.JSON(http.StatusOK, gin.H{"messageID": dup.AcceptedID, "status": "duplicate"})
				return
			}
			if errors.Is(err, outbox.ErrCapped) {
				c.JSON(http.StatusTooManyRequests, gin.H{"error": "session queue is full", "retryAfter": 10})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to enqueue"})
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"messageID": e.ID, "status": "queued"})
		return
	}

	h.syncSend(c, wid, sid, req.Text, nil)
}

// syncSend is the synchronous adapter send shared by SendPromptAsync and
// EnqueueMessage when the outbox is unset (dev/test): the D3 outbox is the
// production accept path; this direct send preserves the queue route's
// pre-outbox contract (accept → adapter.Send → full response) for
// adapter-only deployments without it.
func (h *ProxyHandler) syncSend(c *gin.Context, wid, sid, text string, modelOverride *session.ModelRef) {
	workspace, ok := h.resolveWorkspaceForAdapter(c, wid)
	if !ok {
		return
	}
	defer h.releaseConnection(wid)

	if !h.checkAdapterSessionLimit(c, workspace, wid, sid) {
		return
	}
	if !h.checkAdapterQuota(c, workspace) {
		if sid != "" {
			h.removeActiveSession(c.Request.Context(), wid, sid)
		}
		return
	}
	h.adapterEnsureSSEWatch(wid)

	// Org policy enforcement on the explicit override: ListModels hides
	// and SetModel rejects disallowed models; the send override must not
	// be the remaining bypass. The session slot reserved above is
	// released on denial (#913 review round 3, finding 4).
	if modelOverride != nil && !h.modelOverrideAllowed(c.Request.Context(), workspace, modelOverride) {
		if sid != "" {
			h.removeActiveSession(c.Request.Context(), wid, sid)
		}
		c.JSON(http.StatusForbidden, gin.H{"error": "model not allowed by organization policy"})
		return
	}

	msg, err := h.adapter.Send(c.Request.Context(), "", wid, sid, text, session.SendOpts{
		Model: modelOverride,
	})
	if err != nil {
		h.logger.Error("syncSend: adapter failed", err,
			"workspaceID", wid, "sessionID", sid)
		if sid != "" {
			h.removeActiveSession(c.Request.Context(), wid, sid)
		}

		// #944: typed disk-full classification. ENOSPC cannot be
		// detected from the upstream 500 body (the cause lives only in
		// opencode's in-pod log), so classification uses the CRD disk
		// status already in scope. At/above critical the client gets
		// 507 {"code":"disk_full"} with the usage numbers — the
		// incident's generic 502 rendered as a bare "Failed to fetch"
		// with no cause. Below critical, unrelated provider/pod errors
		// keep the generic 502.
		if systemnotices.LevelForRatio(diskPressureRatio(workspace.Status.DiskUsedBytes, workspace.Status.DiskTotalBytes)) == systemnotices.LevelCritical {
			c.JSON(http.StatusInsufficientStorage, gin.H{
				"code":           "disk_full",
				"message":        "The workspace disk is full; the message could not be processed. Free up space and try again.",
				"diskUsedBytes":  workspace.Status.DiskUsedBytes,
				"diskTotalBytes": workspace.Status.DiskTotalBytes,
			})
			return
		}

		errBody := []byte(`{"error":"failed to send message"}`)
		if h.agentStateChecker != nil {
			changedAt, checkerErr := h.agentStateChecker.GetLastCredentialChangedAt(c.Request.Context(), wid)
			if checkerErr == nil && !changedAt.IsZero() {
				errBody = EnrichChatErrorBody(errBody, true, changedAt, wid)
			}
		}
		c.Data(http.StatusBadGateway, "application/json", errBody)
		return
	}
	h.postAdapterSuccess(c, workspace, wid, sid, true)
	if h.sessionIndex != nil {
		go h.fetchAndPersistTitle(wid, sid)
	}
	c.JSON(http.StatusOK, msg)
}

func (h *ProxyHandler) ListQueue(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	wid := c.Param("id")

	// D3 (#907): the outbox is the real queue — entries listed here ARE
	// pending delivery (or parked error with retry context). Without an
	// outbox there is no queue to list (the V2 queue view died with
	// enqueueV2, #828 batch 2); an empty list is the honest answer.
	if h.outbox != nil {
		entries, err := h.outbox.List(c.Request.Context(), wid, sid)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list queue"})
			return
		}
		result := make([]queuedMessageResponse, 0, len(entries))
		for _, e := range entries {
			result = append(result, queuedMessageResponse{
				ID:              e.ID,
				Text:            e.Text,
				SessionID:       sid,
				WorkspaceID:     wid,
				EnqueuedAt:      e.AcceptedAt.UTC().Format(time.RFC3339),
				Status:          e.Status,
				Attempts:        e.Attempts,
				LastError:       e.LastError,
				ClientMessageID: e.ClientMessageID,
				BlockedInFlight: e.BlockedByInFlight,
				InFlightForMs:   e.InFlightFor.Milliseconds(),
			})
		}
		c.JSON(http.StatusOK, queueListResponse{Messages: result})
		return
	}

	c.JSON(http.StatusOK, queueListResponse{Messages: []queuedMessageResponse{}})
}

func (h *ProxyHandler) DeleteQueueMessage(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	wid := c.Param("id")
	msgID := c.Param("messageId")
	if msgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "messageId required"})
		return
	}

	// D3 (#907): with the outbox wired, dismissal targets the REAL queue
	// — the entry is removed and will not deliver. Without an outbox
	// there is nothing to dismiss (204).
	if h.outbox != nil {
		switch h.outbox.Dismiss(c.Request.Context(), wid, sid, msgID) {
		case outbox.DismissRemoved:
			c.Status(http.StatusNoContent)
		case outbox.DismissBusy:
			// Contention (a delivery or sweep holds the session lock) or
			// a transient store failure — the entry exists; retrying
			// shortly will land. Contention is not absence (r4 review).
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "session busy delivering; retry shortly"})
		default:
			c.JSON(http.StatusNotFound, gin.H{"error": "queue message not found"})
		}
		return
	}

	h.publishQueueEvent(wid, sid, "dismissed", msgID, "")
	c.Status(http.StatusNoContent)
}

// RetryQueueMessage clears an error entry back to pending (the queue
// UI's retry action). Outbox path only.
func (h *ProxyHandler) RetryQueueMessage(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	wid := c.Param("id")
	msgID := c.Param("messageId")
	if msgID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "messageId required"})
		return
	}
	if h.outbox == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "queue retry requires the outbox"})
		return
	}
	switch h.outbox.Retry(c.Request.Context(), wid, sid, msgID) {
	case outbox.RetryUpdated:
		c.Status(http.StatusNoContent)
	case outbox.RetryBusy:
		// Contention (a delivery holds the session lock) or a transient
		// store failure — the entry exists; retrying shortly will land.
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "session busy delivering; retry shortly"})
	default:
		c.JSON(http.StatusNotFound, gin.H{"error": "error entry not found (retry targets error entries only)"})
	}
}
