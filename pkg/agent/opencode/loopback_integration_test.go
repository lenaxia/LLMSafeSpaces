//go:build integration
// +build integration

package opencode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The L2 leg of docs/testing/agentd-mcp-tools-test-plan.md: the loopback
// seam against the REAL opencode binary, with a local mock
// OpenAI-compatible provider so message sends (including image file
// parts) complete offline. Asserts transport/shape/acceptance only —
// never model behavior.

// mockProvider is an OpenAI-compatible /chat/completions endpoint.
type mockProvider struct {
	mu       sync.Mutex
	requests []string // raw bodies, for image-reach assertions
	delay    time.Duration
}

func (m *mockProvider) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Connection-close on EVERY response: opencode's Bun runtime
		// will not complete requests against this server under
		// HTTP/1.1 keep-alive (live-debugged 2026-09-13 — zero requests
		// ever arrived; the Python HTTP/1.0 mock worked). Matching the
		// close-per-response behavior of a naive endpoint fixed it.
		w.Header().Set("Connection", "close")
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		m.mu.Lock()
		m.requests = append(m.requests, string(body))
		delay := m.delay
		m.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}

		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			chunk := map[string]any{
				"id": "chatcmpl-mock", "object": "chat.completion.chunk", "created": 1, "model": "mockmodel",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": nil,
					"delta": map[string]any{"role": "assistant", "content": "MOCK-REPLY"},
				}},
			}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
			// The finish chunk carries usage — the ai-sdk surfaces
			// token accounting from the terminal chunk (streamed
			// responses never see the non-stream JSON's usage block).
			fmt.Fprintf(w, "data: %s\n\n", `{"id":"chatcmpl-mock","object":"chat.completion.chunk","created":1,"model":"mockmodel","choices":[{"index":0,"finish_reason":"stop","delta":{}}],"usage":{"prompt_tokens":84,"completion_tokens":9,"total_tokens":93}}`)
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		out, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-mock", "object": "chat.completion", "created": 1, "model": "mockmodel",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "MOCK-REPLY"},
			}},
			"usage": map[string]int{"prompt_tokens": 42, "completion_tokens": 3, "total_tokens": 45},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(out)))
		_, _ = w.Write(out)
	}
}

func (m *mockProvider) bodies() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.requests...)
}

// startLoopbackL2 boots the real binary against the mock provider and
// returns the seam client. The mock MUST sit on a pinned low port: the
// pinned opencode binary's outbound fetch fails against this pod's
// ephemeral port range (live-debugged 2026-09-13 — identical servers on
// fixed ports answer, httptest's ephemeral ports yield "Cannot connect
// to API" retries), so the listener is explicit, never :0.
func startLoopbackL2(t *testing.T, port, mockPort int, mock *mockProvider) *Client {
	t.Helper()
	// Both ports are probed-and-claimed, not assumed: transient pod
	// services occasionally bind parts of the 141xx range, and a
	// colliding boot fails with "ServeError" while the test talks to
	// the unrelated listener (live-debugged 2026-09-13).
	port, mockPort = claimTwoPorts(t, port, mockPort)
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", mockPort))
	require.NoError(t, err)
	mockSrv := &httptest.Server{Listener: l, Config: &http.Server{Handler: mock.handler(t)}}
	mockSrv.Start()
	t.Cleanup(mockSrv.Close)
	cfg := mockConfigFor(mockSrv.URL + "/v1")
	if u := os.Getenv("L2_MOCK_URL"); u != "" {
		cfg = mockConfigFor(u)
	}
	srv := startOpencodeServerWithConfig(t, port, cfg)
	return NewLoopbackClient(srv.baseURL, "test-password")
}

// claimTwoPorts returns the requested pair when both are free,
// otherwise walks each forward until a free port is found (claimed by
// listen-and-close immediately before use — best-effort against the
// pod's transient binders).
func claimTwoPorts(t *testing.T, a, b int) (int, int) {
	t.Helper()
	claim := func(p int) int {
		for i := 0; i < 50; i++ {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p+i))
			if err != nil {
				continue
			}
			_ = l.Close()
			return p + i
		}
		t.Fatalf("no free port near %d", p)
		return 0
	}
	return claim(a), claim(b)
}

