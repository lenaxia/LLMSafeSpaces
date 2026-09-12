// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/services/inbox"
	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// Unanswered-question inbox glue (#1313, epic-71 / 3a). The store lives in
// api/internal/services/inbox; this file owns the seams: record at
// ask-projection time, the snapshot union, late answers through the
// outbox, and dismiss as the second exit (S11).

// recordInboxAsk upserts one ask record. Failures never block the primary
// path — the snapshot flight re-records (the recovering write) — but they
// are logged: a silent inbox is a silent #1313 regression.
func (h *ProxyHandler) recordInboxAsk(ctx context.Context, workspaceID string, req *abiv1.InputRequest) {
	if h.inbox == nil || req == nil || req.GetId() == "" || req.GetSessionId() == "" {
		return
	}
	rec := inboxRecordFromABI(req)
	if err := h.inbox.Record(ctx, workspaceID, rec); err != nil {
		h.logger.Warn("inbox record failed", "error", err, "workspace", workspaceID, "ask", req.GetId())
	}
}

func nowUTC() time.Time { return time.Now().UTC() }

// recordInboxAskFromSession is the snapshot-path sibling of
// recordInboxAsk — the recovering write under droppable events.
func (h *ProxyHandler) recordInboxAskFromSession(ctx context.Context, workspaceID string, ir session.InputRequest) {
	if h.inbox == nil || ir.ID == "" || ir.SessionID == "" {
		return
	}
	rec := inbox.Record{
		ID:         ir.ID,
		SessionID:  ir.SessionID,
		RecordedAt: nowUTC(),
		Status:     inbox.StatusPending,
	}
	if ir.Tool != nil {
		rec.Tool = &inbox.ToolRef{MessageID: ir.Tool.MessageID, CallID: ir.Tool.CallID}
	}
	if ir.Kind == session.InputPermission {
		rec.Kind = inbox.KindPermission
		rec.Permission = ir.Permission
		rec.Patterns = ir.Patterns
		rec.Always = ir.Always
	} else {
		rec.Kind = inbox.KindQuestion
		rec.Question = ir.Question
		rec.Header = ir.Header
		rec.Multiple = ir.Multiple
		rec.Custom = ir.Custom
		for _, o := range ir.Options {
			rec.Options = append(rec.Options, inbox.Option{Label: o.Label, Description: o.Description})
		}
	}
	if err := h.inbox.Record(ctx, workspaceID, rec); err != nil {
		h.logger.Warn("inbox snapshot record failed", "error", err, "workspace", workspaceID, "ask", ir.ID)
	}
}

func inboxRecordFromABI(req *abiv1.InputRequest) inbox.Record {
	rec := inbox.Record{
		ID:         req.GetId(),
		SessionID:  req.GetSessionId(),
		RecordedAt: nowUTC(),
		Status:     inbox.StatusPending,
	}
	if t := req.GetTool(); t != nil {
		rec.Tool = &inbox.ToolRef{MessageID: t.GetMessageId(), CallID: t.GetCallId()}
	}
	if req.GetKind() == abiv1.InputKind_INPUT_KIND_PERMISSION {
		rec.Kind = inbox.KindPermission
		rec.Permission = req.GetPermission()
		rec.Patterns = req.GetPatterns()
		rec.Always = req.GetAlways()
		return rec
	}
	rec.Kind = inbox.KindQuestion
	rec.Question = req.GetQuestion()
	rec.Header = req.GetHeader()
	rec.Multiple = req.GetMultiple()
	rec.Custom = req.GetCustom()
	for _, o := range req.GetOptions() {
		rec.Options = append(rec.Options, inbox.Option{Label: o.GetLabel(), Description: o.GetDescription()})
	}
	return rec
}

