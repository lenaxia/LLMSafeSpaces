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
	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
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
	// admits counts POST /api/session/:sid/prompt hits (the V2
	// admit-and-return path, design 0052).
	admits int
	// admitStatus, when non-zero, answers the V2 admission POST with
	// that HTTP status (a definitive agent rejection).
	admitStatus int
	// admitReset, when true, hijacks and closes the admission
	// connection mid-flight — a transport cut whose outcome is unknown.
	admitReset bool
	// v2StoreStatus/v2StoreBody override the V2 history read (the
	// verification source under V2 delivery).
	v2StoreStatus int
	v2StoreBody   string
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

	// V2 promotion controls (#1119 contract): the user text persists at
	// PROMOTION, not at admission. promoteDelay == 0 promotes
	// immediately after the admission response (the healthy path);
	// promoteDelay > 0 promotes after that delay (slow drain); < 0 never
	// promotes (defect-class death: model-resolve dies pre-persist, or
	// the row parks — the 10:24Z production anatomy).
	promoteDelay time.Duration
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
		}
		seq := f.admits
		promoteDelay := f.promoteDelay
		admitStatus, admitReset := f.admitStatus, f.admitReset
		f.mu.Unlock()
		// #1119: persist at PROMOTION (async), never at admission.
		if promoteDelay >= 0 {
			go func() {
				if promoteDelay > 0 {
					time.Sleep(promoteDelay)
				}
				f.mu.Lock()
				f.persist(body.Prompt.Text)
				f.mu.Unlock()
			}()
		}
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
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/session/") && strings.HasSuffix(r.URL.Path, "/message") {
		f.mu.Lock()
		status, body := f.v2StoreStatus, f.v2StoreBody
		texts := make([]struct {
			text    string
			created time.Time
		}, len(f.userTexts))
		copy(texts, f.userTexts)
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		// Serve the persisted texts in the V2 envelope, NEWEST-first
		// (the real endpoint's ordering contract).
		var b strings.Builder
		b.WriteString(`{"data":[`)
		for i := len(texts) - 1; i >= 0; i-- {
			if i < len(texts)-1 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id":"msg_%d","type":"user","text":%s,"time":{"created":%d}}`,
				i, quoteJSON(texts[i].text), texts[i].created.UnixMilli())
		}
		b.WriteString(`]}`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(b.String()))
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
// shrinkV2PromotionTimers shrinks the #1119 promotion-await window so the
// delivery path exercises in test time. The await runs INSIDE the
// deliverer's detached context, whose budget is outbox.DeliveryTimeout
// (40ms after shrinkOutboxTimers) — raise it above the shrunk window so
// the await path, not the transport budget, is what the tests exercise.
func shrinkV2PromotionTimers(t *testing.T) {
	t.Helper()
	outbox.DeliveryTimeout = 2 * time.Second
	V2PromotionWait = 400 * time.Millisecond
	v2PromotionPoll = 40 * time.Millisecond
	t.Cleanup(func() {
		V2PromotionWait = 30 * time.Second
		v2PromotionPoll = 2 * time.Second
	})
}

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
// newVerifyEnv wires the e2e env + outbox with the PRODUCTION hook
// wiring (the same funcs Start installs): verifier + OnDelivered.
// With v2 true, the adapter is rebuilt with WithV2Store — the env the
// production wiring produces when OPENCODE_V2_DELIVERY is on (delivery
// and verification share the store; a V1-mode verifier against V2
// delivery reports false absence — the #987 duplicate class).
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

