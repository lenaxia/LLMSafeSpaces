// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package eventbus

// health_test.go — the S3 health surface pinned over miniredis: the
// shapes the operator signals are built from. A group that exists but
// never reads (the dead-group shape), a wedged group whose pending
// idle grows past every reclaim threshold (the stalled shape), the
// healthy drained bus, and the observer seams that feed the counters
// (publish outcomes, every dispatch outcome).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/require"
)

// healthOf snapshots with the test's failure ergonomics.
func healthOf(t *testing.T, bus *RedisBus) *StreamHealth {
	t.Helper()
	h, err := bus.Health(context.Background())
	require.NoError(t, err)
	return h
}

// groupOf returns the named group's health, failing the test when the
// group is absent from the snapshot.
func groupOf(t *testing.T, h *StreamHealth, name string) GroupHealth {
	t.Helper()
	for _, g := range h.Groups {
		if g.Name == name {
			return g
		}
	}
	t.Fatalf("group %q missing from health snapshot: %+v", name, h.Groups)
	return GroupHealth{}
}

// TestHealthEmptyStream — a bus with no publishes and no groups is a
// ZERO snapshot, not an error: an empty bus is healthy until a
// backlog says otherwise.
func TestHealthEmptyStream(t *testing.T) {
	bus, _ := newTestBus(t)
	h := healthOf(t, bus)
	require.Zero(t, h.Length)
	require.Empty(t, h.LastEntryID)
	require.True(t, h.LastPublishAt.IsZero())
	require.Empty(t, h.Groups)
}

// TestHealthReportsLastPublish — the newest entry's `at` field is the
// publish-side liveness signal (the silent-publisher input).
func TestHealthReportsLastPublish(t *testing.T) {
	bus, _ := newTestBus(t)
	at := time.Now().Add(-90 * time.Second).Truncate(time.Millisecond).UTC()
	_, err := bus.Publish(context.Background(), Event{Topic: "session.ses_1.idle", At: at})
	require.NoError(t, err)
	later := at.Add(30 * time.Second)
	_, err = bus.Publish(context.Background(), Event{Topic: "session.ses_2.error", At: later})
	require.NoError(t, err)
	h := healthOf(t, bus)
	require.EqualValues(t, 2, h.Length)
	require.Equal(t, later, h.LastPublishAt, "LastPublishAt must be the NEWEST entry's at")
	require.NotEmpty(t, h.LastEntryID)
}

// TestHealthDeadGroupBacklog — the S3 namesake: a group that exists
// but has NO consumers and NO reads accumulates lag — silence on the
// bus is no longer indistinguishable from a stalled group.
func TestHealthDeadGroupBacklog(t *testing.T) {
	bus, _ := newTestBus(t)
	primeGroup(t, bus, "notifier")
	for i := 0; i < 5; i++ {
		mustPublish(t, bus, fmt.Sprintf("session.ses_%d.idle", i))
	}
	h := healthOf(t, bus)
	g := groupOf(t, h, "notifier")
	require.EqualValues(t, 5, g.Lag, "unread entries must count as the group's lag")
	require.Zero(t, g.Pending, "nothing was delivered, so nothing is pending")
	require.Zero(t, g.Consumers, "no consumer ever joined")
}

// TestHealthLiveGroupDrains — the healthy shape: a consuming group
// holds lag 0 and pending 0 after delivery.
func TestHealthLiveGroupDrains(t *testing.T) {
	bus, _ := newTestBus(t)
	primeGroup(t, bus, "g")
	mustPublish(t, bus, "session.ses_1.idle")
	got := consumeUntil(t, bus, testOpts("g", "c"), oneMessage)
	require.Len(t, got, 1)
	h := healthOf(t, bus)
	gh := groupOf(t, h, "g")
	require.Zero(t, gh.Lag, "a drained group has no backlog")
	require.Zero(t, gh.Pending, "an acked delivery leaves no pending")
	require.EqualValues(t, 1, gh.Consumers)
}

