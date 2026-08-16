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
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

func (h *ProxyHandler) CreateSession(c *gin.Context) {
	if h.adapter != nil {
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
		c.JSON(http.StatusOK, s)
		return
	}
	h.proxyToWorkspace(c, "/session", false, "")
}

func (h *ProxyHandler) ListSessions(c *gin.Context) {
	if h.adapter != nil {
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
		c.JSON(http.StatusOK, sessions)
		return
	}
	h.proxyToWorkspace(c, "/session", false, "")
}

func (h *ProxyHandler) SendMessage(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	wid := c.Param("id")

	// Adapter path (US-65.4): adapter.Send returns a typed
	// session.Message with contract-shaped parts. The response is
	// contract JSON, not raw opencode bytes.
	if h.adapter != nil {
		text, err := extractMessageText(c)
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

		sendSubmittedAt := time.Now()
		msg, err := h.adapter.Send(c.Request.Context(), "", wid, sid, text, session.SendOpts{})
		if err != nil {
			// #817: log the underlying adapter error — without this the
			// 502 body says only "failed to send message" and the root
			// cause (context deadline, connection reset, decode failure)
			// is invisible in production.
			h.logger.Error("SendMessage: adapter failed", err,
				"workspaceID", wid, "sessionID", sid)
			// #817 self-healing: the turn may have completed server-side
			// even though the send response was lost in transit. Verify
			// and recover before failing — see proxy_send_recovery.go.
			if h.attemptSendRecovery(c, workspace, wid, sid, sendSubmittedAt) {
				return
			}
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
		return
	}

	// Legacy path: proxy raw bytes to opencode with error enrichment.

	// US-27b.5: wire chat-error enrichment. The closure captures wid + the
	// agent-state checker so doProxy can rewrite the response body on 4xx
	// with agentNeedsRefresh / hint fields. On 2xx the closure is never
	// invoked (doProxy only buffers on status >= 400).
	var errBodyTransform func(statusCode int, body []byte) []byte
	if h.agentStateChecker != nil {
		errBodyTransform = func(_ int, body []byte) []byte {
			changedAt, checkerErr := h.agentStateChecker.GetLastCredentialChangedAt(c.Request.Context(), wid)
			if checkerErr != nil || changedAt.IsZero() {
				// No pending credentials — pass body through the allowlist
				// (EnrichChatErrorBody with needsRefresh=false just filters
				// unknown fields; no hint added).
				return EnrichChatErrorBody(body, false, time.Time{}, wid)
			}
			h.logger.Info("Chat error enriched with pending-credential hint",
				"workspaceID", wid, "credentialsPendingSince", changedAt.Format("2006-01-02T15:04:05Z"))
			return EnrichChatErrorBody(body, true, changedAt, wid)
		}
	}

	h.proxyToWorkspaceWithErrBody(c, "/session/"+sid+"/message", true, sid, errBodyTransform, true)

	status := c.Writer.Status()
	if status < 300 && h.sessionIndex != nil {
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

	// V2 path (Epic 63): extract text from the V1 parts body and send via
	// PromptV2 with delivery:"queue". opencode admits atomically.
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
	if len(text) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "text must not be empty"})
		return
	}
	if len(text) > 100_000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "text exceeds 100KB limit"})
		return
	}

	// Adapter path: uses synchronous adapter.Send (V1 POST /session/:id/message).
	// Previously used V2 queue (delivery:"queue") which is never drained on
	// opencode 1.18.10 — messages vanished (#755). The frontend receives the
	// assistant response via SSE events regardless of send path.
	if h.adapter != nil {
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

		// Use synchronous Send (V1 POST /session/:id/message) instead
		// of V2 queue (POST /api/session/:id/prompt delivery:queue).
		// The V2 queue is admitted but never drained on opencode 1.18.10
		// — the SSE event taxonomy that the bridge depends on has
		// drifted (see #755, #739). The synchronous path works correctly
		// on all versions. The frontend receives the assistant response
		// via SSE events regardless of which send path is used.
		sendSubmittedAt := time.Now()
		msg, err := h.adapter.Send(c.Request.Context(), "", wid, sid, text, session.SendOpts{})
		if err != nil {
			// #817: log the underlying adapter error for this path too.
			h.logger.Error("SendPromptAsync: adapter failed", err,
				"workspaceID", wid, "sessionID", sid)
			// #817 self-healing: the browser path's failure signature is
			// "turn completed server-side, response lost in transit" —
			// verify and recover before failing. See proxy_send_recovery.go.
			if h.attemptSendRecovery(c, workspace, wid, sid, sendSubmittedAt) {
				return
			}
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
		if h.sessionIndex != nil {
			go h.fetchAndPersistTitle(wid, sid)
		}
		c.JSON(http.StatusOK, msg)
		return
	}

	// Legacy V2 path (no adapter).
	h.enqueueV2(c, wid, sid, text)
}

