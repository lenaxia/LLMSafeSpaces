// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/services/inbox"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

var (
	questionIDPattern   = regexp.MustCompile(`^que_[a-zA-Z0-9]+$`)
	permissionIDPattern = regexp.MustCompile(`^per_[a-zA-Z0-9_]+$`)
)

// ListQuestions returns the pending questions as the normalized
// agent.QuestionRequest envelope (adapter ListPending, questions only).
func (h *ProxyHandler) ListQuestions(c *gin.Context) {
	wid := c.Param("id")
	if h.adapter == nil {
		h.adapterUnavailable(c)
		return
	}
	pending, err := h.adapter.ListPending(c.Request.Context(), "", wid, "")
	if err != nil {
		h.logger.Error("ListQuestions: adapter failed", err, "workspaceID", wid)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to list questions"})
		return
	}
	out := make([]*agent.QuestionRequest, 0, len(pending))
	for _, ir := range pending {
		if ir.Kind == session.InputQuestion {
			out = append(out, h.toQuestionRequest(c.Request.Context(), wid, ir))
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
	if !questionIDPattern.MatchString(requestID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid question request ID format"})
		return
	}
	wid := c.Param("id")
	if h.tryLateAnswer(c, wid, requestID, extractQuestionAnswerBody) {
		return
	}
	if h.adapter == nil {
		h.adapterUnavailable(c)
		return
	}
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
	if err := h.adapter.AnswerQuestion(c.Request.Context(), "", wid, requestID, payload.Answers); err != nil {
		h.logger.Error("QuestionReply: adapter failed", err, "workspaceID", wid, "requestID", requestID)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to answer question"})
		return
	}
	h.resolveInboxOnProxySuccess(c, wid, requestID, "answered")
	h.recordActivityIfTracked(wid)
	c.JSON(http.StatusOK, gin.H{"status": "answered"})
}

// QuestionReject dismisses a live ask. Rejecting a live ask is the
// dismiss exit for its inbox record (#1313 S11): the user saw it and
// dismissed it — no whileAway re-presentation.
func (h *ProxyHandler) QuestionReject(c *gin.Context) {
	requestID := c.Param("requestID")
	if !questionIDPattern.MatchString(requestID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid question request ID format"})
		return
	}
	wid := c.Param("id")
	if h.adapter == nil {
		h.adapterUnavailable(c)
		return
	}
	if err := h.adapter.RejectInput(c.Request.Context(), "", wid, requestID); err != nil {
		h.logger.Error("QuestionReject: adapter failed", err, "workspaceID", wid, "requestID", requestID)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to reject question"})
		return
	}
	h.resolveInboxOnProxySuccess(c, wid, requestID, "dismissed")
	h.recordActivityIfTracked(wid)
	c.JSON(http.StatusOK, gin.H{"status": "dismissed"})
}

// ListPermissions returns the pending permissions as the normalized
// agent.PermissionRequest envelope (adapter ListPending, permissions
// only).
func (h *ProxyHandler) ListPermissions(c *gin.Context) {
	wid := c.Param("id")
	if h.adapter == nil {
		h.adapterUnavailable(c)
		return
	}
	pending, err := h.adapter.ListPending(c.Request.Context(), "", wid, "")
	if err != nil {
		h.logger.Error("ListPermissions: adapter failed", err, "workspaceID", wid)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to list permissions"})
		return
	}
	out := make([]*agent.PermissionRequest, 0, len(pending))
	for _, ir := range pending {
		if ir.Kind == session.InputPermission {
			out = append(out, h.toPermissionRequest(c.Request.Context(), wid, ir))
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
	if !permissionIDPattern.MatchString(requestID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid permission request ID format"})
		return
	}
	wid := c.Param("id")
	if h.tryLateAnswer(c, wid, requestID, extractPermissionReplyBody) {
		return
	}
	if h.adapter == nil {
		h.adapterUnavailable(c)
		return
	}
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
	if err := h.adapter.ReplyPermission(c.Request.Context(), "", wid, requestID, payload.Reply, payload.Message); err != nil {
		h.logger.Error("PermissionReply: adapter failed", err, "workspaceID", wid, "requestID", requestID)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to answer permission"})
		return
	}
	h.resolveInboxOnProxySuccess(c, wid, requestID, "answered")
	h.recordActivityIfTracked(wid)
	c.JSON(http.StatusOK, gin.H{"status": "answered"})
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
	if h.inbox == nil || h.outbox == nil || h.adapter == nil || workspaceID == "" {
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

// resolveInboxOnProxySuccess terminalizes the ask's inbox record when the
// proxied live reply/reject succeeded (2xx from the pod).
func (h *ProxyHandler) resolveInboxOnProxySuccess(c *gin.Context, workspaceID, requestID, status string) {
	if h.inbox == nil || workspaceID == "" || c.Writer.Status() >= 400 {
		return
	}
	rec, ok, err := h.inbox.LookupPending(c.Request.Context(), workspaceID, requestID)
	if err != nil || !ok {
		return
	}
	h.resolveInboxRecord(c.Request.Context(), workspaceID, rec, status)
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
	// two-fetch dialect-parsing legacy tail is deleted). A nil adapter
	// (dev/test wiring) leaves ok=false — the deferred D10 marker fires
	// non-authoritative and clients keep their existing pending state.
	if h.adapter == nil {
		return
	}
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
	if h.adapter == nil {
		h.adapterUnavailable(c)
		return
	}
	// Detached context: the fetch (≤5s) must outlive this request, which
	// returns immediately. The flight's events reach the caller via the
	// user-event stream, not this response.
	go h.emitPendingInputRequests(context.WithoutCancel(c.Request.Context()), workspaceID) //nolint:contextcheck // intentionally detached — the snapshot outlives the request
	c.JSON(http.StatusAccepted, gin.H{"status": "snapshot requested"})
}

// emitPendingViaAdapter uses the Adapter's ListPending to fetch pending
// input requests in a single call, then publishes them as SSE events.
// Converts session.InputRequest to the legacy agent.QuestionRequest /
// agent.PermissionRequest shapes the SSE consumers expect.
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
				Data:      h.toQuestionRequest(ctx, workspaceID, ir),
			})
		case session.InputPermission:
			h.publishWorkspaceAndUserEvent(workspaceID, apitypes.WorkspaceSSEEvent{
				Type:      "agent.permission",
				SessionID: ir.SessionID,
				RequestID: ir.ID,
				Data:      h.toPermissionRequest(ctx, workspaceID, ir),
			})
		}
	}
	// #1313 S5 extension: pending UI = live asks ∪ inbox. Inbox-only
	// records re-present as whileAway prompts with choices still active.
	h.emitInboxOnlyRecords(ctx, workspaceID, liveIDs)
	return true
}

// toQuestionRequest translates a contract InputRequest into the
// normalized agent.QuestionRequest envelope (root-session resolved) —
// the shape both the REST list route and the SSE snapshot emit.
func (h *ProxyHandler) toQuestionRequest(ctx context.Context, workspaceID string, ir session.InputRequest) *agent.QuestionRequest {
	rootSession := ir.SessionID
	if h.sessionParents != nil {
		rootSession = h.sessionParents.resolveRoot(ctx, workspaceID, ir.SessionID)
	}
	questionInfo := agent.QuestionInfo{
		Question: ir.Question,
		Header:   ir.Header,
		Multiple: ir.Multiple,
		Custom:   ir.Custom,
	}
	for _, o := range ir.Options {
		questionInfo.Options = append(questionInfo.Options,
			agent.QuestionOption{Label: o.Label, Description: o.Description})
	}
	req := &agent.QuestionRequest{
		ID:            ir.ID,
		SessionID:     ir.SessionID,
		RootSessionID: rootSession,
		Questions:     []agent.QuestionInfo{questionInfo},
	}
	if ir.Tool != nil {
		req.Tool = &agent.ToolRef{
			MessageID: ir.Tool.MessageID,
			CallID:    ir.Tool.CallID,
		}
	}
	return req
}

// toPermissionRequest translates a contract InputRequest into the
// normalized agent.PermissionRequest envelope (root-session resolved) —
// the shape both the REST list route and the SSE snapshot emit.
func (h *ProxyHandler) toPermissionRequest(ctx context.Context, workspaceID string, ir session.InputRequest) *agent.PermissionRequest {
	rootSession := ir.SessionID
	if h.sessionParents != nil {
		rootSession = h.sessionParents.resolveRoot(ctx, workspaceID, ir.SessionID)
	}
	req := &agent.PermissionRequest{
		ID:            ir.ID,
		SessionID:     ir.SessionID,
		RootSessionID: rootSession,
		Permission:    ir.Permission,
		Patterns:      ir.Patterns,
		Always:        ir.Always,
	}
	if ir.Tool != nil {
		req.Tool = &agent.ToolRef{
			MessageID: ir.Tool.MessageID,
			CallID:    ir.Tool.CallID,
		}
	}
	return req
}
