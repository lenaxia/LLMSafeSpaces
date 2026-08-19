// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"strings"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// mockAdapter is a test double for agent.Adapter. Each method is a
// configurable function field; tests set only the methods they exercise.
// Unset fields panic — tests that call unconfigured methods fail loudly
// rather than silently returning zero values. Exceptions: Capabilities and
// ContextUsageFromEvent default to zero-value answers, because both are on
// the unconditional onRawEvent path and a panic default would break every
// SSE test that doesn't configure them.
type mockAdapter struct {
	getSessionFn        func(ctx context.Context, userID, workspaceID, sessionID string) (*session.Session, error)
	createSessionFn     func(ctx context.Context, userID, workspaceID, title string) (*session.Session, error)
	listSessionsFn      func(ctx context.Context, userID, workspaceID string) ([]session.Session, error)
	renameSessionFn     func(ctx context.Context, userID, workspaceID, sessionID, title string) error
	deleteSessionFn     func(ctx context.Context, userID, workspaceID, sessionID string) error
	sendFn              func(ctx context.Context, userID, workspaceID, sessionID, text string, opts session.SendOpts) (*session.Message, error)
	sendAsyncFn         func(ctx context.Context, userID, workspaceID, sessionID, text string, opts session.SendOpts) (string, error)
	abortFn             func(ctx context.Context, userID, workspaceID, sessionID string) error
	listPendingFn       func(ctx context.Context, userID, workspaceID, sessionID string) ([]session.InputRequest, error)
	resolveFn           func(ctx context.Context, userID, workspaceID, requestID, reply string) error
	getHistoryFn        func(ctx context.Context, userID, workspaceID, sessionID string) ([]session.Message, error)
	formatProviderCfgFn func(providers []agent.LLMProviderData) ([]byte, error)
	validateCredsFn     func(rawConfig []byte) (*agent.CredentialCheckResult, error)
	contextUsageFn      func(eventType string, rawData string) (string, *session.ContextUsage, bool)
}

func (m *mockAdapter) CreateSession(ctx context.Context, uid, wid, title string) (*session.Session, error) {
	if m.createSessionFn != nil {
		return m.createSessionFn(ctx, uid, wid, title)
	}
	panic("mockAdapter.CreateSession not configured")
}
func (m *mockAdapter) GetSession(ctx context.Context, uid, wid, sid string) (*session.Session, error) {
	if m.getSessionFn != nil {
		return m.getSessionFn(ctx, uid, wid, sid)
	}
	panic("mockAdapter.GetSession not configured")
}
func (m *mockAdapter) ListSessions(ctx context.Context, uid, wid string) ([]session.Session, error) {
	if m.listSessionsFn != nil {
		return m.listSessionsFn(ctx, uid, wid)
	}
	panic("mockAdapter.ListSessions not configured")
}
func (m *mockAdapter) RenameSession(ctx context.Context, uid, wid, sid, title string) error {
	if m.renameSessionFn != nil {
		return m.renameSessionFn(ctx, uid, wid, sid, title)
	}
	panic("mockAdapter.RenameSession not configured")
}
func (m *mockAdapter) DeleteSession(ctx context.Context, uid, wid, sid string) error {
	if m.deleteSessionFn != nil {
		return m.deleteSessionFn(ctx, uid, wid, sid)
	}
	panic("mockAdapter.DeleteSession not configured")
}
func (m *mockAdapter) Send(ctx context.Context, uid, wid, sid, text string, opts session.SendOpts) (*session.Message, error) {
	if m.sendFn != nil {
		return m.sendFn(ctx, uid, wid, sid, text, opts)
	}
	panic("mockAdapter.Send not configured")
}
func (m *mockAdapter) SendAsync(ctx context.Context, uid, wid, sid, text string, opts session.SendOpts) (string, error) {
	if m.sendAsyncFn != nil {
		return m.sendAsyncFn(ctx, uid, wid, sid, text, opts)
	}
	panic("mockAdapter.SendAsync not configured")
}
func (m *mockAdapter) Abort(ctx context.Context, uid, wid, sid string) error {
	if m.abortFn != nil {
		return m.abortFn(ctx, uid, wid, sid)
	}
	panic("mockAdapter.Abort not configured")
}
func (m *mockAdapter) GetHistory(ctx context.Context, uid, wid, sid string) ([]session.Message, error) {
	if m.getHistoryFn != nil {
		return m.getHistoryFn(ctx, uid, wid, sid)
	}
	panic("mockAdapter.GetHistory not configured")
}
func (m *mockAdapter) Stream(_ context.Context, _, _, _ string) (<-chan session.Event, error) {
	panic("mockAdapter.Stream not configured")
}
func (m *mockAdapter) ListPending(ctx context.Context, uid, wid, sid string) ([]session.InputRequest, error) {
	if m.listPendingFn != nil {
		return m.listPendingFn(ctx, uid, wid, sid)
	}
	panic("mockAdapter.ListPending not configured")
}
func (m *mockAdapter) Resolve(ctx context.Context, uid, wid, rid, reply string) error {
	if m.resolveFn != nil {
		return m.resolveFn(ctx, uid, wid, rid, reply)
	}
	panic("mockAdapter.Resolve not configured")
}
func (m *mockAdapter) ListAvailableModels(_ context.Context, _, _ string) ([]session.ModelInfo, error) {
	panic("mockAdapter.ListAvailableModels not configured")
}
func (m *mockAdapter) SetModel(_ context.Context, _, _, _ string, _ session.ModelRef) error {
	panic("mockAdapter.SetModel not configured")
}
func (m *mockAdapter) ContextUsageFromEvent(eventType string, rawData string) (string, *session.ContextUsage, bool) {
	if m.contextUsageFn != nil {
		return m.contextUsageFn(eventType, rawData)
	}
	return "", nil, false
}

func (m *mockAdapter) MeteringFromEvent(_ string, _ []byte) (*agent.SessionUsage, bool, error) {
	return nil, false, nil
}

// newUsageStubAdapter returns an adapter stub whose ContextUsageFromEvent
// answers with fixed values keyed by sessionID substring in the raw payload.
// Handler-level wiring tests use it; the real wire→usage translation (both
// opencode shapes and its math) is pinned in pkg/agent/opencode tests.
func newUsageStubAdapter(mapping map[string]int64) *mockAdapter {
	return &mockAdapter{contextUsageFn: func(eventType string, rawData string) (string, *session.ContextUsage, bool) {
		for sid, used := range mapping {
			if strings.Contains(string(rawData), sid) {
				return sid, &session.ContextUsage{Used: used}, true
			}
		}
		return "", nil, false
	}}
}
func (m *mockAdapter) Capabilities() []session.Capability { return nil }
func (m *mockAdapter) FormatProviderConfig(p []agent.LLMProviderData) ([]byte, error) {
	if m.formatProviderCfgFn != nil {
		return m.formatProviderCfgFn(p)
	}
	panic("mockAdapter.FormatProviderConfig not configured")
}
func (m *mockAdapter) ValidateCredentials(raw []byte) (*agent.CredentialCheckResult, error) {
	if m.validateCredsFn != nil {
		return m.validateCredsFn(raw)
	}
	panic("mockAdapter.ValidateCredentials not configured")
}
