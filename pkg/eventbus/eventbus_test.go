// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package eventbus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/require"
)

// newTestBus wires a bus over miniredis with reclaim-friendly
// defaults (tiny MinIdle so tests do not sleep; tiny Block).
func newTestBus(t *testing.T) (*RedisBus, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	bus, err := NewRedisBus(context.Background(), client, StreamKey("ws-test"), 128)
	if err != nil {
		t.Fatalf("NewRedisBus: %v", err)
	}
	return bus, mr
}

func testOpts(group, consumer string, topics ...string) ConsumeOptions {
	return ConsumeOptions{
		Group:       group,
		Consumer:    consumer,
		Topics:      topics,
		Block:       10 * time.Millisecond,
		MinIdle:     30 * time.Millisecond,
		ReclaimTick: 20 * time.Millisecond,
		Batch:       8,
		MaxAttempts: 3,
	}
}

// testReplayOpts is testOpts with StartReplay — for tests whose
// entries pre-date the group (history the group must see).
func testReplayOpts(group, consumer string, topics ...string) ConsumeOptions {
	o := testOpts(group, consumer, topics...)
	o.StartAt = StartReplay
	return o
}

// primeGroup creates the consumer group BEFORE any publish, at the
// stream's end ($): entries published afterwards are the live tail a
// production group sees. Consume's own CREATE then lands on
// BUSYGROUP and proceeds.
func primeGroup(t *testing.T, bus *RedisBus, group string) {
	t.Helper()
	if err := bus.client.XGroupCreateMkStream(context.Background(), bus.key, group, "$").Err(); err != nil {
		if !strings.Contains(err.Error(), "BUSYGROUP") {
			t.Fatalf("primeGroup %s: %v", group, err)
		}
	}
}

