// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package email

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/email"
)

// fakeProvider captures the last message handed to Send. Returns the
// configured error so tests can exercise send-failure paths.
type fakeProvider struct {
	last    *email.Message
	sendErr error
}

func (f *fakeProvider) Send(_ context.Context, msg email.Message) error {
	f.last = &msg
	return f.sendErr
}

func newSvc() (*Service, *fakeProvider) {
	fp := &fakeProvider{}
	return NewService(fp, "https://app.test", "ses"), fp
}

func TestNewService_NilProviderIsSafeToSendTest(t *testing.T) {
	// A nil provider means "email not configured". SendTest must not panic;
	// it returns ErrNotConfigured so the handler can report provider=noop.
	svc := NewService(nil, "https://app.test", "")
	err := svc.SendTest(context.Background(), "ops@test.com")
	assert.ErrorIs(t, err, ErrNotConfigured)
}

func TestProviderName(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{"empty defaults to noop", "", "noop"},
		{"explicit noop", "noop", "noop"},
		{"ses", "ses", "ses"},
		{"unknown normalised to noop", "smtp", "noop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewService(&fakeProvider{}, "https://app.test", tt.input)
			assert.Equal(t, tt.expect, svc.ProviderName())
		})
	}
}

func TestSendTest_BuildsMessageAndSends(t *testing.T) {
	svc, fp := newSvc()
	err := svc.SendTest(context.Background(), "ops@test.com")
	require.NoError(t, err)
	require.NotNil(t, fp.last)
	assert.Equal(t, "ops@test.com", fp.last.To)
	assert.Contains(t, fp.last.Subject, "LLMSafeSpaces")
	assert.NotEmpty(t, fp.last.TextBody)
	assert.NotEmpty(t, fp.last.HTMLBody)
}

func TestSend_PropagatesProviderError(t *testing.T) {
	fp := &fakeProvider{sendErr: &testErr{msg: "ses down"}}
	svc := NewService(fp, "https://app.test", "ses")
	err := svc.SendTest(context.Background(), "ops@test.com")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ses down")
}

func TestSendPasswordReset_NilProvider_ErrNotConfigured(t *testing.T) {
	svc := NewService(nil, "https://app.test", "ses")
	err := svc.SendPasswordReset(context.Background(), "alice@test.com", "tok")
	assert.ErrorIs(t, err, ErrNotConfigured)
}

func TestSendPasswordChanged_NilProvider_ErrNotConfigured(t *testing.T) {
	svc := NewService(nil, "https://app.test", "ses")
	err := svc.SendPasswordChanged(context.Background(), "alice@test.com")
	assert.ErrorIs(t, err, ErrNotConfigured)
}

func TestSendEmailVerification_NilProvider_ErrNotConfigured(t *testing.T) {
	svc := NewService(nil, "https://app.test", "ses")
	err := svc.SendEmailVerification(context.Background(), "bob@test.com", "tok")
	assert.ErrorIs(t, err, ErrNotConfigured)
}

func TestSendEmailVerification_ExplainsExpiry(t *testing.T) {
	svc, fp := newSvc()
	err := svc.SendEmailVerification(context.Background(), "bob@test.com", "tok")
	require.NoError(t, err)
	assert.Contains(t, fp.last.TextBody, "24 hours", "verification email must state 24h expiry")
}

func TestSendEmailVerification_BuildsCorrectLink(t *testing.T) {
	svc, fp := newSvc()
	token := "xyz789verify"
	err := svc.SendEmailVerification(context.Background(), "bob@test.com", token)
	require.NoError(t, err)
	require.NotNil(t, fp.last)
	assert.Equal(t, "bob@test.com", fp.last.To)
	assert.Contains(t, fp.last.TextBody, "https://app.test/verify-email?token="+token)
	assert.Contains(t, fp.last.HTMLBody, "https://app.test/verify-email?token="+token)
}

func TestSendPasswordReset_BuildsCorrectLink(t *testing.T) {
	svc, fp := newSvc()
	token := "abc123reset"
	err := svc.SendPasswordReset(context.Background(), "alice@test.com", token)
	require.NoError(t, err)
	require.NotNil(t, fp.last)
	assert.Equal(t, "alice@test.com", fp.last.To)
	assert.Contains(t, fp.last.TextBody, "https://app.test/reset-password?token="+token)
	assert.Contains(t, fp.last.HTMLBody, "https://app.test/reset-password?token="+token)
}

func TestSendPasswordReset_ExplainsExpiry(t *testing.T) {
	svc, fp := newSvc()
	err := svc.SendPasswordReset(context.Background(), "alice@test.com", "tok")
	require.NoError(t, err)
	assert.Contains(t, fp.last.TextBody, "15 minutes")
}

func TestSendPasswordChanged_BuildsNotification(t *testing.T) {
	svc, fp := newSvc()
	err := svc.SendPasswordChanged(context.Background(), "alice@test.com")
	require.NoError(t, err)
	require.NotNil(t, fp.last)
	assert.Contains(t, fp.last.Subject, "password")
	assert.Contains(t, strings.ToLower(fp.last.TextBody), "if this was not you")
}

func TestBuildLink_TrimsTrailingSlash(t *testing.T) {
	svc := NewService(&fakeProvider{}, "https://app.test///", "ses")
	link := svc.buildLink("/reset-password", "tok")
	assert.Equal(t, "https://app.test/reset-password?token=tok", link)
}

func TestHTMLBody_EscapesDynamicValues(t *testing.T) {
	svc, fp := newSvc()
	err := svc.SendPasswordReset(context.Background(), "alice@test.com", "<script>alert(1)</script>")
	require.NoError(t, err)
	assert.NotContains(t, fp.last.HTMLBody, "<script>alert(1)</script>")
	assert.Contains(t, fp.last.HTMLBody, "%3Cscript%3E")
}

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }
