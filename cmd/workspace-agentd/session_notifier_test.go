// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// session_notifier_test.go — the §4.2 transition-tracker truth table
// (derived flips, saw-busy latch, pending-input gate, subtask filter,
// Reseeded resync), §4.3 debounce, §4.1 registration guards, and the
// seam plumbing. The frame-driven cases feed synthesized StreamFrames
// straight into handleFrame; the resync cases drive a REAL
// sessionstate.Authority (the reconcile_publish_1722_test.go pattern)
// so the notifier is pinned against the projection's actual fold —
// including the #1722 evidence settle, the synthetic transition class
// a harness-SSE hook would miss.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// storePath mints a fresh per-test registry file (the store's own
// TempDir — every fixture gets a clean document).
func storePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "session-subscriptions.json")
}

// ---- fixtures ----

// mapSessionFacts is the deterministic lineage seam.
type mapSessionFacts struct {
	subtasks map[string]bool
	gone     map[string]bool
}

func (f mapSessionFacts) IsSubtask(id string) bool { return f.subtasks[id] }
func (f mapSessionFacts) IsGone(id string) bool    { return f.gone[id] }

// chanDeliverer captures deliveries; failOf injects per-notice
// failures for the outbox lifecycle pins.
type chanDeliverer struct {
	ch     chan settleNotice
	failOf func(settleNotice) bool
}

func newChanDeliverer() *chanDeliverer {
	return &chanDeliverer{ch: make(chan settleNotice, 32)}
}

func (d *chanDeliverer) DeliverSettle(_ context.Context, notice settleNotice) error {
	if d.failOf != nil && d.failOf(notice) {
		return errors.New("injected delivery failure")
	}
	d.ch <- notice
	return nil
}

// notifierFixture is one wired notifier under test.
type notifierFixture struct {
	n        *sessionNotifier
	store    *sessionSubscriptionStore
	deliver  *chanDeliverer
	debounce time.Duration
}

// newNotifierFixture builds a frame-driven notifier: no Stream seam
// (tests call handleFrame directly), tiny debounce, capturing
// deliverer, clean lineage facts.
func newNotifierFixture(t *testing.T, facts sessionFacts) *notifierFixture {
	t.Helper()
	debounce := 25 * time.Millisecond
	deliver := newChanDeliverer()
	store := newSessionSubscriptionStore(storePath(t))
	if facts == nil {
		facts = mapSessionFacts{subtasks: map[string]bool{}, gone: map[string]bool{}}
	}
	n := newSessionNotifier(sessionNotifierConfig{
		Store:    store,
		Facts:    facts,
		Debounce: debounce,
		Deliver:  deliver,
		Logger:   zap.NewNop(),
	})
	t.Cleanup(n.cancelAllDebounces)
	return &notifierFixture{n: n, store: store, deliver: deliver, debounce: debounce}
}

// evtFrame wraps one contract event as a sequenced stream frame. The
// id pair mirrors the EMISSION surface: SessionId carries the phrase,
// InternalSessionId the canonical harness id (the registry's key).
func evtFrame(sid string, typ abiv1.EventType, mutate func(*abiv1.Event)) *abiv1.StreamFrame {
	evt := &abiv1.Event{Type: typ, SessionId: sid, InternalSessionId: sid}
	if mutate != nil {
		mutate(evt)
	}
	return &abiv1.StreamFrame{Frame: &abiv1.StreamFrame_Event{Event: &abiv1.SequencedEvent{Seq: 1, Event: evt}}}
}

func statusFrame(sid string, st abiv1.SessionStatus) *abiv1.StreamFrame {
	return evtFrame(sid, abiv1.EventType_EVENT_TYPE_SESSION_STATUS, func(e *abiv1.Event) { e.Status = st })
}

// snapSession builds one snapshot-flavor session in DERIVED-busy shape
// (the projection's own BusyComponents, as PodSnapshot serves them).
func snapSession(sid string, busy bool, st abiv1.SessionStatus) *abiv1.SessionSnapshot {
	return &abiv1.SessionSnapshot{
		SessionId:         "phrase-" + sid,
		InternalSessionId: sid,
		Status:            st,
		Busy:              &abiv1.BusyComponents{Streaming: busy, Busy: busy},
	}
}

func podSnapFrame(sessions ...*abiv1.SessionSnapshot) *abiv1.StreamFrame {
	return &abiv1.StreamFrame{Frame: &abiv1.StreamFrame_Snapshot{Snapshot: &abiv1.SnapshotFrame{
		AtSeq: 1, Snapshot: &abiv1.PodSnapshot{Sessions: sessions},
	}}}
}

