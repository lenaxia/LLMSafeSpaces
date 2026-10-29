// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// session_notifier_delivery_test.go — task 2 of the subscription
// feature: the §4.4 suppression matrix, the §4.5 compose + delivery
// vehicle (detached POST, outbox ordering, boot retry-once), the
// per-subscriber rate bucket, and the §8 outcome counters. The
// frame-driven cases feed synthesized sequenced frames with EXPLICIT
// seqs (the suppression rule is an ordering predicate — equal-seq
// fixtures would degenerate to "never newer"); the vehicle cases drive
// a real seamSettleDeliverer against an httptest seam.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/session/agentmessage"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ---- helpers ----

// seqEvtFrame is evtFrame with an EXPLICIT seq (the suppression
// tracker's ordering input; task 1's helper pins Seq=1).
func seqEvtFrame(sid string, seq uint64, typ abiv1.EventType, mutate func(*abiv1.Event)) *abiv1.StreamFrame {
	evt := &abiv1.Event{Type: typ, SessionId: sid, InternalSessionId: sid}
	if mutate != nil {
		mutate(evt)
	}
	return &abiv1.StreamFrame{Frame: &abiv1.StreamFrame_Event{Event: &abiv1.SequencedEvent{Seq: seq, Event: evt}}}
}

func seqStatusFrame(sid string, seq uint64, st abiv1.SessionStatus) *abiv1.StreamFrame {
	return seqEvtFrame(sid, seq, abiv1.EventType_EVENT_TYPE_SESSION_STATUS, func(e *abiv1.Event) { e.Status = st })
}

// toolEndFrame is one broadcaster tool leg: a PART_END carrying a
// ToolPart with name/raw-JSON-input/state (contract.proto:110-126).
func toolEndFrame(sid string, seq uint64, name, input string, status abiv1.ToolStatus) *abiv1.StreamFrame {
	return seqEvtFrame(sid, seq, abiv1.EventType_EVENT_TYPE_PART_END, func(e *abiv1.Event) {
		e.Part = &abiv1.Part{Id: "prt_" + name, Type: abiv1.PartType_PART_TYPE_TOOL, Payload: &abiv1.Part_Tool{Tool: &abiv1.ToolPart{
			CallId: "call_" + name,
			Name:   name,
			Input:  []byte(input),
			State:  &abiv1.ToolState{Status: status},
		}}}
	})
}

// notifyOutcome reads one §8 counter (the package-level family is a
// singleton — tests assert DELTAS around the exercised path).
func notifyOutcome(t *testing.T, outcome string) float64 {
	t.Helper()
	return testutil.ToFloat64(sessionNotifyMetrics.outcomes.WithLabelValues(outcome))
}

// withReplica swaps the process-wide short-id replica for one carrying
// the given phrase→canonical mappings (the session_ref_test swap, minus
// the warm worker — the suppression resolution reads memory only).
func withReplica(t *testing.T, mappings map[string]string) {
	t.Helper()
	s := newSessionShortIDStore(filepath.Join(t.TempDir(), "session-shortids.json"))
	for short, canonical := range mappings {
		s.put(short, canonical)
	}
	old := sessionShortIDsAtomic.Load().(*sessionShortIDStore)
	sessionShortIDsAtomic.Store(s)
	t.Cleanup(func() { sessionShortIDsAtomic.Store(old) })
}

// ---- §4.4 suppression matrix ----

