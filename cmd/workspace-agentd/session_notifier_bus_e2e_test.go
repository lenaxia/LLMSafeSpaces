// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// session_notifier_bus_e2e_test.go — design 0072's delivery-layer
// evolution, end to end over the REAL wiring: the same wiringEnv
// battery as session_notifier_wiring_e2e_test.go (real authority,
// real wireSessionNotifier, real loops, real seam-POST vehicle, fake
// harness), plus a REAL eventbus.RedisBus over miniredis —
// publish-once on the settle, the notifier consumer group fanning
// out per subscriber with consumer-group ack/pending as the
// at-least-once engine.
//
// The pinned equivalences (the charter's criterion 3): same
// suppression semantics, same rate bounds, same compose — ONLY the
// transport differs. Every assertion here mirrors its v1 twin in the
// wiring battery; the deltas asserted are: the settle was PUBLISHED
// to the workspace stream, and the outbox stays EMPTY (the stream
// entry is the durable intent on this path).

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/lenaxia/llmsafespaces/pkg/eventbus"
)

const busWorkspaceID = "ws-bus-e2e"

// busEnv is a wiringEnv plus the real bus over miniredis and the
// consumer groups main.go runs (runEventbusConsumers, exactly).
type busEnv struct {
	wiringEnv
	bus *eventbus.RedisBus
	mr  *miniredis.Miniredis
	rdb *redis.Client
}

// newBusEnv wires the full stack with the bus ENABLED: the base
// wiring env (real authority + fake harness), then the engine
// re-wired with the Bus seam, then the loops + consumer groups.
func newBusEnv(t *testing.T, debounce time.Duration, sessions ...fakeHarnessSession) *busEnv {
	t.Helper()
	base := newWiringEnv(t, debounce, sessions...)

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	bus, err := eventbus.NewRedisBus(context.Background(), rdb, eventbus.StreamKey(busWorkspaceID), 128)
	require.NoError(t, err)

	// Re-wire the engine with the bus (the design 0072 switch); the
	// install/cleanup discipline is wiringEnv.rewire's — one more
	// generation over the same store path.
	prev := currentSessionNotifier()
	t.Cleanup(func() { setSessionNotifier(prev) })
	notifier := wireSessionNotifier(sessionNotifierWiring{
		Authority: base.authority,
		Password:  base.password,
		StorePath: base.storePath,
		Bus:       bus,
		Logger:    zap.NewNop(),
	})
	setSessionNotifier(notifier)
	require.Same(t, notifier, currentSessionNotifier(), "the bus-wired engine is the MCP surface's engine")
	base.notifier = notifier

	env := &busEnv{wiringEnv: *base, bus: bus, mr: mr, rdb: rdb}
	base.startConsumer(t)

	// The consumer wiring main.go performs after wireSessionNotifier.
	ctx, cancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}
	busLogger := zap.NewNop()
	if os.Getenv("BUS_E2E_DEBUG") != "" {
		busLogger, _ = zap.NewDevelopment()
	}
	runEventbusConsumers(ctx, wg, env.bus, env.notifier, env.password, busLogger)
	done := make(chan struct{})
	go func() { defer close(done); wg.Wait() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("event bus consumers did not drain within 5s of cancel")
		}
	})
	return env
}

// streamEntryCount is the producer-side observable: how many entries
// the settle fold published to the workspace stream.
func (e *busEnv) streamEntryCount(t *testing.T) int64 {
	t.Helper()
	n, err := e.rdb.XLen(context.Background(), e.bus.Key()).Result()
	require.NoError(t, err)
	return n
}

// groupPending reads the group's pending count (the ack observable).
func (e *busEnv) groupPending(t *testing.T, group string) int64 {
	t.Helper()
	p, err := e.rdb.XPending(context.Background(), e.bus.Key(), group).Result()
	require.NoError(t, err)
	return p.Count
}

// waitForGroup blocks until the consumer group exists
// (runEventbusConsumers creates groups on their goroutines; a test
// that publishes before the group's creation would race StartLive
// semantics — the production boot order is group-then-events, and
// this pins that order without sleeping). Probe: XPENDING answers
// NOGROUP while the group is absent (miniredis has no XINFO GROUPS).
func (e *busEnv) waitForGroup(t *testing.T, group string) {
	t.Helper()
	require.Eventually(t, func() bool {
		_, err := e.rdb.XPending(context.Background(), e.bus.Key(), group).Result()
		return err == nil
	}, 5*time.Second, 5*time.Millisecond, "consumer group %s never appeared", group)
}

func (e *busEnv) postsTo(sessionID string) []fakePost {
	e.harness.mu.Lock()
	defer e.harness.mu.Unlock()
	var out []fakePost
	for _, p := range e.harness.posts {
		if p.path == "/session/"+sessionID+"/message" {
			out = append(out, p)
		}
	}
	return out
}

// ---- (a) the settle publishes once; the notifier consumer delivers ----

