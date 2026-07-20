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

	"github.com/lenaxia/llmsafespaces/api/internal/services/msgqueue"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func (h *ProxyHandler) CreateSession(c *gin.Context) {
	h.proxyToWorkspace(c, "/session", false, "")
}

func (h *ProxyHandler) ListSessions(c *gin.Context) {
	h.proxyToWorkspace(c, "/session", false, "")
}

func (h *ProxyHandler) SendMessage(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	wid := c.Param("id")

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
	if h.isSessionActive(c.Request.Context(), wid, sid) {
		c.Header("Retry-After", "1")
		c.JSON(http.StatusConflict, gin.H{
			"error":      "session is busy; retry after idle",
			"retryAfter": 1,
		})
		return
	}
	// Close the residual race window left by the frontend fix
	// (PR #563): if the client's view of the queue is stale (the
	// refreshQueue poll hasn't landed yet), a direct POST /prompt can
	// still race ahead of the server-side drain goroutine. Check the
	// authoritative source (Redis) and redirect to Enqueue when non-
	// empty. This preserves FIFO ordering regardless of client state
	// staleness.
	if h.queueSvc != nil {
		n, err := h.queueSvc.Len(c.Request.Context(), wid, sid)
		if err != nil {
			h.logger.Warn("SendPromptAsync: queue Len check failed; proceeding with direct send",
				"error", err.Error(), "workspaceID", wid, "sessionID", sid)
		} else if n > 0 {
			h.redirectPromptToQueue(c, wid, sid)
			return
		}
	}
	h.proxyToWorkspace(c, "/session/"+sid+"/prompt_async", true, sid)
}

// redirectPromptToQueue reads the prompt_async request body, extracts
// the text content, enqueues it, and writes the same 202 response shape
// as EnqueueMessage. The body is expected to match the opencode prompt
// shape {parts: [{type: "text", text: "..."}, ...]}; only text parts
// are enqueued (tool parts have no analog in the queue). The original
// body bytes are consumed and not forwarded to opencode.
func (h *ProxyHandler) redirectPromptToQueue(c *gin.Context, wid, sid string) {
	// Cap the body before reading — same pattern as proxy.go:275. The
	// prompt body shape is ~the text size + ~50 bytes of JSON overhead,
	// so 100KB+slack is generous; anything bigger is a malicious/mis-
	// configured client. Without this cap a client could force the API
	// to allocate an arbitrarily large buffer before the 100KB text
	// check below rejects it.
	const maxPromptBodyBytes = 100_000 + 1024 // 100KB text limit + JSON overhead
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

	msgID, err := h.queueSvc.Enqueue(c.Request.Context(), wid, sid, text)
	if err != nil {
		h.logger.Error("SendPromptAsync: redirect to Enqueue failed", err,
			"workspaceID", wid, "sessionID", sid)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to enqueue message"})
		return
	}

	if h.userBroker != nil {
		h.publishWorkspaceEvent(wid, apitypes.WorkspaceSSEEvent{
			Type:      "queue.update",
			SessionID: sid,
			Data: queueUpdateData{
				Event:     "enqueued",
				MessageID: msgID,
			},
		})
	}

	// Session is idle (we just checked in SendPromptAsync). Trigger the
	// drain goroutine immediately so the redirected message does not
	// wait for the next idle SSE event (which will not come — the
	// session is already idle).
	if !h.isSessionActive(c.Request.Context(), wid, sid) && !h.isSessionDeleted(wid, sid) {
		go h.drainQueuedMessage(wid, sid)
	}

	c.JSON(http.StatusAccepted, gin.H{"messageID": msgID})
}

