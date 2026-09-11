// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// --- diskPressureLevelForRatio boundary tests ---

func TestDiskPressureLevelForRatio_Boundaries(t *testing.T) {
	assert.Equal(t, diskPressureNone, diskPressureLevelForRatio(0.0))
	assert.Equal(t, diskPressureNone, diskPressureLevelForRatio(0.50))
	assert.Equal(t, diskPressureNone, diskPressureLevelForRatio(0.899999))
	assert.Equal(t, diskPressureWarning, diskPressureLevelForRatio(0.90), "90% exactly is a warning")
	assert.Equal(t, diskPressureWarning, diskPressureLevelForRatio(0.90+0.000001))
	assert.Equal(t, diskPressureWarning, diskPressureLevelForRatio(0.949999))
	assert.Equal(t, diskPressureCritical, diskPressureLevelForRatio(0.95), "95% exactly is critical")
	assert.Equal(t, diskPressureCritical, diskPressureLevelForRatio(1.0))
}

func TestDiskPressureLevelForRatio_NegativeRatio_IsNone(t *testing.T) {
	assert.Equal(t, diskPressureNone, diskPressureLevelForRatio(-0.1))
}

// --- diskPressureRatio tests ---

func TestDiskPressureRatio_Normal(t *testing.T) {
	assert.InDelta(t, 0.9, diskPressureRatio(900, 1000), 1e-9)
	assert.InDelta(t, 0.5, diskPressureRatio(500, 1000), 1e-9)
}

func TestDiskPressureRatio_ZeroTotal_IsZero(t *testing.T) {
	// A workspace whose disk has not been scraped yet (TotalBytes == 0)
	// must NOT trip the warning — ratio 0 is the fail-safe.
	assert.Equal(t, float64(0), diskPressureRatio(0, 0))
	assert.Equal(t, float64(0), diskPressureRatio(100, 0))
	assert.Equal(t, float64(0), diskPressureRatio(100, -5))
}

// --- diskPressureNotice text tests ---

func TestDiskPressureNotice_Warning_NudgesUser(t *testing.T) {
	notice := diskPressureNotice(diskPressureWarning, 0.90)
	assert.Contains(t, notice, "90%")
	assert.Contains(t, notice, "free up")
	// The warning must NOT authorize deletion — it is a nudge only. It may
	// say "do not delete", but must never grant permission like the critical
	// notice does ("delete ONLY ...").
	assert.NotContains(t, notice, "delete ONLY", "warning must not authorize deletion")
	assert.NotContains(t, notice, "last resort", "logs guidance belongs to the critical tier")
}

func TestDiskPressureNotice_Critical_GuidesSafeCleanup(t *testing.T) {
	notice := diskPressureNotice(diskPressureCritical, 0.95)
	assert.Contains(t, notice, "95%")
	assert.Contains(t, notice, "build artifacts", "must name build artifacts as safe-to-remove")
	assert.Contains(t, notice, "caches", "must name caches as safe-to-remove")
	assert.Contains(t, notice, "last resort", "logs must be framed as the last resort")
	assert.Contains(t, notice, "cannot be reproduced", "must explain why logs are the last resort")
	assert.Contains(t, notice, "approval", "must require user approval before deleting")
}

func TestDiskPressureNotice_WarningAndCritical_Distinct(t *testing.T) {
	w := diskPressureNotice(diskPressureWarning, 0.90)
	c := diskPressureNotice(diskPressureCritical, 0.95)
	assert.NotEqual(t, w, c, "the critical injection must be materially stronger")
}

// --- diskPressureNotice rounding boundary ---

// At ratio 0.949999 the level is warning (< 0.95) but math.Round would
// display "95%", which is confusing alongside warning-level guidance that
// grants no deletion authority. The display must not round up across a
// level boundary.
func TestDiskPressureNotice_Warning_DoesNotRoundUpToCriticalDisplay(t *testing.T) {
	notice := diskPressureNotice(diskPressureWarning, 0.949999)
	assert.NotContains(t, notice, "95%", "warning at 0.949999 must not display 95%")
	assert.Contains(t, notice, "94%", "should floor to the integer below")
}

func TestDiskPressureNotice_Critical_AtExactBoundary_Displays95(t *testing.T) {
	notice := diskPressureNotice(diskPressureCritical, 0.95)
	assert.Contains(t, notice, "95%")
}

// NOTE: the threshold-normalization and env-override parsing tests moved
// with their logic to pkg/agent/systemnotices (the single source since
// #944); see systemnotices_test.go.

// --- adapter-seam integration (#828 batch 1): the injection must reach
// the text the adapter sends ---

// setupWorkspaceWithDiskT registers a workspace CRD whose status carries
// the given disk usage (Phase=Active + PodIP come from makeWorkspaceCRD
// defaults). Registering this expectation alone (no pod-setup call) makes
// the handler's single CRD fetch return it.
func (e *testEnv) setupWorkspaceWithDiskT(t *testing.T, name string, usedBytes, totalBytes int64) {
	t.Helper()
	ws := makeWorkspaceCRD(name, 5)
	ws.Status.DiskUsedBytes = usedBytes
	ws.Status.DiskTotalBytes = totalBytes
	e.wsMock.On("Get", mock.Anything, name, metav1.GetOptions{}).Return(ws, nil).Maybe()
}

// sendTextCapture wires a mock adapter whose Send records the prompt text
// and succeeds with a minimal assistant message.
func sendTextCapture(sent *string) *mockAdapter {
	return &mockAdapter{
		sendFn: func(_ context.Context, _, _, _ string, text string, _ session.SendOpts) (*session.Message, error) {
			*sent = text
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
		},
	}
}

// --- adapter-path contract (#828 batch 1): the handler must NOT inject ---

// The single injection point since #944 is the noticingAdapter decorator
// (systemnotices.Wrap, wired in app.go), which covers every entrypoint.
// The handler-level body-rewrite that served the legacy raw-proxy path
// is deleted with that path; this row pins that SendMessage passes the
// prompt text VERBATIM to the adapter even at critical disk usage — if
// the handler ever re-learns injection, every production send would
// carry the notice twice (decorator + handler).
func TestSendMessage_DiskPressure_HandlerNeverInjects(t *testing.T) {
	var sent string
	env := newTestEnv(t)
	env.setupWorkspaceWithDiskT(t, "ws-1", 990, 1000) // 99% — critical
	env.handler.adapter = sendTextCapture(&sent)

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/ses_1/message",
		strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`))
	require.Equal(t, http.StatusOK, w.Code)

	assert.Equal(t, "hi", sent,
		"injection belongs to the noticingAdapter decorator; the handler must pass text verbatim")
}
