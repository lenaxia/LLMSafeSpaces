// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// D3 outbox handler tests (design 0050 §D3, #907): accept-then-202,
// clientMessageID dedupe, cap 429, queue listing reads the real outbox,
// and the full worker delivery through the adapter seam.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
)

// newOutboxTestEnv wires a miniredis-backed outbox + the REAL opencode
// adapter (fake backend) into an e2e env — the outbox branch requires
// both, and the real adapter is the production delivery seam.
func newOutboxTestEnv(t *testing.T) *e2eEnv {
	t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"info":{"role":"assistant","id":"msg_1"},"parts":[{"type":"text","text":"ok"}]}`))
	}))
	t.Cleanup(backend.Close)
	env := newE2EEnv(t, backend)
	mr := miniredis.RunT(t)
	env.handler.SetOutboxForTest(outbox.New(redis.NewClient(&redis.Options{Addr: mr.Addr()})))
	return env
}

func postPrompt(t *testing.T, env *e2eEnv, body string) *httptest.ResponseRecorder {
	t.Helper()
	return env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/prompt", strings.NewReader(body))
}

func TestOutbox_AcceptReturns202(t *testing.T) {
	env := newOutboxTestEnv(t)

	w := postPrompt(t, env, `{"clientMessageID":"cm-1","parts":[{"type":"text","text":"hello"}]}`)
	require.Equal(t, http.StatusAccepted, w.Code, "accept must 202 immediately, not carry the turn: %s", w.Body.String())
	var body struct {
		MessageID       string `json:"messageID"`
		ClientMessageID string `json:"clientMessageID"`
		Status          string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "cm-1", body.ClientMessageID)
	assert.Equal(t, "queued", body.Status)
	assert.NotEmpty(t, body.MessageID)
}

func TestOutbox_DedupeReturnsOriginal(t *testing.T) {
	env := newOutboxTestEnv(t)

	w1 := postPrompt(t, env, `{"clientMessageID":"cm-dup","parts":[{"type":"text","text":"hello"}]}`)
	require.Equal(t, http.StatusAccepted, w1.Code)
	w2 := postPrompt(t, env, `{"clientMessageID":"cm-dup","parts":[{"type":"text","text":"hello"}]}`)
	assert.Equal(t, http.StatusOK, w2.Code, "retry with the same clientMessageID is 200-idempotent, not a second accept")

	entries := listOutbox(t, env)
	require.Len(t, entries, 1, "exactly one entry exists after a duplicate retry")
}

func TestOutbox_CapReturns429(t *testing.T) {
	env := newOutboxTestEnv(t)

	for i := 0; i < outbox.Cap; i++ {
		w := postPrompt(t, env, fmt.Sprintf(`{"clientMessageID":"cm-%d","parts":[{"type":"text","text":"m%d"}]}`, i, i))
		require.Equal(t, http.StatusAccepted, w.Code, "fill %d", i)
	}
	w := postPrompt(t, env, `{"clientMessageID":"cm-over","parts":[{"type":"text","text":"over"}]}`)
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "outbox cap must 429")
}

func TestOutbox_ListQueueReadsOutbox(t *testing.T) {
	env := newOutboxTestEnv(t)
	env.router.GET("/api/v1/workspaces/:id/sessions/:sessionId/queue", env.handler.ListQueue)

	w := postPrompt(t, env, `{"clientMessageID":"cm-q","parts":[{"type":"text","text":"queued message"}]}`)
	require.Equal(t, http.StatusAccepted, w.Code)

	w2 := env.do(http.MethodGet, "/api/v1/workspaces/ws-1/sessions/ses_1/queue", nil)
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Contains(t, w2.Body.String(), "queued message", "queue listing reads the real outbox")
}

// TestOutbox_WorkerDeliversThroughAdapter is the D3 end-to-end seam test:
// accept (202) → worker → REAL opencode adapter.Send (V1 /message) →
// entry leaves the outbox. The delivery path is the production one,
// including the model selector forwarded in the #917 object wire form.
func TestOutbox_WorkerDeliversThroughAdapter(t *testing.T) {
	var mu sync.Mutex
	var sent []map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			sent = append(sent, body)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"info":{"role":"assistant","id":"msg_1"},"parts":[{"type":"text","text":"ok"}]}`))
	}))
	defer backend.Close()

	env := newE2EEnv(t, backend)
	mr := miniredis.RunT(t)
	env.handler.SetOutboxForTest(outbox.New(redis.NewClient(&redis.Options{Addr: mr.Addr()})))

	w := postPrompt(t, env, `{"clientMessageID":"cm-w","model":{"modelID":"glm-5.3","providerID":"thekaocloud"},"parts":[{"type":"text","text":"deliver me"}]}`)
	require.Equal(t, http.StatusAccepted, w.Code)

	// Drive one delivery tick through the real bridge.
	ok := env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1")
	require.True(t, ok, "worker must find the pending entry")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, sent, 1)
	assert.Equal(t, "deliver me", firstPartText(t, sent[0]))
	m, ok := sent[0]["model"].(map[string]any)
	require.True(t, ok, "model selector forwarded in the object wire form (#917)")
	assert.Equal(t, "glm-5.3", m["modelID"])
	assert.Equal(t, "thekaocloud", m["providerID"])

	entries, err := env.handler.GetOutboxForTest().List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	assert.Empty(t, entries, "delivered entry leaves the outbox")
}