func waitForNotice(t *testing.T, ch chan settleNotice) settleNotice {
	t.Helper()
	select {
	case n := <-ch:
		return n
	case <-time.After(2 * time.Second):
		t.Fatal("expected a settle notification, none arrived")
		return settleNotice{}
	}
}

func assertNoNotice(t *testing.T, ch chan settleNotice, wait time.Duration, msg string) {
	t.Helper()
	select {
	case n := <-ch:
		if msg != "" {
			t.Fatalf("%s (got %+v)", msg, n)
		}
		t.Fatalf("unexpected settle notification: %+v", n)
	case <-time.After(wait):
	}
}

func subscribeForWake(t *testing.T, f *notifierFixture, broadcaster, subscriber string, events []string) {
	t.Helper()
	if events == nil {
		events = []string{subscriptionEventIdle, subscriptionEventError}
	}
	_, err := f.n.Subscribe(broadcaster, subscriber, events)
	require.NoError(t, err)
}

// ---- §4.2 transition tracker: the truth table ----

// TestTrackerBusyToIdleNotifies: THE core row — a busy episode the
// tracker observed live, settling to IDLE, notifies once.
func TestTrackerBusyToIdleNotifies(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_worker", "ses_manager", nil)

	f.n.handleFrame(statusFrame("ses_worker", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(statusFrame("ses_worker", abiv1.SessionStatus_SESSION_STATUS_IDLE))

	notice := waitForNotice(t, f.deliver.ch)
	assert.Equal(t, "ses_worker", notice.Broadcaster)
	assert.Equal(t, "ses_manager", notice.Subscriber)
	assert.Equal(t, subscriptionEventIdle, notice.Event)
	// The dismiss runs after the deliverer returns — the channel read
	// above can win that race, so the clear is awaited, not assumed.
	require.Eventually(t, func() bool { return f.store.outboxLen() == 0 },
		2*time.Second, 5*time.Millisecond, "a successful delivery clears its intent")
	assert.Equal(t, sessionLatch{LastDerivedBusy: false, SawBusy: true}, f.store.latchFor("ses_worker"))
	assertNoNotice(t, f.deliver.ch, f.debounce*3, "")
}

// TestTrackerBusyFromDataCounts: busy-from-data (#1574) — a part start
// marks busy without any status event; the flip fires on the fold, and
// the terminal part end alone does NOT settle (only the IDLE status
// does).
func TestTrackerBusyFromDataCounts(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", nil)

	f.n.handleFrame(evtFrame("ses_w", abiv1.EventType_EVENT_TYPE_PART_START, func(e *abiv1.Event) {
		e.Part = &abiv1.Part{Id: "prt_1", Type: abiv1.PartType_PART_TYPE_TOOL, Payload: &abiv1.Part_Tool{Tool: &abiv1.ToolPart{
			CallId: "c1", Name: "bash",
			State: &abiv1.ToolState{Status: abiv1.ToolStatus_TOOL_STATUS_RUNNING},
		}}}
	}))
	assert.True(t, f.store.latchFor("ses_w").SawBusy, "busy-from-data latches saw-busy with no status event")

	f.n.handleFrame(evtFrame("ses_w", abiv1.EventType_EVENT_TYPE_PART_END, func(e *abiv1.Event) {
		e.PartId = "prt_1"
	}))
	assertNoNotice(t, f.deliver.ch, f.debounce*3, "")

	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_IDLE))
	notice := waitForNotice(t, f.deliver.ch)
	assert.Equal(t, subscriptionEventIdle, notice.Event)
}

// TestTrackerErrorSettleNotifiesItsKind: ruling 1 — an ERROR settle is
// a notify with its own kind; rows select by their event set.
func TestTrackerErrorSettleNotifiesItsKind(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_both", nil)                                    // default: both kinds
	subscribeForWake(t, f, "ses_w", "ses_error_only", []string{subscriptionEventError}) // error only
	subscribeForWake(t, f, "ses_w", "ses_idle_only", []string{subscriptionEventIdle})   // idle only

	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(evtFrame("ses_w", abiv1.EventType_EVENT_TYPE_ERROR, func(e *abiv1.Event) {
		e.Error = &abiv1.Error{Code: "turn_failed", Message: "boom"}
	}))

	got := map[string]string{}
	for i := 0; i < 2; i++ {
		n := waitForNotice(t, f.deliver.ch)
		got[n.Subscriber] = n.Event
	}
	assert.Equal(t, map[string]string{"ses_both": subscriptionEventError, "ses_error_only": subscriptionEventError}, got,
		"the error settle wakes error-subscribed rows with the error kind")
	assertNoNotice(t, f.deliver.ch, f.debounce*3, "")
}

