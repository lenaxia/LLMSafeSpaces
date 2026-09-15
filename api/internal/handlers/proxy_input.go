// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/services/inbox"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// requestIDPattern is the GENERIC request-ID contract (#1302 item 3):
// charset [a-zA-Z0-9._-], length ≤128, no path-traversal. The agent's
// que_/per_ prefixes live behind the dialect seam — the handler accepts
// any conforming ID from any agent.
var requestIDPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,128}$`)

func validRequestID(id string) bool {
	return requestIDPattern.MatchString(id) && !strings.Contains(id, "..")
}

// ListQuestions returns the pending questions as the contract
// InputRequest (kind=question; adapter ListPending, root resolved).
func (h *ProxyHandler) ListQuestions(c *gin.Context) {
	wid := c.Param("id")
	if _, ok := h.resolveWorkspaceForAdapter(c, wid); !ok {
		return
	}
	defer h.releaseConnection(wid)
	pending, err := h.adapter.ListPending(c.Request.Context(), "", wid, "")
	if err != nil {
		// #1302: a ListPending failure is NON-AUTHORITATIVE — 503, never
		// an authoritative empty (and not a 502: clients must not treat
		// this as the agent's definitive answer).
		h.logger.Error("ListQuestions: adapter failed", err, "workspaceID", wid)
		c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSec))
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "failed to list questions"})
		return
	}
	out := make([]session.InputRequest, 0, len(pending))
	for _, ir := range pending {
		if ir.Kind == session.InputQuestion {
			out = append(out, h.resolveInputRequestRoot(c.Request.Context(), wid, ir))
		}
	}
	h.recordActivityIfTracked(wid)
	c.JSON(http.StatusOK, out)
}

// QuestionReply answers a live ask. A reply to a NON-live ask whose
// inbox record is pending (the walk-away case, #1313) composes the Q&A
// user message through the outbox instead.
func (h *ProxyHandler) QuestionReply(c *gin.Context) {
	requestID := c.Param("requestID")
	if !validRequestID(requestID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request ID"})
		return
	}
	wid := c.Param("id")
	if h.tryLateAnswer(c, wid, requestID, extractQuestionAnswerBody) {
		return
	}
	workspace, ok := h.resolveWorkspaceForAdapter(c, wid)
	if !ok {
		return
	}
	defer h.releaseConnection(wid)
	// Validate BEFORE the quota gate (SendMessage's order): the gate is a
	// permanent reservation — a malformed 400 must not burn an
	// llm_request slot (r3).
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unreadable reply body"})
		return
	}
	var payload struct {
		Answers [][]string `json:"answers"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.Answers) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reply carries no answers"})
		return
	}
	if !h.checkAdapterQuota(c, workspace) {
		return
	}
	// 4a (#1302 S1): writes go through agentd Act — the API makes zero
	// mutating harness calls in the authority regime.
	if h.agentdTerminus {
		sessionID, resolvable, unknownSet := h.inputRequestSession(c.Request.Context(), wid, requestID)
		if !resolvable {
			if unknownSet {
				c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSec))
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "input pending set unknown"})
				return
			}
			c.JSON(http.StatusNotFound, gin.H{"error": "no pending input request"})
			return
		}
		// Flatten every answer group — a silent Answers[1:] drop is
		// data loss (the flag-off path posts all arrays; r3 carried).
		opts := make([]string, 0, len(payload.Answers))
		for _, group := range payload.Answers {
			opts = append(opts, group...)
		}
		action := map[string]any{"optionIds": opts}
		if !h.actAnswerInput(c, wid, sessionID, requestID, action) {
			return
		}
		h.resolveInboxOnProxySuccess(c, wid, sessionID, requestID, inbox.KindQuestion, inbox.StatusAnswered)
		h.postAdapterSuccess(c, workspace, wid, "", true)
		c.Status(http.StatusOK)
		return
	}
	if err := h.adapter.AnswerQuestion(c.Request.Context(), "", wid, requestID, payload.Answers); err != nil {
		h.logger.Error("QuestionReply: adapter failed", err, "workspaceID", wid, "requestID", requestID)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to answer question"})
		return
	}
	h.resolveInboxOnProxySuccess(c, wid, "", requestID, inbox.KindQuestion, inbox.StatusAnswered)
	h.postAdapterSuccess(c, workspace, wid, "", true)
	c.Status(http.StatusOK)
}