// TestOutbox_WorkerDisconnectImmunity pins the incident class the outbox
// exists for: the accepting request's context is canceled immediately
// after 202 — the delivery worker's detached context is unaffected and
// the message still delivers.
func TestOutbox_WorkerDisconnectImmunity(t *testing.T) {
	env := newOutboxTestEnv(t)

	// Accept with a context that dies right after the 202.
	ctx, cancel := context.WithCancel(context.Background())
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/prompt",
		strings.NewReader(`{"clientMessageID":"cm-dc","parts":[{"type":"text","text":"survive disconnect"}]}`)).
		WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusAccepted, w.Code)
	cancel() // client gone — iOS killed the POST

	// The worker delivers on its own context.
	ok := env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1")
	require.True(t, ok, "detached worker must deliver after the accepting context died")
	entries, err := env.handler.GetOutboxForTest().List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	assert.Empty(t, entries, "message delivered despite client disconnect — the D3 contract")
}

func firstPartText(t *testing.T, body map[string]any) string {
	t.Helper()
	parts, ok := body["parts"].([]any)
	require.True(t, ok)
	p0, ok := parts[0].(map[string]any)
	require.True(t, ok)
	txt, _ := p0["text"].(string)
	return txt
}

func listOutbox(t *testing.T, env *e2eEnv) []outbox.Entry {
	t.Helper()
	entries, err := env.handler.GetOutboxForTest().List(context.Background(), "ws-1", "ses_1")
	require.NoError(t, err)
	return entries
}

