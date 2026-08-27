// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

const (
	attachNotesPath   = "/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt"
	attachReportPath  = "/workspace/uploads/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee-report.pdf"
	attachNotesLine   = `[llmsafespaces:attachment path="/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt" name="notes.txt"]`
	wantComposedNotes = "review please\n\n" + attachNotesLine + "\n"
)

func postPromptFiles(t *testing.T, env *e2eEnv, body string) *httptest.ResponseRecorder {
	t.Helper()
	return env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/prompt", strings.NewReader(body))
}

func TestPrompt_Files_ComposedAndDispatched_AdapterPath(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	var gotText string
	var gotOpts session.SendOpts
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, text string, opts session.SendOpts) (*session.Message, error) {
			gotText = text
			gotOpts = opts
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"model":{"modelID":"glm-5.3","providerID":"thekaocloud"},"parts":[{"type":"text","text":"review please"}],"files":["`+attachNotesPath+`"]}`))

	h.SendPromptAsync(c)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, wantComposedNotes, gotText)
	require.NotNil(t, gotOpts.Model, "model passthrough must be unaffected by files composition")
	assert.Equal(t, "glm-5.3", gotOpts.Model.ID)
}

func TestPrompt_Files_EmptyText_BlockOnly(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	var gotText string
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, text string, _ session.SendOpts) (*session.Message, error) {
			gotText = text
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"parts":[],"files":["`+attachNotesPath+`"]}`))

	h.SendPromptAsync(c)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, attachNotesLine+"\n", gotText)
}

func TestPrompt_Files_OutboxE2E_DispatchesComposedText(t *testing.T) {
	var mu sync.Mutex
	var sentTexts []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			sentTexts = append(sentTexts, firstPartText(t, body))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"info":{"role":"assistant","id":"msg_1"},"parts":[{"type":"text","text":"ok"}]}`))
	}))
	t.Cleanup(backend.Close)
	env := newE2EEnv(t, backend)
	mr := miniredis.RunT(t)
	env.handler.SetOutboxForTest(outbox.New(redis.NewClient(&redis.Options{Addr: mr.Addr()})))

	w := postPromptFiles(t, env,
		`{"clientMessageID":"cm-f1","parts":[{"type":"text","text":"review please"}],"files":["`+attachNotesPath+`"]}`)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, sentTexts, 1)
	assert.Equal(t, wantComposedNotes, sentTexts[0])
}

func TestPrompt_Files_ValidationRejected(t *testing.T) {
	cases := []struct {
		name  string
		files string
	}{
		{"eleven files", `["` + attachNotesPath + `","` + attachNotesPath + `1","` + attachNotesPath + `2","` + attachNotesPath + `3","` + attachNotesPath + `4","` + attachNotesPath + `5","` + attachNotesPath + `6","` + attachNotesPath + `7","` + attachNotesPath + `8","` + attachNotesPath + `9","` + attachNotesPath + `10"]`},
		{"duplicate path", `["` + attachNotesPath + `","` + attachNotesPath + `"]`},
		{"traversal shape", `["/workspace/uploads/../secret"]`},
		{"outside uploads", `["/etc/passwd"]`},
		{"empty entry", `[""]`},
		{"whitespace entry", `["   "]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newProxyHandlerForAdapterTest(t)
			adapterCalled := false
			h.adapter = &mockAdapter{
				sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
					adapterCalled = true
					return &session.Message{ID: "msg_1"}, nil
				},
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
			c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
				`{"parts":[{"type":"text","text":"hi"}],"files":`+tc.files+`}`))

			h.SendPromptAsync(c)

			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.False(t, adapterCalled, "invalid files must never reach the adapter")
		})
	}
}