// QuestionReject dismisses a live ask. Rejecting a live ask is the
// dismiss exit for its inbox record (#1313 S11): the user saw it and
// dismissed it — no whileAway re-presentation.
func (h *ProxyHandler) QuestionReject(c *gin.Context) {
	requestID := c.Param("requestID")
	if !validRequestID(requestID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request ID"})
		return
	}
	wid := c.Param("id")
	workspace, ok := h.resolveWorkspaceForAdapter(c, wid)
	if !ok {
		return
	}
	defer h.releaseConnection(wid)
	if !h.checkAdapterQuota(c, workspace) {
		return
	}
	// 4a D1: question reject is the dismiss exit — reply="reject" rides
	// the action vocabulary; agentd routes question-reject-first.
	if h.agentdTerminus {
		sessionID, resolvable, unknownSet := h.inputRequestSession(c.Request.Context(), wid, requestID)
		if !resolvable {
			if unknownSet {
				c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSec))
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "input pending set unknown"})
				return
			}
			c.JSON(http.StatusNotFound, gin.H{"error": "no pending input request"})
			return
		}
		if !h.actAnswerInput(c, wid, sessionID, requestID, map[string]any{"reply": "reject"}) {
			return
		}
		h.resolveInboxOnProxySuccess(c, wid, sessionID, requestID, inbox.KindQuestion, inbox.StatusDismissed)
		h.postAdapterSuccess(c, workspace, wid, "", true)
		c.Status(http.StatusOK)
		return
	}
	if err := h.adapter.RejectInput(c.Request.Context(), "", wid, requestID); err != nil {
		h.logger.Error("QuestionReject: adapter failed", err, "workspaceID", wid, "requestID", requestID)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to reject question"})
		return
	}
	h.resolveInboxOnProxySuccess(c, wid, "", requestID, inbox.KindQuestion, inbox.StatusDismissed)
	h.postAdapterSuccess(c, workspace, wid, "", true)
	c.Status(http.StatusOK)
}

// ListPermissions returns the pending permissions as the contract
// InputRequest (kind=permission; adapter ListPending, root resolved).
func (h *ProxyHandler) ListPermissions(c *gin.Context) {
	wid := c.Param("id")
	if _, ok := h.resolveWorkspaceForAdapter(c, wid); !ok {
		return
	}
	defer h.releaseConnection(wid)
	pending, err := h.adapter.ListPending(c.Request.Context(), "", wid, "")
	if err != nil {
		// #1302: non-authoritative — 503, never an authoritative empty.
		h.logger.Error("ListPermissions: adapter failed", err, "workspaceID", wid)
		c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSec))
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "failed to list permissions"})
		return
	}
	out := make([]session.InputRequest, 0, len(pending))
	for _, ir := range pending {
		if ir.Kind == session.InputPermission {
			out = append(out, h.resolveInputRequestRoot(c.Request.Context(), wid, ir))
		}
	}
	h.recordActivityIfTracked(wid)
	c.JSON(http.StatusOK, out)
}