func mockConfigFor(baseURL string) string {
	return fmt.Sprintf(`{
		"$schema": "https://opencode.ai/config.json",
		"provider": {
			"mock": {
				"npm": "@ai-sdk/openai-compatible",
				"options": {"apiKey": "test", "baseURL": %q},
				"models": {"mockmodel": {"name": "Mock Model", "attachment": true, "limit": {"context": 100000, "output": 4096}}}
			}
		},
		"model": "mock/mockmodel"
	}`, baseURL)
}

func tinyPNG() []byte {
	// 1x1 red PNG (hand-built, zlib-free via stored blocks would be
	// longer; this is the canonical minimal PNG bytes).
	b64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		panic(err)
	}
	return data
}

func TestLoopbackL2_SessionLifecycle(t *testing.T) {
	client := startLoopbackL2(t, 14110, 14150, &mockProvider{})
	ctx, contextTimeout := context.WithTimeout(context.Background(), 60*time.Second)
	defer contextTimeout()

	id, err := client.SessionCreate(ctx, "lifecycle-title")
	require.NoError(t, err)
	require.NotEmpty(t, id)

	list, err := client.SessionList(ctx)
	require.NoError(t, err)
	var found *SessionSummary
	for i := range list {
		if list[i].ID == id {
			found = &list[i]
		}
	}
	require.NotNil(t, found, "created session must appear in the list")
	assert.Equal(t, "lifecycle-title", found.Title)
	assert.NotEmpty(t, found.Version)
	// A never-used session carries no model yet (the live 1.18.15 list
	// populates it after the first turn); the send tests pin the echo.
	if found.Model != nil {
		assert.Equal(t, "mockmodel", found.Model.ID)
		assert.Equal(t, "mock", found.Model.ProviderID)
	}
	assert.NotZero(t, found.Time.Created)

	require.NoError(t, client.SessionRename(ctx, id, "renamed-title"))
	// The list view catches up asynchronously (2xx ≠ yet-visible): poll
	// briefly rather than asserting a zero-latency read-your-write.
	require.Eventually(t, func() bool {
		list, err := client.SessionList(ctx)
		if err != nil {
			return false
		}
		for i := range list {
			if list[i].ID == id {
				return list[i].Title == "renamed-title"
			}
		}
		return false
	}, 10*time.Second, 200*time.Millisecond, "renamed title must become visible")

	require.NoError(t, client.SessionDelete(ctx, id))
	require.Eventually(t, func() bool {
		list, err := client.SessionList(ctx)
		if err != nil {
			return false
		}
		for i := range list {
			if list[i].ID == id {
				return false
			}
		}
		return true
	}, 10*time.Second, 200*time.Millisecond, "deleted session must disappear")
	list, err = client.SessionList(ctx)
	require.NoError(t, err)
	for i := range list {
		assert.NotEqual(t, id, list[i].ID, "deleted session must not appear")
	}
}

func TestLoopbackL2_SendWithModelAndImage(t *testing.T) {
	mock := &mockProvider{}
	client := startLoopbackL2(t, 14122, 14162, mock)
	ctx, timeout := context.WithTimeout(context.Background(), 120*time.Second)
	defer timeout()

	id, err := client.SessionCreate(ctx, "send-title")
	require.NoError(t, err)
	defer func() { _ = client.SessionDelete(context.Background(), id) }()

	png := tinyPNG()
	res, err := client.SessionSend(ctx, id, "what is in this image?", "mock/mockmodel", []ImageAttachment{
		{Filename: "px.png", MIME: "image/png", Data: png},
	})
	require.NoError(t, err, "the synchronous send must complete against the mock provider")
	assert.Equal(t, "MOCK-REPLY", res.Text)
	assert.Equal(t, "mockmodel", res.ModelID, "the response echoes the per-prompt override")
	require.NotEmpty(t, res.MessageID)

	// L2 pins SCHEMA ACCEPTANCE: the real agent parses the file part and
	// completes the synchronous turn (200 + text). Provider-side
	// FORWARDING of image bytes is proven by the live-pod leg — on
	// 1.18.15 a data-URL part measurably reached the model (input
	// tokens grew ~100 for a 1px PNG and the model's reply referenced
	// the file) — but is not reproducible against a config-only mock
	// provider, whose attachment forwarding differs from models.dev
	// catalog providers (live-debugged 2026-09-13).
}