func TestPrompt_Files_PromptCapIncludesManifest(t *testing.T) {
	textAtCap := strings.Repeat("x", 100_000-(len(attachNotesLine)+3))
	textOverCap := strings.Repeat("x", 100_001-(len(attachNotesLine)+3))

	for _, tc := range []struct {
		name     string
		text     string
		wantCode int
	}{
		{"composed exactly at cap passes", textAtCap, http.StatusOK},
		{"composed one over cap rejected", textOverCap, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newProxyHandlerForAdapterTest(t)
			h.adapter = &mockAdapter{
				sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
					return &session.Message{ID: "msg_1"}, nil
				},
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
			c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
				`{"parts":[{"type":"text","text":"`+tc.text+`"}],"files":["`+attachNotesPath+`"]}`))

			h.SendPromptAsync(c)

			require.Equal(t, tc.wantCode, w.Code, w.Body.String())
			if tc.wantCode == http.StatusBadRequest {
				assert.Contains(t, w.Body.String(), "text exceeds 100KB limit")
			}
		})
	}
}

func TestQueue_Files_ComposedOnceInEntry(t *testing.T) {
	env := newOutboxTestEnv(t)
	env.router.POST("/api/v1/workspaces/:id/sessions/:sessionId/queue", env.handler.EnqueueMessage)

	w := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/queue", strings.NewReader(
		`{"clientMessageID":"cm-q1","text":"compare these","files":["`+attachReportPath+`","`+attachNotesPath+`"]}`))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	entries := listOutbox(t, env)
	require.Len(t, entries, 1)
	want := "compare these\n\n" +
		`[llmsafespaces:attachment path="/workspace/uploads/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee-report.pdf" name="report.pdf"]` + "\n" +
		attachNotesLine + "\n"
	assert.Equal(t, want, entries[0].Text)
	assert.Equal(t, 2, strings.Count(entries[0].Text, "[llmsafespaces:attachment "))
}

func TestQueue_FilesOnly_BlockOnlyEntry(t *testing.T) {
	env := newOutboxTestEnv(t)
	env.router.POST("/api/v1/workspaces/:id/sessions/:sessionId/queue", env.handler.EnqueueMessage)

	w := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/queue", strings.NewReader(
		`{"clientMessageID":"cm-q2","files":["`+attachNotesPath+`"]}`))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	entries := listOutbox(t, env)
	require.Len(t, entries, 1)
	assert.Equal(t, attachNotesLine+"\n", entries[0].Text)
}

func TestOutbox_Files_RetryRedeliversStoredTextOnceManifest(t *testing.T) {
	var mu sync.Mutex
	var sentTexts []string
	attempt := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			attempt++
			n := attempt
			sentTexts = append(sentTexts, firstPartText(t, body))
			mu.Unlock()
			if n == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/message"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"info":{"role":"assistant","id":"msg_1"},"parts":[{"type":"text","text":"ok"}]}`))
	}))
	t.Cleanup(backend.Close)
	env := newE2EEnv(t, backend)
	mr := miniredis.RunT(t)
	env.handler.SetOutboxForTest(outbox.New(redis.NewClient(&redis.Options{Addr: mr.Addr()})))

	origBackoff, origMax := outbox.RetryBackoff, outbox.MaxBackoff
	outbox.RetryBackoff, outbox.MaxBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { outbox.RetryBackoff, outbox.MaxBackoff = origBackoff, origMax })

	w := postPromptFiles(t, env,
		`{"clientMessageID":"cm-r1","parts":[{"type":"text","text":"review please"}],"files":["`+attachNotesPath+`"]}`)
	require.Equal(t, http.StatusAccepted, w.Code)

	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"), "first attempt must run")
	time.Sleep(10 * time.Millisecond)
	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"), "retry must run")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, sentTexts, 2, "exactly two dispatch attempts: initial + retry")
	assert.Equal(t, sentTexts[0], sentTexts[1], "retry re-dispatches the stored text byte-identically")
	for _, text := range sentTexts {
		assert.Equal(t, 1, strings.Count(text, "[llmsafespaces:attachment "), "exactly one manifest line per dispatch")
	}

	entries, err := env.handler.GetOutboxForTest().List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	assert.Empty(t, entries, "delivered entry leaves the outbox")
}

