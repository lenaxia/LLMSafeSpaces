// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// session_notifier_wiring_e2e_test.go — session idle-subscriptions v1,
// task 4: the FULL production wiring under test. Everything the battery
// touches is the real machinery: newStateAuthority (the production
// store reader + actor + probes against a fake harness HTTP server),
// wireSessionNotifier / startSessionNotifierLoops (main.go's wiring,
// invoked exactly as runMain invokes it), the real authority Stream +
// snapshot, and the real seam-POST vehicle (seamSettleDeliverer over
// the loopback client). The only fakes are the harness HTTP server
// (opencode stand-in: session list, live registry, asks, the V1
// message POST target) and the event source (Authority.IngestForTest —
// the reconcile_publish_1722_test.go pattern: synthetic scripts across
// the REAL fold + fanout).
//
// Battery (the charter's six):
//   (a) harness idle settle AND #1722-style evidence settle each yield
//       exactly one debounced notification POST;
//   (b) ERROR settle notifies with the error wording;
//   (c) suppression e2e both directions (completed send_message to the
//       subscriber then idle ⇒ no POST; edit after the send ⇒ POST);
//   (d) debounce cancel (idle→busy inside the window ⇒ no POST; the
//       later idle ⇒ exactly one POST);
//   (e) consumer drop + resubscribe ⇒ latch reconciliation through the
//       real Stream snapshot, no loss/dup;
//   (f) agentd restart (store reload from disk) ⇒ outbox retried
//       exactly once, latches survive.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// ---- the fake harness (opencode stand-in) ---------------------------------

// fakeHarnessSession is one GET /session list row.
type fakeHarnessSession struct {
	id       string
	title    string
	parentID string
}

// fakePost is one recorded V1 message POST (the notification vehicle's
// wire shape: {"parts":[{"type":"text","text":…}]}).
type fakePost struct {
	path string
	body string
}

// fakeHarness serves every route the wiring touches: the session list
// (ListSessions for the store reader + SessionListRaw for the lineage
// pull — one body serves both decoders), the live turn registry
// (#1722's settle gate), the ask lists (authoritative-empty), the V1
// message POST target, and per-session GETs. failPosts arms 500s on
// the POST route (the outbox scenario); liveBusy seeds /session/status.
type fakeHarness struct {
	srv      *httptest.Server
	mu       sync.Mutex
	posts    []fakePost
	failPost bool
	sessions map[string]fakeHarnessSession
	liveBusy map[string]bool
}