// TestSuppressionSendThenIdleSuppresses: THE core row — a completed
// send_message to the subscriber with no mutation after it: the
// notification is a duplicate the subscriber must triage; suppressed.
func TestSuppressionSendThenIdleSuppresses(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", nil)
	before := notifyOutcome(t, notifyOutcomeSuppressed)

	f.n.handleFrame(seqStatusFrame("ses_w", 10, abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(toolEndFrame("ses_w", 11, "send_message",
		`{"destination":"ses_m","message":"final report"}`, abiv1.ToolStatus_TOOL_STATUS_COMPLETED))
	f.n.handleFrame(seqStatusFrame("ses_w", 12, abiv1.SessionStatus_SESSION_STATUS_IDLE))

	assertNoNotice(t, f.deliver.ch, f.debounce*4, "the broadcaster's last word to the subscriber postdates its last mutation — already notified")
	assert.Equal(t, 0, f.store.outboxLen(), "a suppressed wake-up writes no outbox intent")
	assert.Greater(t, notifyOutcome(t, notifyOutcomeSuppressed), before, "suppressed_already_notified ticks")
}

// TestSuppressionSendMutateThenIdleNotifies: a mutation (edit/write/
// bash, completed) after the send un-suppresses — the subscriber's
// copy is stale again.
func TestSuppressionSendMutateThenIdleNotifies(t *testing.T) {
	for _, mut := range []string{"edit", "write", "bash"} {
		t.Run(mut, func(t *testing.T) {
			f := newNotifierFixture(t, nil)
			subscribeForWake(t, f, "ses_w", "ses_m", nil)

			f.n.handleFrame(seqStatusFrame("ses_w", 10, abiv1.SessionStatus_SESSION_STATUS_BUSY))
			f.n.handleFrame(toolEndFrame("ses_w", 11, "send_message",
				`{"destination":"ses_m","message":"draft"}`, abiv1.ToolStatus_TOOL_STATUS_COMPLETED))
			f.n.handleFrame(toolEndFrame("ses_w", 12, mut, `{"path":"x"}`, abiv1.ToolStatus_TOOL_STATUS_COMPLETED))
			f.n.handleFrame(seqStatusFrame("ses_w", 13, abiv1.SessionStatus_SESSION_STATUS_IDLE))

			notice := waitForNotice(t, f.deliver.ch)
			assert.Equal(t, "ses_w", notice.Broadcaster)
			assert.Equal(t, subscriptionEventIdle, notice.Event)
		})
	}
}

// TestSuppressionErroredSendDoesNotSuppress: a FAILED send_message
// informed nobody — the notification must still fire.
func TestSuppressionErroredSendDoesNotSuppress(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", nil)

	f.n.handleFrame(seqStatusFrame("ses_w", 10, abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(toolEndFrame("ses_w", 11, "send_message",
		`{"destination":"ses_m","message":"never landed"}`, abiv1.ToolStatus_TOOL_STATUS_ERROR))
	f.n.handleFrame(seqStatusFrame("ses_w", 12, abiv1.SessionStatus_SESSION_STATUS_IDLE))

	notice := waitForNotice(t, f.deliver.ch)
	assert.Equal(t, "ses_m", notice.Subscriber)
}

// TestSuppressionErroredMutationStillNotifies: the conservative row —
// an ERRORED edit/write/bash may still have mutated (the bash that
// half-ran); false suppression is the costly case, so it un-suppresses
// exactly like a completed one.
func TestSuppressionErroredMutationStillNotifies(t *testing.T) {
	for _, mut := range []string{"edit", "write", "bash"} {
		t.Run(mut, func(t *testing.T) {
			f := newNotifierFixture(t, nil)
			subscribeForWake(t, f, "ses_w", "ses_m", nil)

			f.n.handleFrame(seqStatusFrame("ses_w", 10, abiv1.SessionStatus_SESSION_STATUS_BUSY))
			f.n.handleFrame(toolEndFrame("ses_w", 11, "send_message",
				`{"destination":"ses_m","message":"draft"}`, abiv1.ToolStatus_TOOL_STATUS_COMPLETED))
			f.n.handleFrame(toolEndFrame("ses_w", 12, mut, `{"path":"x"}`, abiv1.ToolStatus_TOOL_STATUS_ERROR))
			f.n.handleFrame(seqStatusFrame("ses_w", 13, abiv1.SessionStatus_SESSION_STATUS_IDLE))

			notice := waitForNotice(t, f.deliver.ch)
			assert.Equal(t, subscriptionEventIdle, notice.Event)
		})
	}
}

// TestSuppressionPhraseAndCanonicalDestinations: all three addressing
// forms resolve to the canonical registry key — a phrase-form send
// suppresses exactly the canonical subscriber it named. Mutations
// between episodes reset the pair (the suppression ledger is
// per-broadcaster and monotone, so each episode's expectations are
// staged by seq).
func TestSuppressionPhraseAndCanonicalDestinations(t *testing.T) {
	withReplica(t, map[string]string{
		"crimson-finch-glides": "ses_m",
		"amber-wolf-runs":      "ses_other",
	})

	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", nil)
	subscribeForWake(t, f, "ses_w", "ses_other", nil)

	// Episode 1 — PHRASE form (any case; the replica normalizes)
	// targets ses_m: ses_m is informed, ses_other still wakes.
	f.n.handleFrame(seqStatusFrame("ses_w", 10, abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(toolEndFrame("ses_w", 11, "send_message",
		`{"destination":"Crimson-Finch-Glides","message":"done"}`, abiv1.ToolStatus_TOOL_STATUS_COMPLETED))
	f.n.handleFrame(seqStatusFrame("ses_w", 12, abiv1.SessionStatus_SESSION_STATUS_IDLE))
	notice := waitForNotice(t, f.deliver.ch)
	assert.Equal(t, "ses_other", notice.Subscriber)
	assertNoNotice(t, f.deliver.ch, f.debounce*3, "")

	// A mutation resets the ledger; episode 2 — CANONICAL form targets
	// ses_other: now ses_m wakes.
	f.n.handleFrame(toolEndFrame("ses_w", 15, "bash", `{"command":"ls"}`, abiv1.ToolStatus_TOOL_STATUS_COMPLETED))
	f.n.handleFrame(seqStatusFrame("ses_w", 20, abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(toolEndFrame("ses_w", 21, "send_message",
		`{"destination":"ses_other","message":"done"}`, abiv1.ToolStatus_TOOL_STATUS_COMPLETED))
	f.n.handleFrame(seqStatusFrame("ses_w", 22, abiv1.SessionStatus_SESSION_STATUS_IDLE))
	notice = waitForNotice(t, f.deliver.ch)
	assert.Equal(t, "ses_m", notice.Subscriber)
	assertNoNotice(t, f.deliver.ch, f.debounce*3, "")

	// Episode 3 — the LEGACY session_id spelling targets ses_m: the
	// other subscriber wakes.
	f.n.handleFrame(toolEndFrame("ses_w", 25, "bash", `{"command":"ls"}`, abiv1.ToolStatus_TOOL_STATUS_COMPLETED))
	f.n.handleFrame(seqStatusFrame("ses_w", 30, abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(toolEndFrame("ses_w", 31, "send_message",
		`{"session_id":"ses_m","message":"legacy alias"}`, abiv1.ToolStatus_TOOL_STATUS_COMPLETED))
	f.n.handleFrame(seqStatusFrame("ses_w", 32, abiv1.SessionStatus_SESSION_STATUS_IDLE))
	notice = waitForNotice(t, f.deliver.ch)
	assert.Equal(t, "ses_other", notice.Subscriber)
	assertNoNotice(t, f.deliver.ch, f.debounce*3, "")
}

// TestSuppressionUnresolvableDestinationNeverSuppresses: a destination
// the replica cannot resolve (cold window / alias / corrupted JSON)
// tracks nothing — the conservative direction.
func TestSuppressionUnresolvableDestinationNeverSuppresses(t *testing.T) {
	withReplica(t, map[string]string{}) // cold replica: nothing resolves

	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", nil)

	f.n.handleFrame(seqStatusFrame("ses_w", 10, abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(toolEndFrame("ses_w", 11, "send_message",
		`{"destination":"crimson-finch-glides","message":"cold replica"}`, abiv1.ToolStatus_TOOL_STATUS_COMPLETED))
	f.n.handleFrame(toolEndFrame("ses_w", 12, "send_message",
		`{"destination":`, abiv1.ToolStatus_TOOL_STATUS_COMPLETED)) // corrupted input JSON
	f.n.handleFrame(seqStatusFrame("ses_w", 13, abiv1.SessionStatus_SESSION_STATUS_IDLE))

	notice := waitForNotice(t, f.deliver.ch)
	assert.Equal(t, "ses_m", notice.Subscriber)
}

// TestSuppressionTrackerUnit: the pure ordering predicate, directly.
func TestSuppressionTrackerUnit(t *testing.T) {
	tr := newSuppressionTracker()
	assert.False(t, tr.suppresses("b", "s"), "no send recorded — never suppress")

	tr.observeSend("b", "s", 100)
	assert.True(t, tr.suppresses("b", "s"), "send with no mutation after — suppress")
	assert.False(t, tr.suppresses("b", "other"), "per-target scoping")
	assert.False(t, tr.suppresses("other", "s"), "per-broadcaster scoping")

	tr.observeMutation("b", 90) // an OLDER mutation changes nothing
	assert.True(t, tr.suppresses("b", "s"))

	tr.observeMutation("b", 150) // newer mutation un-suppresses
	assert.False(t, tr.suppresses("b", "s"))

	tr.observeSend("b", "s", 200) // a newer send re-suppresses
	assert.True(t, tr.suppresses("b", "s"))

	// Degenerate inputs never record.
	tr2 := newSuppressionTracker()
	tr2.observeSend("", "s", 5)
	tr2.observeSend("b", "", 5)
	tr2.observeMutation("", 5)
	assert.False(t, tr2.suppresses("b", "s"))

	// sendMessageDestination: destination wins, session_id is the
	// fallback, garbage yields "".
	assert.Equal(t, "ses_x", sendMessageDestination([]byte(`{"destination":"ses_x","message":"m"}`)))
	assert.Equal(t, "ses_legacy", sendMessageDestination([]byte(`{"session_id":"ses_legacy"}`)))
	assert.Equal(t, "", sendMessageDestination([]byte(`not json`)))
	assert.Equal(t, "", sendMessageDestination(nil))
}

// ---- §4.5 compose ----

func TestComposeSettleNotification(t *testing.T) {
	body, err := composeSettleNotification("crimson-finch-glides", "Fix the pump", subscriptionEventIdle)
	require.NoError(t, err)

	origin, found, text := agentmessage.Parse(body)
	require.True(t, found, "the v1 sentinel leads the notification")
	assert.Equal(t, "crimson-finch-glides", origin.FromSession, "fromSession is the broadcaster PHRASE")
	assert.Equal(t, notifySentinelMode, origin.Mode, "the additive mode is \"notify\"")
	assert.Equal(t, "notify", origin.Mode)
	assert.Contains(t, text, `session crimson-finch-glides ("Fix the pump") is now idle — its turn is complete.`)
	assert.Contains(t, text, "No reply required unless you want to act on it.")

	errBody, err := composeSettleNotification("amber-wolf-runs", "Investigate flake", subscriptionEventError)
	require.NoError(t, err)
	_, found, errText := agentmessage.Parse(errBody)
	require.True(t, found)
	assert.Contains(t, errText, `session amber-wolf-runs ("Investigate flake") ended its turn in error — the work may be incomplete.`)
	assert.NotContains(t, errText, "is now idle", "the ERROR wording is distinct")

	// Degenerate title: no empty parenthetical.
	bare, err := composeSettleNotification("solo-heron-waits", "", subscriptionEventIdle)
	require.NoError(t, err)
	assert.NotContains(t, bare, `("")`)
	assert.Contains(t, bare, "session solo-heron-waits is now idle")

	// Compose is idempotent (the sentinel contract): a re-compose of a
	// composed body never stacks sentinels.
	double, err := composeSettleNotification("solo-heron-waits", "", subscriptionEventIdle)
	require.NoError(t, err)
	_, found, _ = agentmessage.Parse(double)
	require.True(t, found)
	assert.Equal(t, 1, strings.Count(double, "lsp:agent-message-v1"))
}

// ---- §4.5 delivery: the detached vehicle ----

// seamSrv is a recording fake V1 seam: every POST /session/{id}/message
// lands in posts; per-test flags customize the response.
type seamSrv struct {
	srv      *httptest.Server
	mu       sync.Mutex
	posts    []seamPost
	fail     bool
	block    chan struct{} // non-nil ⇒ each request parks until released
	inflight chan struct{} // signaled when a request starts (block mode)
}

type seamPost struct {
	path      string
	body      string
	hasDeadln bool
	ctxErr    string
}

func newSeamSrv(t *testing.T) *seamSrv {
	t.Helper()
	s := &seamSrv{inflight: make(chan struct{}, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("/session/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/message") || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"parts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var sb strings.Builder
		for _, p := range body.Parts {
			sb.WriteString(p.Text)
		}
		_, hasDeadln := r.Context().Deadline()
		s.mu.Lock()
		s.posts = append(s.posts, seamPost{
			path:      r.URL.Path,
			body:      sb.String(),
			hasDeadln: hasDeadln,
			ctxErr:    ctxErrString(r.Context().Err()),
		})
		s.mu.Unlock()
		if s.block != nil {
			s.inflight <- struct{}{}
			<-s.block
		}
		if s.fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"info":{"id":"msg_fake"},"parts":[]}`))
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func ctxErrString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *seamSrv) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.posts)
}

func (s *seamSrv) last() seamPost {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posts[len(s.posts)-1]
}

// waitForSeamPosts blocks until the fake seam served n posts.
func waitForSeamPosts(t *testing.T, s *seamSrv, n int) int {
	t.Helper()
	require.Eventually(t, func() bool { return s.count() >= n },
		2*time.Second, 5*time.Millisecond, "the seam never served %d posts", n)
	return s.count()
}

// seamDeliverer wires the real vehicle at the fake seam.
func seamDeliverer(s *seamSrv) *seamSettleDeliverer {
	return newSeamSettleDeliverer(func() *opencode.Client {
		return opencode.NewLoopbackClient(s.srv.URL, "pw")
	}, nil, zap.NewNop())
}

// TestDeliveryDetachedWithoutCancelNoDeadline: the two §4.5 vehicle
// pins in one — a parent context CANCELED BEFORE the call must not
// cut the POST (context.WithoutCancel), and the served request must
// carry NO deadline (no artificial cap on the queue-wait that IS the
// wake-up).
func TestDeliveryDetachedWithoutCancelNoDeadline(t *testing.T) {
	s := newSeamSrv(t)
	d := seamDeliverer(s)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the delivery parent is already dead — the POST still runs
	err := d.DeliverSettle(ctx, settleNotice{
		Broadcaster: "ses_w", Subscriber: "ses_m", Event: subscriptionEventIdle, Title: "Fix the pump",
	})
	require.NoError(t, err, "the detached POST survives parent cancellation")

	require.Equal(t, 1, s.count())
	post := s.last()
	assert.Equal(t, "/session/ses_m/message", post.path)
	assert.False(t, post.hasDeadln, "no artificial deadline on the delivery request")
	assert.Empty(t, post.ctxErr, "WithoutCancel: the served context is live even though the parent was canceled")

	origin, found, _ := agentmessage.Parse(post.body)
	assert.True(t, found)
	assert.Equal(t, "notify", origin.Mode)
	assert.Contains(t, post.body, "is now idle — its turn is complete.")
}

// TestDeliveryCarriesErrorWording: an ERROR settle through the vehicle
// lands the "ended in error" body.
func TestDeliveryCarriesErrorWording(t *testing.T) {
	s := newSeamSrv(t)
	d := seamDeliverer(s)

	require.NoError(t, d.DeliverSettle(context.Background(), settleNotice{
		Broadcaster: "ses_w", Subscriber: "ses_m", Event: subscriptionEventError,
	}))
	require.Equal(t, 1, s.count())
	assert.Contains(t, s.last().body, "ended its turn in error — the work may be incomplete.")
}

// TestOutboxWriteBeforeSendClearOnSuccess: the at-least-once ordering
// against the REAL vehicle — the intent is durable while the POST is
// in flight, and cleared only after the seam acks.
func TestOutboxWriteBeforeSendClearOnSuccess(t *testing.T) {
	s := newSeamSrv(t)
	s.block = make(chan struct{})
	store := newSessionSubscriptionStore(storePath(t))
	n := newSessionNotifier(sessionNotifierConfig{
		Store:    store,
		Facts:    mapSessionFacts{subtasks: map[string]bool{}, gone: map[string]bool{}},
		Debounce: 25 * time.Millisecond,
		Deliver:  seamDeliverer(s),
		Logger:   zap.NewNop(),
	})
	t.Cleanup(n.cancelAllDebounces)
	_, err := n.Subscribe("ses_w", "ses_m", nil)
	require.NoError(t, err)

	n.handleFrame(seqStatusFrame("ses_w", 10, abiv1.SessionStatus_SESSION_STATUS_BUSY))
	n.handleFrame(seqStatusFrame("ses_w", 11, abiv1.SessionStatus_SESSION_STATUS_IDLE))

	select {
	case <-s.inflight:
	case <-time.After(2 * time.Second):
		t.Fatal("the delivery POST never started")
	}
	require.Equal(t, 1, store.outboxLen(), "the intent is durable BEFORE the POST completes (ruling 4)")
	close(s.block)

	require.Eventually(t, func() bool { return store.outboxLen() == 0 },
		2*time.Second, 10*time.Millisecond, "the ack clears the intent")
	re := newSessionSubscriptionStore(store.path)
	assert.Equal(t, 0, re.outboxLen(), "the clear is durable across a restart")
}

// TestOutboxRetryOnceOnRestart: ruling 4's boot drain — intents a prior
// generation left durable are retried EXACTLY once: delivered clears;
// a failed retry drops with the loud warn (never an unbounded ladder).
func TestOutboxRetryOnceOnRestart(t *testing.T) {
	t.Run("delivered", func(t *testing.T) {
		s := newSeamSrv(t)
		before := notifyOutcome(t, notifyOutcomeOutboxRetriedSent)

		path := storePath(t)
		store := newSessionSubscriptionStore(path)
		store.enqueue(pendingNotification{Broadcaster: "ses_w", Subscriber: "ses_m",
			Event: subscriptionEventIdle, EnqueuedAt: time.Now().Add(-time.Minute)})
		re := newSessionSubscriptionStore(path)
		require.Equal(t, 1, re.outboxLen(), "the intent survived the restart (durable before send)")

		n := newSessionNotifier(sessionNotifierConfig{Store: re, Deliver: seamDeliverer(s), Logger: zap.NewNop()})
		n.RetryOutbox(context.Background())

		assert.Equal(t, 0, re.outboxLen(), "a successful retry clears the intent")
		assert.Equal(t, 1, s.count())
		assert.Greater(t, notifyOutcome(t, notifyOutcomeOutboxRetriedSent), before)
		origin, found, _ := agentmessage.Parse(s.last().body)
		assert.True(t, found)
		assert.Equal(t, notifySentinelMode, origin.Mode, "the retry rides the same compose path")
	})

	t.Run("dropped after one retry", func(t *testing.T) {
		s := newSeamSrv(t)
		s.fail = true
		before := notifyOutcome(t, notifyOutcomeOutboxRetriedDropped)

		store := newSessionSubscriptionStore(storePath(t))
		store.enqueue(pendingNotification{Broadcaster: "ses_w", Subscriber: "ses_m",
			Event: subscriptionEventIdle, EnqueuedAt: time.Now().Add(-time.Minute)})

		n := newSessionNotifier(sessionNotifierConfig{Store: store, Deliver: seamDeliverer(s), Logger: zap.NewNop()})
		n.RetryOutbox(context.Background())

		assert.Equal(t, 1, s.count(), "the retry fired exactly ONCE")
		assert.Equal(t, 0, store.outboxLen(), "the failed retry is DROPPED, not re-enqueued")
		assert.Greater(t, notifyOutcome(t, notifyOutcomeOutboxRetriedDropped), before)

		// A second boot pass finds nothing left to retry.
		n.RetryOutbox(context.Background())
		assert.Equal(t, 1, s.count())
	})
}

// ---- §4.5 rate bound ----

func TestNotifyRateLimiterBucket(t *testing.T) {
	// The default: 60/h — the whole hour's budget bursts, then refuses.
	l := newNotifyRateLimiter()
	for i := 0; i < notifyRatePerHour; i++ {
		require.True(t, l.allow("ses_m"), "the 60/h budget bursts at once (advisory wake-ups)")
	}
	assert.False(t, l.allow("ses_m"), "the 61st within the window is refused")
	assert.True(t, l.allow("ses_other"), "the bound is per subscriber")

	// Parameterized refill: burst 2, 2 per 40ms ⇒ ~50 tokens/s.
	f := newNotifyRateLimiterAt(2, 2, 40*time.Millisecond)
	assert.True(t, f.allow("ses_s"))
	assert.True(t, f.allow("ses_s"))
	assert.False(t, f.allow("ses_s"))
	time.Sleep(60 * time.Millisecond) // ≥ 1 token refilled
	assert.True(t, f.allow("ses_s"), "tokens refill over time")

	// Degenerate config falls back to the default shape.
	d := newNotifyRateLimiterAt(0, -1, 0)
	assert.True(t, d.allow("ses_m"))
}

// TestRateRefusalDropsIntent: the storm backstop's refusal drops the
// wake-up (and its outbox intent) loudly — a boot retry would only
// deliver a stale wake after the storm the bound just stopped.
func TestRateRefusalDropsIntent(t *testing.T) {
	s := newSeamSrv(t)
	limiter := newNotifyRateLimiterAt(1, 1, time.Hour) // one wake, then starved
	d := newSeamSettleDeliverer(func() *opencode.Client {
		return opencode.NewLoopbackClient(s.srv.URL, "pw")
	}, limiter, zap.NewNop())

	store := newSessionSubscriptionStore(storePath(t))
	n := newSessionNotifier(sessionNotifierConfig{
		Store:    store,
		Facts:    mapSessionFacts{subtasks: map[string]bool{}, gone: map[string]bool{}},
		Debounce: 25 * time.Millisecond,
		Deliver:  d,
		Logger:   zap.NewNop(),
	})
	t.Cleanup(n.cancelAllDebounces)
	_, err := n.Subscribe("ses_w", "ses_m", nil)
	require.NoError(t, err)

	before := notifyOutcome(t, notifyOutcomeRefusedRate)

	// Episode 1 spends the token (wait for its delivery — the §4.3
	// debounce would otherwise cancel it into episode 2's onset);
	// episode 2 is refused before the vehicle.
	n.handleFrame(seqStatusFrame("ses_w", 10, abiv1.SessionStatus_SESSION_STATUS_BUSY))
	n.handleFrame(seqStatusFrame("ses_w", 11, abiv1.SessionStatus_SESSION_STATUS_IDLE))
	require.Equal(t, 1, waitForSeamPosts(t, s, 1))

	n.handleFrame(seqStatusFrame("ses_w", 20, abiv1.SessionStatus_SESSION_STATUS_BUSY))
	n.handleFrame(seqStatusFrame("ses_w", 21, abiv1.SessionStatus_SESSION_STATUS_IDLE))

	require.Eventually(t, func() bool { return notifyOutcome(t, notifyOutcomeRefusedRate) > before },
		2*time.Second, 10*time.Millisecond, "refused_rate ticks")
	require.Equal(t, 0, store.outboxLen(), "a refused wake-up drops its intent")
	assert.Equal(t, 1, s.count(), "exactly one POST landed — the second was refused before the vehicle")
}

// ---- §8 counters wired ----

func TestNotifyMetricsWired(t *testing.T) {
	f := newNotifierFixture(t, nil)

	sentBefore := notifyOutcome(t, notifyOutcomeSent)
	selfBefore := notifyOutcome(t, notifyOutcomeRefusedSelf)
	cycleBefore := notifyOutcome(t, notifyOutcomeRefusedCycle)
	capBefore := notifyOutcome(t, notifyOutcomeRefusedCap)
	pendingBefore := notifyOutcome(t, notifyOutcomeSkippedPendingInput)
	debouncedBefore := notifyOutcome(t, notifyOutcomeDebounced)

	// refused_self
	_, err := f.n.Subscribe("ses_a", "ses_a", nil)
	require.ErrorIs(t, err, errNotifySelfSubscription)
	// refused_cycle: B→A exists; A→B closes the loop.
	_, err = f.n.Subscribe("ses_a", "ses_b", nil)
	require.NoError(t, err)
	_, err = f.n.Subscribe("ses_b", "ses_a", nil)
	require.ErrorIs(t, err, errNotifyCycle)
	// refused_cap
	for i := 0; i < maxSubscribersPerBroadcaster; i++ {
		_, e := f.n.Subscribe("ses_cap", fmt.Sprintf("ses_s_%02d", i), nil)
		require.NoError(t, e)
	}
	_, err = f.n.Subscribe("ses_cap", "ses_extra", nil)
	require.ErrorIs(t, err, errNotifyCap)

	// skipped_pending_input + debounced + sent: one pending-gated
	// settle, then a re-busy cancel, then a clean episode.
	subscribeForWake(t, f, "ses_w", "ses_m", nil)
	f.n.handleFrame(seqStatusFrame("ses_w", 10, abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(evtFrame("ses_w", abiv1.EventType_EVENT_TYPE_INPUT_REQUEST, func(e *abiv1.Event) {
		e.Input = &abiv1.InputRequest{Id: "ask_1", Kind: abiv1.InputKind_INPUT_KIND_QUESTION}
	}))
	f.n.handleFrame(seqStatusFrame("ses_w", 11, abiv1.SessionStatus_SESSION_STATUS_IDLE))
	assert.Greater(t, notifyOutcome(t, notifyOutcomeSkippedPendingInput), pendingBefore)

	f.n.handleFrame(evtFrame("ses_w", abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED, func(e *abiv1.Event) {
		e.Input = &abiv1.InputRequest{Id: "ask_1"}
	}))
	f.n.handleFrame(seqStatusFrame("ses_w", 20, abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(seqStatusFrame("ses_w", 21, abiv1.SessionStatus_SESSION_STATUS_IDLE)) // arm
	f.n.handleFrame(seqStatusFrame("ses_w", 22, abiv1.SessionStatus_SESSION_STATUS_BUSY)) // cancel inside the window
	assert.Greater(t, notifyOutcome(t, notifyOutcomeDebounced), debouncedBefore)
	f.n.handleFrame(seqStatusFrame("ses_w", 23, abiv1.SessionStatus_SESSION_STATUS_IDLE))
	waitForNotice(t, f.deliver.ch)
	assert.Greater(t, notifyOutcome(t, notifyOutcomeSent), sentBefore,
		"the capturing fixture's deliverer counts as a sent notification")

	// skipped_subtask: a subtask broadcaster's settle never notifies —
	// the §4.6 gate skips it at notify time (row planted through the
	// store: the registration-time guard would have refused it).
	subtaskFacts := mapSessionFacts{subtasks: map[string]bool{"ses_sub": true}, gone: map[string]bool{}}
	sf := newNotifierFixture(t, subtaskFacts)
	subBefore := notifyOutcome(t, notifyOutcomeSkippedSubtask)
	sf.store.subscribe("ses_sub", "ses_m", []string{subscriptionEventIdle}, time.Now())
	sf.n.handleFrame(statusFrame("ses_sub", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	sf.n.handleFrame(statusFrame("ses_sub", abiv1.SessionStatus_SESSION_STATUS_IDLE))
	assertNoNotice(t, sf.deliver.ch, sf.debounce*4, "")
	assert.Greater(t, notifyOutcome(t, notifyOutcomeSkippedSubtask), subBefore)

	assert.Greater(t, notifyOutcome(t, notifyOutcomeRefusedSelf), selfBefore)
	assert.Greater(t, notifyOutcome(t, notifyOutcomeRefusedCycle), cycleBefore)
	assert.Greater(t, notifyOutcome(t, notifyOutcomeRefusedCap), capBefore)
}
