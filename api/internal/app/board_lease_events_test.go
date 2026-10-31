// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_lease_events_test.go — the S4 publisher pins (design 0072):
// the nudge ladder's bus-first delivery with the v1 point-to-point
// fallback (bus down ⇒ behavior unchanged — the charter's preserve
// clause), the sink's topic/payload mapping, and the breaker that
// keeps a dead Redis from taxing every sentinel.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/eventbus"
)

// leaseTestBus arms a publisher over a live miniredis with the
// recording scope (every task lives in ws-1).
func leaseTestBus(t *testing.T) (*boardLeaseEventbus, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	bus := newBoardLeaseEventbus(mr.Addr(), "", func(context.Context, string) (string, string, error) {
		return "user-1", "ws-1", nil
	})
	require.NotNil(t, bus, "a live Redis must arm the publisher")
	return bus, mr
}

// streamEntries reads the workspace stream's raw entries (topic field
// first, the payload after) — the wire truth both consumers see.
func streamEntries(t *testing.T, addr, workspaceID string) []map[string]string {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()
	res, err := client.XRange(context.Background(), eventbus.StreamKey(workspaceID), "-", "+").Result()
	require.NoError(t, err)
	out := make([]map[string]string, 0, len(res))
	for _, m := range res {
		row := map[string]string{}
		for k, v := range m.Values {
			if s, ok := v.(string); ok {
				row[k] = s
			}
		}
		out = append(out, row)
	}
	return out
}

// --- the OFF posture ----------------------------------------------------------

func TestBoardLeaseEventbus_UnarmedIsNilAndInert(t *testing.T) {
	assert.Nil(t, newBoardLeaseEventbus("  ", "", nil), "an empty addr is the OFF posture")
	assert.Nil(t, newBoardLeaseEventbus("127.0.0.1:1", "", nil),
		"an unreachable Redis at boot is the OFF posture (the v1 leg keeps the ladder whole)")

	// The nil pointer is nil-receiver-safe: the sink handed to the
	// engine unconditionally must be inert, never a panic.
	var inert *boardLeaseEventbus
	inert.LeaseEventSink(context.Background(), board.LeaseEvent{Kind: board.LeaseEventRenewed, TaskID: "t"})
	assert.Error(t, inert.publish(context.Background(), "ws", "lease.t.nudged", nil), "an unwired publish reports it (the nudger's fallback keys on this)")
}

func TestBoardLeaseEventbus_MalformedAddrIsOff(t *testing.T) {
	assert.Nil(t, newBoardLeaseEventbus("redis://u:p@missing-host:6379", "pw", nil))
}

// --- the nudger: bus first, v1 fallback ---------------------------------------