// extractPromptText parses a prompt_async body and returns the
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

	// Parse + validate pagination params before touching the cluster — a
	// malformed ?limit shouldn't waste a connection slot or a k8s API call.
	limit, err := parseHistoryLimit(c.Query("limit"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	before := c.Query("before")

	// Fetch upstream history. We need the FULL body (opencode doesn't
	// paginate), so go through fetchUpstreamHistory rather than the
	// streaming proxyToWorkspace path.
	body, status, fetchErr := h.fetchUpstreamHistory(c, sid)
	if fetchErr != nil {
		// fetchUpstreamHistory has already written the error response.
		return
	}
	if status >= 400 {
		// Pass the upstream status through; do NOT mask as 200 empty page.
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
			"phase":      workspace.Status.Phase,
			"retryAfter": retryAfterSec,
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
				"retryAfter": retryAfterSec,
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
	req.SetBasicAuth("opencode", password)
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
	h.proxyToWorkspace(c, "/session/"+sid, false, sid)
}

func (h *ProxyHandler) AbortSession(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	wid := c.Param("id")

	// Proxy the abort to opencode first. Only if that succeeds do we take
	// ownership of queued messages — this avoids clearing the queue when the
	// abort itself fails (network error, workspace not active, etc.).
	h.proxyToWorkspace(c, "/session/"+sid+"/abort", false, sid)

	if h.queueSvc == nil || c.Writer.Status() >= 400 {
		return
	}

	// Abort succeeded. Peek then clear the session queue. Note: PeekAll and
	// Clear are separate Redis commands — a message enqueued between them will
	// be cleared without a dismissed SSE event. This is acceptable: the message
	// is still discarded (the intent of abort), just silently.
	flushed, err := h.queueSvc.PeekAll(c.Request.Context(), wid, sid)
	if err != nil {
		h.logger.Error("AbortSession: failed to peek queue after abort", err, "workspaceID", wid, "sessionID", sid)
		return
	}
	if len(flushed) == 0 {
		return
	}
	if err := h.queueSvc.Clear(c.Request.Context(), wid, sid); err != nil {
		h.logger.Error("AbortSession: failed to clear queue after abort", err, "workspaceID", wid, "sessionID", sid)
		return
	}
	// Publish dismissed SSE so UIs remove the pills immediately.
	for _, msg := range flushed {
		h.publishQueueEvent(wid, sid, "dismissed", msg.ID, "")
	}

	// In the background: wait for idle, then send each flushed message one at a
	// time (with an idle-wait between each) and abort again at the end. This
	// ensures messages appear in the transcript without being processed.
	go h.flushAndAbortAfterIdle(wid, sid, flushed)
}

// flushAndAbortAfterIdle waits for the session to become idle (after an abort),
// then sends each flushed message one at a time to opencode. Between each send
// it waits for the session to go idle again before sending the next, ensuring
// no 409 "session busy" errors. After all messages are sent it aborts once more
// so they appear in the transcript but are not processed further.
func (h *ProxyHandler) flushAndAbortAfterIdle(workspaceID, sessionID string, msgs []msgqueue.QueuedMessage) {
	if h.sseTracker == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// waitIdle subscribes to the SSE drain and returns once this session signals idle,
	// or when ctx is done.
	waitIdle := func() bool {
		idleCh := make(chan struct{}, 1)
		unsub := h.sseTracker.SubscribeDrain(workspaceID,
			func(_, sid string) {
				if sid == sessionID {
					select {
					case idleCh <- struct{}{}:
					default:
					}
				}
			},
			func(_, _ string) {},
		)
		defer unsub()
		select {
		case <-idleCh:
			return true
		case <-ctx.Done():
			return false
		}
	}

	// Wait for idle from the initial abort before sending anything.
	if !waitIdle() {
		h.logger.Warn("flushAndAbortAfterIdle: timed out waiting for initial idle",
			"workspaceID", workspaceID, "sessionID", sessionID)
		return
	}

	// Send each message one at a time, waiting for idle after each send.
	for i, msg := range msgs {
		if err := h.sendQueuedToOpencode(ctx, workspaceID, sessionID, &msg); err != nil {
			h.logger.Warn("flushAndAbortAfterIdle: failed to send flushed message",
				"workspaceID", workspaceID, "sessionID", sessionID,
				"messageID", msg.ID, "index", i, "error", err)
			// Stop on first error — remaining messages would also fail.
			break
		}
		// Wait for this message's turn to complete before sending the next.
		if i < len(msgs)-1 {
			if !waitIdle() {
				h.logger.Warn("flushAndAbortAfterIdle: timed out waiting for idle between messages",
					"workspaceID", workspaceID, "sessionID", sessionID, "sentSoFar", i+1)
				break
			}
		}
	}

	// Abort again to stop processing the flushed messages.
	podIP, password, err := h.getPodIPAndPassword(ctx, workspaceID)
	if err != nil {
		return
	}
	abortURL := fmt.Sprintf("http://%s:%d/session/%s/abort", podIP, opencodePort, sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, abortURL, nil)
	if err != nil {
		return
	}
	req.SetBasicAuth(agentd.AuthUsername, password)
	resp, err := h.httpClient.Do(req)
	if err != nil {
		h.logger.Warn("flushAndAbortAfterIdle: second abort failed",
			"workspaceID", workspaceID, "sessionID", sessionID, "error", err)
		return
	}
	_ = resp.Body.Close()
}

func (h *ProxyHandler) DeleteSession(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	workspaceID := c.Param("id")
	h.proxyToWorkspace(c, "/session/"+sid, false, sid)

	if c.Writer.Status() >= 400 {
		return
	}

	// Detached ctx (not c.Request.Context()): the tombstone suppresses late
	// SSE events arriving after deletion, so it MUST be written even if the
	// client disconnects mid-request. Matches the sibling session-index
	// delete below (which uses context.Background() for the same reason).
	h.state().MarkSessionDeleted(context.Background(), workspaceID, sid) //nolint:contextcheck // G118: tombstone must survive client disconnect

	if h.sessionIndex != nil {
		// Use context.Background() so a client disconnect after the agent
		// has already deleted the session doesn't leave the index in an
		// inconsistent state (agent deleted, index still has it).
		if err := h.sessionIndex.DeleteSession(context.Background(), workspaceID, sid); err != nil { //nolint:contextcheck
			h.logger.Error("failed to delete session from index", err, "workspaceID", workspaceID, "sessionID", sid)
		}
	}

	go func() {
		// Background ctx (not c.Request.Context()): this outlives the request
		// (fire-and-forget, must survive client disconnect) and capturing c
		// here would race with gin reusing the Context on the next ServeHTTP.
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
	// redirectPromptToQueue above and proxy.go:275. 100KB text limit + 1KB
	// slack for JSON overhead.
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

	if h.queueSvc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "message queue not available"})
		return
	}

	msgID, err := h.queueSvc.Enqueue(c.Request.Context(), wid, sid, req.Text)
	if err != nil {
		h.logger.Error("Failed to enqueue message", err, "workspaceID", wid, "sessionID", sid)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to enqueue message"})
		return
	}

	if h.userBroker != nil {
		h.publishWorkspaceEvent(wid, apitypes.WorkspaceSSEEvent{
			Type:      "queue.update",
			SessionID: sid,
			Data: queueUpdateData{
				Event:     "enqueued",
				MessageID: msgID,
			},
		})
	}

	// If the session is already idle (not in activeSess), drain immediately.
	// This handles the case where the user queues a message after the agent
	// finished — no session.status=idle event will arrive because the session
	// is already quiet. Without this check the message would sit in Redis
	// until the next SSE reconnect triggers reconcileSessionState.
	if !h.isSessionActive(c.Request.Context(), wid, sid) && !h.isSessionDeleted(wid, sid) {
		go h.drainQueuedMessage(wid, sid)
	}

	c.JSON(http.StatusAccepted, gin.H{"messageID": msgID})
}

