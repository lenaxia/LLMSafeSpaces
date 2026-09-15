// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"time"

	abi "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// proxy_sessions_act.go — #1372 (epic-71 / s1-sessions): the
// sessions-cluster write core. In the authority regime the five session
// writes (create/send/abort/delete/rename) ride agentd's Act op — the
// same terminus transport discipline as actAnswerInputCtx (S1: the API
// makes ZERO mutating harness calls; agentd is the sole writer, and the
// action serializes against delivery via the pod's per-session
// single-flight). Flag-off keeps the typed adapter methods.
//
// Payload keys are protojson camelCase over the bare Connect-JSON body
// (abiAct); the result decodes as an ActionResult via protojson.
// Errors deliberately do NOT map through mapConnectError: these routes'
// wire shapes are pre-existing (the response contract does not move —
// only the write path), so every Act failure surfaces the route's own
// 502 body exactly as the adapter path's failures do.

// actSessionAction forwards one typed ActionRequest to the pod's Act op
// and decodes the ActionResult. The caller's sessionID is authoritative
// and injected here ("" for create — the harness mints the id).
func (h *ProxyHandler) actSessionAction(ctx context.Context, workspaceID, sessionID string, action map[string]any) (*abi.ActionResult, error) {
	base, pw, err := h.agentdEndpoint(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errAgentdEndpointUnresolved, err)
	}
	payload := map[string]any{"sessionId": sessionID}
	for k, v := range action {
		payload[k] = v
	}
	var out abi.ActionResult
	if err := abiActProto(ctx, base, pw, payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// actCreateSession creates a session through Act. The action carries no
// session id (the harness mints it); the caller stamps the workspace id
// on the returned session — agentd cannot know it.
func (h *ProxyHandler) actCreateSession(ctx context.Context, workspaceID, title string) (*session.Session, error) {
	create := map[string]any{}
	if title != "" {
		create["title"] = title
	}
	res, err := h.actSessionAction(ctx, workspaceID, "", map[string]any{"createSession": create})
	if err != nil {
		return nil, err
	}
	s := res.GetCreateSession().GetSession()
	if s == nil {
		return nil, fmt.Errorf("act create_session: no session in result")
	}
	return sessionFromABI(s), nil
}

// actSend delivers one synchronous send through Act and returns the
// completed assistant message (the V1 message contract — the REST
// response is this message verbatim).
func (h *ProxyHandler) actSend(ctx context.Context, workspaceID, sessionID, text string, model *session.ModelRef) (*session.Message, error) {
	send := map[string]any{"text": text}
	if model != nil {
		send["model"] = map[string]any{"id": model.ID, "provider": model.Provider}
	}
	res, err := h.actSessionAction(ctx, workspaceID, sessionID, map[string]any{"send": send})
	if err != nil {
		return nil, err
	}
	m := res.GetSend().GetMessage()
	if m == nil {
		return nil, fmt.Errorf("act send: no message in result")
	}
	return messageFromABI(m), nil
}

// actAbort interrupts the in-flight turn through Act — the existing
// interrupt verb IS the V1 abort route (D1: no new verb for abort).
func (h *ProxyHandler) actAbort(ctx context.Context, workspaceID, sessionID string) error {
	_, err := h.actSessionAction(ctx, workspaceID, sessionID, map[string]any{"interrupt": map[string]any{}})
	return err
}

// actDeleteSession deletes the session through Act.
func (h *ProxyHandler) actDeleteSession(ctx context.Context, workspaceID, sessionID string) error {
	_, err := h.actSessionAction(ctx, workspaceID, sessionID, map[string]any{"deleteSession": map[string]any{}})
	return err
}

// actRenameSession renames the session through Act (the PATCH semantics
// ride the actor — the handler stays dialect-free).
func (h *ProxyHandler) actRenameSession(ctx context.Context, workspaceID, sessionID, title string) error {
	_, err := h.actSessionAction(ctx, workspaceID, sessionID,
		map[string]any{"renameSession": map[string]any{"title": title}})
	return err
}

// --- ABI → contract converters -------------------------------------------
//
// The handlers-side halves (inputRequestFromABI in proxy_usagestream is
// the precedent); the wiring-side halves live beside questionToABI in
// agentd. The regime-parity pins hold both sides to the contract JSON.

func modelRefFromABI(m *abi.ModelRef) *session.ModelRef {
	if m == nil {
		return nil
	}
	return &session.ModelRef{ID: m.GetId(), Provider: m.GetProvider()}
}

func sessionStatusFromABI(s abi.SessionStatus) session.Status {
	switch s {
	case abi.SessionStatus_SESSION_STATUS_IDLE:
		return session.StatusIdle
	case abi.SessionStatus_SESSION_STATUS_BUSY:
		return session.StatusBusy
	case abi.SessionStatus_SESSION_STATUS_ERROR:
		return session.StatusError
	case abi.SessionStatus_SESSION_STATUS_COMPACTING:
		return session.StatusCompacting
	case abi.SessionStatus_SESSION_STATUS_ARCHIVED:
		return session.StatusArchived
	default:
		return session.StatusUnknown
	}
}

func costFromABI(c *abi.Cost) *session.Cost {
	if c == nil {
		return nil
	}
	return &session.Cost{
		InputTokens:      c.GetInputTokens(),
		OutputTokens:     c.GetOutputTokens(),
		ReasoningTokens:  c.GetReasoningTokens(),
		CacheReadTokens:  c.GetCacheReadTokens(),
		CacheWriteTokens: c.GetCacheWriteTokens(),
		TotalTokens:      c.GetTotalTokens(),
		CostUSD:          c.GetCostUsd(),
	}
}

func tsFromABI(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil || !ts.IsValid() {
		return nil
	}
	t := ts.AsTime().UTC()
	return &t
}

func timeRangeFromABI(tr *abi.TimeRange) *session.TimeRange {
	if tr == nil {
		return nil
	}
	started := tsFromABI(tr.GetStartedAt())
	if started == nil {
		return nil
	}
	out := &session.TimeRange{StartedAt: *started}
	if completed := tsFromABI(tr.GetCompletedAt()); completed != nil {
		out.CompletedAt = completed
	}
	return out
}

func contextUsageFromABI(cu *abi.ContextUsage) *session.ContextUsage {
	if cu == nil {
		return nil
	}
	return &session.ContextUsage{Used: cu.GetUsed(), Window: cu.GetWindow()}
}

func sessionFromABI(s *abi.Session) *session.Session {
	return &session.Session{
		ID:           s.GetId(),
		WorkspaceID:  s.GetWorkspaceId(),
		ParentID:     s.GetParentId(),
		Title:        s.GetTitle(),
		AgentID:      s.GetAgentId(),
		Model:        modelRefFromABI(s.GetModel()),
		Status:       sessionStatusFromABI(s.GetStatus()),
		Cost:         costFromABI(s.GetCost()),
		ContextUsage: contextUsageFromABI(s.GetContextUsage()),
		Time:         timeRangeFromABI(s.GetTime()),
		Summary:      s.GetSummary(),
		Archived:     s.GetArchived(),
	}
}

func messageTypeFromABI(t abi.MessageType) session.MessageType {
	switch t {
	case abi.MessageType_MESSAGE_TYPE_USER:
		return session.MessageUser
	case abi.MessageType_MESSAGE_TYPE_ASSISTANT:
		return session.MessageAssistant
	case abi.MessageType_MESSAGE_TYPE_SHELL:
		return session.MessageShell
	case abi.MessageType_MESSAGE_TYPE_AGENT_SWITCH:
		return session.MessageAgentSwitch
	case abi.MessageType_MESSAGE_TYPE_MODEL_SWITCH:
		return session.MessageModelSwitch
	case abi.MessageType_MESSAGE_TYPE_COMPACTION:
		return session.MessageCompaction
	case abi.MessageType_MESSAGE_TYPE_SYSTEM:
		return session.MessageSystem
	default:
		return ""
	}
}

func toolStatusFromABI(s abi.ToolStatus) session.ToolStatus {
	switch s {
	case abi.ToolStatus_TOOL_STATUS_PENDING:
		return session.ToolStatusPending
	case abi.ToolStatus_TOOL_STATUS_RUNNING:
		return session.ToolStatusRunning
	case abi.ToolStatus_TOOL_STATUS_COMPLETED:
		return session.ToolStatusCompleted
	case abi.ToolStatus_TOOL_STATUS_ERROR:
		return session.ToolStatusError
	default:
		return ""
	}
}

func changeStatusFromABI(s abi.ChangeStatus) session.ChangeStatus {
	switch s {
	case abi.ChangeStatus_CHANGE_STATUS_ADDED:
		return session.ChangeAdded
	case abi.ChangeStatus_CHANGE_STATUS_MODIFIED:
		return session.ChangeModified
	case abi.ChangeStatus_CHANGE_STATUS_DELETED:
		return session.ChangeDeleted
	case abi.ChangeStatus_CHANGE_STATUS_RENAMED:
		return session.ChangeRenamed
	default:
		return ""
	}
}

func partFromABI(p *abi.Part) session.Part {
	out := session.Part{ID: p.GetId()}
	switch p.GetType() {
	case abi.PartType_PART_TYPE_TEXT:
		out.Type = session.PartText
		out.Text = p.GetText()
	case abi.PartType_PART_TYPE_REASONING:
		out.Type = session.PartReasoning
		out.Reasoning = p.GetReasoning()
	case abi.PartType_PART_TYPE_TOOL:
		t := p.GetTool()
		if t == nil {
			return out
		}
		out.Type = session.PartTool
		tool := &session.ToolPart{CallID: t.GetCallId(), Name: t.GetName(), Input: t.GetInput(), Output: t.GetOutput()}
		if st := t.GetState(); st != nil {
			tool.State = session.ToolState{Status: toolStatusFromABI(st.GetStatus()), Error: st.GetError()}
			if ts := tsFromABI(st.GetStartedAt()); ts != nil {
				tool.State.StartedAt = ts
			}
			if ts := tsFromABI(st.GetCompletedAt()); ts != nil {
				tool.State.CompletedAt = ts
			}
		}
		out.Tool = tool
	case abi.PartType_PART_TYPE_FILE_CHANGE:
		f := p.GetFileChange()
		if f == nil {
			return out
		}
		out.Type = session.PartFileChange
		out.FileChange = &session.FileDiff{
			Path:      f.GetPath(),
			OldPath:   f.GetOldPath(),
			Status:    changeStatusFromABI(f.GetStatus()),
			Patch:     f.GetPatch(),
			Additions: int(f.GetAdditions()),
			Deletions: int(f.GetDeletions()),
		}
	case abi.PartType_PART_TYPE_CUSTOM:
		c := p.GetCustom()
		if c == nil {
			return out
		}
		out.Type = session.PartCustom
		out.Custom = &session.CustomPart{Kind: c.GetKind(), Data: c.GetData()}
	}
	return out
}

func messageFromABI(m *abi.Message) *session.Message {
	out := &session.Message{
		ID:        m.GetId(),
		SessionID: m.GetSessionId(),
		Type:      messageTypeFromABI(m.GetType()),
		Text:      m.GetText(),
		Command:   m.GetCommand(),
		FromAgent: m.GetFromAgent(),
		ToAgent:   m.GetToAgent(),
		FromModel: modelRefFromABI(m.GetFromModel()),
		ToModel:   modelRefFromABI(m.GetToModel()),
		Model:     modelRefFromABI(m.GetModel()),
		Cost:      costFromABI(m.GetCost()),
	}
	if ts := tsFromABI(m.GetCreatedAt()); ts != nil {
		out.CreatedAt = ts
	}
	if m.ExitCode != nil {
		v := int(m.GetExitCode())
		out.ExitCode = &v
	}
	if e := m.GetError(); e != nil {
		out.Error = &session.Error{Code: e.GetCode(), Message: e.GetMessage()}
	}
	for _, p := range m.GetParts() {
		if p.GetType() == abi.PartType_PART_TYPE_UNSPECIFIED {
			continue
		}
		out.Parts = append(out.Parts, partFromABI(p))
	}
	return out
}