// extractMessageText reads the request body and extracts the
// concatenated text from opencode's {parts:[{type:"text",text:"..."}]}
// shape. Caps at 100KB to match the SendPromptAsync body limit.
func extractMessageText(c *gin.Context) (string, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 100_000+1024)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read request body: %w", err)
	}
	_ = c.Request.Body.Close()
	return extractPromptText(body)
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

// historyPageDefaultLimit is the default page size when ?limit= is omitted.
// Mirrors the value the frontend (api/messages.ts) already requests.
const historyPageDefaultLimit = 50

// historyPageMaxLimit caps ?limit= so a misbehaving client cannot force
// the API to materialize an unbounded message slice in memory.
const historyPageMaxLimit = 200

// upstreamHistoryBodyCap bounds how much we'll read from opencode's
// /session/{id}/message endpoint. opencode returns the entire history
// array in one shot; 16 MiB covers ~10k typical text-only messages and
// leaves headroom before we'd OOM the API pod.
const upstreamHistoryBodyCap = 16 * 1024 * 1024

// GetHistory returns a chronological page of displayable messages for a
// session.
//
// Query parameters:
//   - limit: page size (default 50, max 200). Counts displayable messages
//     only — system-role messages and messages whose parts collapse to
//     nothing visible (e.g. only step-start/step-finish) do not count
//     against the limit. Rejecting invalid limits (<=0 or non-numeric)
//     surfaces client bugs early.
//   - before: opaque cursor — the message id of the OLDEST message in the
//     previously-rendered page. Returns messages strictly older than
//     this cursor. Absent => return the newest `limit` messages.
//
// Response:
//   - body: JSON array of opencode message objects, oldest-first within
//     the page. Schema preserved as-is so the frontend's transformHistory
//     keeps working.
//   - X-Next-Cursor header: present iff more (older) messages exist; its
//     value is the id of the OLDEST message in the returned page. Absent
//     means there are no more messages to fetch.
//
// The handler fetches the FULL upstream array from opencode (which does
// not paginate), filters to displayable messages server-side, then
// slices. Filtering server-side prevents jumpy page sizes that would
// otherwise happen if the frontend filtered after receiving the page.
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

	// Adapter path (US-65.4): typed session.Message[] from the Adapter,
	// contract-shaped JSON to the client. The Adapter translator already
	// drops step-start/step-finish and collects patch file paths, so the
	// response is clean contract data — no opencode-specific shapes.
	if h.adapter != nil {
		_, ok := h.resolveWorkspaceForAdapter(c, wid)
		if !ok {
			return
		}
		defer h.releaseConnection(wid)
		h.adapterEnsureSSEWatch(wid)

		msgs, err := h.adapter.GetHistory(c.Request.Context(), "", wid, sid)
		if err != nil {
			h.logger.Error("GetHistory: adapter failed", err, "sessionID", sid)
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch history"})
			return
		}
		page, nextCursor := paginateContractHistory(msgs, limit, before)
		if page == nil {
			page = []session.Message{}
		}
		if nextCursor != "" {
			c.Header("X-Next-Cursor", nextCursor)
		}
		c.JSON(http.StatusOK, page)
		return
	}

	// Legacy path: fetch raw opencode bytes + inline parse + paginate.
	body, status, fetchErr := h.fetchUpstreamHistory(c, sid)
	if fetchErr != nil {
		return
	}
	if status >= 400 {
		c.Data(status, "application/json", body)
		return
	}

	page, nextCursor, parseErr := paginateOpencodeHistory(body, limit, before)
	if parseErr != nil {
		h.logger.Error("Failed to parse opencode history", parseErr,
			"sessionID", sid, "size", len(body))
		c.JSON(http.StatusBadGateway, gin.H{"error": "malformed upstream history"})
		return
	}

	if nextCursor != "" {
		c.Header("X-Next-Cursor", nextCursor)
	}
	c.Data(http.StatusOK, "application/json", page)
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

