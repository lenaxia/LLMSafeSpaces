// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package systemnotices

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// fakeAdapter records Send/SendAsync plus every method exercised by
// the delegation test. Unexported extra methods come from the embedded
// nil interface — never called by these tests.
type fakeAdapter struct {
	agent.Adapter
	sendText    string
	sendOpts    session.SendOpts
	asyncText   string
	asyncOpts   session.SendOpts
	otherCalled map[string]int
}

func newFakeAdapter() *fakeAdapter {
	return &fakeAdapter{otherCalled: map[string]int{}}
}

func (f *fakeAdapter) CreateSession(ctx context.Context, userID, workspaceID, title string) (*session.Session, error) {
	f.otherCalled["CreateSession"]++
	return nil, nil
}
func (f *fakeAdapter) GetSession(ctx context.Context, userID, workspaceID, sessionID string) (*session.Session, error) {
	f.otherCalled["GetSession"]++
	return nil, nil
}
func (f *fakeAdapter) ListSessions(ctx context.Context, userID, workspaceID string) ([]session.Session, error) {
	f.otherCalled["ListSessions"]++
	return nil, nil
}
func (f *fakeAdapter) RenameSession(ctx context.Context, userID, workspaceID, sessionID, title string) error {
	f.otherCalled["RenameSession"]++
	return nil
}
func (f *fakeAdapter) DeleteSession(ctx context.Context, userID, workspaceID, sessionID string) error {
	f.otherCalled["DeleteSession"]++
	return nil
}
func (f *fakeAdapter) Send(ctx context.Context, userID, workspaceID, sessionID, text string, opts session.SendOpts) (*session.Message, error) {
	f.sendText = text
	f.sendOpts = opts
	return &session.Message{ID: "msg_1"}, nil
}
func (f *fakeAdapter) SendAsync(ctx context.Context, userID, workspaceID, sessionID, text string, opts session.SendOpts) (string, error) {
	f.asyncText = text
	f.asyncOpts = opts
	return "msg_1", nil
}

func (f *fakeAdapter) SetSessionModel(_ context.Context, _, _, _ string, _ *session.ModelRef) error {
	return nil
}
func (f *fakeAdapter) VerifyDelivery(_ context.Context, _, _, _, _ string, _ time.Time) (bool, bool, error) {
	return false, false, nil // inconclusive: test fake without transcript verification
}

func (f *fakeAdapter) Abort(ctx context.Context, userID, workspaceID, sessionID string) error {
	f.otherCalled["Abort"]++
	return nil
}
func (f *fakeAdapter) GetHistory(ctx context.Context, userID, workspaceID, sessionID string) ([]session.Message, error) {
	f.otherCalled["GetHistory"]++
	return nil, nil
}
func (f *fakeAdapter) GetHistoryPage(ctx context.Context, userID, workspaceID, sessionID string, limit int) ([]session.Message, error) {
	f.otherCalled["GetHistoryPage"]++
	return nil, nil
}
func (f *fakeAdapter) Stream(ctx context.Context, userID, workspaceID, sessionID string) (<-chan session.Event, error) {
	f.otherCalled["Stream"]++
	return nil, nil
}
func (f *fakeAdapter) ListPending(ctx context.Context, userID, workspaceID, sessionID string) ([]session.InputRequest, error) {
	f.otherCalled["ListPending"]++
	return nil, nil
}
func (f *fakeAdapter) Resolve(ctx context.Context, userID, workspaceID, requestID, reply string) error {
	f.otherCalled["Resolve"]++
	return nil
}
func (f *fakeAdapter) Capabilities() []session.Capability {
	f.otherCalled["Capabilities"]++
	return nil
}

var _ agent.Adapter = (*fakeAdapter)(nil)

// fakeUsage serves fixed disk numbers.
type fakeUsage struct {
	used, total int64
	err         error
	calls       int
}

func (f *fakeUsage) DiskUsage(_ context.Context, _ string) (int64, int64, error) {
	f.calls++
	return f.used, f.total, f.err
}

var _ WorkspaceDiskUsage = (*fakeUsage)(nil)

// --- tiers + wording ---

func TestNotice_Tiers(t *testing.T) {
	assert.Equal(t, "", Notice(LevelNone, 0.5))
	assert.Contains(t, Notice(LevelWarning, 0.90), "90%")
	assert.Contains(t, Notice(LevelWarning, 0.90), "Do not delete anything yourself")
	assert.Contains(t, Notice(LevelCritical, 0.95), "95%")
	assert.Contains(t, Notice(LevelCritical, 0.95), "critically low")
	assert.Contains(t, Notice(LevelCritical, 0.95), "Never delete anything without the user's explicit approval")
}

