// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #987 handler-level integration tests: the outbox delivery lifecycle
// against a stateful fake opencode — the SSE queue.update/sent emission
// on every confirmed delivery path, the definitive-vs-ambiguous failure
// classification, and the V2 admission path. The transcript-verify
// oracle that once resolved ambiguous outcomes was deleted per the
// admission-ID matrix disposition (#1219): with no verifier wired the
// outbox degrades ambiguous entries to its bounded retry ladder.

import (
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
	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// fakeAgentBackend is a stateful opencode stand-in: it records prompts,
// optionally stalls the turn past the delivery timeout (the incident
// shape), and serves history from the recorded messages.
type fakeAgentBackend struct {
	mu sync.Mutex
	// userTexts are the persisted user messages (created timestamps).
	userTexts []struct {
		text    string
		created time.Time
	}
	posts int
	// admits counts POST /api/session/:sid/prompt hits (the V2
	// admit-and-return path, design 0052).
	admits int
	// admitStatus, when non-zero, answers the V2 admission POST with
	// that HTTP status (a definitive agent rejection).
	admitStatus int
	// admitReset, when true, hijacks and closes the admission
	// connection mid-flight — a transport cut whose outcome is unknown.
	admitReset bool
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

	// modelSets counts POST /api/session/:sid/model hits; callOrder
	// records model-vs-admit ordering (R4: model BEFORE admission);
	// modelSetStatus overrides the model-set response (failure paths).
	modelSets      int
	callOrder      []string
	modelSetStatus int
	reqLog         []string
	lastModelID    string
}

func (f *fakeAgentBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Session model-set (design 0054 R4): records call order relative to
	// admissions — the outbox must set the model BEFORE the prompt.
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/session/") && strings.HasSuffix(r.URL.Path, "/model") {
		var mb struct {
			Model struct {
				ID string `json:"id"`
			} `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&mb)
		f.mu.Lock()
		f.reqLog = append(f.reqLog, "MODEL")
		if mb.Model.ID != "" {
			// Real set — capability probes POST {"model":{}} (empty id).
			f.modelSets++
			f.callOrder = append(f.callOrder, "model")
			f.lastModelID = mb.Model.ID
		}
		f.mu.Unlock()
		if f.modelSetStatus != 0 {
			w.WriteHeader(f.modelSetStatus)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// V2 admit-and-return (design 0052): persist immediately, answer
	// with the admission envelope — no turn blocking.
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/session/") && strings.HasSuffix(r.URL.Path, "/prompt") {
		var body struct {
			Prompt struct {
				Text string `json:"text"`
			} `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.reqLog = append(f.reqLog, "PROMPT")
		if body.Prompt.Text != "" {
			// Real admission — capability probes POST {} (empty text).
			f.admits++
			f.callOrder = append(f.callOrder, "admit")
			f.persist(body.Prompt.Text)
		}
		seq := f.admits
		admitStatus, admitReset := f.admitStatus, f.admitReset
		f.mu.Unlock()
		if admitReset {
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					_ = conn.Close()
					return
				}
			}
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if admitStatus != 0 {
			w.WriteHeader(admitStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"admittedSeq":%d,"id":"msg_v2_%d","sessionID":"ses_1","prompt":{"text":%s},"delivery":"queue","timeCreated":%d}}`,
			seq, seq, quoteJSON(body.Prompt.Text), time.Now().UnixMilli())
		return
	}
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
// wiring (the same funcs Start installs): OnDelivered/OnStaged — no
// verifier (the oracle is deleted; the outbox's nil-verifier retry
// ladder is the production shape on the non-terminus paths).
// With v2 true, the adapter is rebuilt with WithV2Store — the env the
// production wiring produces when OPENCODE_V2_DELIVERY is on.
func newVerifyEnv(t *testing.T, backend *fakeAgentBackend, v2 ...bool) *e2eEnv {
	t.Helper()
	srv := httptest.NewServer(backend)
	env := newE2EEnv(t, srv)
	if len(v2) > 0 && v2[0] {
		handler := env.handler
		handler.SetAdapter(opencode.NewAdapter(
			handler.AdapterPasswordResolver(),
			handler.AdapterPodIPResolver(),
			nil,
			opencode.WithAdapterHTTPClient(srv.Client()),
			opencode.WithAdapterPort(extractPort(t, srv.URL)),
			opencode.WithV2Store(true),
		))
	}
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	mr := miniredis.RunT(t)
	ob := outbox.New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
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
// path (attempts count), never the ambiguous path.
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

// TestOutboxVerify_TimeoutMidTurnDegradesToRetryLadder pins the
// post-oracle contract for the #987 incident shape: a turn that
// outlives the delivery timeout is outcome-unknown (ambiguous), and
// with the transcript verifier deleted (#1219 disposition) the outbox
// degrades it to the bounded retry ladder — the duplicate turn that
// results is the documented, accepted trade of the deletion (the
// alternative was an entry stranded in verifying forever). The entry
// must still complete and emit queue.update/sent.
func TestOutboxVerify_TimeoutMidTurnDegradesToRetryLadder(t *testing.T) {
	shrinkOutboxTimers(t)
	backend := &fakeAgentBackend{stall: 120 * time.Millisecond, persistFirst: true} // > DeliveryTimeout
	env := newVerifyEnv(t, backend)
	events := subscribeQueueUpdates(t, env)

	w := postPrompt(t, env, `{"clientMessageID":"cm-987","parts":[{"type":"text","text":"make regression tests"}]}`)
	require.Equal(t, http.StatusAccepted, w.Code)

	// First worker pass: the send stalls past the timeout → ambiguous →
	// with no verifier wired, the retry ladder (not verifying).
	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
	entries := listOutbox(t, env)
	require.Len(t, entries, 1)
	assert.Equal(t, outbox.StatusPending, entries[0].Status, "nil-verifier degrade: ambiguous goes to the bounded retry ladder")
	assert.Equal(t, 1, entries[0].Attempts)

	// Second pass retries and completes: clear the stall (the incident
	// shape is a one-off slow turn) and let the shrunk backoff elapse.
	backend.mu.Lock()
	backend.stall = 0
	backend.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
	assert.Empty(t, listOutbox(t, env), "the retry ladder completes the entry")

	backend.mu.Lock()
	posts := backend.posts
	backend.mu.Unlock()
	assert.Equal(t, 2, posts, "the documented duplicate-risk trade: the retry re-POSTs (persist-first means the stalled attempt also landed)")

	select {
	case e := <-events:
		data, _ := json.Marshal(e.Data)
		assert.Contains(t, string(data), `"sent"`, "queue.update/sent emitted on confirmed delivery")
	case <-time.After(2 * time.Second):
		t.Fatal("no queue.update/sent emitted on confirmed delivery")
	}
}

// TestOutboxDeliver_V2CompletesAtAdmission: with SetV2Delivery on, the
// deliverer applies the session model, admits once, and completes —
// the admission→promotion await was deleted with its only observer
// (the text-scan oracle, #1219 disposition); stranded-admitted rows
// are the agentd ledger's state machine's to surface (design 0055
// M2/I6), and this legacy branch only runs in the R8 rollback regime.
func TestOutboxDeliver_V2CompletesAtAdmission(t *testing.T) {
	shrinkOutboxTimers(t)
	backend := &fakeAgentBackend{}
	env := newVerifyEnv(t, backend, true)
	env.handler.SetV2Delivery(true)
	events := subscribeQueueUpdates(t, env)

	w := postPrompt(t, env, `{"clientMessageID":"cm-v2","parts":[{"type":"text","text":"v2 admission"}]}`)
	require.Equal(t, http.StatusAccepted, w.Code)

	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
	assert.Empty(t, listOutbox(t, env), "admission completes the entry in one pass")

	backend.mu.Lock()
	admits, posts := backend.admits, backend.posts
	backend.mu.Unlock()
	assert.Equal(t, 1, admits, "exactly one V2 admission POST")
	assert.Zero(t, posts, "the V1 turn-blocking sync send must NOT be used in V2 mode")

	select {
	case e := <-events:
		data, _ := json.Marshal(e.Data)
		assert.Contains(t, string(data), `"sent"`, "queue.update/sent fires on the admission path")
	case <-time.After(2 * time.Second):
		t.Fatal("no queue.update/sent emitted on the V2 admission path")
	}
}

// TestOutboxDeliver_V2UnhappyPaths (design 0052): the failure
// classification contract under V2 delivery.
func TestOutboxDeliver_V2UnhappyPaths(t *testing.T) {
	shrinkOutboxTimers(t)

	t.Run("admission 5xx is definitive — backoff retry, never error-parked or ambiguous", func(t *testing.T) {
		backend := &fakeAgentBackend{admitStatus: http.StatusServiceUnavailable}
		env := newVerifyEnv(t, backend, true)
		env.handler.SetV2Delivery(true)

		w := postPrompt(t, env, `{"clientMessageID":"cm-5xx","parts":[{"type":"text","text":"five oh three"}]}`)
		require.Equal(t, http.StatusAccepted, w.Code)

		require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
		entries := listOutbox(t, env)
		require.Len(t, entries, 1)
		assert.Equal(t, outbox.StatusPending, entries[0].Status,
			"an HTTP-status rejection is definitive: bounded retry with backoff")
		assert.Equal(t, 1, entries[0].Attempts)
	})

	t.Run("admission transport cut is ambiguous — the nil-verifier retry ladder, bounded by Attempts", func(t *testing.T) {
		backend := &fakeAgentBackend{admitReset: true}
		env := newVerifyEnv(t, backend, true)
		env.handler.SetV2Delivery(true)

		w := postPrompt(t, env, `{"clientMessageID":"cm-cut","parts":[{"type":"text","text":"connection cut"}]}`)
		require.Equal(t, http.StatusAccepted, w.Code)

		require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
		entries := listOutbox(t, env)
		require.Len(t, entries, 1)
		assert.Equal(t, outbox.StatusPending, entries[0].Status,
			"unknown-outcome transport cut degrades to the retry ladder (verifier deleted, #1219)")
		assert.Equal(t, 1, entries[0].Attempts)

		backend.mu.Lock()
		admits := backend.admits
		backend.mu.Unlock()
		assert.Equal(t, 1, admits, "exactly one admission attempt so far")

		// The retry re-admits — the documented bounded-duplicate-risk
		// trade of the oracle deletion.
		time.Sleep(5 * time.Millisecond)
		require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
		backend.mu.Lock()
		admits = backend.admits
		backend.mu.Unlock()
		assert.Equal(t, 2, admits, "the ladder re-admits after the cut")
	})
}