// TestBusE2E_HarnessIdleSettlePublishesAndConsumerDelivers: the plain
// busy→idle fold through the real stream and debounce — on the bus
// path the settle becomes ONE stream entry and the notifier consumer
// group turns it into the per-subscriber POST; the outbox stays empty
// (the stream entry is the durable intent).
func TestBusE2E_HarnessIdleSettlePublishesAndConsumerDelivers(t *testing.T) {
	env := newBusEnv(t, 40*time.Millisecond,
		fakeHarnessSession{id: e2eBroadcaster, title: "worker"},
		fakeHarnessSession{id: e2eSubscriber, title: "manager"},
	)
	e2eSubscribe(t, &env.wiringEnv, nil)

	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_BUSY)
	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_IDLE)

	// The delivery (consumer group → seam POST) — the same observable
	// as v1: one wake-up POST to the subscriber, v1 wording + sentinel.
	require.Eventually(t, func() bool { return len(env.postsTo(e2eSubscriber)) >= 1 },
		5*time.Second, 10*time.Millisecond, "the notifier consumer never delivered the settle")
	time.Sleep(200 * time.Millisecond)
	posts := env.postsTo(e2eSubscriber)
	assert.Len(t, posts, 1, "exactly one wake-up POST on the bus path")
	require.Len(t, posts, 1)
	assert.Contains(t, posts[0].body, "is now idle — its turn is complete")
	assert.Contains(t, posts[0].body, "lsp:agent-message-v1", "the v1 provenance sentinel composes unchanged")

	// The producer-side pin: ONE stream entry for the settle, on the
	// workspace-scoped key.
	require.Eventually(t, func() bool { return env.streamEntryCount(t) >= 1 }, 2*time.Second, 10*time.Millisecond)
	assert.EqualValues(t, 1, env.streamEntryCount(t), "publish-once per settle")

	// The outbox stays empty on the bus path (the pending-entry
	// semantics replace the write-before-send intent).
	assert.Zero(t, env.notifier.store.outboxLen(), "no outbox intent on the bus path")

	// The group's pending list drains (the entry was ACKed).
	require.Eventually(t, func() bool { return env.groupPending(t, eventbus.GroupNotifier) == 0 },
		2*time.Second, 10*time.Millisecond, "the delivered settle was never acked")
}

// ---- (c) suppression: same semantics through the consumer ----

// TestBusE2E_SuppressionSendThenIdleSuppresses: the broadcaster's
// completed send_message to the subscriber postdates its last
// mutation — the CONSUMER suppresses (the ledger sits in the engine,
// bus-agnostic; only the transport changed).
func TestBusE2E_SuppressionSendThenIdleSuppresses(t *testing.T) {
	env := newBusEnv(t, 40*time.Millisecond,
		fakeHarnessSession{id: e2eBroadcaster, title: "worker"},
		fakeHarnessSession{id: e2eSubscriber, title: "manager"},
	)
	e2eSubscribe(t, &env.wiringEnv, nil)

	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_BUSY)
	// Mutate, then send the final word to the subscriber.
	e2eToolEndEvt(env.authority, e2eBroadcaster, "prt_edit", "edit", `{"path":"x"}`, abiv1.ToolStatus_TOOL_STATUS_COMPLETED)
	e2eToolEndEvt(env.authority, e2eBroadcaster, "prt_send", "send_message",
		`{"destination":"`+e2eSubscriber+`","message":"done"}`, abiv1.ToolStatus_TOOL_STATUS_COMPLETED)
	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_IDLE)

	// The settle still publishes (the producer does not suppress —
	// suppression is a per-subscriber consumer decision).
	require.Eventually(t, func() bool { return env.streamEntryCount(t) >= 1 }, 2*time.Second, 10*time.Millisecond,
		"the settle never published")
	// ... but NO delivery lands, and the entry acks out.
	require.Eventually(t, func() bool { return env.groupPending(t, eventbus.GroupNotifier) == 0 },
		3*time.Second, 10*time.Millisecond, "the suppressed settle never acked")
	assert.Empty(t, env.postsTo(e2eSubscriber), "suppressed settle must not deliver (§4.4 through the consumer)")
}

// ---- the at-least-once engine: pending redelivery ----