// TestHealthWedgedGroupPendingIdle — the second S3 namesake: a group
// whose handler never succeeds (or whose consumers all died holding
// deliveries) shows the pending entry's idle GROWING past every
// reclaim threshold. The miniredis clock advances deterministically —
// no sleeps. The idle pin is ABSOLUTE (review r1: a unit-conversion
// bug shipped green under a lower-bound-only assertion), and
// MaxPendingAttempts is pinned to 1 after exactly one delivery
// (Redis's delivery count is 1-based).
func TestHealthWedgedGroupPendingIdle(t *testing.T) {
	bus, mr := newTestBus(t)
	mr.SetTime(time.Now().UTC()) // freeze: idle is now clock arithmetic
	t.Cleanup(func() { mr.SetTime(time.Time{}) })
	mustPublish(t, bus, "session.ses_1.idle")

	// Deliver RAW (one XREADGROUP, no ack): exactly one delivery, no
	// reclaim loop racing the clock — RetryCount 1 and lastDelivery at
	// the frozen T0, both deterministic.
	ctx := context.Background()
	require.NoError(t, bus.client.XGroupCreateMkStream(ctx, bus.key, "g", "0-0").Err())
	res, err := bus.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: "g", Consumer: "c", Streams: []string{bus.key, ">"},
		Count: 8, Block: 50 * time.Millisecond,
	}).Result()
	require.NoError(t, err)
	require.Len(t, res[0].Messages, 1)

	h := healthOf(t, bus)
	g := groupOf(t, h, "g")
	require.EqualValues(t, 1, g.Pending, "the delivered-unacked entry must sit on the PEL")
	require.EqualValues(t, 1, g.MaxPendingAttempts, "one delivery = attempts 1 (Redis's delivery count is 1-based — no off-by-one)")
	require.Zero(t, g.Lag, "the entry WAS delivered (cursor advanced) — it is pending, not lagging")
	require.EqualValues(t, 1, g.Consumers)

	// Advance the wall clock by EXACTLY 2 minutes: the pending entry's
	// idle must be 2 minutes — an absolute pin (± a small ε), not a
	// lower bound.
	mr.SetTime(time.Now().UTC().Add(2 * time.Minute))
	h = healthOf(t, bus)
	g = groupOf(t, h, "g")
	require.GreaterOrEqual(t, g.OldestPendingIdle, 2*time.Minute,
		"pending idle must track the clock — the stalled-group signal")
	require.LessOrEqual(t, g.OldestPendingIdle, 2*time.Minute+30*time.Second,
		"pending idle must not be inflated (a unit bug reads as years — review r1's finding)")

	// A live consumer reclaims past MinIdle and acks: the healthy
	// resolution of a pending episode.
	got := consumeUntil(t, bus, testOpts("g", "c"), oneMessage)
	require.Len(t, got, 1)
	h = healthOf(t, bus)
	require.Zero(t, groupOf(t, h, "g").Pending, "the reclaimed+acked entry leaves the PEL")
}

// TestHealthLagCappedAtProbeBound — a backlog past HealthMaxLag
// reports the cap, not the exact count: unambiguously stalled is
// enough, and the exact number is not worth the rounds.
func TestHealthLagCappedAtProbeBound(t *testing.T) {
	bus, _ := newTestBus(t) // maxLen 128 — publish raw to exceed the cap
	ctx := context.Background()
	require.NoError(t, bus.client.XGroupCreateMkStream(ctx, bus.key, "g", "$").Err())
	n := int(HealthMaxLag + 200)
	for i := 0; i < n; i++ {
		require.NoError(t, bus.client.XAdd(ctx, &redis.XAddArgs{
			Stream: bus.key,
			Values: map[string]interface{}{
				FieldTopic: fmt.Sprintf("session.ses_%d.idle", i),
				FieldAt:    time.Now().UTC().Format(time.RFC3339Nano),
			},
		}).Err())
	}
	h := healthOf(t, bus)
	g := groupOf(t, h, "g")
	require.Equal(t, HealthMaxLag, g.Lag, "lag must clamp at the probe bound")
}