// TestOutboxDeliver_V2CompletesAtPromotion (#1119 fix): with SetV2Delivery
// on, admission alone must NOT complete the entry — the user text persists
// at PROMOTION, and a defect-class death (model-resolve, park) consumes or
// strands the admitted row with no signal (the 10:24Z production anatomy:
// sent fired at admission, the turn died pre-persist, the message
// vanished). The deliverer now waits for promotion — proven by the
// persisted text, the same oracle the #987 verifier uses — and completes
// only then. Happy path: promotion lands within the window, entry
// completes in one worker pass, queue.update/sent fires at real delivery.
func TestOutboxDeliver_V2CompletesAtPromotion(t *testing.T) {
	shrinkOutboxTimers(t)
	shrinkV2PromotionTimers(t)
	backend := &fakeAgentBackend{persistFirst: true, promoteDelay: 0}
	env := newVerifyEnv(t, backend)
	env.handler.SetV2Delivery(true)
	events := subscribeQueueUpdates(t, env)

	w := postPrompt(t, env, `{"clientMessageID":"cm-v2","parts":[{"type":"text","text":"v2 admission"}]}`)
	require.Equal(t, http.StatusAccepted, w.Code)

	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
	assert.Empty(t, listOutbox(t, env), "promoted admission completes the entry in one pass")

	backend.mu.Lock()
	admits, posts := backend.admits, backend.posts
	backend.mu.Unlock()
	assert.Equal(t, 1, admits, "exactly one V2 admission POST")
	assert.Zero(t, posts, "the V1 turn-blocking sync send must NOT be used in V2 mode")

	select {
	case e := <-events:
		data, _ := json.Marshal(e.Data)
		assert.Contains(t, string(data), `"sent"`, "queue.update/sent fires at real delivery (promotion), not at admission")
	case <-time.After(2 * time.Second):
		t.Fatal("no queue.update/sent emitted on V2 promotion")
	}
}

// TestOutboxDeliver_V2NoPromotionNeverFalselyCompletes (#1119): an
// admission whose row never promotes (defect-class death) must NOT
// complete — the entry goes ambiguous→verifying, the verifier's
// absent-after-window verdict re-admits (the nudge), bounded by
// Attempts. The sent-at-admission silent drop is impossible.
func TestOutboxDeliver_V2NoPromotionNeverFalselyCompletes(t *testing.T) {
	shrinkOutboxTimers(t)
	shrinkV2PromotionTimers(t)
	backend := &fakeAgentBackend{promoteDelay: -1} // never promotes
	env := newVerifyEnv(t, backend)
	env.handler.SetV2Delivery(true)

	w := postPrompt(t, env, `{"clientMessageID":"cm-dead","parts":[{"type":"text","text":"dead row"}]}`)
	require.Equal(t, http.StatusAccepted, w.Code)

	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
	entries := listOutbox(t, env)
	require.Len(t, entries, 1, "a never-promoted admission must NOT complete at pickup")
	assert.Equal(t, outbox.StatusVerifying, entries[0].Status,
		"unobserved promotion is outcome-unknown: verify, never blind-complete (#987)")
	assert.Contains(t, entries[0].LastError, "promotion", "the await-window expiry is the ambiguity cause")

	// Pass 2 (verifying round): resolves absent — nothing ever
	// persisted — and flips the entry to pending-with-backoff for a
	// re-admit. Pass 3 executes the nudge: a second admission, which
	// again awaits promotion (still none → verifying again, bounded by
	// Attempts). No silent completion anywhere in the sequence.
	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
	entries = listOutbox(t, env)
	require.Len(t, entries, 1)
	assert.Equal(t, outbox.StatusPending, entries[0].Status, "absent-after-window verdict schedules the re-admit nudge")

	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
	entries = listOutbox(t, env)
	require.Len(t, entries, 1)
	assert.NotEqual(t, outbox.StatusError, entries[0].Status, "first nudge is a re-admit, not an error park")
	backend.mu.Lock()
	admits := backend.admits
	backend.mu.Unlock()
	assert.GreaterOrEqual(t, admits, 2, "the nudge re-admits after the window")
}