func mustPublish(t *testing.T, bus *RedisBus, topic string) string {
	t.Helper()
	id, err := bus.Publish(context.Background(), Event{Topic: topic, At: time.Now(), Payload: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatalf("publish %s: %v", topic, err)
	}
	return id
}

// consumeUntil runs one consumer until cond passes (or the deadline
// fails the test), then waits for the group's pending list to drain
// (acks settle) before the cancel — a cancel from inside a handler
// would race its own ack. Returns the consumer's collected messages.
func consumeUntil(t *testing.T, bus *RedisBus, opts ConsumeOptions, cond func([]Message) bool) []Message {
	t.Helper()
	var mu sync.Mutex
	var got []Message
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = bus.Consume(ctx, opts, func(_ context.Context, m Message) error {
			mu.Lock()
			got = append(got, m)
			mu.Unlock()
			return nil
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		ok := cond(got)
		mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("consumeUntil: condition not met in 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Settle: every delivered entry's ack must land before the cancel.
	pelDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(pelDeadline) {
		p, err := bus.client.XPending(context.Background(), bus.key, opts.Group).Result()
		if err != nil || p.Count == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	return got
}

// ---- Vocabulary + matching ----

func TestTopicVocabulary(t *testing.T) {
	if got := SessionSettleTopic("ses_123", SettleIdle); got != "session.ses_123.idle" {
		t.Fatalf("settle idle topic = %q", got)
	}
	if got := SessionSettleTopic("ses_123", SettleError); got != "session.ses_123.error" {
		t.Fatalf("settle error topic = %q", got)
	}
	if got := TaskStageTopic("tsk-1"); got != "task.tsk-1.stage_changed" {
		t.Fatalf("stage topic = %q", got)
	}
	if got := LeaseNudgeTopic("tsk-1"); got != "lease.tsk-1.nudged" {
		t.Fatalf("nudge topic = %q", got)
	}
	if got := LeaseWedgeTopic("tsk-1"); got != "lease.tsk-1.wedge_verdict" {
		t.Fatalf("wedge topic = %q", got)
	}
	if got := StreamKey("ws-1"); got != "workspace:ws-1:events" {
		t.Fatalf("stream key = %q", got)
	}
	sid, kind, ok := ParseSessionSettle("session.ses_9.error")
	if !ok || sid != "ses_9" || kind != "error" {
		t.Fatalf("ParseSessionSettle = %q %q %v", sid, kind, ok)
	}
	if _, _, ok := ParseSessionSettle("lease.tsk-1.nudged"); ok {
		t.Fatal("ParseSessionSettle accepted a lease topic")
	}
	task, arm, ok := ParseLease("lease.tsk-2.nudged")
	if !ok || task != "tsk-2" || arm != "nudged" {
		t.Fatalf("ParseLease = %q %q %v", task, arm, ok)
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, topic string
		want           bool
	}{
		{"session.*.idle", "session.ses_1.idle", true},
		{"session.*.idle", "session.ses_1.error", false},
		{"session.ses_1.*", "session.ses_1.idle", true},
		{"session.ses_1.*", "session.ses_1.error", true},
		{"session.ses_1.*", "session.ses_2.idle", false},
		{"lease.*.*", "lease.tsk-1.nudged", true},
		{"lease.tsk-1.nudged", "lease.tsk-1.nudged", true},
		{"*", "session.ses_1.idle", false}, // one segment only
		{"**", "session.ses_1.idle", true},
		{"session.**", "session.ses_1.idle", true},
		{"session.**", "lease.tsk-1.nudged", false},
		{"session.ses_1.idle", "session.ses_1.idle.extra", false},
		{"", "session.ses_1.idle", false},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.topic); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.topic, got, c.want)
		}
	}
	if !MatchAny(nil, "anything.at.all") {
		t.Error("MatchAny(empty) must match everything (subscribe-all default)")
	}
	if MatchAny([]string{"session.*.idle"}, "lease.tsk-1.nudged") {
		t.Error("MatchAny leaked a non-matching topic")
	}
}

func TestEventValidate(t *testing.T) {
	if err := (Event{Topic: "  "}).Validate(); err == nil {
		t.Error("empty topic accepted")
	}
	if err := (Event{Topic: "a b"}).Validate(); err == nil {
		t.Error("whitespace topic accepted")
	}
	if err := (Event{Topic: "t", Payload: map[string]string{FieldTopic: "x"}}).Validate(); err == nil {
		t.Error("reserved field accepted")
	}
	if err := (Event{Topic: "t", Payload: map[string]string{"fine": "x"}}).Validate(); err != nil {
		t.Errorf("valid event refused: %v", err)
	}
}

// ---- Transport ----

func TestPublishRoundTrip(t *testing.T) {
	bus, _ := newTestBus(t)
	ctx := context.Background()
	primeGroup(t, bus, "g1")
	at := time.Now().Truncate(time.Millisecond).UTC()
	id, err := bus.Publish(ctx, Event{
		Topic: SessionSettleTopic("ses_1", SettleError),
		At:    at,
		Payload: map[string]string{
			"session": "ses_1", "settle": SettleError, "title": "worker-1",
		},
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if id == "" {
		t.Fatal("publish returned empty id")
	}
	got := consumeUntil(t, bus, testOpts("g1", "c1"), func(ms []Message) bool { return len(ms) >= 1 })
	if len(got) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(got))
	}
	m := got[0]
	if m.Event.Topic != "session.ses_1.error" {
		t.Errorf("topic = %q", m.Event.Topic)
	}
	if m.Event.Payload["session"] != "ses_1" || m.Event.Payload["title"] != "worker-1" {
		t.Errorf("payload = %v", m.Event.Payload)
	}
	if m.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", m.Attempts)
	}
	if !m.Event.At.Equal(at) {
		t.Errorf("at = %v, want %v", m.Event.At, at)
	}
	if m.ID != id {
		t.Errorf("id = %q, want %q", m.ID, id)
	}
}

func TestConsumerGroupsFanOutIndependently(t *testing.T) {
	bus, _ := newTestBus(t)
	primeGroup(t, bus, "notifier")
	primeGroup(t, bus, "lease")
	mustPublish(t, bus, "session.ses_1.idle")
	// TWO groups, one entry each — the fan-out: every group reads the
	// SAME stream at its own cursor.
	g1 := consumeUntil(t, bus, testOpts("notifier", "n1"), oneMessage)
	g2 := consumeUntil(t, bus, testOpts("lease", "l1"), oneMessage)
	if len(g1) != 1 || g1[0].Event.Topic != "session.ses_1.idle" {
		t.Fatalf("group notifier got %v", g1)
	}
	if len(g2) != 1 || g2[0].Event.Topic != "session.ses_1.idle" {
		t.Fatalf("group lease got %v", g2)
	}
}

func oneMessage(ms []Message) bool { return len(ms) >= 1 }

func TestPatternSubscriptionFiltersAndAcks(t *testing.T) {
	bus, _ := newTestBus(t)
	mustPublish(t, bus, "session.ses_1.idle")
	mustPublish(t, bus, "lease.tsk-1.nudged")
	mustPublish(t, bus, "session.ses_2.error")
	got := consumeUntil(t, bus, testReplayOpts("g", "c", "session.*.idle"), func(ms []Message) bool { return len(ms) >= 1 })
	if len(got) != 1 || got[0].Event.Topic != "session.ses_1.idle" {
		t.Fatalf("pattern consumer got %v", got)
	}
	// The filtered-out entries were ACKED (cursor advanced), not left
	// pending: a fresh consumer in the SAME group sees nothing.
	g2 := consumeUntil2(t, bus, testOpts("g", "c2"), 300*time.Millisecond)
	if len(g2) != 0 {
		t.Fatalf("second consumer of same group re-saw %d entries", len(g2))
	}
}

// flakyProxy fronts a backend addr with a switchable circuit: while
// the circuit is OPEN every new connection is closed immediately; a
// CLOSED circuit pipes bytes both ways. Consume's transient-error
// tolerance is proven through it: the consumer must outlive the
// outage and deliver what was published on the other side.
type flakyProxy struct {
	addr    string
	backend string
	mu      sync.Mutex
	open    bool
	ln      net.Listener
	done    chan struct{}
}

func newFlakyProxy(t *testing.T, backend string) *flakyProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("flakyProxy listen: %v", err)
	}
	p := &flakyProxy{addr: ln.Addr().String(), backend: backend, ln: ln, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); <-p.done })
	return p
}