// TestTrackerErrorStatusSettle: the STATUS event carrying ERROR (not
// the error event) is equally a settle — the projection folds both.
func TestTrackerErrorStatusSettle(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", []string{subscriptionEventError})

	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_ERROR))
	notice := waitForNotice(t, f.deliver.ch)
	assert.Equal(t, subscriptionEventError, notice.Event)
}

// TestTrackerUnknownToIdleWithoutLatchIsSilent: a fresh projection's
// idle (reseed/restart) is NOT a settle — unknown→idle never notifies
// (§4.2's latch rule, the store reader's idle-bias defense).
func TestTrackerUnknownToIdleWithoutLatchIsSilent(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", nil)

	f.n.handleFrame(podSnapFrame(snapSession("ses_w", false, abiv1.SessionStatus_SESSION_STATUS_IDLE)))
	assertNoNotice(t, f.deliver.ch, f.debounce*4, "")
	assert.Equal(t, sessionLatch{}, f.store.latchFor("ses_w"), "an idle snapshot neither latches nor settles")
}

// TestTrackerUnknownToIdleWithLatchSettles: the restart attribution —
// the PREVIOUS generation persisted saw-busy + busy-edge; the fresh
// snapshot says idle through the gates ⇒ treat as settle (§7.3's
// at-least-once-for-the-latched-case).
func TestTrackerUnknownToIdleWithLatchSettles(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", nil)
	f.store.setLatch("ses_w", sessionLatch{LastDerivedBusy: true, SawBusy: true})

	f.n.handleFrame(podSnapFrame(snapSession("ses_w", false, abiv1.SessionStatus_SESSION_STATUS_IDLE)))
	notice := waitForNotice(t, f.deliver.ch)
	assert.Equal(t, subscriptionEventIdle, notice.Event)
}

// TestTrackerLatchBusyEdgeWithoutSawBusyIsSilent: a persisted busy EDGE
// with no observed busy (store hand-edited / pathological) is not
// attributable — no notification.
func TestTrackerLatchBusyEdgeWithoutSawBusyIsSilent(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", nil)
	f.store.setLatch("ses_w", sessionLatch{LastDerivedBusy: true, SawBusy: false})

	f.n.handleFrame(podSnapFrame(snapSession("ses_w", false, abiv1.SessionStatus_SESSION_STATUS_IDLE)))
	assertNoNotice(t, f.deliver.ch, f.debounce*4, "")
}

// TestTrackerBusyThroughUnknownToIdle: busy → (status unknown) → idle
// still notifies — the flip is on DERIVED busy, not status strings,
// and the busy was observed.
func TestTrackerBusyThroughUnknownToIdle(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", nil)

	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_UNKNOWN))
	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_IDLE))
	notice := waitForNotice(t, f.deliver.ch)
	assert.Equal(t, subscriptionEventIdle, notice.Event)
}

// TestTrackerPendingInputGate: a settle blocked on a pending
// QUESTION/PERMISSION ask is not "complete" — no notification; when
// the answered turn runs and settles, the next flip fires normally.
func TestTrackerPendingInputGate(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", nil)

	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(evtFrame("ses_w", abiv1.EventType_EVENT_TYPE_INPUT_REQUEST, func(e *abiv1.Event) {
		e.Input = &abiv1.InputRequest{Id: "ask_1", Kind: abiv1.InputKind_INPUT_KIND_QUESTION, Question: "which?"}
	}))
	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_IDLE))
	assertNoNotice(t, f.deliver.ch, f.debounce*4, "a session blocked on a question is not complete")

	// The user answers: a fresh busy episode runs and settles.
	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(evtFrame("ses_w", abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED, func(e *abiv1.Event) {
		e.Input = &abiv1.InputRequest{Id: "ask_1"}
	}))
	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_IDLE))
	notice := waitForNotice(t, f.deliver.ch)
	assert.Equal(t, subscriptionEventIdle, notice.Event)
}

