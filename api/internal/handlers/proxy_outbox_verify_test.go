// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #987 handler-level integration tests: the outbox delivery lifecycle
// against a stateful fake opencode — the incident scenario (send times
// out mid-turn, message persisted anyway), the safe-retry scenario
// (definitive absence), and the SSE queue.update/sent emission on every
// confirmed delivery path.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
)

// fakeAgentBackend is a stateful opencode stand-in: it records prompts,
// optionally stalls the turn past the delivery timeout (the incident
// shape), and serves cursor-less history from the recorded messages.
type fakeAgentBackend struct {
	mu sync.Mutex
	// userTexts are the persisted user messages (created timestamps).
	userTexts []struct {
		text    string
		created time.Time
	}
	posts int
	// stall makes each POST /message sleep past the (test-shrunk)
	// DeliveryTimeout before responding 200 — the send outcome goes
	// ambiguous while the message IS persisted up front (opencode's
	// persist-before-turn contract).
	stall time.Duration
	// respondStatus, when non-zero, overrides the POST response code
	// AFTER stalling (a definitive HTTP rejection).
	respondStatus int
	// persistFirst models the opencode contract: the user message is
	// persisted BEFORE the turn. False simulates a transport cut where
	// nothing ever reached the agent (persist only after the stall —
	// which the timed-out sender never sees).
	persistFirst bool
}

func (f *fakeAgentBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message") {
		var body struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		text := ""
		if len(body.Parts) > 0 {
			text = body.Parts[0].Text
		}
		f.mu.Lock()
		f.posts++
		if f.persistFirst {
			f.persist(text)
		}
		stall, status := f.stall, f.respondStatus
		f.mu.Unlock()
		if stall > 0 {
			time.Sleep(stall) // outside the lock: history stays readable mid-turn
		}
		if !f.persistFirst {
			f.mu.Lock()
			f.persist(text)
			f.mu.Unlock()
		}
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"info":{"role":"assistant","id":"msg_a"},"parts":[{"type":"text","text":"ok"}]}`))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/message"):
		var b strings.Builder
		b.WriteString("[")
		for i, m := range f.userTexts {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"info":{"role":"user","id":"msg_%d","time":{"created":%d}},"parts":[{"type":"text","text":%s}]}`,
				i, m.created.UnixMilli(), quoteJSON(m.text))
		}
		b.WriteString("]")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(b.String()))

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeAgentBackend) persist(text string) {
	f.userTexts = append(f.userTexts, struct {
		text    string
		created time.Time
	}{text, time.Now()})
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// shrinkOutboxTimers compresses delivery/verify timing for tests and
// restores on cleanup.
func shrinkOutboxTimers(t *testing.T) {
	t.Helper()
	origTimeout, origVD, origVB, origMVB := outbox.DeliveryTimeout, outbox.VerifyDelay, outbox.VerifyBackoff, outbox.MaxVerifyBackoff
	origRB, origMB := outbox.RetryBackoff, outbox.MaxBackoff
	outbox.DeliveryTimeout = 40 * time.Millisecond
	outbox.VerifyDelay = 5 * time.Millisecond
	outbox.VerifyBackoff, outbox.MaxVerifyBackoff = time.Millisecond, time.Millisecond
	outbox.RetryBackoff, outbox.MaxBackoff = time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		outbox.DeliveryTimeout, outbox.VerifyDelay, outbox.VerifyBackoff, outbox.MaxVerifyBackoff = origTimeout, origVD, origVB, origMVB
		outbox.RetryBackoff, outbox.MaxBackoff = origRB, origMB
	})
}

// newVerifyEnv wires the e2e env + outbox with the PRODUCTION hook
// wiring (the same funcs Start installs): verifier + OnDelivered.
func newVerifyEnv(t *testing.T, backend *fakeAgentBackend) *e2eEnv {
	t.Helper()
	env := newE2EEnv(t, httptest.NewServer(backend))
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	mr := miniredis.RunT(t)
	ob := outbox.New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	ob.SetVerifier(env.handler.outboxVerify)
	ob.SetOnDelivered(env.handler.outboxOnDelivered)
	env.handler.SetOutboxForTest(ob)
	return env
}

// subscribeQueueUpdates captures queue.update events for ws-1.
func subscribeQueueUpdates(t *testing.T, env *e2eEnv) <-chan apitypes.WorkspaceSSEEvent {
	t.Helper()
	sub, err := env.handler.userBroker.SubscribeWorkspace("ws-1")
	require.NoError(t, err)
	t.Cleanup(func() { env.handler.userBroker.UnsubscribeWorkspace("ws-1", sub) })
	out := make(chan apitypes.WorkspaceSSEEvent, 16)
	go func() {
		for e := range sub.Ch {
			if e.Type == "queue.update" {
				out <- e
			}
		}
	}()
	return out
}