// TestNotice_FlooredPercent pins the display floor: a ratio below the
// critical threshold must never display as the critical percentage.
func TestNotice_FlooredPercent(t *testing.T) {
	assert.Equal(t, "94%", pctString(0.9499999), "0.9499… is warning tier and must display 94%, not 95%")
}

func TestLevelForRatio(t *testing.T) {
	assert.Equal(t, LevelNone, LevelForRatio(0.0))
	assert.Equal(t, LevelNone, LevelForRatio(0.899))
	assert.Equal(t, LevelWarning, LevelForRatio(0.90))
	assert.Equal(t, LevelWarning, LevelForRatio(0.949))
	assert.Equal(t, LevelCritical, LevelForRatio(0.95))
	assert.Equal(t, LevelCritical, LevelForRatio(1.0))
}

// --- decorator ---

func TestWrap_Send_PrependsNoticeAtWarning(t *testing.T) {
	inner := newFakeAdapter()
	usage := &fakeUsage{used: 91, total: 100}
	decorated := Wrap(inner, usage)

	model := &session.ModelRef{ID: "glm-5.3", Provider: "thekaocloud"}
	_, err := decorated.Send(context.Background(), "u1", "ws-1", "ses_1", "hello", session.SendOpts{Model: model})
	require.NoError(t, err)

	require.True(t, strings.HasPrefix(inner.sendText, "System notice:"),
		"notice must be prepended, got: %q", inner.sendText)
	assert.Contains(t, inner.sendText, "hello", "original text must survive after the notice")
	assert.Equal(t, "glm-5.3", inner.sendOpts.Model.ID,
		"opts must pass through untouched")
}

func TestWrap_SendAsync_PrependsNoticeAtCritical(t *testing.T) {
	inner := newFakeAdapter()
	usage := &fakeUsage{used: 96, total: 100}
	decorated := Wrap(inner, usage)

	_, err := decorated.SendAsync(context.Background(), "u1", "ws-1", "ses_1", "hello", session.SendOpts{})
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(inner.asyncText, "System notice:"))
	assert.Contains(t, inner.asyncText, "critically low")
	assert.Contains(t, inner.asyncText, "hello")
}

func TestWrap_BelowThreshold_NoInjection(t *testing.T) {
	inner := newFakeAdapter()
	decorated := Wrap(inner, &fakeUsage{used: 50, total: 100})

	_, err := decorated.Send(context.Background(), "u1", "ws-1", "ses_1", "hello", session.SendOpts{})
	require.NoError(t, err)
	assert.Equal(t, "hello", inner.sendText, "no notice below the warning threshold")
}

func TestWrap_UsageError_FailOpen(t *testing.T) {
	inner := newFakeAdapter()
	decorated := Wrap(inner, &fakeUsage{err: errors.New("crd read failed")})

	_, err := decorated.Send(context.Background(), "u1", "ws-1", "ses_1", "hello", session.SendOpts{})
	require.NoError(t, err)
	assert.Equal(t, "hello", inner.sendText, "usage read errors must pass the message through unchanged")
}

func TestWrap_ZeroTotal_FailOpen(t *testing.T) {
	inner := newFakeAdapter()
	decorated := Wrap(inner, &fakeUsage{used: 0, total: 0})

	_, err := decorated.Send(context.Background(), "u1", "ws-1", "ses_1", "hello", session.SendOpts{})
	require.NoError(t, err)
	assert.Equal(t, "hello", inner.sendText, "unknown total (not-yet-scraped workspace) must not trip the notice")
}

func TestWrap_NilUsage_Disabled(t *testing.T) {
	inner := newFakeAdapter()
	decorated := Wrap(inner, nil)

	_, err := decorated.Send(context.Background(), "u1", "ws-1", "ses_1", "hello", session.SendOpts{})
	require.NoError(t, err)
	assert.Equal(t, "hello", inner.sendText, "nil usage source disables injection entirely")
}