// fetchUpstreamHistory is a non-streaming GET of opencode's
// /session/{id}/message. Returns (body, status, err). On err, the
// handler has already written a 4xx/5xx to the client and the caller
// should just return.
//
// This duplicates parts of proxyToWorkspaceWithErrBody intentionally:
// the streaming proxy path doesn't allow us to inspect+slice the
// response body, which is what pagination requires.
func (h *ProxyHandler) fetchUpstreamHistory(c *gin.Context, sessionID string) ([]byte, int, error) {
	workspaceID := c.Param("id")
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace ID required"})
		return nil, 0, fmt.Errorf("missing workspace id")
	}

	var workspace *v1.Workspace
	if cached, exists := c.Get("workspace"); exists {
		if sb, ok := cached.(*v1.Workspace); ok {
			workspace = sb
		}
	}
	if workspace == nil {
		v1Client, vErr := h.k8sClient.LlmsafespacesV1()
		if vErr != nil {
			h.logger.Error("Failed to get LLMSafespacesV1 client", vErr, "workspaceID", workspaceID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return nil, 0, vErr
		}
		var getErr error
		workspace, getErr = v1Client.Workspaces(h.namespace).Get(c.Request.Context(), workspaceID, metav1.GetOptions{})
		if getErr != nil {
			h.logger.Error("Failed to get workspace CRD", getErr, "workspaceID", workspaceID)
			c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
			return nil, 0, getErr
		}
	}
	if workspace.Status.Phase != phaseActive || workspace.Status.PodIP == "" {
		c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSec))
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":      "workspace not ready",
			"code":       "service_unavailable",
			"reason":     "not_ready",
			"phase":      workspace.Status.Phase,
			"retryAfter": retryAfterSec,
			"message":    fmt.Sprintf("Workspace is %s. This usually takes a few seconds.", strings.ToLower(string(workspace.Status.Phase))),
		})
		return nil, 0, fmt.Errorf("workspace not ready")
	}

	password, err := h.getPassword(c.Request.Context(), workspaceID)
	if err != nil {
		h.logger.Error("Failed to get workspace password", err, "workspaceID", workspaceID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve workspace credentials"})
		return nil, 0, err
	}

	if !h.acquireConnection(workspaceID) {
		c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSec))
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":      "connection limit reached",
			"retryAfter": retryAfterSec,
		})
		return nil, 0, fmt.Errorf("connection limit")
	}
	defer h.releaseConnection(workspaceID)

	// Forward only non-pagination query params to opencode. The
	// pagination contract (limit/before) is owned by the API, not
	// opencode — opencode currently ignores them but forwarding them is
	// noise at best, future breakage at worst.
	upstreamQuery := stripPaginationQuery(stripVerboseQuery(c.Request.URL.RawQuery))

	podIP := workspace.Status.PodIP
	body, status, doErr := h.doHistoryRequest(c.Request.Context(), podIP, workspaceID, sessionID, password, upstreamQuery, c.ClientIP())

	// Stale-IP retry: if the first attempt failed with a connection error,
	// the pod may have been rescheduled to a new IP since the CRD was last
	// read from cache. Refetch the workspace and try once more if the IP
	// actually changed. Mirrors the same recovery in proxy.go:290-302.
	if doErr != nil && isConnectionError(doErr) {
		freshWS, getErr := func() (*v1.Workspace, error) {
			v1Client, vErr := h.k8sClient.LlmsafespacesV1()
			if vErr != nil {
				return nil, vErr
			}
			return v1Client.Workspaces(h.namespace).Get(c.Request.Context(), workspaceID, metav1.GetOptions{})
		}()
		if getErr == nil && freshWS.Status.PodIP != "" && freshWS.Status.PodIP != podIP && freshWS.Status.Phase == phaseActive {
			h.logger.Info("Retrying history with fresh pod IP",
				"workspaceID", workspaceID, "oldIP", podIP, "newIP", freshWS.Status.PodIP)
			body, status, doErr = h.doHistoryRequest(c.Request.Context(), freshWS.Status.PodIP, workspaceID, sessionID, password, upstreamQuery, c.ClientIP())
		}
	}

	if doErr != nil {
		if isConnectionError(doErr) {
			h.logger.Warn("History upstream connection error", "error", doErr, "workspaceID", workspaceID)
			c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSec))
			// 503 (not 502) preserves the contract asserted by
			// TestProxyBuffer_GETHistoryNotBufferedReturns503: read-only
			// GETs against a non-bufferable upstream return 503 with a
			// "workspace connection failed" body so the frontend can
			// distinguish a transient pod-restart from a malformed history
			// (which surfaces as 502). The 503 is a fast-fail, not a
			// buffered retry — buffering is reserved for writes.
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":      "workspace connection failed",
				"code":       "service_unavailable",
				"reason":     "agent_unreachable",
				"retryAfter": retryAfterSec,
				"message":    "Chat history is temporarily unavailable — the agent is restarting or recovering. Please try again in a moment.",
			})
			return nil, 0, doErr
		}
		h.logger.Error("History upstream request failed", doErr, "workspaceID", workspaceID)
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return nil, 0, doErr
	}

	return body, status, nil
}

// doHistoryRequest performs one round-trip against opencode's
// /session/{id}/message endpoint and returns (body, status, error).
// Extracted from fetchUpstreamHistory so the stale-IP retry path can
// reuse it without duplicating header / body-cap handling.
// workspaceID is used for the LLMSafeSpaces#488 upstream-5xx observability
// signal — logged and used as a metric label.
func (h *ProxyHandler) doHistoryRequest(ctx context.Context, podIP, workspaceID, sessionID, password, query, clientIP string) ([]byte, int, error) {
	upstreamURL := fmt.Sprintf("http://%s:%d/session/%s/message", podIP, opencodePort, sessionID)
	if query != "" {
		upstreamURL += "?" + query
	}

	req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, upstreamURL, nil)
	if reqErr != nil {
		return nil, 0, fmt.Errorf("build upstream history request: %w", reqErr)
	}
	req.SetBasicAuth(agentd.AuthUsername, password)
	req.Header.Set("X-Forwarded-For", clientIP)

	resp, doErr := h.httpClient.Do(req)
	if doErr != nil {
		return nil, 0, doErr
	}
	defer func() { _ = resp.Body.Close() }()

	limited := io.LimitReader(resp.Body, upstreamHistoryBodyCap+1)
	body, readErr := io.ReadAll(limited)
	if readErr != nil {
		return nil, 0, fmt.Errorf("read upstream history body: %w", readErr)
	}
	if len(body) > upstreamHistoryBodyCap {
		return nil, 0, fmt.Errorf("upstream history body > %d bytes", upstreamHistoryBodyCap)
	}

	// LLMSafeSpaces#488: log + count upstream 5xx. The body is already
	// buffered here (unlike doProxy's streaming path), so include a
	// preview to make the opencode error-ref discoverable without a
	// second kubectl-exec round-trip.
	if resp.StatusCode >= 500 {
		historyPath := fmt.Sprintf("/session/%s/message", sessionID)
		recordUpstream5xx(h.logger, workspaceID, historyPath, resp.StatusCode, body)
	}

	return body, resp.StatusCode, nil
}

// stripPaginationQuery removes the limit and before parameters that
// the API consumes for itself. This is a complement to stripVerboseQuery
// which removes the API's verbose/workspace/directory flags.
func stripPaginationQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	v, err := url.ParseQuery(rawQuery)
	if err != nil {
		return rawQuery
	}
	v.Del("limit")
	v.Del("before")
	return v.Encode()
}