func TestBoardLeaseNudger_BusFirstThenV1Fallback(t *testing.T) {
	bus, mr := leaseTestBus(t)
	hub := newBoardLivenessHub(nil)
	sender := &recordingV1Sender{}
	scope := func(context.Context, string) (string, string, error) { return "user-1", "ws-1", nil }
	nudger := newBoardNudger(scope, sender, hub, bus)

	p := board.NudgePayload{NudgeID: "n-1", TaskID: "task-9", TaskShortID: "tsk-x"}
	require.NoError(t, nudger(context.Background(), "ses_worker", p))

	// The bus leg: the nudged event is on the workspace stream, built
	// by the shared constructor, and NO v1 send happened — the group
	// is the delivery leg.
	entries := streamEntries(t, mr.Addr(), "ws-1")
	require.Len(t, entries, 1, "the nudge publishes exactly one event")
	assert.Equal(t, eventbus.LeaseNudgeTopic("task-9"), entries[0]["topic"])
	got := map[string]string{}
	for k, v := range entries[0] {
		if k != "topic" && k != "at" {
			got[k] = v
		}
	}
	assert.Equal(t, eventbus.LeaseNudgePayload("task-9", "tsk-x", "n-1", "ses_worker"), got,
		"the payload is the shared builder's contract verbatim")
	assert.Zero(t, sender.count(), "bus-delivered nudges take NO v1 direct send")

	// The ordinal send-mark exists BEFORE any delivery — the bus leg
	// keeps the mark-before-sentinel contract (a mark that precedes
	// its sentinel can only err false-alive).
	hub.mu.Lock()
	_, marked := hub.nudges["n-1"]
	hub.mu.Unlock()
	assert.True(t, marked, "the bus leg still marks the sentinel's ordinal anchor")

	// The bus dies mid-flight: the next nudge falls back to the v1
	// direct send VERBATIM (behavior unchanged — the phantom-nudge
	// contract: never record a sentinel that never landed).
	mr.Close()
	p2 := board.NudgePayload{NudgeID: "n-2", TaskID: "task-9", TaskShortID: "tsk-x"}
	require.NoError(t, nudger(context.Background(), "ses_worker", p2))
	require.Equal(t, 1, sender.count(), "a failed publish falls back to the v1 leg")
	assert.Contains(t, sender.sent[0].text, `"nudgeId":"n-2"`, "the fallback sentinel is the v1 grammar")

	// The breaker opened: the next nudge fails fast on the paused bus
	// and still delivers v1 (a dead Redis must not tax every sentinel
	// with a fresh timeout).
	start := time.Now()
	p3 := board.NudgePayload{NudgeID: "n-3", TaskID: "task-9", TaskShortID: "tsk-x"}
	require.NoError(t, nudger(context.Background(), "ses_worker", p3))
	require.Equal(t, 2, sender.count(), "the paused bus keeps falling back")
	assert.Less(t, time.Since(start), time.Second,
		"the breaker makes the fallback immediate while the bus pauses one sweep window")
}

func TestBoardLeaseNudger_ScopeErrorIsStillTheSeamError(t *testing.T) {
	bus, _ := leaseTestBus(t)
	sender := &recordingV1Sender{}
	scope := func(context.Context, string) (string, string, error) { return "", "", context.DeadlineExceeded }
	nudger := newBoardNudger(scope, sender, newBoardLivenessHub(nil), bus)
	err := nudger(context.Background(), "ses_worker", board.NudgePayload{NudgeID: "n", TaskID: "t"})
	require.Error(t, err, "the scope answer precedes any delivery path — bus or v1")
	assert.Zero(t, sender.count())
}

// --- the sink: the ladder's decisions on the stream ---------------------------

func TestBoardLeaseEventbus_SinkProjectsEveryKind(t *testing.T) {
	bus, mr := leaseTestBus(t)
	ctx := context.Background()

	bus.LeaseEventSink(ctx, board.LeaseEvent{
		Kind: board.LeaseEventRenewed, TaskID: "t1", TaskShortID: "tsk-1", ProjectID: "p",
		Assignee: "ses_w", Source: board.RenewSourceBusy,
	})
	bus.LeaseEventSink(ctx, board.LeaseEvent{
		Kind: board.LeaseEventWedged, TaskID: "t2", TaskShortID: "tsk-2", ProjectID: "p",
		Assignee: "ses_w", NudgeID: "ndg-9", Evidence: "no session events after the sentinel",
	})
	bus.LeaseEventSink(ctx, board.LeaseEvent{
		Kind: board.LeaseEventReclaimed, TaskID: "t3", TaskShortID: "tsk-3", ProjectID: "p",
		Assignee: "", Reason: board.ReclaimReasonDeliveryFailed, FailureCount: 4,
	})
	// Unknown kinds are not published (the vocabulary is a convention).
	bus.LeaseEventSink(ctx, board.LeaseEvent{Kind: "somenewkind", TaskID: "t4"})

	entries := streamEntries(t, mr.Addr(), "ws-1")
	require.Len(t, entries, 3)
	assert.Equal(t, eventbus.LeaseRenewedTopic("t1"), entries[0]["topic"])
	assert.Equal(t, "busy_gate", entries[0]["source"], "the renewal carries WHICH rung answered alive")
	assert.Equal(t, eventbus.LeaseWedgeTopic("t2"), entries[1]["topic"])
	assert.Equal(t, "no session events after the sentinel", entries[1]["evidence"],
		"the wedge verdict carries its evidence summary")
	assert.Equal(t, eventbus.LeaseReclaimedTopic("t3"), entries[2]["topic"])
	assert.Equal(t, "nudge_delivery_failed", entries[2]["reason"])
	assert.Equal(t, "4", entries[2]["failure_count"])
}