func (h *ProxyHandler) ListQueue(c *gin.Context) {
	sid := c.Param("sessionId")
	if err := validateSessionID(sid); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid sessionId: " + err.Error()})
		return
	}
	wid := c.Param("id")

	if h.queueSvc == nil {
		c.JSON(http.StatusOK, gin.H{"messages": []msgqueue.QueuedMessage{}})
		return
	}

	msgs, err := h.queueSvc.PeekAll(c.Request.Context(), wid, sid)
	if err != nil {
		h.logger.Error("Failed to list queue", err, "workspaceID", wid, "sessionID", sid)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list queue"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"messages": msgs})
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

	if h.queueSvc == nil {
		c.Status(http.StatusNoContent)
		return
	}

	if err := h.queueSvc.Remove(c.Request.Context(), wid, sid, msgID); err != nil {
		h.logger.Error("Failed to remove queue message", err, "workspaceID", wid, "sessionID", sid, "messageID", msgID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove message"})
		return
	}

	if h.userBroker != nil {
		h.publishWorkspaceEvent(wid, apitypes.WorkspaceSSEEvent{
			Type:      "queue.update",
			SessionID: sid,
			Data: queueUpdateData{
				Event:     "dismissed",
				MessageID: msgID,
			},
		})
	}

	c.Status(http.StatusNoContent)
}