// TestOutbox_RetryRedeliversAfterTimeout pins the post-oracle retry
// contract: the D3 r2 transcript-dedupe pre-check (skip redelivery when
// the prior attempt's text already persisted) was deleted with the
// verify oracle (#1219 disposition). A retried entry re-POSTs and
// completes through the normal path — the duplicate-turn risk on the
// non-terminus regimes is the documented, accepted trade of the
// deletion (the agentd terminus, the production regime, dedupes in the
// ledger by entryID).
func TestOutbox_RetryRedeliversAfterTimeout(t *testing.T) {
	var mu sync.Mutex
	sendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message") {
			mu.Lock()
			sendCalls++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"info":{"role":"assistant","id":"msg_1"},"parts":[{"type":"text","text":"ok"}]}`))
	}))
	defer backend.Close()

	env := newE2EEnv(t, backend)
	mr := miniredis.RunT(t)
	env.handler.SetOutboxForTest(outbox.New(redis.NewClient(&redis.Options{Addr: mr.Addr()})))

	// Shrink the backoff so the retried entry is immediately due.
	origBackoff, origMax := outbox.RetryBackoff, outbox.MaxBackoff
	outbox.RetryBackoff, outbox.MaxBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { outbox.RetryBackoff, outbox.MaxBackoff = origBackoff, origMax })

	// Seed an entry, then mark one failed attempt through the service
	// seam (attempts=1 — a prior attempt existed and timed out).
	ob := env.handler.GetOutboxForTest()
	_, err := ob.Accept(context.Background(), "ws-1", "ses_1", "u-1", "cm-r", "already ran", nil)
	require.NoError(t, err)
	_ = ob.DeliverOnce(context.Background(), "ws-1", "ses_1", func(ctx context.Context, _, _ string, _ outbox.Entry) error {
		return errors.New("simulated timeout")
	})
	// Let the (shrunk) backoff gate elapse AFTER the failed attempt.
	time.Sleep(10 * time.Millisecond)

	// The handler-bridge delivery re-POSTs (no transcript pre-check)
	// and completes.
	ok := env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1")
	require.True(t, ok)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, sendCalls, "the retry re-delivers — the transcript dedupe pre-check is gone with the oracle")

	after, _ := ob.List(context.Background(), "ws-1", "ses_1")
	assert.Empty(t, after, "entry delivered and removed")
}

// TestOutbox_RetryEndpoint_Integration exercises the retry flow at the
// HTTP wire level (the review's missing integration case): park an entry
// terminal via real deliveries, then POST /queue/:id/retry → 204, entry
// is pending again with attempts reset, and the next bridge delivery
// completes it. Also covers the 404 for an unknown id (the frontend's
// fallback trigger).
func TestOutbox_RetryEndpoint_Integration(t *testing.T) {
	env := newOutboxTestEnv(t)
	env.router.POST("/api/v1/workspaces/:id/sessions/:sessionId/queue/:messageId/retry", env.handler.RetryQueueMessage)
	env.router.DELETE("/api/v1/workspaces/:id/sessions/:sessionId/queue/:messageId", env.handler.DeleteQueueMessage)
	env.router.GET("/api/v1/workspaces/:id/sessions/:sessionId/queue", env.handler.ListQueue)

	origBackoff, origMax := outbox.RetryBackoff, outbox.MaxBackoff
	outbox.RetryBackoff, outbox.MaxBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() { outbox.RetryBackoff, outbox.MaxBackoff = origBackoff, origMax })

	// Accept one entry.
	w := postPrompt(t, env, `{"clientMessageID":"cm-it","parts":[{"type":"text","text":"integration retry"}]}`)
	require.Equal(t, http.StatusAccepted, w.Code)
	var acc struct {
		MessageID string `json:"messageID"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &acc))
	require.NotEmpty(t, acc.MessageID)

	// Park it terminal: MaxAttempts failing deliveries through the real
	// bridge (the fake backend 200s, so fail via a dead session id is
	// not possible — instead drive the service seam with a failing
	// deliverer, which is the same state machine).
	ob := env.handler.GetOutboxForTest()
	for i := 0; i < outbox.MaxAttempts; i++ {
		_ = ob.DeliverOnce(context.Background(), "ws-1", "ses_1",
			func(context.Context, string, string, outbox.Entry) error { return errors.New("down") })
		time.Sleep(2 * time.Millisecond)
	}
	entries, _ := ob.List(context.Background(), "ws-1", "ses_1")
	require.Len(t, entries, 1)
	require.Equal(t, outbox.StatusError, entries[0].Status, "parked terminal after MaxAttempts")

	// ListQueue shows it as error (the UI's retry affordance).
	w2 := env.do(http.MethodGet, "/api/v1/workspaces/ws-1/sessions/ses_1/queue", nil)
	require.Equal(t, http.StatusOK, w2.Code)
	require.Contains(t, w2.Body.String(), `"status":"error"`)
	require.Contains(t, w2.Body.String(), acc.MessageID)

	// POST retry → 204; entry pending, attempts zeroed.
	w3 := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/queue/"+acc.MessageID+"/retry", nil)
	require.Equal(t, http.StatusNoContent, w3.Code)
	entries, _ = ob.List(context.Background(), "ws-1", "ses_1")
	require.Len(t, entries, 1)
	require.Equal(t, outbox.StatusPending, entries[0].Status)
	require.Zero(t, entries[0].Attempts)

	// Unknown id → 404 (the frontend's fallback-to-local-re-enqueue path).
	w4 := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/queue/ob_missing/retry", nil)
	require.Equal(t, http.StatusNotFound, w4.Code)

	// Bridge delivery now completes it (fake backend 200s).
	ok := env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1")
	require.True(t, ok)
	entries, _ = ob.List(context.Background(), "ws-1", "ses_1")
	assert.Empty(t, entries, "retried entry delivers and leaves the outbox")
}

// TestOutbox_RetryQueueMessage_NoOutbox: the 501 shape when the outbox
// is unwired (dev/test fallback), so the frontend's catch falls through
// to local re-enqueue deterministically.
func TestOutbox_RetryQueueMessage_NoOutbox(t *testing.T) {
	gin.SetMode(gin.TestMode)
	env := newTestEnv(t)
	router := gin.New()
	router.POST("/api/v1/workspaces/:id/sessions/:sessionId/queue/:messageId/retry", env.handler.RetryQueueMessage)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/queue/ob_x/retry", nil))
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}