// TestOutboxVerify_LongTurnDeliveredExactlyOnce is THE incident pin
// (#987): a turn that outlives the delivery timeout. Old behavior: the
// timeout fired, the entry retried as a fresh POST, and the user's text
// ran THREE times. Required behavior: one POST ever, the entry verified
// against the transcript, the queue cleared, and queue.update/sent
// emitted exactly once.
func TestOutboxVerify_LongTurnDeliveredExactlyOnce(t *testing.T) {
	shrinkOutboxTimers(t)
	backend := &fakeAgentBackend{stall: 120 * time.Millisecond, persistFirst: true} // > DeliveryTimeout
	env := newVerifyEnv(t, backend)
	events := subscribeQueueUpdates(t, env)

	w := postPrompt(t, env, `{"clientMessageID":"cm-987","parts":[{"type":"text","text":"make regression tests"}]}`)
	require.Equal(t, http.StatusAccepted, w.Code)

	// First worker pass: the send stalls past the timeout → ambiguous.
	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
	entries := listOutbox(t, env)
	require.Len(t, entries, 1)
	assert.Equal(t, outbox.StatusVerifying, entries[0].Status, "timeout mid-turn is unknown-outcome, never a retry trigger")

	// Wait out the verify delay; second pass resolves via the transcript.
	time.Sleep(15 * time.Millisecond)
	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
	assert.Empty(t, listOutbox(t, env), "verified-delivered entry leaves the queue")

	backend.mu.Lock()
	posts := backend.posts
	backend.mu.Unlock()
	assert.Equal(t, 1, posts, "exactly ONE POST despite the turn outliving the delivery timeout — was 3 in the incident")

	select {
	case e := <-events:
		data, _ := json.Marshal(e.Data)
		assert.Contains(t, string(data), `"sent"`, "queue.update/sent emitted on the verified path")
		assert.Contains(t, string(data), entries[0].ID)
	case <-time.After(2 * time.Second):
		t.Fatal("no queue.update/sent emitted on confirmed delivery")
	}
}

// TestOutboxVerify_DefinitiveAbsenceRetriesSafely: when verification
// PROVES the message never persisted, a retry is safe — two POSTs total
// (one unknown-outcome, one confirmed), entry gone, sent event fired.
func TestOutboxVerify_DefinitiveAbsenceRetriesSafely(t *testing.T) {
	shrinkOutboxTimers(t)
	// The first POST stalls past the timeout AND nothing persisted
	// (transport cut before the agent received anything): the verify
	// pass must PROVE absence and retry safely.
	backend := &fakeAgentBackend{stall: 120 * time.Millisecond, persistFirst: false}
	env := newVerifyEnv(t, backend)
	events := subscribeQueueUpdates(t, env)

	w := postPrompt(t, env, `{"clientMessageID":"cm-abs","parts":[{"type":"text","text":"never landed"}]}`)
	require.Equal(t, http.StatusAccepted, w.Code)

	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1")) // ambiguous
	time.Sleep(15 * time.Millisecond)
	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1")) // verifies absent → pending
	entries := listOutbox(t, env)
	require.Len(t, entries, 1)
	assert.Equal(t, outbox.StatusPending, entries[0].Status, "proven-absent returns to pending")
	assert.Equal(t, 1, entries[0].Attempts)

	// Fresh send now behaves like the real agent (persist + respond).
	backend.mu.Lock()
	backend.stall = 0
	backend.persistFirst = true
	backend.mu.Unlock()
	time.Sleep(5 * time.Millisecond) // let the retry backoff gate elapse
	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
	assert.Empty(t, listOutbox(t, env))

	backend.mu.Lock()
	posts := backend.posts
	backend.mu.Unlock()
	assert.Equal(t, 2, posts, "one unknown-outcome send + one confirmed retry")

	select {
	case e := <-events:
		data, _ := json.Marshal(e.Data)
		assert.Contains(t, string(data), `"sent"`)
	case <-time.After(2 * time.Second):
		t.Fatal("no queue.update/sent on the retry-success path")
	}
}

// TestOutboxVerify_SyncSuccessEmitsSent: the fast synchronous 2xx path
// clears the queue AND emits queue.update/sent — previously the outbox
// path never emitted it and the frontend pill cleared only via poll.
func TestOutboxVerify_SyncSuccessEmitsSent(t *testing.T) {
	shrinkOutboxTimers(t)
	backend := &fakeAgentBackend{}
	env := newVerifyEnv(t, backend)
	events := subscribeQueueUpdates(t, env)

	w := postPrompt(t, env, `{"clientMessageID":"cm-sync","parts":[{"type":"text","text":"quick one"}]}`)
	require.Equal(t, http.StatusAccepted, w.Code)

	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
	assert.Empty(t, listOutbox(t, env))

	select {
	case e := <-events:
		data, _ := json.Marshal(e.Data)
		assert.Contains(t, string(data), `"sent"`)
	case <-time.After(2 * time.Second):
		t.Fatal("sync delivery must emit queue.update/sent")
	}
}

// TestOutboxVerify_HTTPRejectionIsDefinitive: an HTTP 409/4xx from the
// agent is PROCESSED-and-rejected — it must go down the normal retry
// path (attempts count), not the verifying path.
func TestOutboxVerify_HTTPRejectionIsDefinitive(t *testing.T) {
	shrinkOutboxTimers(t)
	backend := &fakeAgentBackend{respondStatus: http.StatusConflict}
	env := newVerifyEnv(t, backend)

	w := postPrompt(t, env, `{"clientMessageID":"cm-409","parts":[{"type":"text","text":"busy session"}]}`)
	require.Equal(t, http.StatusAccepted, w.Code)

	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
	entries := listOutbox(t, env)
	require.Len(t, entries, 1)
	assert.Equal(t, outbox.StatusPending, entries[0].Status, "HTTP rejection is a definitive failure — plain retry")
	assert.Equal(t, 1, entries[0].Attempts)
	assert.NotContains(t, entries[0].LastError, "ambiguous")
}

var _ = context.Background // keep context import if unused by future edits