func TestOutbox_Files_ClientMessageIDRetryDeterministic(t *testing.T) {
	env := newOutboxTestEnv(t)

	body := `{"clientMessageID":"cm-df","parts":[{"type":"text","text":"review please"}],"files":["` + attachNotesPath + `"]}`
	w1 := postPromptFiles(t, env, body)
	require.Equal(t, http.StatusAccepted, w1.Code)
	w2 := postPromptFiles(t, env, body)
	require.Equal(t, http.StatusOK, w2.Code, "duplicate clientMessageID is idempotent")

	entries := listOutbox(t, env)
	require.Len(t, entries, 1)
	assert.Equal(t, wantComposedNotes, entries[0].Text)
}

func TestMessage_Files_RejectedOnAdapterPath(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			t.Fatal("adapter must not be called when files is present on /message")
			return nil, nil
		},
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"parts":[{"type":"text","text":"hi"}],"files":["`+attachNotesPath+`"]}`))

	h.SendMessage(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "files not supported on this route; use /prompt")
}

func TestMessage_Files_RejectedOnLegacyPath(t *testing.T) {
	backendHits := 0
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		backendHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"info":{"role":"assistant","id":"msg_1"},"parts":[{"type":"text","text":"ok"}]}`))
	})
	env.setupWorkspaceWithT(t, "ws-1", 5)
	env.setupPasswordWithT(t, "ws-1", "test-password")

	w := env.doRequestWithT(t, http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/message",
		strings.NewReader(`{"parts":[{"type":"text","text":"hi"}],"files":["`+attachNotesPath+`"]}`))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "files not supported on this route; use /prompt")
	assert.Zero(t, backendHits, "rejected body must never be proxied")
}

func TestMessage_NoFiles_BodyProxiedVerbatim(t *testing.T) {
	var captured []byte
	env := newTestEnvWithBackend(t, captureBackend(&captured))
	env.setupWorkspaceWithT(t, "ws-1", 5)
	env.setupPasswordWithT(t, "ws-1", "test-password")

	body := `{"parts":[{"type":"text","text":"hi"}],"messageID":"custom-1"}`
	w := env.doRequestWithT(t, http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/message", strings.NewReader(body))

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, body, string(captured), "the files probe must leave the proxied body byte-identical")
}

func TestMessage_EmptyFilesArray_Allowed(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
		},
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"parts":[{"type":"text","text":"hi"}],"files":[]}`))

	h.SendMessage(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestPrompt_Files_DiskPressure_NoInteraction(t *testing.T) {
	h := newProxyHandlerForAdapterTestWithDisk(t, 96, 100)
	var gotText string
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, text string, _ session.SendOpts) (*session.Message, error) {
			gotText = text
			return &session.Message{ID: "msg_1", Type: session.MessageAssistant}, nil
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "sessionId", Value: "ses_1"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"parts":[{"type":"text","text":"review please"}],"files":["`+attachNotesPath+`"]}`))

	h.SendPromptAsync(c)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, wantComposedNotes, gotText, "disk at 96% must not alter /prompt composition (upload-side gate, D5)")
	assert.NotContains(t, gotText, "System notice")
	assert.NotContains(t, gotText, "disk")
}

func newProxyHandlerForAdapterTestWithDisk(t *testing.T, usedBytes, totalBytes int64) *ProxyHandler {
	t.Helper()
	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()

	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	wsCRD := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Status: v1.WorkspaceStatus{
			Phase:          v1.WorkspacePhaseActive,
			PodIP:          "10.0.0.1",
			PodName:        "test-pod",
			DiskUsedBytes:  usedBytes,
			DiskTotalBytes: totalBytes,
		},
	}
	wsMock.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(wsCRD, nil)

	h, err := NewProxyHandler(k8sMock, &testLogger{}, "default", nil, nil)
	require.NoError(t, err)
	return h
}

func TestExtractPromptFiles(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"files present", `{"files":["/workspace/uploads/x"]}`, []string{"/workspace/uploads/x"}},
		{"files absent", `{"parts":[]}`, nil},
		{"files null", `{"files":null}`, nil},
		{"files empty", `{"files":[]}`, []string{}},
		{"malformed json", `not-json`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractPromptFiles([]byte(tc.body))
			if tc.want == nil {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