// TestTrackerSubtaskBroadcasterExcluded: §4.6 broadcaster side — never
// notify about subtask transitions (the parent gets the results
// synchronously through the task tool). Registration refuses a KNOWN
// subtask broadcaster; the row here is planted through the store to
// simulate the cold-facts window (parentage the registration-time
// snapshot did not know) — the settle-time gate must skip it anyway.
func TestTrackerSubtaskBroadcasterExcluded(t *testing.T) {
	facts := mapSessionFacts{subtasks: map[string]bool{"ses_sub": true}, gone: map[string]bool{}}
	f := newNotifierFixture(t, facts)

	_, err := f.n.Subscribe("ses_sub", "ses_m", nil)
	require.ErrorIs(t, err, errNotifySubtaskSide, "registration refuses a subtask broadcaster loudly")

	f.store.subscribe("ses_sub", "ses_m", []string{subscriptionEventIdle}, time.Now())
	f.n.handleFrame(statusFrame("ses_sub", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(statusFrame("ses_sub", abiv1.SessionStatus_SESSION_STATUS_IDLE))
	assertNoNotice(t, f.deliver.ch, f.debounce*4, "")
}

// TestTrackerSubtaskSubscriberSkippedAtDispatch: §4.6 subscriber side —
// registration refuses subagent callers loudly, and the dispatch-time
// belt skips a row whose parentage only the later facts refresh
// revealed.
func TestTrackerSubtaskSubscriberSkippedAtDispatch(t *testing.T) {
	facts := mapSessionFacts{subtasks: map[string]bool{"ses_known_sub": true}, gone: map[string]bool{}}
	f := newNotifierFixture(t, facts)

	_, err := f.n.Subscribe("ses_w", "ses_known_sub", nil)
	require.ErrorIs(t, err, errNotifySubtaskSide, "registration refuses a KNOWN subtask subscriber loudly")

	// A row registered while parentage was unknown (cold facts — the
	// registration guard passed before the refresh revealed it), then
	// revealed: the dispatch-time check drops it.
	facts.subtasks["ses_late_sub"] = true
	f.store.subscribe("ses_w", "ses_late_sub", []string{subscriptionEventIdle, subscriptionEventError}, time.Now())
	subscribeForWake(t, f, "ses_w", "ses_ok", nil)

	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_IDLE))
	notice := waitForNotice(t, f.deliver.ch)
	assert.Equal(t, "ses_ok", notice.Subscriber)
	assertNoNotice(t, f.deliver.ch, f.debounce*3, "")
}

// TestTrackerCanonicalKeying: frames carry the phrase in the wire id
// and the canonical id additively — the registry stays canonical-keyed
// (§7.8).
func TestTrackerCanonicalKeying(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_canon", "ses_m", nil)

	phraseEvt := &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
		SessionId: "phrase-crimson-finch", InternalSessionId: "ses_canon"}
	f.n.handleFrame(&abiv1.StreamFrame{Frame: &abiv1.StreamFrame_Event{Event: &abiv1.SequencedEvent{Event: phraseEvt}}})
	phraseEvt.Status = abiv1.SessionStatus_SESSION_STATUS_BUSY
	f.n.handleFrame(&abiv1.StreamFrame{Frame: &abiv1.StreamFrame_Event{Event: &abiv1.SequencedEvent{Event: phraseEvt}}})
	phraseEvt.Status = abiv1.SessionStatus_SESSION_STATUS_IDLE
	f.n.handleFrame(&abiv1.StreamFrame{Frame: &abiv1.StreamFrame_Event{Event: &abiv1.SequencedEvent{Event: phraseEvt}}})

	notice := waitForNotice(t, f.deliver.ch)
	assert.Equal(t, "ses_canon", notice.Broadcaster)
}

// TestTrackerUnwatchedSessionArmsNothing: no rows ⇒ no timer, no
// delivery — the fan-out set gates the debounce.
func TestTrackerUnwatchedSessionArmsNothing(t *testing.T) {
	f := newNotifierFixture(t, nil)
	f.n.handleFrame(statusFrame("ses_lonely", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(statusFrame("ses_lonely", abiv1.SessionStatus_SESSION_STATUS_IDLE))
	assertNoNotice(t, f.deliver.ch, f.debounce*4, "")
}

// ---- §4.3 debounce ----

// TestDebounceCancelsOnRebusy: an armed timer is canceled when
// derived busy returns within the window (the idle→busy flutter); the
// episode that follows fires exactly once.
func TestDebounceCancelsOnRebusy(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", nil)

	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_IDLE)) // arm
	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_BUSY)) // cancel inside the window
	assertNoNotice(t, f.deliver.ch, f.debounce*3, "the re-busy canceled the armed notification")

	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_IDLE)) // re-arm
	notice := waitForNotice(t, f.deliver.ch)
	assert.Equal(t, subscriptionEventIdle, notice.Event)
	assertNoNotice(t, f.deliver.ch, f.debounce*3, "once per busy episode")
}

// TestDebounceFiresOncePerEpisode: two COMPLETE episodes notify twice
// — the wake primitive works per turn, not once ever.
func TestDebounceFiresOncePerEpisode(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", nil)

	for i := 0; i < 2; i++ {
		f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_BUSY))
		f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_IDLE))
		waitForNotice(t, f.deliver.ch)
	}
	assertNoNotice(t, f.deliver.ch, f.debounce*3, "exactly two notifications for two episodes")
	require.Eventually(t, func() bool { return f.store.outboxLen() == 0 },
		2*time.Second, 5*time.Millisecond, "both deliveries cleared their intents")
}