// paginateOpencodeHistory parses an opencode message array body, filters
// out non-displayable messages, and slices the result into one page.
//
// Contract (mirrors the test file proxy_history_pagination_test.go):
//   - Input body is a JSON array of opencode message objects, oldest-first.
//   - Output is a JSON array of the same shape (preserving opencode's
//     schema), oldest-first within the page.
//   - If `before` is empty: return the LAST `limit` displayable messages.
//   - If `before` is set: return up to `limit` displayable messages that
//     appear strictly before the message whose info.id == before. If the
//     cursor isn't found, return an empty array (defensive — better than
//     accidentally returning the head of history).
//   - Returns (pageBytes, nextCursor, error). nextCursor is the info.id of
//     the OLDEST message in the returned page; it is empty if there are
//     no older displayable messages remaining.
func paginateOpencodeHistory(body []byte, limit int, before string) ([]byte, string, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(body, &arr); err != nil {
		return nil, "", fmt.Errorf("decode upstream array: %w", err)
	}

	// Walk once, capture (idx, id) pairs for displayable messages.
	type entry struct {
		raw json.RawMessage
		id  string
	}
	displayable := make([]entry, 0, len(arr))
	for _, raw := range arr {
		id, ok := messageIsDisplayable(raw)
		if !ok {
			continue
		}
		displayable = append(displayable, entry{raw: raw, id: id})
	}

	// Determine the inclusive end of the slice (exclusive of the cursor
	// itself, which the client already has).
	endExclusive := len(displayable)
	if before != "" {
		idx := -1
		for i, e := range displayable {
			if e.id == before {
				idx = i
				break
			}
		}
		if idx < 0 {
			// Unknown cursor: empty page, no cursor. The frontend will
			// treat this as end-of-history.
			return []byte("[]"), "", nil
		}
		endExclusive = idx
	}

	// Take the last `limit` entries up to endExclusive.
	start := endExclusive - limit
	if start < 0 {
		start = 0
	}
	pageEntries := displayable[start:endExclusive]

	// Build the JSON array of raw messages, oldest-first within the page.
	out := make([]json.RawMessage, len(pageEntries))
	for i, e := range pageEntries {
		out[i] = e.raw
	}
	pageBytes, err := json.Marshal(out)
	if err != nil {
		return nil, "", fmt.Errorf("encode page: %w", err)
	}

	// Emit a cursor IFF there are older displayable messages we did not
	// include in this page. The cursor value is the OLDEST id we just
	// returned — passing it as ?before= yields the next-older page.
	nextCursor := ""
	if start > 0 && len(pageEntries) > 0 {
		nextCursor = pageEntries[0].id
	}
	return pageBytes, nextCursor, nil
}

// paginateContractHistory applies the same cursor-based pagination as
// paginateOpencodeHistory but on typed session.Message values from the
// Adapter. Simpler because the Adapter translator already dropped
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

