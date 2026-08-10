// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// mockAdapter is a test double for agent.Adapter. Each method is a
// configurable function field; tests set only the methods they exercise.
// Unset fields panic — tests that call unconfigured methods fail loudly
// rather than silently returning zero values.
type mockAdapter struct {
	getSessionFn        func(ctx context.Context, userID, workspaceID, sessionID string) (*session.Session, error)
	createSessionFn     func(ctx context.Context, userID, workspaceID, title string) (*session.Session, error)
	listSessionsFn      func(ctx context.Context, userID, workspaceID string) ([]session.Session, error)
	renameSessionFn     func(ctx context.Context, userID, workspaceID, sessionID, title string) error
	listPendingFn       func(ctx context.Context, userID, workspaceID, sessionID string) ([]session.InputRequest, error)
	resolveFn           func(ctx context.Context, userID, workspaceID, requestID, reply string) error
	getHistoryFn        func(ctx context.Context, userID, workspaceID, sessionID string) ([]session.Message, error)
	formatProviderCfgFn func(providers []agent.LLMProviderData) ([]byte, error)
	validateCredsFn     func(rawConfig []byte) (*agent.CredentialCheckResult, error)
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
func (m *mockAdapter) DeleteSession(_ context.Context, _, _, _ string) error {
	panic("mockAdapter.DeleteSession not configured")
}
func (m *mockAdapter) Send(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
	panic("mockAdapter.Send not configured")
}
func (m *mockAdapter) SendAsync(_ context.Context, _, _, _, _ string, _ session.SendOpts) (string, error) {
	panic("mockAdapter.SendAsync not configured")
}
func (m *mockAdapter) Abort(_ context.Context, _, _, _ string) error {
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