// emitInboxOnlyRecords publishes the inbox's pending records that the
// live pending set does NOT carry — the "while you were away" half of
// the snapshot union (S5 extension). Live IDs win: a record whose ask is
// still live was just emitted from the live set.
func (h *ProxyHandler) emitInboxOnlyRecords(ctx context.Context, workspaceID string, liveIDs map[string]bool) {
	if h.inbox == nil {
		return
	}
	records, err := h.inbox.ListWorkspace(ctx, workspaceID)
	if err != nil {
		h.logger.Warn("inbox list for snapshot union failed", "error", err, "workspace", workspaceID)
		return
	}
	for _, rec := range records {
		if liveIDs[rec.ID] {
			continue
		}
		root := rec.SessionID
		if h.sessionParents != nil {
			root = h.sessionParents.resolveRoot(ctx, workspaceID, rec.SessionID)
		}
		switch rec.Kind {
		case inbox.KindQuestion:
			req := questionRequestFromRecord(rec, root)
			h.publishWorkspaceAndUserEvent(workspaceID, apitypes.WorkspaceSSEEvent{
				Type:      "agent.question",
				SessionID: rec.SessionID,
				RequestID: rec.ID,
				Data:      req,
			})
		case inbox.KindPermission:
			req := permissionRequestFromRecord(rec, root)
			h.publishWorkspaceAndUserEvent(workspaceID, apitypes.WorkspaceSSEEvent{
				Type:      "agent.permission",
				SessionID: rec.SessionID,
				RequestID: rec.ID,
				Data:      req,
			})
		}
	}
}

func questionRequestFromRecord(rec inbox.Record, root string) *agent.QuestionRequest {
	info := agent.QuestionInfo{
		Question: rec.Question,
		Header:   rec.Header,
		Multiple: rec.Multiple,
		Custom:   rec.Custom,
	}
	for _, o := range rec.Options {
		info.Options = append(info.Options, agent.QuestionOption{Label: o.Label, Description: o.Description})
	}
	req := &agent.QuestionRequest{
		ID:            rec.ID,
		SessionID:     rec.SessionID,
		RootSessionID: root,
		Questions:     []agent.QuestionInfo{info},
		WhileAway:     true,
	}
	if rec.Tool != nil {
		req.Tool = &agent.ToolRef{MessageID: rec.Tool.MessageID, CallID: rec.Tool.CallID}
	}
	return req
}

func permissionRequestFromRecord(rec inbox.Record, root string) *agent.PermissionRequest {
	req := &agent.PermissionRequest{
		ID:            rec.ID,
		SessionID:     rec.SessionID,
		RootSessionID: root,
		Permission:    rec.Permission,
		Patterns:      rec.Patterns,
		Always:        rec.Always,
		WhileAway:     true,
	}
	if rec.Tool != nil {
		req.Tool = &agent.ToolRef{MessageID: rec.Tool.MessageID, CallID: rec.Tool.CallID}
	}
	return req
}

// askIsLive consults the harness pending set. An unreachable harness is
// NOT proof of death: returns false only with positive evidence that the
// ask is absent from a successful listing.
func (h *ProxyHandler) askIsLive(ctx context.Context, workspaceID, sessionID, askID string) bool {
	if h.adapter == nil {
		return false
	}
	pending, err := h.adapter.ListPending(ctx, "", workspaceID, sessionID)
	if err != nil {
		return false
	}
	for _, ir := range pending {
		if ir.ID == askID {
			return true
		}
	}
	return false
}

// composeQA renders the late-answer user message. Framed from G1
// evidence: the harness's own answer format is plain text
// (`User has answered your questions: "…"="…"`) — the Q&A message rides
// the same register so the model continues without re-asking.
func composeQA(rec inbox.Record, answer string) string {
	var b strings.Builder
	if rec.Kind == inbox.KindPermission {
		b.WriteString("Permission decision on your earlier request")
		if rec.Permission != "" {
			fmt.Fprintf(&b, " (%s", rec.Permission)
			if len(rec.Patterns) > 0 {
				fmt.Fprintf(&b, ": %s", strings.Join(rec.Patterns, ", "))
			}
			b.WriteString(")")
		}
		fmt.Fprintf(&b, ": %s. The original request already ended; treat this as guidance for future turns.", answer)
		return b.String()
	}
	b.WriteString("Answering your earlier question")
	if rec.Question != "" {
		fmt.Fprintf(&b, " — %q", rec.Question)
	}
	fmt.Fprintf(&b, ": %s. Continue with this answer in mind.", answer)
	return b.String()
}