// TestDebounceDefaultFromConstruction: a zero config debounce falls
// back to the ruling-3 default.
func TestDebounceDefaultFromConstruction(t *testing.T) {
	n := newSessionNotifier(sessionNotifierConfig{Logger: zap.NewNop()})
	assert.Equal(t, notifyDebounceDefault, n.debounce)
}

// TestNotifyDebounceFromEnv: ruling 3's tunable — a Go duration
// overrides; malformed or non-positive values warn and fall back.
func TestNotifyDebounceFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"unset", "", notifyDebounceDefault},
		{"blank", "   ", notifyDebounceDefault},
		{"override", "500ms", 500 * time.Millisecond},
		{"long", "1m", time.Minute},
		{"malformed", "not-a-duration", notifyDebounceDefault},
		{"zero", "0s", notifyDebounceDefault},
		{"negative", "-5s", notifyDebounceDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"AGENTD_NOTIFY_DEBOUNCE": tc.env}
			got := notifyDebounceFromEnv(func(k string) string { return env[k] }, zap.NewNop())
			assert.Equal(t, tc.want, got)
		})
	}
}

// ---- §4.1 registration guards ----

func TestSubscribeGuards(t *testing.T) {
	f := newNotifierFixture(t, nil)

	_, err := f.n.Subscribe("ses_a", "ses_a", nil)
	assert.ErrorIs(t, err, errNotifySelfSubscription)

	// Cycle: B watches A, C watches B — A watching C closes the loop.
	subscribeForWake(t, f, "ses_a", "ses_b", nil)
	subscribeForWake(t, f, "ses_b", "ses_c", nil)
	_, err = f.n.Subscribe("ses_c", "ses_a", nil)
	assert.ErrorIs(t, err, errNotifyCycle)
	// The reverse direction stays legal.
	_, err = f.n.Subscribe("ses_a", "ses_d", nil)
	require.NoError(t, err)

	// Cap: the 33rd distinct subscriber of one broadcaster refuses;
	// existing rows keep updating (idempotent carve-out).
	for i := 0; i < maxSubscribersPerBroadcaster; i++ {
		_, err = f.n.Subscribe("ses_cap", fmt.Sprintf("ses_s_%02d", i), nil)
		require.NoError(t, err)
	}
	_, err = f.n.Subscribe("ses_cap", "ses_s_extra", nil)
	assert.ErrorIs(t, err, errNotifyCap)
	_, err = f.n.Subscribe("ses_cap", "ses_s_00", []string{subscriptionEventError})
	require.NoError(t, err, "an existing subscriber updates its events past the cap")

	_, err = f.n.Subscribe("ses_b", "ses_m", []string{"busy"})
	assert.Error(t, err, "closed vocabulary")

	_, err = f.n.Subscribe("", "ses_m", nil)
	assert.Error(t, err, "ids are required")
}

// TestUnsubscribeIdempotent: removing an absent edge reports false
// quietly — the MCP layer maps that to not_subscribed (§5).
func TestUnsubscribeIdempotent(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", nil)
	assert.True(t, f.n.Unsubscribe("ses_w", "ses_m"))
	assert.False(t, f.n.Unsubscribe("ses_w", "ses_m"))
}

// ---- §4.5 outbox ordering (delivery seam lifecycle) ----

// TestOutboxPersistsOnFailedDelivery: the intent is durable BEFORE the
// POST and survives a failed delivery AND a process restart — the
// at-least-once half of ruling 4.
func TestOutboxPersistsOnFailedDelivery(t *testing.T) {
	path := storePath(t)
	deliver := newChanDeliverer()
	deliver.failOf = func(settleNotice) bool { return true }
	store := newSessionSubscriptionStore(path)
	n := newSessionNotifier(sessionNotifierConfig{
		Store: store, Facts: mapSessionFacts{subtasks: map[string]bool{}, gone: map[string]bool{}},
		Debounce: 25 * time.Millisecond, Deliver: deliver, Logger: zap.NewNop(),
	})
	t.Cleanup(n.cancelAllDebounces)
	_, err := n.Subscribe("ses_w", "ses_m", nil)
	require.NoError(t, err)

	n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_IDLE))

	require.Eventually(t, func() bool { return store.outboxLen() == 1 },
		2*time.Second, 10*time.Millisecond, "the failed delivery keeps its intent")
	select {
	case n := <-deliver.ch:
		t.Fatalf("failed deliveries must not reach the channel: %+v", n)
	default:
	}

	// The intent outlives the process.
	re := newSessionSubscriptionStore(path)
	require.Equal(t, 1, re.outboxLen())
}