// TestWrap_NonMessageMethods_Untouched pins that the decorator only
// intercepts the message legs — everything else is a pure delegation and
// never touches the usage source.
func TestWrap_NonMessageMethods_Untouched(t *testing.T) {
	inner := newFakeAdapter()
	usage := &fakeUsage{used: 99, total: 100}
	decorated := Wrap(inner, usage)
	ctx := context.Background()

	_, _ = decorated.CreateSession(ctx, "u1", "ws-1", "t")
	_, _ = decorated.GetSession(ctx, "u1", "ws-1", "ses_1")
	_, _ = decorated.ListSessions(ctx, "u1", "ws-1")
	_ = decorated.RenameSession(ctx, "u1", "ws-1", "ses_1", "t")
	_ = decorated.DeleteSession(ctx, "u1", "ws-1", "ses_1")
	_ = decorated.Abort(ctx, "u1", "ws-1", "ses_1")
	_, _ = decorated.GetHistory(ctx, "u1", "ws-1", "ses_1")
	_, _ = decorated.GetHistoryPage(ctx, "u1", "ws-1", "ses_1", 50)
	_, _ = decorated.Stream(ctx, "u1", "ws-1", "ses_1")
	_, _ = decorated.ListPending(ctx, "u1", "ws-1", "ses_1")
	_ = decorated.Resolve(ctx, "u1", "ws-1", "que_1", "yes")

	assert.Equal(t, 0, usage.calls, "non-message methods must not read disk usage")
	for _, m := range []string{"CreateSession", "GetSession", "ListSessions", "RenameSession", "DeleteSession", "Abort", "GetHistory", "GetHistoryPage", "Stream", "ListPending", "Resolve"} {
		assert.Equal(t, 1, inner.otherCalled[m], "%s must delegate exactly once", m)
	}
}

// TestWrap_ImplementsAdapter pins the interface satisfaction at compile
// time — the decorator must stay substitutable wherever an Adapter is
// wired. (The explicit declaration form is deliberate; staticcheck's
// omit-type suggestion would defeat the assertion's purpose.)
func TestWrap_ImplementsAdapter(t *testing.T) {
	decorated := Wrap(newFakeAdapter(), nil)
	_, err := decorated.SendAsync(context.Background(), "u1", "ws-1", "ses_1", "x", session.SendOpts{})
	require.NoError(t, err, "wrapped adapter must remain callable through the interface")
}

// --- threshold normalization + env parsing (moved from the proxy's
// disk-pressure tests — this package is the single source since #944) ---

func TestNormalizeThresholds_Inverted_FallsBackToDefaults(t *testing.T) {
	// warning >= critical makes the warning tier unreachable (critical is
	// checked first). Fall back to defaults so the feature degrades to the
	// documented 90%/95% behavior.
	w, c := normalizeThresholds(0.98, 0.50, 0.90, 0.95)
	assert.Equal(t, 0.90, w)
	assert.Equal(t, 0.95, c)
}

func TestNormalizeThresholds_Equal_FallsBackToDefaults(t *testing.T) {
	w, c := normalizeThresholds(0.92, 0.92, 0.90, 0.95)
	assert.Equal(t, 0.90, w)
	assert.Equal(t, 0.95, c)
}

func TestNormalizeThresholds_Valid_Unchanged(t *testing.T) {
	w, c := normalizeThresholds(0.85, 0.93, 0.90, 0.95)
	assert.Equal(t, 0.85, w)
	assert.Equal(t, 0.93, c)
}

// TestEnvFloatOr exercises the env-override parsing path. Package vars
// initialize at load time, so the full override path can't be tested
// without process restart; the parsing primitive is the only logic.
func TestEnvFloatOr(t *testing.T) {
	t.Setenv("LSS_TEST_FLOAT", "0.77")
	assert.InDelta(t, 0.77, envFloatOr("LSS_TEST_FLOAT", 0.90), 1e-9)

	t.Setenv("LSS_TEST_FLOAT", "")
	assert.InDelta(t, 0.90, envFloatOr("LSS_TEST_FLOAT", 0.90), 1e-9)

	t.Setenv("LSS_TEST_FLOAT", "not-a-number")
	assert.InDelta(t, 0.90, envFloatOr("LSS_TEST_FLOAT", 0.90), 1e-9)

	for _, bad := range []string{"0", "1", "-0.5", "1.5", "2"} {
		t.Run(bad, func(t *testing.T) {
			t.Setenv("LSS_TEST_FLOAT", bad)
			assert.InDelta(t, 0.90, envFloatOr("LSS_TEST_FLOAT", 0.90), 1e-9,
				"value %s must be rejected", bad)
		})
	}

	t.Setenv("LSS_TEST_FLOAT", "0.001")
	assert.InDelta(t, 0.001, envFloatOr("LSS_TEST_FLOAT", 0.90), 1e-9)
	t.Setenv("LSS_TEST_FLOAT", "0.999")
	assert.InDelta(t, 0.999, envFloatOr("LSS_TEST_FLOAT", 0.90), 1e-9)
}

// TestThresholdsDefaults pins the active tier boundaries (env unset).
func TestThresholdsDefaults(t *testing.T) {
	w, c := Thresholds()
	assert.InDelta(t, 0.90, w, 1e-9)
	assert.InDelta(t, 0.95, c, 1e-9)
}