// PermissionReply answers a live permission ask. A reply to a non-live
// ask with a pending inbox record lands as guidance-in-history (#1313):
// the original permission already terminated not-granted; the late
// decision informs future turns.
func (h *ProxyHandler) PermissionReply(c *gin.Context) {
	requestID := c.Param("requestID")
	if !validRequestID(requestID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request ID"})
		return
	}
	wid := c.Param("id")
	if h.tryLateAnswer(c, wid, requestID, extractPermissionReplyBody) {
		return
	}
	workspace, ok := h.resolveWorkspaceForAdapter(c, wid)
	if !ok {
		return
	}
	defer h.releaseConnection(wid)
	// Validate BEFORE the quota gate (r3): a malformed 400 must not burn
	// a permanent llm_request reservation.
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unreadable reply body"})
		return
	}
	var payload struct {
		Reply   string `json:"reply"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Reply == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reply carries no decision"})
		return
	}
	if !h.checkAdapterQuota(c, workspace) {
		return
	}
	// 4a (#1302 S1 + D2): the permission vocabulary and the deny
	// feedback ride AnswerInputAction.reply/message through Act.
	if h.agentdTerminus {
		sessionID, resolvable, unknownSet := h.inputRequestSession(c.Request.Context(), wid, requestID)
		if !resolvable {
			if unknownSet {
				c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSec))
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "input pending set unknown"})
				return
			}
			c.JSON(http.StatusNotFound, gin.H{"error": "no pending input request"})
			return
		}
		action := map[string]any{"reply": payload.Reply}
		if payload.Message != "" {
			action["message"] = payload.Message
		}
		if !h.actAnswerInput(c, wid, sessionID, requestID, action) {
			return
		}
		disposition := inbox.StatusAnswered
		if payload.Reply == "reject" {
			disposition = inbox.StatusDismissed
		}
		h.resolveInboxOnProxySuccess(c, wid, sessionID, requestID, inbox.KindPermission, disposition)
		h.postAdapterSuccess(c, workspace, wid, "", true)
		c.Status(http.StatusOK)
		return
	}
	if err := h.adapter.ReplyPermission(c.Request.Context(), "", wid, requestID, payload.Reply, payload.Message); err != nil {
		h.logger.Error("PermissionReply: adapter failed", err, "workspaceID", wid, "requestID", requestID)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to answer permission"})
		return
	}
	h.resolveInboxOnProxySuccess(c, wid, "", requestID, inbox.KindPermission, inbox.StatusAnswered)
	h.postAdapterSuccess(c, workspace, wid, "", true)
	c.Status(http.StatusOK)
}

// tryLateAnswer serves the walk-away flow: the ask is dead in the
// harness but its inbox record is pending. Unknown liveness (unreachable
// harness — e.g. the workspace is suspended) ALSO takes this path: an
// answer is safe against a possibly-live ask by construction (the Q&A
// message lands in history regardless; a still-live ask dies with its
// turn). A TERMINAL-ANSWERED record takes it too (r2 f8): the re-click
// maps to the original outbox entry via the dedupe marker (202
// duplicate:true) — other tabs clear on the resolved event, which can
// lag. A DISMISSED record is rejected (r3 f3): the user's explicit
// dismissal is terminal (two exits, no third state) — a stale tab's
// click must not re-open the conversation or mint a post-mortem turn.
// Buffers the reply body, composes the Q&A message, routes it through
// the outbox (S1/S2: one delivery regime, ask-scoped dedupe), and
// reports handled=true.
func (h *ProxyHandler) tryLateAnswer(c *gin.Context, workspaceID, requestID string, extract func([]byte) string) bool {
	if h.inbox == nil || h.outbox == nil || workspaceID == "" {
		return false
	}
	rec, ok, err := h.inbox.Lookup(c.Request.Context(), workspaceID, requestID)
	if err != nil || !ok {
		return false
	}
	if rec.Status == inbox.StatusDismissed {
		c.JSON(http.StatusConflict, gin.H{"error": "this prompt was dismissed"})
		return true
	}
	if h.askLivenessOf(c.Request.Context(), workspaceID, rec.SessionID, requestID) == askLive {
		return false
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unreadable reply body"})
		return true
	}
	answer := extract(body)
	if answer == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reply carries no answer"})
		return true
	}
	h.lateAnswerInboxAsk(c, workspaceID, rec, answer)
	return true
}

// actAnswerInput forwards one AnswerInputAction to the pod's Act op —
// the #1302/4a S1 shape: the API makes ZERO mutating harness calls in
// the authority regime; agentd is the sole writer. Payload keys are
// protojson camelCase over the bare Connect-JSON body (abiAct).
func (h *ProxyHandler) actAnswerInput(c *gin.Context, workspace, sessionID, requestID string, action map[string]any) bool {
	if err := h.actAnswerInputCtx(c.Request.Context(), workspace, sessionID, requestID, action); err != nil {
		if errors.Is(err, errAgentdEndpointUnresolved) {
			h.logger.Error("input Act: endpoint unresolved", err, "workspaceID", workspace, "requestID", requestID)
			c.JSON(http.StatusConflict, gin.H{"error": gin.H{"code": "unresolved", "detail": "agentd endpoint unavailable"}})
			return false
		}
		status, code := mapConnectError(err)
		h.logger.Error("input Act failed", err, "workspaceID", workspace, "requestID", requestID)
		c.JSON(status, gin.H{"error": gin.H{"code": code, "detail": "failed to answer input"}})
		return false
	}
	return true
}

// errAgentdEndpointUnresolved marks an Act that never left the API
// because the pod's ABI endpoint could not be resolved.
var errAgentdEndpointUnresolved = errors.New("agentd endpoint unavailable")

// actAnswerInputCtx is the context-level AnswerInputAction Act — the
// HTTP-free core shared by the reply routes and the headless
// auto-approve goroutine.
func (h *ProxyHandler) actAnswerInputCtx(ctx context.Context, workspace, sessionID, requestID string, action map[string]any) error {
	base, pw, err := h.agentdEndpoint(ctx, workspace)
	if err != nil {
		return fmt.Errorf("%w: %v", errAgentdEndpointUnresolved, err)
	}
	action["inputId"] = requestID
	payload := map[string]any{
		"sessionId":      sessionID,
		"answerQuestion": action,
	}
	var out json.RawMessage
	return abiAct(ctx, base, pw, payload, &out)
}

// inputRequestSession resolves the session an ask belongs to: live
// pending first, then the inbox record (a dead ask's Act still lands —
// agentd's resolve-by-absence, #1310/1a). resolvable=false +
// unknownSet=true means the live set could not be read AND no record
// identifies the ask — callers must answer NON-authoritatively (503,
// the #1302 doctrine), never an authoritative 404.
func (h *ProxyHandler) inputRequestSession(ctx context.Context, workspaceID, requestID string) (sessionID string, resolvable bool, unknownSet bool) {
	pending, err := h.adapter.ListPending(ctx, "", workspaceID, "")
	if err == nil {
		for _, ir := range pending {
			if ir.ID == requestID {
				return ir.SessionID, true, false
			}
		}
	} else {
		unknownSet = true
	}
	if h.inbox != nil {
		if rec, ok, err := h.inbox.Lookup(ctx, workspaceID, requestID); err == nil && ok {
			return rec.SessionID, true, false
		}
	}
	return "", false, unknownSet
}

// resolveInboxOnProxySuccess terminalizes the ask's inbox record when one
// is pending and publishes the resolved event UNCONDITIONALLY (#1365):
// the clicker's 2xx is not the signal other tabs clear on, and the
// incident's dead pill carried NO pending record — a record-gated
// publish is exactly how a resolved ask stays on screen forever. The
// record's kind and session win over the caller's derivation when
// present; a duplicate publish for a live ask (the bridge's
// InputResolved follows) is idempotent client-side: removal keys on the
// request ID.
func (h *ProxyHandler) resolveInboxOnProxySuccess(c *gin.Context, workspaceID, sessionID, requestID string, kind string, status string) {
	if workspaceID == "" || requestID == "" {
		return
	}
	if h.inbox != nil {
		if rec, ok, err := h.inbox.LookupPending(c.Request.Context(), workspaceID, requestID); err == nil && ok {
			if rec.Kind != "" {
				kind = rec.Kind
			}
			if sessionID == "" {
				sessionID = rec.SessionID
			}
			h.resolveInboxRecord(c.Request.Context(), workspaceID, rec, status)
		}
	}
	reason := "answered"
	if status == inbox.StatusDismissed {
		reason = "dismissed"
	}
	h.publishInputResolved(c, workspaceID, sessionID, requestID, kind, reason)
}

// emitPendingInputRequests fetches pending questions and permissions from the pod
// and publishes them as synthetic events so reconnecting browsers see them immediately.
//
// Accepts the parent's context (typically `c.Request.Context()` for the
// SSE/snapshot path) so contextcheck is happy and a client disconnect
// cancels the in-flight pod fetch promptly. Internally derives a 5s
// timeout from the parent to keep the bounded-per-call cap.
func (h *ProxyHandler) emitPendingInputRequests(ctx context.Context, workspaceID string) {
	// D10: ok reports whether the pending-set fetch actually succeeded.
	// The deferred marker carries it so clients can distinguish
	// "authoritative empty" from "fetch failed — keep your state".
	ok := false

	// D10: flight ID — identifies this snapshot attempt on both the begin
	// and complete markers. Two flights can run concurrently for one
	// workspace (workspace-SSE connect + user-stream connect on a hard page
	// load); clients key per-flight staging on this ID so one flight's
	// commit cannot consume another's staged events. Unix-nano is unique
	// per workspace per invocation.
	flightID := fmt.Sprintf("%s-%d", workspaceID, time.Now().UnixNano())

	// D9/D10: emit the snapshot-complete marker unconditionally on exit,
	// even on timeout/error. On success this commits the flight's staged
	// set; on failure (ok=false) clients keep their existing pending state.
	defer func() {
		if h.userBroker == nil {
			return
		}
		if userID := h.userBroker.WorkspaceOwner(workspaceID); userID != "" {
			h.userBroker.PublishToUser(userID, apitypes.WorkspaceSSEEvent{
				Type:        "agent.input.snapshot_complete",
				WorkspaceID: workspaceID,
				SnapshotOK:  &ok,
				SnapshotID:  flightID,
			})
		}
	}()

	// D10: announce the snapshot before fetching so clients open a per-flight
	// staging window — question/permission events that follow are
	// snapshot-emitted and must survive the marker's authoritative commit.
	if h.userBroker != nil {
		if userID := h.userBroker.WorkspaceOwner(workspaceID); userID != "" {
			h.userBroker.PublishToUser(userID, apitypes.WorkspaceSSEEvent{
				Type:        "agent.input.snapshot_begin",
				WorkspaceID: workspaceID,
				SnapshotID:  flightID,
			})
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Unified ListPending returns both questions and permissions in one
	// call, already typed as session.InputRequest (#828 batch 3: the
	// two-fetch dialect-parsing legacy tail is deleted).
	ok = h.emitPendingViaAdapter(ctx, workspaceID)
}

// RequestInputSnapshot triggers an input-snapshot flight for the workspace:
// emits agent.input.snapshot_begin → pending question/permission events →
// agent.input.snapshot_complete (with snapshot_ok) on the user stream.
//
// Clients call this when they need a FRESH authoritative snapshot without
// reconnecting an SSE stream — e.g. ChatPage arming reconnect mode after an
// in-workspace session switch (no stream reconnect fires, so no snapshot
// flight would otherwise run and the stuck-session gate would wait forever).
func (h *ProxyHandler) RequestInputSnapshot(c *gin.Context) {
	workspaceID := c.Param("id")
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace ID required"})
		return
	}
	// Detached context: the fetch (≤5s) must outlive this request, which
	// returns immediately. The flight's events reach the caller via the
	// user-event stream, not this response.
	go h.emitPendingInputRequests(context.WithoutCancel(c.Request.Context()), workspaceID) //nolint:contextcheck // intentionally detached — the snapshot outlives the request
	c.Status(http.StatusAccepted)
}

// emitPendingViaAdapter uses the Adapter's ListPending to fetch pending
// input requests in a single call, then publishes them as SSE events in
// the contract InputRequest shape (one shape for REST and SSE, #1302).
// Returns true when the ListPending call succeeded.
//
// #1313: a ListPending failure still emits the inbox-only half of the
// union (liveIDs empty — the live set is unknown, not empty) before
// returning false. The ok=false marker keeps the flight
// non-authoritative (clients never wipe live prompts they already hold),
// and clients apply ADDITIVE staged prompts only — the whileAway
// re-presentation therefore survives the suspension window, the exact
// scenario the inbox exists for.
func (h *ProxyHandler) emitPendingViaAdapter(ctx context.Context, workspaceID string) bool {
	pending, err := h.adapter.ListPending(ctx, "", workspaceID, "")
	if err != nil {
		h.emitInboxOnlyRecords(ctx, workspaceID, nil)
		return false
	}
	autoApprove := h.shouldAutoApprovePermissions(ctx, workspaceID)

	liveIDs := make(map[string]bool, len(pending))
	for _, ir := range pending {
		if ir.Kind == session.InputPermission && autoApprove {
			continue
		}
		liveIDs[ir.ID] = true
		// #1313 decision 3: the snapshot path re-records every live ask —
		// events are droppable (busy-gated stream, broker replay bounds);
		// this is the recovering write.
		h.recordInboxAskFromSession(ctx, workspaceID, ir)
		switch ir.Kind {
		case session.InputQuestion:
			h.publishWorkspaceAndUserEvent(workspaceID, apitypes.WorkspaceSSEEvent{
				Type:      "agent.question",
				SessionID: ir.SessionID,
				RequestID: ir.ID,
				Data:      h.resolveInputRequestRoot(ctx, workspaceID, ir),
			})
		case session.InputPermission:
			h.publishWorkspaceAndUserEvent(workspaceID, apitypes.WorkspaceSSEEvent{
				Type:      "agent.permission",
				SessionID: ir.SessionID,
				RequestID: ir.ID,
				Data:      h.resolveInputRequestRoot(ctx, workspaceID, ir),
			})
		}
	}
	// #1313 S5 extension: pending UI = live asks ∪ inbox. Inbox-only
	// records re-present as whileAway prompts with choices still active.
	h.emitInboxOnlyRecords(ctx, workspaceID, liveIDs)
	return true
}

// resolveInputRequestRoot returns the contract InputRequest with the
// root session resolved (subtask prompts bubble to the user-visible
// ancestor) — ONE shape for both the REST lists and the SSE emitter
// (#1302 items 1/4).
func (h *ProxyHandler) resolveInputRequestRoot(ctx context.Context, workspaceID string, ir session.InputRequest) session.InputRequest {
	if h.sessionParents != nil {
		ir.RootSessionID = h.sessionParents.resolveRoot(ctx, workspaceID, ir.SessionID)
	} else {
		ir.RootSessionID = ir.SessionID
	}
	return ir
}