func TestBoardLeaseEventbus_SinkDropsAreFailOpen(t *testing.T) {
	bus, mr := leaseTestBus(t)
	ctx := context.Background()

	// A scope failure drops the event (observability, never a
	// dependency) — and the drop counts on the failed gauge.
	failedBefore := boardLeaseEventsFailed.Value()
	publishedBefore := boardLeaseEventsPublished.Value()
	bus.scope = func(context.Context, string) (string, string, error) { return "", "", context.DeadlineExceeded }
	bus.LeaseEventSink(ctx, board.LeaseEvent{Kind: board.LeaseEventRenewed, TaskID: "t1"})
	assert.Equal(t, failedBefore+1, boardLeaseEventsFailed.Value(), "the drop is counted")
	assert.Equal(t, publishedBefore, boardLeaseEventsPublished.Value(), "nothing landed on the stream")
	assert.Empty(t, streamEntries(t, mr.Addr(), "ws-1"), "the sink never publishes an unresolved event")
}

// --- the breaker --------------------------------------------------------------

func TestBoardLeaseEventbus_BreakerPausesOneWindow(t *testing.T) {
	bus, mr := leaseTestBus(t)
	ctx := context.Background()

	require.NoError(t, bus.publish(ctx, "ws-9", eventbus.LeaseNudgeTopic("t"), eventbus.LeaseNudgePayload("t", "s", "n", "a")))
	publishedBefore := boardLeaseEventsPublished.Value()

	// The transport dies: the in-flight publish fails and trips the
	// breaker; the workspace's bus is evicted.
	mr.Close()
	require.Error(t, bus.publish(ctx, "ws-9", eventbus.LeaseNudgeTopic("t"), nil))

	// While paused, publishes fail FAST (no per-event timeout against
	// a dead transport) ...
	start := time.Now()
	require.Error(t, bus.publish(ctx, "ws-9", eventbus.LeaseNudgeTopic("t"), nil))
	assert.Less(t, time.Since(start), 500*time.Millisecond, "the open breaker fails fast")
	assert.Equal(t, publishedBefore, boardLeaseEventsPublished.Value(), "nothing published while paused")

	// ... and the window heals: force the breaker closed and confirm
	// the publisher re-arms against a fresh transport (the eviction
	// means the bus rewires on the next successful event).
	mr2 := miniredis.RunT(t)
	bus.client = redis.NewClient(&redis.Options{Addr: mr2.Addr()})
	bus.breakerNanos.Store(0)
	require.NoError(t, bus.publish(ctx, "ws-9", eventbus.LeaseNudgeTopic("t"), nil))
	assert.Equal(t, publishedBefore+1, boardLeaseEventsPublished.Value())
}

// --- the address leniency -----------------------------------------------------

func TestLeaseEventbusOptions_TwoSpellings(t *testing.T) {
	opts, err := leaseEventbusOptions("redis.example:6379", "pw")
	require.NoError(t, err)
	assert.Equal(t, "redis.example:6379", opts.Addr)
	assert.Equal(t, "pw", opts.Password, "the password env composes onto credentialess addresses")

	opts, err = leaseEventbusOptions("redis://:urlpw@redis.example:6379/2", "envpw")
	require.NoError(t, err)
	assert.Equal(t, "urlpw", opts.Password, "a URL's own credentials win over the env spelling")
	assert.Equal(t, 2, opts.DB)

	_, err = leaseEventbusOptions("not a redis addr://", "")
	assert.Error(t, err, "a malformed address refuses at the seam")
	assert.False(t, strings.Contains(err.Error(), "not a redis addr"), "the malformed-addr error never echoes the raw address")
}