func TestLoopbackL2_SendBareModelRejectedPreCall(t *testing.T) {
	mock := &mockProvider{}
	client := startLoopbackL2(t, 14112, 14152, mock)
	ctx, timeout := context.WithTimeout(context.Background(), 60*time.Second)
	defer timeout()

	_, err := client.SessionSend(ctx, "ses_anything", "hi", "baremodel", nil)
	require.Error(t, err)
	assert.Empty(t, mock.bodies(), "no provider call may happen for an unexpressible ref")
}

func TestLoopbackL2_StatusesBusyFlip(t *testing.T) {
	mock := &mockProvider{delay: 3 * time.Second}
	client := startLoopbackL2(t, 14113, 14153, mock)
	ctx, timeout := context.WithTimeout(context.Background(), 120*time.Second)
	defer timeout()

	id, err := client.SessionCreate(ctx, "busy-flip")
	require.NoError(t, err)
	defer func() { _ = client.SessionDelete(context.Background(), id) }()

	done := make(chan error, 1)
	go func() {
		_, err := client.SessionSend(context.Background(), id, "slow please", "", nil)
		done <- err
	}()

	// The turn is running: the status map must report busy.
	require.Eventually(t, func() bool {
		busy, err := client.GetSessionStatuses(ctx)
		return err == nil && busy[id] == "busy"
	}, 10*time.Second, 200*time.Millisecond, "status must flip busy while the turn runs")

	require.NoError(t, <-done)
	require.Eventually(t, func() bool {
		busy, err := client.GetSessionStatuses(ctx)
		return err == nil && busy[id] != "busy"
	}, 10*time.Second, 200*time.Millisecond, "status must leave busy after the turn")
}

func TestLoopbackL2_MessagesContextAndTokens(t *testing.T) {
	mock := &mockProvider{}
	client := startLoopbackL2(t, 14114, 14154, mock)
	ctx, timeout := context.WithTimeout(context.Background(), 180*time.Second)
	defer timeout()

	id, err := client.SessionCreate(ctx, "meta")
	require.NoError(t, err)
	defer func() { _ = client.SessionDelete(context.Background(), id) }()

	for i := 0; i < 3; i++ {
		_, err := client.SessionSend(ctx, id, fmt.Sprintf("turn %d", i), "", nil)
		require.NoError(t, err)
	}

	count, err := client.SessionMessageCount(ctx, id)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, 6, "three exchanges → at least six messages")

	// The V2 context endpoint serves the agent-runner's view; sessions
	// driven purely through V1 sends report 0 there (live-proven
	// 2026-09-13: native agent sessions populate it, raw-V1 ones do
	// not). L2 pins the no-error contract; the count itself is covered
	// by the live-pod leg.
	nCtx, err := client.SessionContextCount(ctx, id)
	require.NoError(t, err)
	_ = nCtx

	tokens := client.SessionPromptTokens(ctx, id)
	assert.Greater(t, tokens, int64(0), "prompt tokens from the last assistant usage")
}

func TestLoopbackL2_SummarizeCompacts(t *testing.T) {
	mock := &mockProvider{}
	client := startLoopbackL2(t, 14115, 14155, mock)
	ctx, timeout := context.WithTimeout(context.Background(), 180*time.Second)
	defer timeout()

	id, err := client.SessionCreate(ctx, "compact-me")
	require.NoError(t, err)
	defer func() { _ = client.SessionDelete(context.Background(), id) }()

	for i := 0; i < 3; i++ {
		_, err := client.SessionSend(ctx, id, fmt.Sprintf("turn %d with some substance", i), "", nil)
		require.NoError(t, err)
	}
	before, err := client.SessionMessageCount(ctx, id)
	require.NoError(t, err)

	require.NoError(t, client.SessionSummarize(ctx, id, "mock", "mockmodel"))

	// Offline the context-collapse effect is not observable (V2 context
	// stays 0 for V1-driven sessions — see MessagesContextAndTokens);
	// the LIVE leg pinned collapse (3 exchanges → 1 in-context message
	// after summarize). L2 pins the mechanism: summarize drives a real
	// mock-model call and returns 2xx.
	mCount, err := client.SessionMessageCount(ctx, id)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, mCount, before, "history persists through summarize")
}