// TestOutboxDeliver_V2SlowPromotionCompletesInWindow (#1119): promotion
// that lands inside the await window completes without any verifying
// round — the poll observes it.
func TestOutboxDeliver_V2SlowPromotionCompletesInWindow(t *testing.T) {
	shrinkOutboxTimers(t)
	shrinkV2PromotionTimers(t)
	backend := &fakeAgentBackend{promoteDelay: 3 * v2PromotionPoll}
	env := newVerifyEnv(t, backend)
	env.handler.SetV2Delivery(true)

	w := postPrompt(t, env, `{"clientMessageID":"cm-slow","parts":[{"type":"text","text":"slow promotion"}]}`)
	require.Equal(t, http.StatusAccepted, w.Code)

	require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
	assert.Empty(t, listOutbox(t, env), "promotion inside the window completes the entry — no verifying round")

	backend.mu.Lock()
	admits := backend.admits
	backend.mu.Unlock()
	assert.Equal(t, 1, admits, "no nudge: the single admission eventually promoted")
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
			"an HTTP-status rejection is definitive: bounded retry with backoff, not the verifying round-trip")
		assert.Equal(t, 1, entries[0].Attempts)
	})

	t.Run("admission transport cut is ambiguous — verifying, never blind-retried", func(t *testing.T) {
		backend := &fakeAgentBackend{admitReset: true, persistFirst: true}
		env := newVerifyEnv(t, backend, true)
		env.handler.SetV2Delivery(true)

		w := postPrompt(t, env, `{"clientMessageID":"cm-cut","parts":[{"type":"text","text":"connection cut"}]}`)
		require.Equal(t, http.StatusAccepted, w.Code)

		require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
		entries := listOutbox(t, env)
		require.Len(t, entries, 1)
		assert.Equal(t, outbox.StatusVerifying, entries[0].Status,
			"unknown-outcome transport cut must verify, never re-send blindly (#987)")

		backend.mu.Lock()
		admits := backend.admits
		backend.mu.Unlock()
		assert.Equal(t, 1, admits, "exactly one admission attempt so far")

		// The store read confirms delivery (persistFirst modeled the
		// cut AFTER admission landed) — entry resolves and leaves.
		time.Sleep(15 * time.Millisecond)
		require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
		assert.Empty(t, listOutbox(t, env))
	})

	t.Run("V2 store 5xx during verify is inconclusive — stays verifying, absent is never claimed", func(t *testing.T) {
		backend := &fakeAgentBackend{admitReset: true, persistFirst: false,
			v2StoreStatus: http.StatusServiceUnavailable, v2StoreBody: `agent starting`}
		env := newVerifyEnv(t, backend, true)
		env.handler.SetV2Delivery(true)

		w := postPrompt(t, env, `{"clientMessageID":"cm-s5","parts":[{"type":"text","text":"store down"}]}`)
		require.Equal(t, http.StatusAccepted, w.Code)

		require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
		time.Sleep(15 * time.Millisecond)
		require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
		entries := listOutbox(t, env)
		require.Len(t, entries, 1)
		assert.Equal(t, outbox.StatusVerifying, entries[0].Status,
			"an unreachable store is inconclusive — it must never be read as definitive absence (which would re-send and duplicate)")
	})

	t.Run("V2 store malformed body during verify is inconclusive", func(t *testing.T) {
		backend := &fakeAgentBackend{admitReset: true, persistFirst: false,
			v2StoreStatus: http.StatusOK, v2StoreBody: `<html>not json</html>`}
		env := newVerifyEnv(t, backend, true)
		env.handler.SetV2Delivery(true)

		w := postPrompt(t, env, `{"clientMessageID":"cm-mal","parts":[{"type":"text","text":"garbage store"}]}`)
		require.Equal(t, http.StatusAccepted, w.Code)

		require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
		time.Sleep(15 * time.Millisecond)
		require.True(t, env.handler.DeliverOutboxOnceForTest("ws-1", "ses_1"))
		entries := listOutbox(t, env)
		require.Len(t, entries, 1)
		assert.Equal(t, outbox.StatusVerifying, entries[0].Status,
			"a decode failure is inconclusive, never definitive absence")
	})
}
