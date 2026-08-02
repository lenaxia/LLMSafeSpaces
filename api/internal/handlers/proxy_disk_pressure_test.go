// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/msgqueue"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
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

// --- injectDiskPressureNotice tests ---

func TestInjectDiskPressureNotice_Warning_PrependsNoticePart(t *testing.T) {
	body := []byte(`{"parts":[{"type":"text","text":"hi"}]}`)
	out := injectDiskPressureNotice(body, 0.90)
	require.True(t, len(out) > 0, "injection must produce a body")

	var parsed promptRequestBody
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Len(t, parsed.Parts, 2, "notice part + original part")
	assert.Equal(t, "text", parsed.Parts[0].Type)
	assert.Contains(t, parsed.Parts[0].Text, "90%")
	assert.Equal(t, "hi", parsed.Parts[1].Text, "original message part must be preserved")
}

func TestInjectDiskPressureNotice_Critical_UsesStrongerText(t *testing.T) {
	body := []byte(`{"parts":[{"type":"text","text":"hi"}]}`)
	out := injectDiskPressureNotice(body, 0.95)

	var parsed promptRequestBody
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Len(t, parsed.Parts, 2)
	assert.Contains(t, parsed.Parts[0].Text, "95%")
	assert.Contains(t, parsed.Parts[0].Text, "last resort")
}

func TestInjectDiskPressureNotice_BelowWarning_Unchanged(t *testing.T) {
	body := []byte(`{"parts":[{"type":"text","text":"hi"}]}`)
	out := injectDiskPressureNotice(body, 0.85)
	assert.Equal(t, body, out, "below 90% the body must pass through byte-identical")
}

func TestInjectDiskPressureNotice_ExactlyAtWarningThreshold_Injects(t *testing.T) {
	body := []byte(`{"parts":[{"type":"text","text":"hi"}]}`)
	out := injectDiskPressureNotice(body, 0.90)
	assert.NotEqual(t, body, out)
}

func TestInjectDiskPressureNotice_EmptyBody_Unchanged(t *testing.T) {
	body := []byte{}
	out := injectDiskPressureNotice(body, 0.99)
	assert.Equal(t, body, out, "empty body must pass through unchanged (fail-open)")
}

func TestInjectDiskPressureNotice_MalformedBody_Unchanged(t *testing.T) {
	body := []byte(`not-json`)
	out := injectDiskPressureNotice(body, 0.99)
	assert.Equal(t, body, out, "malformed body must pass through unchanged (fail-open)")
}

func TestInjectDiskPressureNotice_PreservesMessageID(t *testing.T) {
	body := []byte(`{"parts":[{"type":"text","text":"hi"}],"messageID":"msg_123"}`)
	out := injectDiskPressureNotice(body, 0.95)

	var parsed struct {
		Parts     []promptPart `json:"parts"`
		MessageID string       `json:"messageID"`
	}
	require.NoError(t, json.Unmarshal(out, &parsed))
	assert.Equal(t, "msg_123", parsed.MessageID, "caller-supplied messageID must survive injection")
	require.Len(t, parsed.Parts, 2)
}

func TestInjectDiskPressureNotice_PreservesNonTextParts(t *testing.T) {
	body := []byte(`{"parts":[{"type":"text","text":"hi"},{"type":"tool","tool":"x"}]}`)
	out := injectDiskPressureNotice(body, 0.95)

	var parsed promptRequestBody
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Len(t, parsed.Parts, 3, "notice + text part + tool part")
	assert.Equal(t, "tool", parsed.Parts[2].Type, "tool part must be preserved after the injected notice")
}