// TestOutboxClearedOnSuccess and the empty-outbox steady state. The
// dismiss runs after the deliverer returns (the channel read can win
// that race), so the clear is awaited, not assumed.
func TestOutboxClearedOnSuccess(t *testing.T) {
	f := newNotifierFixture(t, nil)
	subscribeForWake(t, f, "ses_w", "ses_m", nil)
	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	f.n.handleFrame(statusFrame("ses_w", abiv1.SessionStatus_SESSION_STATUS_IDLE))
	waitForNotice(t, f.deliver.ch)
	require.Eventually(t, func() bool { return f.store.outboxLen() == 0 },
		2*time.Second, 5*time.Millisecond, "a successful delivery clears its intent")
	re := newSessionSubscriptionStore(f.store.path)
	assert.Equal(t, 0, re.outboxLen(), "the clear is durable")
}

// ---- §4.6 facts + lazy cleanup ----

func TestHarnessSessionFacts(t *testing.T) {
	facts := newHarnessSessionFacts()
	facts.Load([]session.Session{
		{ID: "ses_top", Title: "manager"},
		{ID: "ses_child", ParentID: "ses_top"},
		{ID: "ses_archived", Archived: true},
		{ID: "ses_doomed"},
	})
	assert.True(t, facts.IsSubtask("ses_child"), "ParentID != \"\" is the subtask signal (both sides excluded)")
	assert.False(t, facts.IsSubtask("ses_top"))
	assert.True(t, facts.IsGone("ses_archived"))
	assert.False(t, facts.IsGone("ses_top"))
	assert.False(t, facts.IsGone("ses_never_listed"), "the #1452 class (live but never listed) is never 'deleted'")

	// Deletion hysteresis: previously-present → absent.
	facts.Load([]session.Session{{ID: "ses_top"}})
	assert.True(t, facts.IsGone("ses_doomed"))
	assert.False(t, facts.IsGone("ses_top"))
	// Gone is sticky across refreshes.
	facts.Load([]session.Session{{ID: "ses_top"}, {ID: "ses_doomed"}})
	assert.True(t, facts.IsGone("ses_doomed"), "pruned rows stay pruned")
}