func (p *flakyProxy) serve(conn net.Conn) {
	p.mu.Lock()
	open := p.open
	p.mu.Unlock()
	if open {
		_ = conn.Close()
		return
	}
	up, err := net.Dial("tcp", p.backend)
	if err != nil {
		_ = conn.Close()
		return
	}
	go func() { _, _ = io.Copy(up, conn); _ = up.Close() }()
	_, _ = io.Copy(conn, up)
	_ = conn.Close()
}

func (p *flakyProxy) setOpen(open bool) {
	p.mu.Lock()
	p.open = open
	p.mu.Unlock()
}

// TestConsumerSurvivesTransientOutage (review r1, Finding 1): a
// mid-consume transport outage (every connection refused for a
// window) must NOT kill the consumer — the consumer retries with
// backoff and delivers entries published on the other side of the
// outage once the transport returns. The lost-wake-up failure class
// is the one the design names as the failure that matters.
func TestConsumerSurvivesTransientOutage(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	proxy := newFlakyProxy(t, mr.Addr())

	client := redis.NewClient(&redis.Options{
		Addr: proxy.addr, DialTimeout: 50 * time.Millisecond,
		ReadTimeout: 250 * time.Millisecond, WriteTimeout: 250 * time.Millisecond,
	})
	defer client.Close()
	bus, err := NewRedisBus(context.Background(), client, StreamKey("ws-outage"), 128)
	require.NoError(t, err)

	// Prime the group, then break the transport mid-consume.
	require.NoError(t, client.XGroupCreateMkStream(context.Background(), bus.key, "g", "$").Err())
	got := make(chan string, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := ConsumeOptions{
		Group: "g", Consumer: "c",
		Block: 20 * time.Millisecond, MinIdle: 30 * time.Millisecond,
		ReclaimTick: 20 * time.Millisecond, Batch: 8, Backoff: 15 * time.Millisecond,
	}
	go func() {
		_ = bus.Consume(ctx, opts, func(_ context.Context, m Message) error {
			got <- m.Event.Topic
			return nil
		})
	}()
	proxy.setOpen(true)
	time.Sleep(150 * time.Millisecond) // the consumer is mid-outage

	// Publish THROUGH the backend — the entry sits in the stream
	// while the consumer cannot reach it.
	pubClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer pubClient.Close()
	pub, err := NewRedisBus(context.Background(), pubClient, StreamKey("ws-outage"), 128)
	require.NoError(t, err)
	_, err = pub.Publish(context.Background(), Event{Topic: "session.ses_1.idle"})
	require.NoError(t, err)

	// Heal; the consumer must still be alive and deliver.
	proxy.setOpen(false)
	select {
	case topic := <-got:
		require.Equal(t, "session.ses_1.idle", topic)
	case <-time.After(5 * time.Second):
		t.Fatal("the consumer did not survive the transient outage — delivery never resumed")
	}
	// And keeps delivering the live tail afterwards.
	_, err = pub.Publish(context.Background(), Event{Topic: "session.ses_2.error"})
	require.NoError(t, err)
	select {
	case topic := <-got:
		require.Equal(t, "session.ses_2.error", topic)
	case <-time.After(5 * time.Second):
		t.Fatal("post-outage live tail never delivered")
	}
}

// TestTrimmedCursorResumesAtHead (review r1, criterion-4 pin): a
// group whose last-delivered entry has been TRIMMED off the front
// (MAXLEN) resumes at the stream's head — no error, no phantom
// re-delivery of the trimmed past; the at-least-once contract is
// unaffected (the design §5 claim, now pinned instead of prose).
func TestTrimmedCursorResumesAtHead(t *testing.T) {
	bus, _ := newTestBus(t) // maxLen 128
	ctx := context.Background()
	require.NoError(t, bus.client.XGroupCreateMkStream(ctx, bus.key, "g", "$").Err())
	// Publish MORE than the cap with nobody consuming: the group's
	// cursor ($-anchored at creation) falls off the front.
	for i := 0; i < 300; i++ {
		_, err := bus.Publish(ctx, Event{Topic: fmt.Sprintf("session.ses_%d.idle", i)})
		require.NoError(t, err)
	}
	retained, err := bus.Len(ctx)
	require.NoError(t, err)
	require.Less(t, retained, int64(300), "MAXLEN did not trim")

	// The consumer joins the over-trimmed group and must simply
	// consume from the head.
	var mu sync.Mutex
	var topics []string
	consumeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = bus.Consume(consumeCtx, ConsumeOptions{
			Group: "g", Consumer: "c", Block: 20 * time.Millisecond,
			MinIdle: 30 * time.Millisecond, ReclaimTick: 20 * time.Millisecond,
			Batch: 8, MaxAttempts: 3,
		}, func(_ context.Context, m Message) error {
			mu.Lock()
			topics = append(topics, m.Event.Topic)
			mu.Unlock()
			return nil
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(topics)
		mu.Unlock()
		if n >= int(retained) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, topics, "the trimmed-cursor group never consumed")
	first, _, ok := ParseSessionSettle(topics[0])
	require.True(t, ok)
	idx, err := strconvParseInt(strings.TrimPrefix(first, "ses_"))
	require.NoError(t, err)
	// 300 entries, ~128 retained: the head is well past ses_150 — a
	// first delivery below that would be a trimmed-entry resurrection.
	require.Greater(t, idx, int64(150), "a trimmed (long-gone) entry was delivered — the cursor did not resume at the head")
	require.LessOrEqual(t, int64(len(topics)), retained+1, "more delivered than the stream retains")
}

// TestExistingGroupCursorResumesAcrossConsumers (review r1,
// criterion-4 pin): Redis persists a group's last-delivered id — a
// LATER consumer in the same group sees only what arrived after the
// earlier consumer acked, never a replay of the acked past (the
// "replay from last-acknowledged" resume, cursor flavor).
func TestExistingGroupCursorResumesAcrossConsumers(t *testing.T) {
	bus, _ := newTestBus(t)
	primeGroup(t, bus, "g")
	mustPublish(t, bus, "session.ses_1.idle")
	first := consumeUntil(t, bus, testOpts("g", "first"), oneMessage)
	require.Len(t, first, 1)
	// The acked past must not replay for the second consumer.
	mustPublish(t, bus, "session.ses_2.error")
	second := consumeUntil(t, bus, testOpts("g", "second"), oneMessage)
	require.Len(t, second, 1)
	require.Equal(t, "session.ses_2.error", second[0].Event.Topic,
		"the second consumer replayed the first's acked entry instead of resuming at the group cursor")
}

func strconvParseInt(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// consumeUntil2 runs a consumer for a fixed window (negative-result
// assertions need a floor, not a condition).

func consumeUntil2(t *testing.T, bus *RedisBus, opts ConsumeOptions, window time.Duration) []Message {
	t.Helper()
	var mu sync.Mutex
	var got []Message
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = bus.Consume(ctx, opts, func(_ context.Context, m Message) error {
			mu.Lock()
			got = append(got, m)
			mu.Unlock()
			return nil
		})
	}()
	time.Sleep(window)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	return got
}

func TestHandlerErrorRedeliversAtLeastOnce(t *testing.T) {
	bus, _ := newTestBus(t)
	mustPublish(t, bus, "session.ses_1.idle")
	var mu sync.Mutex
	attempts := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = bus.Consume(ctx, testReplayOpts("g", "c"), func(_ context.Context, m Message) error {
			mu.Lock()
			attempts++
			n := attempts
			mu.Unlock()
			if n == 1 {
				return errors.New("transient failure") // stays pending
			}
			return nil // acks
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := attempts
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	mu.Lock()
	defer mu.Unlock()
	if attempts < 2 {
		t.Fatalf("message delivered %d times, want >=2 (at-least-once redelivery)", attempts)
	}
}

func TestReclaimAfterConsumerDeath(t *testing.T) {
	bus, mr := newTestBus(t)
	_ = mr
	id := mustPublish(t, bus, "lease.tsk-1.nudged")
	// Consumer A receives but never acks (handler error), then dies.
	aCtx, aCancel := context.WithCancel(context.Background())
	go func() {
		_ = bus.Consume(aCtx, testReplayOpts("g", "dead-worker"), func(context.Context, Message) error {
			return errors.New("wedged handler")
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pend, err := bus.client.XPending(context.Background(), bus.key, "g").Result()
		if err == nil && pend.Count >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	aCancel()
	// Consumer B (same group, different name) reclaims A's pending
	// after MinIdle and delivers it.
	var gotIDs sync.Map
	bCtx, bCancel := context.WithCancel(context.Background())
	defer bCancel()
	go func() {
		_ = bus.Consume(bCtx, testOpts("g", "heir-worker"), func(_ context.Context, m Message) error {
			gotIDs.Store(m.ID, m.Attempts)
			return nil
		})
	}()
	bDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(bDeadline) {
		if _, ok := gotIDs.Load(id); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	bCancel()
	got, ok := gotIDs.Load(id)
	if !ok {
		t.Fatal("heir consumer never reclaimed the dead consumer's pending entry")
	}
	if got.(int64) < 2 {
		t.Fatalf("reclaimed message attempts = %d, want >=2 (durable delivery count)", got)
	}
}

func TestRestartUnderOwnNameResumesPending(t *testing.T) {
	bus, _ := newTestBus(t)
	id := mustPublish(t, bus, "session.ses_1.idle")
	// First incarnation receives, fails (pending), and exits.
	firstDone := make(chan struct{})
	ctx1, cancel1 := context.WithCancel(context.Background())
	go func() {
		defer close(firstDone)
		_ = bus.Consume(ctx1, testReplayOpts("g", "same-name"), func(context.Context, Message) error {
			cancel1()
			return errors.New("died mid-handler")
		})
	}()
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("first incarnation never finished")
	}
	// Second incarnation, SAME consumer name: drain-own-pending
	// delivers it immediately (no MinIdle wait).
	var got sync.Map
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() {
		_ = bus.Consume(ctx2, testOpts("g", "same-name"), func(_ context.Context, m Message) error {
			got.Store(m.ID, true)
			return nil
		})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := got.Load(id); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel2()
	if _, ok := got.Load(id); !ok {
		t.Fatal("restart under own name did not resume its pending entry")
	}
}

func TestReplayFromStreamStartForNewGroup(t *testing.T) {
	bus, _ := newTestBus(t)
	// History published BEFORE the group exists.
	mustPublish(t, bus, "session.ses_1.idle")
	mustPublish(t, bus, "session.ses_2.error")
	opts := testOpts("latecomer", "c")
	opts.StartAt = StartReplay
	got := consumeUntil(t, bus, opts, func(ms []Message) bool { return len(ms) >= 2 })
	if len(got) != 2 {
		t.Fatalf("replaying group saw %d entries, want 2", len(got))
	}
	if got[0].Event.Topic != "session.ses_1.idle" || got[1].Event.Topic != "session.ses_2.error" {
		t.Fatalf("replay order wrong: %v", got)
	}
	// And the live tail still flows after the replay.
	if _, err := bus.Publish(context.Background(), Event{Topic: "session.ses_3.idle"}); err != nil {
		t.Fatalf("publish live tail: %v", err)
	}
	got2 := consumeUntil(t, bus, opts, func(ms []Message) bool { return len(ms) >= 1 })
	if len(got2) != 1 || got2[0].Event.Topic != "session.ses_3.idle" {
		t.Fatalf("live tail after replay: %v", got2)
	}
}

func TestStartLiveSkipsHistory(t *testing.T) {
	bus, _ := newTestBus(t)
	mustPublish(t, bus, "session.ses_1.idle") // pre-group history
	got := consumeUntil2(t, bus, testOpts("live-only", "c"), 300*time.Millisecond)
	if len(got) != 0 {
		t.Fatalf("StartLive group saw %d pre-existing entries, want 0", len(got))
	}
}

func TestMaxLenCapsStream(t *testing.T) {
	bus, _ := newTestBus(t) // maxLen 128
	ctx := context.Background()
	for i := 0; i < 300; i++ {
		if _, err := bus.Publish(ctx, Event{Topic: fmt.Sprintf("session.ses_%d.idle", i)}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	n, err := bus.Len(ctx)
	if err != nil {
		t.Fatalf("XLEN: %v", err)
	}
	// Approximate trim (~): 128 + a small margin, never 300.
	if n > 160 {
		t.Fatalf("stream length %d exceeds the ~128 cap", n)
	}
}

func TestPoisonDropAfterMaxAttempts(t *testing.T) {
	bus, _ := newTestBus(t)
	mustPublish(t, bus, "lease.tsk-1.nudged")
	var mu sync.Mutex
	deliveries := 0
	poisoned := make(chan Message, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		opts := testReplayOpts("g", "c")
		opts.OnPoison = func(m Message) { poisoned <- m }
		_ = bus.Consume(ctx, opts, func(context.Context, Message) error {
			mu.Lock()
			deliveries++
			mu.Unlock()
			return errors.New("always fails")
		})
	}()
	select {
	case m := <-poisoned:
		if m.Event.Topic != "lease.tsk-1.nudged" {
			t.Fatalf("poison message topic = %q", m.Event.Topic)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("poison message never dropped")
	}
	cancel()
	mu.Lock()
	defer mu.Unlock()
	if deliveries > 4 { // cap 3 + in-flight margin
		t.Fatalf("poison delivered %d times past the cap", deliveries)
	}
}

func TestOnErrorObservesHandlerFailures(t *testing.T) {
	bus, _ := newTestBus(t)
	mustPublish(t, bus, "session.ses_1.idle")
	opts := testReplayOpts("g", "c")
	saw := make(chan struct{}, 4)
	opts.OnError = func(_ Message, err error) {
		if err == nil || err.Error() != "boom" {
			t.Errorf("OnError got %v", err)
		}
		saw <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = bus.Consume(ctx, opts, func(context.Context, Message) error { return errors.New("boom") })
	}()
	select {
	case <-saw:
	case <-time.After(5 * time.Second):
		t.Fatal("OnError never observed the handler failure")
	}
}

func TestConsumeOptionValidation(t *testing.T) {
	bus, _ := newTestBus(t)
	ctx := context.Background()
	if err := bus.Consume(ctx, ConsumeOptions{Consumer: "c"}, nil); err == nil {
		t.Error("missing group accepted")
	}
	if err := bus.Consume(ctx, ConsumeOptions{Group: "g"}, nil); err == nil {
		t.Error("missing consumer accepted")
	}
	if err := bus.Consume(ctx, ConsumeOptions{Group: "g", Consumer: "c", StartAt: "middle"}, nil); err == nil {
		t.Error("bad StartAt accepted")
	}
	if err := bus.Consume(ctx, ConsumeOptions{Group: "g", Consumer: "c"}, nil); err == nil {
		t.Error("nil handler accepted")
	}
}

func TestNewRedisBusRefusesDeadClient(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.Close() // dead
	if _, err := NewRedisBus(context.Background(), client, StreamKey("ws"), 0); err == nil {
		t.Fatal("bus constructed over a dead redis")
	}
	if _, err := NewRedisBus(context.Background(), nil, "k", 0); err == nil {
		t.Fatal("bus constructed over a nil client")
	}
}