func TestLoopbackL2_ModelInfoCatalog(t *testing.T) {
	client := startLoopbackL2(t, 14116, 14156, &mockProvider{})
	ctx, timeout := context.WithTimeout(context.Background(), 60*time.Second)
	defer timeout()

	info, err := client.ModelInfo(ctx, "mock", "mockmodel")
	require.NoError(t, err)
	assert.Equal(t, int64(100000), info.ContextLimit)
}

// TestLoopbackL2_BusyMessageDeliversAtBoundary settles the load-bearing
// question for send_message (PR #1382 review finding 1): the summarize
// route was PROVEN to queue server-side on busy, but the /message route
// was only ever observed to BLOCK (the liveprobe killed the POST at
// 20s). This test holds a turn open against the real binary, POSTs a
// message to the busy session with a generous budget, and asserts the
// POST completes AND the message becomes the next turn — delivery at
// the turn boundary, not a drop.
func TestLoopbackL2_BusyMessageDeliversAtBoundary(t *testing.T) {
	mock := &mockProvider{delay: 4 * time.Second}
	client := startLoopbackL2(t, 14117, 14157, mock)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	id, err := client.SessionCreate(ctx, "busy-message")
	require.NoError(t, err)
	defer func() { _ = client.SessionDelete(context.Background(), id) }()

	// Turn 1 runs for ~4s (every mock completion costs delay).
	first := make(chan error, 1)
	go func() {
		_, err := client.SessionSend(context.Background(), id, "turn one: write a word", "", nil)
		first <- err
	}()
	require.Eventually(t, func() bool {
		busy, err := client.GetSessionStatuses(ctx)
		return err == nil && busy[id] == "busy"
	}, 15*time.Second, 200*time.Millisecond, "turn one must be running")

	// The message POST lands while the session is busy. Budget covers
	// turn one's tail plus turn two's full delay plus margin.
	msgDone := make(chan error, 1)
	go func() {
		_, err := client.SessionSend(context.Background(), id, "queued message: reply QUEUED-OK", "", nil)
		msgDone <- err
	}()
	time.Sleep(2 * time.Second) // the POST is in flight against the busy session

	select {
	case err := <-first:
		require.NoError(t, err, "turn one must complete cleanly")
	case <-time.After(60 * time.Second):
		t.Fatal("turn one never completed")
	}

	select {
	case err := <-msgDone:
		require.NoError(t, err, "the busy-targeted message POST must complete (delivery at boundary), not drop or hang")
	case <-time.After(120 * time.Second):
		t.Fatal("message POST never completed — the /message route does NOT deliver at the boundary; send_message's busy semantics must be redesigned")
	}

	// The queued message actually became the next turn: count user and
	// assistant messages (every mock completion replies identically, so
	// reply TEXT is tautological — ordering and counts are the real
	// assertion: 2 user turns, 2 completed assistant turns).
	body, _, err := client.SessionMessagesRaw(ctx, id, 10, "")
	require.NoError(t, err)
	var msgs []struct {
		Info struct {
			Role string `json:"role"`
		} `json:"info"`
	}
	require.NoError(t, json.Unmarshal(body, &msgs))
	userTurns, assistantTurns := 0, 0
	for _, m := range msgs {
		switch m.Info.Role {
		case "user":
			userTurns++
		case "assistant":
			assistantTurns++
		}
	}
	sawQueued := strings.Contains(string(body), "queued message: reply QUEUED-OK")
	assert.Equal(t, 2, userTurns, "two user turns: the original + the queued message")
	assert.Equal(t, 2, assistantTurns, "the queued message was ANSWERED as its own turn")
	assert.True(t, sawQueued, "the queued message must be persisted as the next user turn")
}