// TestHealthMultipleGroupsFanOutView — two groups, one stream: each
// group's lag is its own (one drained, one never-read), the fan-out
// per-group view the operator needs.
func TestHealthMultipleGroupsFanOutView(t *testing.T) {
	bus, _ := newTestBus(t)
	primeGroup(t, bus, "drained")
	primeGroup(t, bus, "never-read")
	mustPublish(t, bus, "session.ses_1.idle")
	got := consumeUntil(t, bus, testOpts("drained", "c"), oneMessage)
	require.Len(t, got, 1)
	h := healthOf(t, bus)
	require.Zero(t, groupOf(t, h, "drained").Lag)
	require.EqualValues(t, 1, groupOf(t, h, "never-read").Lag)
}

// ---- the observer seams (the counter inputs) ----

// TestPublishObserverSeesOutcomes — every Publish outcome passes the
// seam: success with its id, validation failure, transport failure.
func TestPublishObserverSeesOutcomes(t *testing.T) {
	bus, _ := newTestBus(t)
	var mu sync.Mutex
	type obs struct {
		topic string
		id    string
		err   error
	}
	var seen []obs
	bus.SetPublishObserver(func(ev Event, id string, err error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, obs{ev.Topic, id, err})
	})

	// Success: id present, no error.
	id, err := bus.Publish(context.Background(), Event{Topic: "session.ses_1.idle"})
	require.NoError(t, err)
	// Validation failure: no id, the validation error.
	_, err = bus.Publish(context.Background(), Event{Topic: " "})
	require.Error(t, err)

	// Transport failure on a SEPARATE bus whose Redis died: its own
	// observer sees the XADD error.
	dead, derr := miniredisDeadBus(t)
	require.NoError(t, derr)
	deadSeen := make(chan error, 1)
	dead.SetPublishObserver(func(ev Event, id string, err error) {
		select {
		case deadSeen <- err:
		default:
		}
	})
	_, err = dead.Publish(context.Background(), Event{Topic: "session.ses_2.idle"})
	require.Error(t, err)
	select {
	case observed := <-deadSeen:
		require.Error(t, observed, "the transport failure must pass the observer")
	case <-time.After(5 * time.Second):
		t.Fatal("the dead-bus publish was never observed")
	}

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, seen, 2, "success and validation failure both pass the seam")
	require.Equal(t, "session.ses_1.idle", seen[0].topic)
	require.Equal(t, id, seen[0].id)
	require.NoError(t, seen[0].err)
	require.Empty(t, seen[1].id, "a failed publish carries no id")
	require.Error(t, seen[1].err)
}

// miniredisDeadBus builds a live bus over miniredis, then kills the
// server — Publish on the returned bus fails at the transport.
func miniredisDeadBus(t *testing.T) (*RedisBus, error) {
	t.Helper()
	mr2, err := miniredis.Run()
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(&redis.Options{Addr: mr2.Addr()})
	bus, err := NewRedisBus(context.Background(), client, StreamKey("ws-dead"), 0)
	if err != nil {
		mr2.Close()
		return nil, err
	}
	t.Cleanup(func() { _ = client.Close() })
	mr2.Close()
	return bus, nil
}