func newFakeHarness(t *testing.T, sessions ...fakeHarnessSession) *fakeHarness {
	t.Helper()
	h := &fakeHarness{
		sessions: map[string]fakeHarnessSession{},
		liveBusy: map[string]bool{},
	}
	for _, s := range sessions {
		h.sessions[s.id] = s
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/session/status", func(w http.ResponseWriter, _ *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		out := map[string]map[string]string{}
		for id, busy := range h.liveBusy {
			if busy {
				out[id] = map[string]string{"type": "busy"}
			}
		}
		writeJSONForTest(w, out)
	})
	mux.HandleFunc("/session", func(w http.ResponseWriter, _ *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		out := make([]map[string]any, 0, len(h.sessions))
		for _, s := range h.sessions {
			out = append(out, map[string]any{"id": s.id, "title": s.title, "parentID": s.parentID})
		}
		writeJSONForTest(w, out)
	})
	mux.HandleFunc("/session/", func(w http.ResponseWriter, r *http.Request) {
		rest := r.URL.Path[len("/session/"):]
		if r.Method == http.MethodPost && len(rest) > len("/message") && rest[len(rest)-len("/message"):] == "/message" {
			sid := rest[:len(rest)-len("/message")]
			var body struct {
				Parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"parts"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			text := ""
			for _, p := range body.Parts {
				text += p.Text
			}
			h.mu.Lock()
			h.posts = append(h.posts, fakePost{path: r.URL.Path, body: text})
			fail := h.failPost
			h.mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"info":{"id":"msg_%s"},"parts":[]}`, sid)
			return
		}
		if r.Method == http.MethodGet {
			h.mu.Lock()
			s, ok := h.sessions[rest]
			h.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			writeJSONForTest(w, map[string]any{"id": s.id, "title": s.title, "parentID": s.parentID})
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/question", func(w http.ResponseWriter, _ *http.Request) { writeJSONForTest(w, []any{}) })
	mux.HandleFunc("/permission", func(w http.ResponseWriter, _ *http.Request) { writeJSONForTest(w, []any{}) })
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONForTest(w, map[string]any{"healthy": true, "version": "test-harness"})
	})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func writeJSONForTest(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (h *fakeHarness) postCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.posts)
}

func (h *fakeHarness) post(i int) fakePost {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.posts[i]
}

func (h *fakeHarness) setFailPost(fail bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failPost = fail
}

func waitForHarnessPosts(t *testing.T, h *fakeHarness, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return h.postCount() >= n },
		4*time.Second, 5*time.Millisecond, "the harness never served %d notification POSTs (got %d)", n, h.postCount())
}

func assertNoHarnessPosts(t *testing.T, h *fakeHarness, wait time.Duration, msg string) {
	t.Helper()
	time.Sleep(wait)
	assert.Equal(t, 0, h.postCount(), "%s (got %d posts)", msg, h.postCount())
}

// ---- the wiring environment -----------------------------------------------

// wiringEnv is one generation of the production wiring against the
// fake harness: real authority, real wiring functions, real loops. The
// consumer can be stopped/relaunched (the drop/resubscribe scenario)
// and the whole engine rewired from the same store path (the restart
// scenario).
type wiringEnv struct {
	harness   *fakeHarness
	authority *sessionstate.Authority
	password  string
	storePath string
	notifier  *sessionNotifier
	cancel    context.CancelFunc
	wg        *sync.WaitGroup
}

// newWiringEnv builds the full stack. debounce arms the
// AGENTD_NOTIFY_DEBOUNCE knob — the wiring resolves it from the
// environment exactly as production does (and the knob is thereby
// pinned end-to-end on every test in this file).
func newWiringEnv(t *testing.T, debounce time.Duration, sessions ...fakeHarnessSession) *wiringEnv {
	t.Helper()
	h := newFakeHarness(t, sessions...)

	origAddr := getAgentAddr()
	t.Cleanup(func() { setAgentAddr(origAddr) })
	setAgentAddr(h.srv.URL)

	t.Setenv("LLMSAFESPACES_PLATFORM_DIR", t.TempDir())
	t.Setenv("AGENTD_NOTIFY_DEBOUNCE", debounce.String())

	const password = "pw"
	client := &OpenCodeClient{password: password, client: &http.Client{Timeout: 5 * time.Second}}
	authority := newStateAuthority(client, password, "")
	require.NotNil(t, authority, "production authority wiring against the fake harness")
	t.Cleanup(func() { _ = authority.Close() })

	env := &wiringEnv{
		harness:   h,
		authority: authority,
		password:  password,
		storePath: filepath.Join(t.TempDir(), "session-subscriptions.json"),
	}
	env.rewire(t)
	return env
}

// rewire constructs (and globally installs) a fresh engine generation
// over the same store path + harness — the agentd-restart leg. Does
// NOT start the loops; call startConsumer.
func (e *wiringEnv) rewire(t *testing.T) *sessionNotifier {
	t.Helper()
	prev := currentSessionNotifier()
	t.Cleanup(func() { setSessionNotifier(prev) })
	e.notifier = wireSessionNotifier(sessionNotifierWiring{
		Authority: e.authority,
		Password:  e.password,
		StorePath: e.storePath,
		Logger:    zap.NewNop(),
	})
	require.NotNil(t, currentSessionNotifier())
	assert.Same(t, e.notifier, currentSessionNotifier(), "the wired engine is the MCP surface's engine")
	return e.notifier
}

// startConsumer launches the wiring's loops on a fresh context; the
// cleanup joins them (the clean-shutdown pin: cancel must drain the
// WaitGroup promptly).
func (e *wiringEnv) startConsumer(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}
	e.cancel = cancel
	e.wg = wg
	startSessionNotifierLoops(ctx, wg, e.notifier)
	t.Cleanup(func() { e.stopConsumer(t) })
	require.Eventually(t, func() bool { return e.authority.Metrics().Subscribers >= 1 },
		2*time.Second, 5*time.Millisecond, "the notifier never subscribed to the authority stream")
}

// stopConsumer cancels the loops' context and JOINS them — bounded, so
// a leaked Run/ticker/POST fails loudly instead of hanging the binary.
func (e *wiringEnv) stopConsumer(t *testing.T) {
	t.Helper()
	if e.cancel == nil {
		return
	}
	e.cancel()
	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("session notifier loops did not drain within 5s of cancel — lifecycle is not clean")
	}
	e.cancel = nil
}

// ---- synthetic event scripts (the Authority.Ingest pattern) ----------------

func e2eStatusEvt(a *sessionstate.Authority, sid string, st abiv1.SessionStatus) {
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: sid, Status: st})
}

func e2eToolEndEvt(a *sessionstate.Authority, sid, partID, name, input string, st abiv1.ToolStatus) {
	a.IngestForTest(&abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_PART_END, SessionId: sid, PartId: partID,
		Part: &abiv1.Part{Id: partID, Type: abiv1.PartType_PART_TYPE_TOOL,
			Payload: &abiv1.Part_Tool{Tool: &abiv1.ToolPart{
				CallId: "call_" + partID, Name: name, Input: []byte(input),
				State: &abiv1.ToolState{Status: st},
			}}},
	})
}

// e2eWedgeBusy reproduces the #1722 wedge: busy mark + a RUNNING tool
// part whose terminal update is about to be lost.
func e2eWedgeBusy(a *sessionstate.Authority, sid string) {
	e2eStatusEvt(a, sid, abiv1.SessionStatus_SESSION_STATUS_BUSY)
	a.IngestForTest(&abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_PART_START, SessionId: sid, PartId: "prt_run",
		Part: &abiv1.Part{Id: "prt_run", Type: abiv1.PartType_PART_TYPE_TOOL,
			Payload: &abiv1.Part_Tool{Tool: &abiv1.ToolPart{
				CallId: "call_run", Name: "bash", Input: []byte(`{"command":"sleep 2880"}`),
				State: &abiv1.ToolState{Status: abiv1.ToolStatus_TOOL_STATUS_RUNNING},
			}}},
	})
}

const (
	e2eBroadcaster = "ses_worker"
	e2eSubscriber  = "ses_manager"
)

func e2eSubscribe(t *testing.T, env *wiringEnv, events []string) {
	t.Helper()
	_, err := env.notifier.Subscribe(e2eBroadcaster, e2eSubscriber, events)
	require.NoError(t, err)
}

// ---- (a) exactly one notification per settle, both settle origins ----------

// TestWiringE2E_HarnessIdleSettleNotifiesOnce: the plain busy→idle
// fold through the real stream, real debounce, real vehicle — exactly
// one POST, outbox drained, nothing more follows.
func TestWiringE2E_HarnessIdleSettleNotifiesOnce(t *testing.T) {
	env := newWiringEnv(t, 40*time.Millisecond,
		fakeHarnessSession{id: e2eBroadcaster, title: "T4 worker"},
		fakeHarnessSession{id: e2eSubscriber, title: "T4 manager"},
	)
	env.startConsumer(t)
	e2eSubscribe(t, env, nil)

	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_BUSY)
	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_IDLE)

	waitForHarnessPosts(t, env.harness, 1)
	post := env.harness.post(0)
	assert.Equal(t, "/session/"+e2eSubscriber+"/message", post.path)
	assert.Contains(t, post.body, "is now idle — its turn is complete")
	assert.Contains(t, post.body, "lsp:agent-message-v1", "the v1 provenance sentinel stamps the body")

	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 1, env.harness.postCount(), "exactly one notification per busy episode")
	require.Equal(t, 0, env.notifier.store.outboxLen(), "the delivered intent cleared the outbox")
}

// TestWiringE2E_EvidenceSettleNotifiesOnce: the #1722 pin through the
// FULL wiring — the wedge's clear burst never arrives; the reconcile
// pass's SYNTHETIC idle publication (what a harness-SSE hook would
// never see) drives the notification. Exactly one POST.
func TestWiringE2E_EvidenceSettleNotifiesOnce(t *testing.T) {
	env := newWiringEnv(t, 40*time.Millisecond,
		fakeHarnessSession{id: e2eBroadcaster, title: "T4 worker"},
		fakeHarnessSession{id: e2eSubscriber, title: "T4 manager"},
	)
	env.startConsumer(t)
	e2eSubscribe(t, env, nil)

	e2eWedgeBusy(env.authority, e2eBroadcaster)
	require.True(t, env.authority.State().Sessions[e2eBroadcaster].Busy, "precondition: wedged busy")

	stats := env.authority.Reconcile(context.Background())
	require.NotZero(t, stats.BusyCleared, "the evidence settle published (the harness list is idle, the registry holds no live turn)")

	waitForHarnessPosts(t, env.harness, 1)
	assert.Contains(t, env.harness.post(0).body, "is now idle — its turn is complete")
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 1, env.harness.postCount(), "exactly one notification for the synthetic settle")
}

// ---- (b) ERROR settle wording ----------------------------------------------

func TestWiringE2E_ErrorSettleNotifiesWithErrorWording(t *testing.T) {
	env := newWiringEnv(t, 40*time.Millisecond,
		fakeHarnessSession{id: e2eBroadcaster, title: "T4 worker"},
		fakeHarnessSession{id: e2eSubscriber, title: "T4 manager"},
	)
	env.startConsumer(t)
	e2eSubscribe(t, env, nil) // default event set wakes on error too (ruling 1)

	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_BUSY)
	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_ERROR)

	waitForHarnessPosts(t, env.harness, 1)
	assert.Contains(t, env.harness.post(0).body, "ended its turn in error — the work may be incomplete",
		"the manager must read death differently from completion")
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 1, env.harness.postCount())
}

// ---- (c) suppression e2e, both directions ----------------------------------

// TestWiringE2E_SuppressionSendThenIdleSuppresses: the broadcaster's
// COMPLETED send_message to the subscriber postdates its every
// mutation — the settle POST is suppressed (the subscriber already
// holds the final state).
func TestWiringE2E_SuppressionSendThenIdleSuppresses(t *testing.T) {
	env := newWiringEnv(t, 40*time.Millisecond,
		fakeHarnessSession{id: e2eBroadcaster, title: "T4 worker"},
		fakeHarnessSession{id: e2eSubscriber, title: "T4 manager"},
	)
	env.startConsumer(t)
	e2eSubscribe(t, env, nil)

	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_BUSY)
	e2eToolEndEvt(env.authority, e2eBroadcaster, "prt_send", "send_message",
		`{"destination":"`+e2eSubscriber+`"}`, abiv1.ToolStatus_TOOL_STATUS_COMPLETED)
	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_IDLE)

	assertNoHarnessPosts(t, env.harness, 350*time.Millisecond,
		"the already-notified rule suppressed the settle notification")
	require.Equal(t, 0, env.notifier.store.outboxLen(), "a suppressed settle enqueues nothing")
}

// TestWiringE2E_SuppressionEditAfterSendNotifies: an edit AFTER the
// send invalidates the already-informed state — the notification
// fires.
func TestWiringE2E_SuppressionEditAfterSendNotifies(t *testing.T) {
	env := newWiringEnv(t, 40*time.Millisecond,
		fakeHarnessSession{id: e2eBroadcaster, title: "T4 worker"},
		fakeHarnessSession{id: e2eSubscriber, title: "T4 manager"},
	)
	env.startConsumer(t)
	e2eSubscribe(t, env, nil)

	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_BUSY)
	e2eToolEndEvt(env.authority, e2eBroadcaster, "prt_send", "send_message",
		`{"destination":"`+e2eSubscriber+`"}`, abiv1.ToolStatus_TOOL_STATUS_COMPLETED)
	e2eToolEndEvt(env.authority, e2eBroadcaster, "prt_edit", "edit",
		`{"file_path":"/workspace/x.go"}`, abiv1.ToolStatus_TOOL_STATUS_COMPLETED)
	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_IDLE)

	waitForHarnessPosts(t, env.harness, 1)
	assert.Contains(t, env.harness.post(0).body, "is now idle")
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 1, env.harness.postCount())
}

// ---- (d) debounce cancel ----------------------------------------------------

// TestWiringE2E_DebounceCancelOnRebusy: idle→busy INSIDE the window
// cancels the armed notification (the boundary flutter); the episode's
// true end fires exactly one. Long debounce (750ms) keeps the re-busy
// deterministically inside the window on a loaded runner.
func TestWiringE2E_DebounceCancelOnRebusy(t *testing.T) {
	env := newWiringEnv(t, 750*time.Millisecond,
		fakeHarnessSession{id: e2eBroadcaster, title: "T4 worker"},
		fakeHarnessSession{id: e2eSubscriber, title: "T4 manager"},
	)
	env.startConsumer(t)
	e2eSubscribe(t, env, nil)

	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_BUSY)
	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_IDLE) // arms 750ms
	time.Sleep(80 * time.Millisecond)                                                    // well inside the window
	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_BUSY) // the queued message starts the next turn

	assertNoHarnessPosts(t, env.harness, 400*time.Millisecond,
		"the idle→busy flutter inside the window canceled the notification")

	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_IDLE) // the true end
	waitForHarnessPosts(t, env.harness, 1)
	assert.Contains(t, env.harness.post(0).body, "is now idle")
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 1, env.harness.postCount(), "one notification for the whole episode — the cancel lost nothing and the fire duplicated nothing")
}

// ---- (e) consumer drop + resubscribe ---------------------------------------

// TestWiringE2E_DropResubscribeLatchReconciliation: the consumer goes
// away mid-episode (the M3.4 slow-consumer drop's aftermath — the
// channel closes, Run reconnects; modeled here by stopping and
// relaunching the loops, the identical resubscribe path). While it is
// gone the wedge settles through the evidence pass; the resubscribe's
// mandatory snapshot frame reconciles latch-vs-snapshot (latched busy +
// snapshot idle ⇒ the settle) and fires EXACTLY once — no loss, no
// duplication.
func TestWiringE2E_DropResubscribeLatchReconciliation(t *testing.T) {
	env := newWiringEnv(t, 40*time.Millisecond,
		fakeHarnessSession{id: e2eBroadcaster, title: "T4 worker"},
		fakeHarnessSession{id: e2eSubscriber, title: "T4 manager"},
	)
	env.startConsumer(t)
	e2eSubscribe(t, env, nil)

	e2eWedgeBusy(env.authority, e2eBroadcaster)
	require.Eventually(t, func() bool {
		return env.notifier.store.latchFor(e2eBroadcaster).SawBusy
	}, 2*time.Second, 5*time.Millisecond, "the busy onset latched before the drop")

	// The drop: consumer gone. The clear burst never arrives AND the
	// reconcile settle publishes to zero subscribers — the episode
	// finishes unwitnessed.
	env.stopConsumer(t)
	stats := env.authority.Reconcile(context.Background())
	require.NotZero(t, stats.BusyCleared, "the episode settled while the consumer was down")
	require.False(t, env.authority.State().Sessions[e2eBroadcaster].Busy)
	assert.Equal(t, 0, env.harness.postCount(), "nothing was watching — no POST")

	// Resubscribe: the fresh connection's snapshot performs the latch
	// reconciliation. Loss would be a silent manager; duplication would
	// be a storm — neither is acceptable.
	env.startConsumer(t)
	waitForHarnessPosts(t, env.harness, 1)
	assert.Contains(t, env.harness.post(0).body, "is now idle")
	time.Sleep(400 * time.Millisecond)
	assert.Equal(t, 1, env.harness.postCount(), "the reconciled settle notified exactly once")
	require.Equal(t, 0, env.notifier.store.outboxLen())
}

// ---- (f) agentd restart: store reload, outbox retried once ------------------

// TestWiringE2E_RestartOutboxRetriedOnceLatchesSurvive: a generation
// dies with a durable intent (the POST failed 500 — write-before-send
// left it in the outbox); the next generation reloads the registry from
// disk, retries the intent EXACTLY once, and the saw-busy latch
// survived the reload.
func TestWiringE2E_RestartOutboxRetriedOnceLatchesSurvive(t *testing.T) {
	env := newWiringEnv(t, 40*time.Millisecond,
		fakeHarnessSession{id: e2eBroadcaster, title: "T4 worker"},
		fakeHarnessSession{id: e2eSubscriber, title: "T4 manager"},
	)
	env.startConsumer(t)
	e2eSubscribe(t, env, nil)
	env.harness.setFailPost(true)

	retriedBefore := notifyOutcome(t, "outbox_retried_delivered")

	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_BUSY)
	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_IDLE)

	// The failed POST is recorded, the intent stays durable.
	waitForHarnessPosts(t, env.harness, 1)
	require.Eventually(t, func() bool { return env.notifier.store.outboxLen() == 1 },
		2*time.Second, 5*time.Millisecond, "the failed delivery left the intent in the outbox")

	// The restart: consumer drained, whole engine rewired from the same
	// store path (a fresh store instance — the reload), harness healed.
	env.stopConsumer(t)
	env.harness.setFailPost(false)
	retried := env.rewire(t)

	// The reload carried the outbox intent AND the latch — durable
	// state across generations, before any loop runs.
	require.Len(t, retried.store.pendingNotifications(), 1, "the outbox intent survived the restart on disk")
	latch := retried.store.latchFor(e2eBroadcaster)
	assert.True(t, latch.SawBusy, "the saw-busy latch survived the restart (the attribution)")

	env.startConsumer(t) // the boot leg: harness answers, RetryOutbox drains once
	waitForHarnessPosts(t, env.harness, 2)
	assert.Contains(t, env.harness.post(1).body, "is now idle")
	require.Eventually(t, func() bool { return retried.store.outboxLen() == 0 },
		2*time.Second, 5*time.Millisecond, "the retried delivery cleared the intent")

	time.Sleep(400 * time.Millisecond)
	assert.Equal(t, 2, env.harness.postCount(), "the retry fired exactly once — never an unbounded ladder")
	assert.InDelta(t, 1, notifyOutcome(t, "outbox_retried_delivered")-retriedBefore, 0,
		"the outbox_retried_delivered counter ticked exactly once for this restart")
}

// ---- the knob + surface-swap wiring pins ------------------------------------

// TestWiringE2E_KnobAndEngineSwap: AGENTD_NOTIFY_DEBOUNCE flows through
// the wiring into the armed timers; a malformed value falls back to the
// 5s default; the wired engine is installed on the MCP surface.
func TestWiringE2E_KnobAndEngineSwap(t *testing.T) {
	env := newWiringEnv(t, 31*time.Millisecond, fakeHarnessSession{id: "ses_x"})
	assert.Equal(t, 31*time.Millisecond, env.notifier.debounce, "the AGENTD_NOTIFY_DEBOUNCE knob rode the wiring")

	prev := currentSessionNotifier()
	t.Cleanup(func() { setSessionNotifier(prev) })
	t.Setenv("AGENTD_NOTIFY_DEBOUNCE", "not-a-duration")
	n := wireSessionNotifier(sessionNotifierWiring{StorePath: filepath.Join(t.TempDir(), "s.json")})
	assert.Equal(t, notifyDebounceDefault, n.debounce, "malformed knob → the 5s default")
	assert.NotNil(t, currentSessionNotifier().store, "the unwired-authority generation still carries the durable registry")
	assert.Nil(t, currentSessionNotifier().cfg.Stream, "nil authority wires no stream — Run degrades to idle")
}