// lateAnswerInboxAsk routes a reply on a non-live ask through the
// standard outbox path: compose the Q&A user message, accept it with the
// ask-scoped dedupe key (S2 via the outbox's cmid marker), and
// terminalize the record as answered. Exactly-once per ask: a second
// click hits the dedupe marker and maps to the original entry.
func (h *ProxyHandler) lateAnswerInboxAsk(c *gin.Context, workspaceID string, rec inbox.Record, answer string) {
	cmid := "inbox-" + rec.ID + "-answer"
	entry, err := h.outbox.Accept(c.Request.Context(), workspaceID, rec.SessionID, "", cmid, composeQA(rec, answer), nil)
	if err != nil {
		var dup *outbox.Duplicate
		if errors.As(err, &dup) {
			h.resolveInboxRecord(c.Request.Context(), workspaceID, rec, inbox.StatusAnswered)
			c.JSON(http.StatusAccepted, gin.H{
				"status":          "queued",
				"clientMessageID": cmid,
				"messageID":       dup.AcceptedID,
				"duplicate":       true,
			})
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "inbox late answer unavailable: " + err.Error()})
		return
	}
	h.resolveInboxRecord(c.Request.Context(), workspaceID, rec, inbox.StatusAnswered)
	c.JSON(http.StatusAccepted, gin.H{
		"status":          "queued",
		"clientMessageID": cmid,
		"messageID":       entry.ID,
	})
}

func (h *ProxyHandler) resolveInboxRecord(ctx context.Context, workspaceID string, rec inbox.Record, status string) {
	if h.inbox == nil {
		return
	}
	if err := h.inbox.Resolve(ctx, workspaceID, rec.SessionID, rec.ID, status); err != nil {
		h.logger.Warn("inbox resolve failed", "error", err, "workspace", workspaceID, "ask", rec.ID, "status", status)
	}
}

// DismissInboxRecord is the dismiss exit (S11): DELETE
// /workspaces/:id/sessions/:sessionId/inbox/:requestID. A live ask is
// rejected first (the two-exits hole), then the record terminalizes as
// dismissed and a resolved event clears other tabs.
func (h *ProxyHandler) DismissInboxRecord(c *gin.Context) {
	workspaceID := c.Param("id")
	sessionID := c.Param("sessionId")
	askID := c.Param("requestID")
	if workspaceID == "" || sessionID == "" || askID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace, session, and request IDs required"})
		return
	}
	if h.inbox == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "inbox not configured"})
		return
	}
	rec, ok, err := h.inbox.LookupPending(c.Request.Context(), workspaceID, askID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !ok || rec.SessionID != sessionID {
		c.JSON(http.StatusNotFound, gin.H{"error": "no pending inbox record"})
		return
	}
	if h.askIsLive(c.Request.Context(), workspaceID, sessionID, askID) {
		if h.adapter == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "input adapter not configured"})
			return
		}
		if err := h.adapter.RejectInput(c.Request.Context(), "", workspaceID, askID); err != nil {
			h.logger.Warn("inbox dismiss: live reject failed", "error", err, "workspace", workspaceID, "ask", askID)
			c.JSON(http.StatusBadGateway, gin.H{"error": "live ask reject failed: " + err.Error()})
			return
		}
	}
	h.resolveInboxRecord(c.Request.Context(), workspaceID, rec, inbox.StatusDismissed)
	if h.userBroker != nil {
		if userID := h.userBroker.WorkspaceOwner(workspaceID); userID != "" {
			resolvedType := "agent.question.resolved"
			if rec.Kind == inbox.KindPermission {
				resolvedType = "agent.permission.resolved"
			}
			h.userBroker.PublishToUser(userID, apitypes.WorkspaceSSEEvent{
				Type:        resolvedType,
				WorkspaceID: workspaceID,
				SessionID:   sessionID,
				RequestID:   askID,
				Data: map[string]string{
					"request_id": askID,
					"session_id": sessionID,
					"reason":     "dismissed",
				},
			})
		}
	}
	c.Status(http.StatusNoContent)
}

// extractQuestionAnswerBody flattens the question reply body into the
// display answer carried by the Q&A message: {"answers":[["Yes","No"]]}
// → `"Yes", "No"`.
func extractQuestionAnswerBody(body []byte) string {
	var payload struct {
		Answers [][]string `json:"answers"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	var parts []string
	for _, group := range payload.Answers {
		parts = append(parts, group...)
	}
	return quoteJoin(parts)
}

func quoteJoin(parts []string) string {
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = `"` + p + `"`
	}
	return strings.Join(quoted, ", ")
}

// extractPermissionReplyBody pulls the decision out of the permission
// reply body: {"reply":"once"|"always"|"reject", "message"?: string}.
func extractPermissionReplyBody(body []byte) string {
	var payload struct {
		Reply   string `json:"reply"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	if payload.Message != "" {
		return payload.Reply + " (" + payload.Message + ")"
	}
	return payload.Reply
}