// TestBusE2E_ConsumerFailureRedelivers: the seam POST fails under the
// consumer — the message stays PENDING (no ack), and after the
// harness heals, the group's reclaim redelivers it: the retry the v1
// outbox used to own, now consumer-group machinery. Driven on a
// short-window consumer loop (production MinIdle is 60s; the seam
// under test is ConsumeSettleEvent + the bus, not the clock).
func TestBusE2E_ConsumerFailureRedelivers(t *testing.T) {
	env := newBusEnv(t, 40*time.Millisecond,
		fakeHarnessSession{id: e2eBroadcaster, title: "worker"},
		fakeHarnessSession{id: e2eSubscriber, title: "manager"},
	)
	e2eSubscribe(t, &env.wiringEnv, nil)

	// A short-window notifier consumer (second member of the SAME
	// group the env's runEventbusConsumers created).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = env.bus.Consume(ctx, eventbus.ConsumeOptions{
			Group: eventbus.GroupNotifier, Consumer: "e2e-fast",
			Topics:  []string{"session.*.*"},
			Block:   20 * time.Millisecond,
			MinIdle: 80 * time.Millisecond, ReclaimTick: 25 * time.Millisecond,
			Batch: 8, MaxAttempts: 8,
		}, env.notifier.ConsumeSettleEvent)
	}()

	env.harness.setFailPost(true)
	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_BUSY)
	e2eStatusEvt(env.authority, e2eBroadcaster, abiv1.SessionStatus_SESSION_STATUS_IDLE)

	// The entry lands pending (delivered, delivery failed, no ack).
	require.Eventually(t, func() bool { return env.groupPending(t, eventbus.GroupNotifier) >= 1 },
		5*time.Second, 10*time.Millisecond, "the failed delivery never became pending")

	// The harness heals; the reclaim redelivers; the retry delivers
	// and acks.
	env.harness.setFailPost(false)
	require.Eventually(t, func() bool { return len(env.postsTo(e2eSubscriber)) >= 1 },
		8*time.Second, 20*time.Millisecond, "the pending settle was never redelivered after the harness healed")
	require.Eventually(t, func() bool { return env.groupPending(t, eventbus.GroupNotifier) == 0 },
		5*time.Second, 10*time.Millisecond, "the redelivered settle was never acked")
	assert.Zero(t, env.notifier.store.outboxLen(), "the bus path never touches the outbox — even on retry")
}

// ---- the lease consumer over the real bus ----

// TestBusE2E_LeaseConsumerDeliversNudge: publish lease.{task}.nudged
// exactly as the API's nudge ladder will — the lease consumer group
// translates it into the sentinel SessionSend (the NudgeSender
// generalization). The sentinel body is pinned byte-exact against
// the grammar the API composes (api/internal/app/board_liveness.go).
func TestBusE2E_LeaseConsumerDeliversNudge(t *testing.T) {
	env := newBusEnv(t, 40*time.Millisecond,
		fakeHarnessSession{id: "ses_assignee", title: "worker"},
	)
	env.waitForGroup(t, eventbus.GroupLease)
	_, err := env.bus.Publish(context.Background(), eventbus.Event{
		Topic: eventbus.LeaseNudgeTopic("0197f0aa-1111-2222-3333-444455556666"),
		At:    time.Now(),
		Payload: map[string]string{
			"task":          "0197f0aa-1111-2222-3333-444455556666",
			"task_short_id": "tsk-crimson-finch-blooms",
			"nudge_id":      "ndg-123",
			"assignee":      "ses_assignee",
		},
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool { return len(env.postsTo("ses_assignee")) >= 1 },
		5*time.Second, 10*time.Millisecond, "the lease consumer never delivered the nudge sentinel (xlen=%d pend=%d posts=%d)",
		env.streamEntryCount(t), env.groupPending(t, eventbus.GroupLease), env.harness.postCount())
	posts := env.postsTo("ses_assignee")
	require.Len(t, posts, 1)
	body := posts[0].body
	assert.Contains(t, body, "<!-- lsp:agent-message-v1 ", "the sentinel marker line")
	assert.Contains(t, body, `"mode":"lease-nudge"`, "the sentinel's mode key")
	assert.Contains(t, body, `"taskId":"tsk-crimson-finch-blooms"`, "the ordinal anchor's task")
	assert.Contains(t, body, `"nudgeId":"ndg-123"`, "the ordinal anchor")
	assert.Contains(t, body, `"expects":"respond|advance|fail"`, "the response contract")
	assert.Contains(t, body, "BOARD LEASE NUDGE: your claim on task tsk-crimson-finch-blooms has gone quiet", "the directive prose")
	assert.Contains(t, body, "First RESPOND (call board_heartbeat once", "the respond-first ruling")
	require.Eventually(t, func() bool { return env.groupPending(t, eventbus.GroupLease) == 0 },
		2*time.Second, 10*time.Millisecond, "the delivered nudge was never acked")
}

// TestBusE2E_LeaseWedgeVerdictLogsNotMessages: the wedge verdict is
// informational at this consumer — the row is reclaimed at the
// engine; a message to a wedged worker is noise. Pinned: acked, no
// POST.
func TestBusE2E_LeaseWedgeVerdictLogsNotMessages(t *testing.T) {
	env := newBusEnv(t, 40*time.Millisecond,
		fakeHarnessSession{id: "ses_assignee", title: "worker"},
	)
	env.waitForGroup(t, eventbus.GroupLease)
	_, err := env.bus.Publish(context.Background(), eventbus.Event{
		Topic: eventbus.LeaseWedgeTopic("0197f0aa-1111-2222-3333-444455556666"),
		At:    time.Now(),
		Payload: map[string]string{
			"task": "0197f0aa-1111-2222-3333-444455556666", "assignee": "ses_assignee", "verdict": "wedged",
		},
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return env.groupPending(t, eventbus.GroupLease) == 0 },
		3*time.Second, 10*time.Millisecond, "the wedge verdict never acked")
	assert.Zero(t, env.harness.postCount(), "a wedge verdict must not message the wedged worker")
}
