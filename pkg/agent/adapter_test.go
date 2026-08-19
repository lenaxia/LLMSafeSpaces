// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// US-65.3 Adapter interface — verify the shape pins correctly and
// satisfies the design 0049 §4.6 surface. The opencode implementation
// lives in pkg/agent/opencode/adapter.go (US-65.3 follow-up commit).

// fakeAdapter is a minimal in-memory implementation used to verify the
// Adapter interface is usable as documented.
type fakeAdapter struct{}

func (f *fakeAdapter) CreateSession(_ context.Context, _, _, _ string) (*session.Session, error) {
	return &session.Session{}, nil
}
func (f *fakeAdapter) GetSession(_ context.Context, _, _, _ string) (*session.Session, error) {
	return &session.Session{}, nil
}
func (f *fakeAdapter) ListSessions(_ context.Context, _, _ string) ([]session.Session, error) {
	return nil, nil
}
func (f *fakeAdapter) RenameSession(_ context.Context, _, _, _, _ string) error { return nil }
func (f *fakeAdapter) DeleteSession(_ context.Context, _, _, _ string) error    { return nil }
func (f *fakeAdapter) Send(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
	return &session.Message{}, nil
}
func (f *fakeAdapter) SendAsync(_ context.Context, _, _, _, _ string, _ session.SendOpts) (string, error) {
	return "msg_fake", nil
}
func (f *fakeAdapter) Abort(_ context.Context, _, _, _ string) error { return nil }
func (f *fakeAdapter) GetHistory(_ context.Context, _, _, _ string) ([]session.Message, error) {
	return nil, nil
}
func (f *fakeAdapter) Stream(_ context.Context, _, _, _ string) (<-chan session.Event, error) {
	return nil, nil
}
func (f *fakeAdapter) ListPending(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
	return nil, nil
}
func (f *fakeAdapter) Resolve(_ context.Context, _, _, _, _ string) error { return nil }
func (f *fakeAdapter) ListAvailableModels(_ context.Context, _, _ string) ([]session.ModelInfo, error) {
	return nil, nil
}
func (f *fakeAdapter) SetModel(_ context.Context, _, _, _ string, _ session.ModelRef) error {
	return nil
}
func (f *fakeAdapter) Capabilities() []session.Capability { return nil }
func (f *fakeAdapter) ContextUsageFromEvent(_ string, _ string) (string, *session.ContextUsage, bool) {
	return "", nil, false
}
func (f *fakeAdapter) FormatProviderConfig(_ []LLMProviderData) ([]byte, error) {
	return nil, nil
}
func (f *fakeAdapter) ValidateCredentials(_ []byte) (*CredentialCheckResult, error) {
	return &CredentialCheckResult{State: CredentialStatePresent}, nil
}

// Compile-time assertion: *fakeAdapter satisfies Adapter.
var _ Adapter = (*fakeAdapter)(nil)

func TestAdapter_InterfaceShape(t *testing.T) {
	// Platform code holds an Adapter (interface). The fake satisfies
	// it; the real opencode implementation must too. This test pins
	// the method set so adding a method to Adapter without updating
	// every implementation fails the build.
	ctx := context.Background()
	var a Adapter = &fakeAdapter{}

	if _, err := a.CreateSession(ctx, "u", "w", "title"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := a.Send(ctx, "u", "w", "s", "hi", session.SendOpts{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
}