// Regression: the frontend sends a `model` field on /prompt when the user
// picked a specific model (frontend/src/hooks/useChatStream.ts). The
// injector must NOT drop unknown top-level fields — it rewrites only
// `parts`. See PR #632 review: silently dropping `model` routed the
// message to opencode's default model whenever disk pressure was active.
func TestInjectDiskPressureNotice_PreservesUnknownTopLevelFields(t *testing.T) {
	body := []byte(`{"parts":[{"type":"text","text":"hi"}],"model":{"providerID":"openai","modelID":"gpt-4"}}`)
	out := injectDiskPressureNotice(body, 0.95)

	var parsed struct {
		Parts []promptPart `json:"parts"`
		Model struct {
			ProviderID string `json:"providerID"`
			ModelID    string `json:"modelID"`
		} `json:"model"`
		MessageID string `json:"messageID"`
	}
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Len(t, parsed.Parts, 2, "notice + original part")
	assert.Equal(t, "openai", parsed.Model.ProviderID, "model.providerID must survive injection")
	assert.Equal(t, "gpt-4", parsed.Model.ModelID, "model.modelID must survive injection")
}

// Regression: multiple unknown sibling fields must all survive, not just
// the first. Guards against a partial-preservation regression.
func TestInjectDiskPressureNotice_PreservesMultipleUnknownFields(t *testing.T) {
	body := []byte(`{"parts":[{"type":"text","text":"hi"}],"mode":"build","model":{"p":"x","m":"y"},"custom":123}`)
	out := injectDiskPressureNotice(body, 0.90)

	var parsed map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out, &parsed))
	assert.Contains(t, parsed, "parts")
	assert.Contains(t, parsed, "mode", "mode field must survive")
	assert.Contains(t, parsed, "model", "model field must survive")
	assert.Contains(t, parsed, "custom", "custom field must survive")
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

// --- normalizeDiskThresholds cross-validation ---

func TestNormalizeDiskThresholds_Inverted_FallsBackToDefaults(t *testing.T) {
	// warning >= critical makes the warning tier unreachable (critical is
	// checked first). Fall back to defaults so the feature degrades to the
	// documented 90%/95% behavior.
	w, c := normalizeDiskThresholds(0.98, 0.50, 0.90, 0.95)
	assert.Equal(t, 0.90, w)
	assert.Equal(t, 0.95, c)
}

func TestNormalizeDiskThresholds_Equal_FallsBackToDefaults(t *testing.T) {
	w, c := normalizeDiskThresholds(0.92, 0.92, 0.90, 0.95)
	assert.Equal(t, 0.90, w)
	assert.Equal(t, 0.95, c)
}

func TestNormalizeDiskThresholds_Valid_Unchanged(t *testing.T) {
	w, c := normalizeDiskThresholds(0.85, 0.93, 0.90, 0.95)
	assert.Equal(t, 0.85, w)
	assert.Equal(t, 0.93, c)
}

// --- proxy integration: the injection must reach the upstream request ---

// setupWorkspaceWithDiskT registers a workspace CRD whose status carries
// the given disk usage (Phase=Active + PodIP come from makeWorkspaceCRD
// defaults). Registering this expectation alone (no pod-setup call) makes
// the proxy's single CRD fetch return it.
func (e *testEnv) setupWorkspaceWithDiskT(t *testing.T, name string, usedBytes, totalBytes int64) {
	t.Helper()
	ws := makeWorkspaceCRD(name, 5)
	ws.Status.DiskUsedBytes = usedBytes
	ws.Status.DiskTotalBytes = totalBytes
	e.wsMock.On("Get", mock.Anything, name, metav1.GetOptions{}).Return(ws, nil).Maybe()
}

// captureBackend returns a backend handler that records the request body
// bytes and responds with the standard opencode message shape.
func captureBackend(captured *[]byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*captured = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(opencodeMessageBody))
	}
}