// messageIsDisplayable returns the message id and true iff the message
// is one a user would see in the chat transcript:
//   - role must be "user" or "assistant" (system messages are hidden)
//   - parts must contain at least one part whose type is text, thinking,
//     reasoning, or tool. Pure step-start/step-finish/patch messages do
//     not count as displayable.
//
// Returns ("", false) for anything not displayable. The id is sourced
// from info.id with a fallback to top-level id (mirrors the frontend's
// transformHistory).
func messageIsDisplayable(raw json.RawMessage) (string, bool) {
	var probe struct {
		Info struct {
			Role string `json:"role"`
			ID   string `json:"id"`
		} `json:"info"`
		ID    string `json:"id"`
		Role  string `json:"role"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", false
	}
	role := probe.Info.Role
	if role == "" {
		role = probe.Role
	}
	if role != "user" && role != "assistant" {
		return "", false
	}
	hasDisplayable := false
	for _, p := range probe.Parts {
		switch p.Type {
		case "text", "thinking", "reasoning":
			if p.Text != "" {
				hasDisplayable = true
			}
		case "tool":
			hasDisplayable = true
		}
		if hasDisplayable {
			break
		}
	}
	if !hasDisplayable {
		return "", false
	}
	id := probe.Info.ID
	if id == "" {
		id = probe.ID
	}
	return id, true
}

func (h *ProxyHandler) GetSession(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	if h.adapter != nil {
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
		c.JSON(http.StatusOK, s)
		return
	}
	h.proxyToWorkspace(c, "/session/"+sid, false, sid)
}

func (h *ProxyHandler) AbortSession(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	wid := c.Param("id")

	// Adapter path (US-65.4): abort via adapter.Abort. The V1
	// POST /session/:id/abort (the only interrupt endpoint on opencode
	// 1.18.10+) destructively stops the in-flight turn — queued input
	// is not preserved, unlike the old V2 interrupt which was removed
	// in 1.18.10. We clear pending tracking so US-63.9 stranded-input
	// recovery doesn't re-wake a session the user explicitly aborted.
	if h.adapter != nil {
		if err := h.adapter.Abort(c.Request.Context(), "", wid, sid); err != nil {
			h.logger.Error("AbortSession: adapter abort failed", err, "workspaceID", wid, "sessionID", sid)
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to abort session"})
			return
		}
		if h.v2Pending != nil {
			h.v2Pending.remove(wid, sid)
		}
		c.Status(http.StatusNoContent)
		return
	}

	// V2 path (Epic 63): non-destructive interrupt. Queued messages survive
	// and drain on the next execution.wake (F8).
	h.abortV2(c, wid, sid)
}

func (h *ProxyHandler) DeleteSession(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	workspaceID := c.Param("id")

	// Adapter path (US-65.4): delegate to adapter, then run the same
	// post-delete side effects (tombstone, session index cleanup, SSE
	// tombstone publish) that the legacy path runs.
	if h.adapter != nil {
		if err := h.adapter.DeleteSession(c.Request.Context(), "", workspaceID, sid); err != nil {
			// #817: same observability gap — log the underlying error.
			h.logger.Error("DeleteSession: adapter failed", err,
				"workspaceID", workspaceID, "sessionID", sid)
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to delete session"})
			return
		}
		c.Status(http.StatusNoContent)
	} else {
		h.proxyToWorkspace(c, "/session/"+sid, false, sid)
		if c.Writer.Status() >= 400 {
			return
		}
	}

	// Post-delete side effects run in both adapter and legacy paths.
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

	// Adapter path (US-65.4).
	if h.adapter != nil {
		return h.adapter.RenameSession(ctx, "", workspaceID, sessionID, title)
	}

	// Legacy path.
	v1Client, err := h.k8sClient.LlmsafespacesV1()
	if err != nil {
		return fmt.Errorf("initialize LLMSafespacesV1 client: %w", err)
	}
	ws, err := v1Client.Workspaces(h.namespace).Get(ctx, workspaceID, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get workspace CRD: %w", err)
	}
	if ws.Status.Phase != phaseActive || ws.Status.PodIP == "" {
		return fmt.Errorf("workspace not active")
	}

	password, err := h.getPassword(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("get password: %w", err)
	}

	type sessionUpdate struct {
		Title string `json:"title"`
	}
	payload := sessionUpdate{Title: title}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	targetURL := fmt.Sprintf("http://%s:%d/session/%s", ws.Status.PodIP, opencodePort, sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, targetURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(agentd.AuthUsername, password)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request to agent: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("agent returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
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

// getPodIPAndPassword returns the pod IP and opencode password for the given
// workspace. It is a convenience helper shared by several background goroutines.
func (h *ProxyHandler) getPodIPAndPassword(ctx context.Context, workspaceID string) (podIP, password string, err error) {
	v1Client, err := h.k8sClient.LlmsafespacesV1()
	if err != nil {
		return "", "", fmt.Errorf("getting v1 client: %w", err)
	}
	ws, err := v1Client.Workspaces(h.namespace).Get(ctx, workspaceID, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("getting workspace: %w", err)
	}
	if ws.Status.Phase != phaseActive || ws.Status.PodIP == "" {
		return "", "", fmt.Errorf("workspace not active")
	}
	pw, err := h.getPassword(ctx, workspaceID)
	if err != nil {
		return "", "", fmt.Errorf("getting password: %w", err)
	}
	return ws.Status.PodIP, pw, nil
}

type enqueueRequest struct {
	Text string `json:"text" binding:"required"`
}

// queuedMessageResponse is the typed JSON shape for a queue list entry.
// Mirrors the queued-message wire shape so the frontend is unchanged.
type queuedMessageResponse struct {
	ID          string `json:"id"`
	Text        string `json:"text"`
	SessionID   string `json:"session_id"`
	WorkspaceID string `json:"workspace_id"`
	EnqueuedAt  string `json:"enqueued_at"`
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
	if len(req.Text) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "text must not be empty"})
		return
	}
	if len(req.Text) > 100_000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "text exceeds 100KB limit"})
		return
	}

	// V2 path (Epic 63): send via PromptV2 with delivery:"queue".
	h.enqueueV2(c, wid, sid, req.Text)
}

func (h *ProxyHandler) ListQueue(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	wid := c.Param("id")

	// US-63.10: read from the Redis-backed shadow marker. The shadow is
	// populated by the SSE bridge on PromptAdmitted events and cleared on
	// Prompted events.
	if h.v2Shadow != nil {
		entries := h.v2Shadow.List(c.Request.Context(), wid, sid)
		result := make([]queuedMessageResponse, 0, len(entries))
		for _, e := range entries {
			result = append(result, queuedMessageResponse{
				ID:          e.ID,
				Text:        e.Text,
				SessionID:   sid,
				WorkspaceID: wid,
				EnqueuedAt:  time.Unix(e.EnqueuedAt, 0).UTC().Format(time.RFC3339),
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

	// US-63.10: remove from the shadow marker. Dismissed messages must not
	// reappear on fresh load.
	if h.v2Shadow != nil {
		h.v2Shadow.Remove(c.Request.Context(), wid, sid, msgID)
	}
	h.publishQueueEvent(wid, sid, "dismissed", msgID, "")
	c.Status(http.StatusNoContent)
}