// TestOnDeliverySeesEveryOutcome — the dispatch outcomes the consume
// counters are built from: delivered, filtered, handler_error, poison
// — plus the durable Attempts count that marks redeliveries.
func TestOnDeliverySeesEveryOutcome(t *testing.T) {
	bus, _ := newTestBus(t)
	primeGroup(t, bus, "g")
	// One matching, one filtered, one that fails forever (poison).
	mustPublish(t, bus, "session.ses_1.idle")
	mustPublish(t, bus, "lease.tsk-1.nudged")
	mustPublish(t, bus, "session.ses_2.error")

	var mu sync.Mutex
	var events []DeliveryEvent
	opts := testOpts("g", "c", "session.*.idle", "session.*.error")
	opts.OnDelivery = func(ev DeliveryEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}
	// ses_1.idle: deliver+ack on first attempt. ses_2.error: fail once
	// (handler_error), then redeliver (attempts=2) and ack. The lease
	// topic is filtered. The poison cap is exercised by its own test
	// below.
	failOnce := map[string]bool{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = bus.Consume(ctx, opts, func(_ context.Context, m Message) error {
			if m.Event.Topic == "session.ses_2.error" && !failOnce[m.ID] {
				failOnce[m.ID] = true
				return errors.New("transient")
			}
			return nil
		})
	}()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) >= 4
	}, 5*time.Second, 5*time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	byOutcome := map[DeliveryOutcome]int{}
	redelivered := false
	for _, ev := range events {
		byOutcome[ev.Outcome]++
		require.Equal(t, "g", ev.Group)
		require.Equal(t, "c", ev.Consumer)
		require.NotEmpty(t, ev.Topic)
		if ev.Outcome == DeliveryDelivered && ev.Attempts > 1 {
			redelivered = true
		}
	}
	require.Equal(t, 1, byOutcome[DeliveryFiltered], "the lease topic must observe filtered")
	require.GreaterOrEqual(t, byOutcome[DeliveryDelivered], 2, "ses_1 (first try) and ses_2 (redelivery) both deliver")
	require.Equal(t, 1, byOutcome[DeliveryHandlerError], "ses_2's first attempt must observe handler_error")
	require.True(t, redelivered, "the redelivered delivery must carry Attempts > 1")
}

// TestOnDeliverySeesPoison — the poison drop (ack-and-drop past the
// cap) observes its own outcome.
func TestOnDeliverySeesPoison(t *testing.T) {
	bus, _ := newTestBus(t)
	mustPublish(t, bus, "lease.tsk-1.nudged")
	var mu sync.Mutex
	var outcomes []DeliveryOutcome
	opts := testReplayOpts("g", "c")
	opts.OnDelivery = func(ev DeliveryEvent) {
		mu.Lock()
		defer mu.Unlock()
		outcomes = append(outcomes, ev.Outcome)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = bus.Consume(ctx, opts, func(context.Context, Message) error {
			return errors.New("always fails")
		})
	}()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, o := range outcomes {
			if o == DeliveryPoison {
				return true
			}
		}
		return false
	}, 5*time.Second, 5*time.Millisecond)
	cancel()
	<-done
}

// TestOnDeliveryAckErrorAndNilObserver — the two dispatch edges the
// e2e paths cannot reach deterministically over live miniredis
// (review r1's missing cases): an ACK failure observes
// DeliveryAckError alongside OnError, and a nil OnDelivery leaves the
// pre-S3 dispatch behavior intact (no observation, no panic, same
// outcome).
func TestOnDeliveryAckErrorAndNilObserver(t *testing.T) {
	bus, mr := newTestBus(t)
	ctx := context.Background()
	msg := Message{ID: mustPublish(t, bus, "session.ses_1.idle"), Event: Event{Topic: "session.ses_1.idle"}, Attempts: 1}

	// Force EVERY command to fail: the handler succeeds, the XAck
	// inside dispatch fails → DeliveryAckError.
	mr.SetError("LOADING redis is loading the dataset in memory")
	t.Cleanup(func() { mr.SetError("") })
	var mu sync.Mutex
	var got []DeliveryEvent
	var onErr error
	opts := testOpts("g", "c")
	opts.OnDelivery = func(ev DeliveryEvent) { mu.Lock(); got = append(got, ev); mu.Unlock() }
	opts.OnError = func(_ Message, err error) { onErr = err }
	bus.dispatch(ctx, &opts, func(context.Context, Message) error { return nil }, msg)
	mu.Lock()
	require.Len(t, got, 1, "the failed ack must observe exactly one outcome")
	require.Equal(t, DeliveryAckError, got[0].Outcome)
	require.Error(t, onErr, "OnError still carries the ack failure")
	mu.Unlock()

	// nil OnDelivery: identical flow, no observation, no panic.
	opts2 := testOpts("g", "c") // OnDelivery nil
	require.NotPanics(t, func() {
		bus.dispatch(ctx, &opts2, func(context.Context, Message) error { return nil }, msg)
	})
	mu.Lock()
	require.Len(t, got, 1, "a nil observer must not add observations")
	mu.Unlock()
}