func TestProxy_DiskPressureWarning_InjectsNoticeIntoUpstream(t *testing.T) {
	var captured []byte
	env := newTestEnvWithBackend(t, captureBackend(&captured))
	env.setupWorkspaceWithDiskT(t, "ws-1", 900, 1000) // exactly 90%
	env.setupPasswordWithT(t, "ws-1", "test-password")

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/ses_1/message",
		strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`))
	assert.Equal(t, http.StatusOK, w.Code)

	var sent promptRequestBody
	require.NoError(t, json.Unmarshal(captured, &sent))
	require.Len(t, sent.Parts, 2, "notice part must be prepended to the upstream message")
	assert.Contains(t, sent.Parts[0].Text, "90%", "warning notice must state the usage")
	assert.Equal(t, "hi", sent.Parts[1].Text, "original user part must be preserved")
}

func TestProxy_DiskPressureCritical_InjectsCriticalNoticeIntoUpstream(t *testing.T) {
	var captured []byte
	env := newTestEnvWithBackend(t, captureBackend(&captured))
	env.setupWorkspaceWithDiskT(t, "ws-1", 950, 1000) // exactly 95%
	env.setupPasswordWithT(t, "ws-1", "test-password")

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/ses_1/message",
		strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`))
	assert.Equal(t, http.StatusOK, w.Code)

	var sent promptRequestBody
	require.NoError(t, json.Unmarshal(captured, &sent))
	require.Len(t, sent.Parts, 2)
	assert.Contains(t, sent.Parts[0].Text, "95%")
	assert.Contains(t, sent.Parts[0].Text, "last resort", "critical notice must carry the logs-last guidance")
}

func TestProxy_DiskPressureBelowWarning_PassesBodyThroughUnchanged(t *testing.T) {
	var captured []byte
	env := newTestEnvWithBackend(t, captureBackend(&captured))
	env.setupWorkspaceWithDiskT(t, "ws-1", 890, 1000) // 89% — below warning
	env.setupPasswordWithT(t, "ws-1", "test-password")

	body := `{"parts":[{"type":"text","text":"hi"}]}`
	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/ses_1/message",
		strings.NewReader(body))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, body, string(captured), "below 90% the upstream body must be byte-identical")
}

func TestProxy_DiskPressure_NotInjectedWhenDiskUnknown(t *testing.T) {
	// DiskTotalBytes == 0 (controller has not scraped yet) must NOT trip
	// the injection — fail-open.
	var captured []byte
	env := newTestEnvWithBackend(t, captureBackend(&captured))
	env.setupWorkspaceWithDiskT(t, "ws-1", 0, 0)
	env.setupPasswordWithT(t, "ws-1", "test-password")

	body := `{"parts":[{"type":"text","text":"hi"}]}`
	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/ses_1/message",
		strings.NewReader(body))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, body, string(captured), "unknown disk state must pass through unchanged")
}

func TestProxy_DiskPressure_InjectsIntoPromptAsync(t *testing.T) {
	var captured []byte
	env := newTestEnvWithBackend(t, captureBackend(&captured))
	env.setupWorkspaceWithDiskT(t, "ws-1", 960, 1000) // 96% — critical
	env.setupPasswordWithT(t, "ws-1", "test-password")

	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions/ses_1/prompt",
		strings.NewReader(`{"parts":[{"type":"text","text":"hi"}]}`))
	assert.Equal(t, http.StatusOK, w.Code)

	var sent promptRequestBody
	require.NoError(t, json.Unmarshal(captured, &sent))
	require.Len(t, sent.Parts, 2, "prompt_async must receive the injection too")
	assert.Contains(t, sent.Parts[0].Text, "critically low")
}

func TestProxy_DiskPressure_NotInjectedOnNonPromptPath(t *testing.T) {
	// POST /session (session creation) carries a body but is not LLM-bound —
	// the injection gate must leave it alone even at 99% usage.
	var captured []byte
	env := newTestEnvWithBackend(t, captureBackend(&captured))
	env.setupWorkspaceWithDiskT(t, "ws-1", 990, 1000)
	env.setupPasswordWithT(t, "ws-1", "test-password")

	body := `{"parts":[{"type":"text","text":"hi"}]}`
	w := env.doRequestWithT(t, "POST", "/api/v1/workspaces/ws-1/sessions",
		strings.NewReader(body))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, body, string(captured), "non-LLM-bound paths must never receive the injection")
}

// --- queue-drain path parity ---