// TestPullHarnessSessionListCarriesParentID: the FactsPull adapter's
// whole reason to exist — GET /session is the ONLY surface carrying
// ParentID (investigation C1: the authority projection does not), and
// the exclusion signal must survive the wire→contract translation.
func TestPullHarnessSessionListCarriesParentID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/session", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id": "ses_top", "title": "manager"},
			{"id": "ses_child", "parentID": "ses_top", "isSubtask": true},
			{"id": "ses_old", "archived": true}
		]`))
	}))
	t.Cleanup(srv.Close)

	t.Setenv("WORKSPACE_ID", "ws_test")
	got, err := pullHarnessSessionList(context.Background(), opencode.NewLoopbackClient(srv.URL, "pw"))
	require.NoError(t, err)
	require.Len(t, got, 3)

	byID := map[string]session.Session{}
	for _, s := range got {
		byID[s.ID] = s
	}
	assert.Equal(t, "ses_top", byID["ses_child"].ParentID, "the parent fact rides the wire list path (translate.go)")
	assert.Empty(t, byID["ses_top"].ParentID)
	assert.True(t, byID["ses_old"].Archived)

	facts := newHarnessSessionFacts()
	facts.Load(got)
	assert.True(t, facts.IsSubtask("ses_child"))
	assert.False(t, facts.IsSubtask("ses_top"))
	assert.True(t, facts.IsGone("ses_old"))
}

func TestRefreshFactsPrunesDeadRows(t *testing.T) {
	// The real facts seam (harnessSessionFacts): gone-detection is
	// hysteresis-based — a session PREVIOUSLY listed that vanishes is
	// deleted; archived reads directly off the record; never-listed
	// sessions are never "deleted" (the #1452 class).
	f := newNotifierFixture(t, newHarnessSessionFacts())
	subscribeForWake(t, f, "ses_b_deleted", "ses_m", nil)
	subscribeForWake(t, f, "ses_b_archived", "ses_m", nil)
	subscribeForWake(t, f, "ses_b_ok", "ses_m", nil)
	subscribeForWake(t, f, "ses_b_ok", "ses_m_gone", nil)

	live := []session.Session{
		{ID: "ses_b_deleted"}, {ID: "ses_b_archived"}, {ID: "ses_b_ok"}, {ID: "ses_m"}, {ID: "ses_m_gone"},
	}
	pull := func(context.Context) ([]session.Session, error) { return live, nil }
	f.n.cfg.FactsPull = pull

	// First refresh: everything is present — nothing prunes yet.
	f.n.RefreshFacts(context.Background())
	require.Len(t, f.n.store.allRows(), 4)

	// The harness state moves on: one session deleted (absent), one
	// archived, one subscriber gone.
	live = []session.Session{{ID: "ses_b_ok"}, {ID: "ses_m"}}
	f.n.RefreshFacts(context.Background())

	rows := f.n.store.allRows()
	require.Len(t, rows, 1, "broadcaster-dead (deleted and archived) and subscriber-dead edges all drop")
	assert.Equal(t, "ses_b_ok", rows[0].Broadcaster)
	assert.Equal(t, "ses_m", rows[0].Subscriber)
}

// ---- Resync against a REAL authority (the reconcile_publish_1722
// pattern: the notifier pinned on the projection's actual fold) ----

// notifierAuthorityStore is the StoreReader fake: canned seeds.
type notifierAuthorityStore struct {
	states map[string]sessionstate.SessionSeed
}

func (s notifierAuthorityStore) SessionStates(context.Context) (map[string]sessionstate.SessionSeed, error) {
	return s.states, nil
}

func (s notifierAuthorityStore) MessagePresence(context.Context, string, []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (s notifierAuthorityStore) PendingInputs(context.Context) (map[string][]*abiv1.InputRequest, error) {
	return map[string][]*abiv1.InputRequest{}, nil
}

type nopEventParser struct{}

func (nopEventParser) Parse([]byte) (*abiv1.Event, bool, error) { return nil, false, nil }

// nopAdmitter arms the delivery ledger (Admitter non-nil ⇒ the ledger
// exists ⇒ the #1722 evidence sweep runs and publishes settles — the
// reconcile_publish_1722_test.go wiring).
type nopAdmitter struct{}

func (nopAdmitter) Admit(context.Context, string, string, string, string) (string, error) {
	return "", errors.New("not under test")
}

// newNotifierAuthority builds a real sessionstate authority (nop
// parser — the tests drive IngestForTest).
func newNotifierAuthority(t *testing.T, states map[string]sessionstate.SessionSeed) *sessionstate.Authority {
	t.Helper()
	a, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      nopEventParser{},
		Store:       notifierAuthorityStore{states: states},
		Passwords:   []string{"pw"},
		Admitter:    nopAdmitter{},
		FastCursor:  true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// runNotifierOnAuthority wires the notifier onto the authority's real
// stream + state snapshot and runs the consumer loop.
func runNotifierOnAuthority(t *testing.T, a *sessionstate.Authority, store *sessionSubscriptionStore, deliver *chanDeliverer) context.CancelFunc {
	t.Helper()
	n := newSessionNotifier(sessionNotifierConfig{
		Store:    store,
		Stream:   a.Stream,
		Snapshot: a.State,
		Facts:    mapSessionFacts{subtasks: map[string]bool{}, gone: map[string]bool{}},
		Debounce: 25 * time.Millisecond,
		Deliver:  deliver,
		Logger:   zap.NewNop(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	go n.Run(ctx)
	t.Cleanup(cancel)
	t.Cleanup(n.cancelAllDebounces)
	return cancel
}

// TestAuthority1722EvidenceSettleNotifies: the #1722 pin at the
// notifier seam — the reconcile pass's SYNTHETIC idle publication (the
// settle a harness-SSE hook would never see) drives the notification.
func TestAuthority1722EvidenceSettleNotifies(t *testing.T) {
	states := map[string]sessionstate.SessionSeed{
		"ses_w": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	}
	a := newNotifierAuthority(t, states)
	store := newSessionSubscriptionStore(storePath(t))
	deliver := newChanDeliverer()
	runNotifierOnAuthority(t, a, store, deliver)
	store.subscribe("ses_w", "ses_m", []string{subscriptionEventIdle}, time.Now())
	requireNotifierSubscribed(t, a)

	// Wedge: busy + a RUNNING tool part (the abort-burst-lost shape).
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
		SessionId: "ses_w", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_PART_START, SessionId: "ses_w", PartId: "prt_run",
		Part: &abiv1.Part{Id: "prt_run", Type: abiv1.PartType_PART_TYPE_TOOL, Payload: &abiv1.Part_Tool{Tool: &abiv1.ToolPart{
			CallId: "call_run", Name: "bash",
			State: &abiv1.ToolState{Status: abiv1.ToolStatus_TOOL_STATUS_RUNNING},
		}}}})
	require.True(t, a.State().Sessions["ses_w"].Busy, "precondition: wedged busy")

	stats := a.Reconcile(context.Background())
	require.NotZero(t, stats.BusyCleared, "the evidence settle published")

	notice := waitForNotice(t, deliver.ch)
	assert.Equal(t, "ses_w", notice.Broadcaster)
	assert.Equal(t, subscriptionEventIdle, notice.Event)
}

// requireNotifierSubscribed blocks until the notifier's Run loop holds
// a stream subscription (the ingests that follow must be observed
// live, not via the connection snapshot).
func requireNotifierSubscribed(t *testing.T, a *sessionstate.Authority) {
	t.Helper()
	require.Eventually(t, func() bool { return a.Metrics().Subscribers == 1 },
		2*time.Second, 5*time.Millisecond, "notifier never subscribed to the authority stream")
}

// TestAuthorityReseededResyncSettles: a Reseeded frame rebuilds the
// projection from store truth; the notifier resyncs from the stamped
// snapshot and — the latch reconciliation — treats latched-busy +
// snapshot-idle as the settle.
func TestAuthorityReseededResyncSettles(t *testing.T) {
	states := map[string]sessionstate.SessionSeed{
		"ses_w": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	}
	a := newNotifierAuthority(t, states)
	store := newSessionSubscriptionStore(storePath(t))
	deliver := newChanDeliverer()
	runNotifierOnAuthority(t, a, store, deliver)
	store.subscribe("ses_w", "ses_m", []string{subscriptionEventIdle}, time.Now())

	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
		SessionId: "ses_w", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	require.Eventually(t, func() bool {
		return store.latchFor("ses_w").SawBusy
	}, 2*time.Second, 10*time.Millisecond, "the busy onset latched before the reseed")

	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))

	notice := waitForNotice(t, deliver.ch)
	assert.Equal(t, subscriptionEventIdle, notice.Event)
}

// TestAuthorityReseededUnknownIdleIsSilent: reseed to idle for a
// session whose busy was NEVER observed (fresh tracker, clean latch)
// emits nothing — the resync is not a transition.
func TestAuthorityReseededUnknownIdleIsSilent(t *testing.T) {
	states := map[string]sessionstate.SessionSeed{
		"ses_quiet": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	}
	a := newNotifierAuthority(t, states)
	store := newSessionSubscriptionStore(storePath(t))
	deliver := newChanDeliverer()
	runNotifierOnAuthority(t, a, store, deliver)
	store.subscribe("ses_quiet", "ses_m", []string{subscriptionEventIdle}, time.Now())

	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange))
	assertNoNotice(t, deliver.ch, 150*time.Millisecond, "")
}

// TestRunResubscribesAfterDrop: the M3.4 slow-consumer drop closes the
// channel; Run reconnects, and the fresh connection's snapshot frame
// performs the latch reconciliation (latched busy + snapshot idle ⇒
// settle) — the no-replay resilience pin.
func TestRunResubscribesAfterDrop(t *testing.T) {
	store := newSessionSubscriptionStore(storePath(t))
	deliver := newChanDeliverer()
	store.subscribe("ses_w", "ses_m", []string{subscriptionEventIdle}, time.Now())
	store.setLatch("ses_w", sessionLatch{LastDerivedBusy: true, SawBusy: true})

	closed := make(chan struct{})
	drops := 0
	stream := func(ctx context.Context) (<-chan *abiv1.StreamFrame, func(), error) {
		if drops > 0 {
			// Second connection: fresh snapshot says the latched-busy
			// session settled idle.
			ch := make(chan *abiv1.StreamFrame, 1)
			ch <- podSnapFrame(snapSession("ses_w", false, abiv1.SessionStatus_SESSION_STATUS_IDLE))
			close(ch)
			return ch, func() {}, nil
		}
		drops++
		ch := make(chan *abiv1.StreamFrame, 1)
		ch <- podSnapFrame(snapSession("ses_w", true, abiv1.SessionStatus_SESSION_STATUS_BUSY))
		close(ch)
		close(closed)
		return ch, func() {}, nil
	}
	n := newSessionNotifier(sessionNotifierConfig{
		Store: store, Stream: stream,
		Facts:    mapSessionFacts{subtasks: map[string]bool{}, gone: map[string]bool{}},
		Debounce: 25 * time.Millisecond, Deliver: deliver, Logger: zap.NewNop(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("first connection never opened")
	}
	notice := waitForNotice(t, deliver.ch)
	assert.Equal(t, subscriptionEventIdle, notice.Event,
		"the resubscribe's snapshot reconciled the dropped episode (latched busy + snapshot idle)")
}