func TestDrainQueuedMessage_DiskPressureCritical_InjectsNotice(t *testing.T) {
	var captured []byte
	backend := httptestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = body
		w.WriteHeader(http.StatusNoContent)
	})

	transport := &redirectTransport{server: backend}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	k8sMock := newMockK8sWithDisk(t, "ws-1", "10.0.0.1", 950, 1000)
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	svc := msgqueue.NewWithClient(client)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()
	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	_, err = svc.Enqueue(context.Background(), "ws-1", "ses-1", "queued msg")
	require.NoError(t, err)

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	go handler.drainQueuedMessage("ws-1", "ses-1")

	require.Eventually(t, func() bool {
		select {
		case evt := <-sub.Ch:
			if evt.Type != "queue.update" {
				return false
			}
			data, ok := evt.Data.(queueUpdateData)
			return ok && data.Event == "sent"
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond, "should publish queue.update with event=sent")

	var sent promptRequestBody
	require.NoError(t, json.Unmarshal(captured, &sent))
	require.Len(t, sent.Parts, 2, "queued message must receive the injection")
	assert.Contains(t, sent.Parts[0].Text, "95%")
	assert.Contains(t, sent.Parts[0].Text, "last resort")
	assert.Equal(t, "queued msg", sent.Parts[1].Text, "the queued text must be preserved")
}

func TestDrainQueuedMessage_DiskBelowWarning_NoInjection(t *testing.T) {
	var captured []byte
	backend := httptestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = body
		w.WriteHeader(http.StatusNoContent)
	})

	transport := &redirectTransport{server: backend}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	k8sMock := newMockK8sWithDisk(t, "ws-1", "10.0.0.1", 800, 1000) // 80%
	handler, err := NewProxyHandler(k8sMock, &testLogger{}, "default", httpClient, nil)
	require.NoError(t, err)

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	svc := msgqueue.NewWithClient(client)
	handler.SetMessageQueueService(svc)
	handler.userBroker = eventbroker.NewUserEventBroker()
	setupPasswordSecret(t, handler, "ws-1", "test-pw")

	_, err = svc.Enqueue(context.Background(), "ws-1", "ses-1", "queued msg")
	require.NoError(t, err)

	sub, _ := handler.userBroker.SubscribeWorkspace("ws-1")
	defer handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	go handler.drainQueuedMessage("ws-1", "ses-1")

	require.Eventually(t, func() bool {
		select {
		case evt := <-sub.Ch:
			if evt.Type != "queue.update" {
				return false
			}
			data, ok := evt.Data.(queueUpdateData)
			return ok && data.Event == "sent"
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond, "should publish queue.update with event=sent")

	var sent promptRequestBody
	require.NoError(t, json.Unmarshal(captured, &sent))
	require.Len(t, sent.Parts, 1, "below 90% no notice part may be injected")
	assert.Equal(t, "queued msg", sent.Parts[0].Text)
}

// newMockK8sWithDisk returns a mock k8s client whose workspace CRD carries
// the given disk usage in status. Used by the queue-drain integration tests
// (sendQueuedToOpencode reads disk via workspaceDiskRatio).
func newMockK8sWithDisk(t *testing.T, workspaceID, podIP string, usedBytes, totalBytes int64) *k8smocks.MockKubernetesClient {
	t.Helper()
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil).Maybe()
	llmMock.On("Workspaces", "default").Return(wsMock).Maybe()
	ws := makeWorkspaceCRDWithStatus(workspaceID, podIP, string(v1.WorkspacePhaseActive), workspaceID)
	ws.Status.DiskUsedBytes = usedBytes
	ws.Status.DiskTotalBytes = totalBytes
	wsMock.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(ws, nil).Maybe()
	fakeClientset := k8sfake.NewSimpleClientset()
	k8sMock.On("Clientset").Return(fakeClientset).Maybe()
	return k8sMock
}

// httptestServer wraps httptest.NewServer with cleanup registration.
func httptestServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}
