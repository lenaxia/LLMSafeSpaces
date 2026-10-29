// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// workspace_eventbus_test.go — design 0072's agentd wiring units: the
// env gate (bus disabled is the honest default), the consumer-name
// mint, the topics alias meeting the v1 registry, and the lease
// sentinel's byte-parity pin against the grammar the API composes.

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/eventbus"
)

func TestEventbusFromEnvDisabledByDefault(t *testing.T) {
	// Unset addr: point-to-point v1 stays; nil is the disabled state.
	assert.Nil(t, eventbusFromEnv(func(string) string { return "" }, zap.NewNop()))
}

func TestEventbusFromEnvRefusesWithoutWorkspaceID(t *testing.T) {
	bus := eventbusFromEnv(func(k string) string {
		if k == "AGENTD_EVENTBUS_ADDR" {
			return "127.0.0.1:1"
		}
		return ""
	}, zap.NewNop())
	assert.Nil(t, bus, "a bus without a workspace scope must disable, never publish unscoped")
}

func TestEventbusFromEnvRefusesDeadRedis(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	addr := mr.Addr()
	mr.Close() // dead
	bus := eventbusFromEnv(func(k string) string {
		switch k {
		case "AGENTD_EVENTBUS_ADDR":
			return addr
		case "WORKSPACE_ID":
			return "ws-1"
		}
		return ""
	}, zap.NewNop())
	assert.Nil(t, bus, "an unreachable redis must disable the bus, never the wake-ups")
}

func TestEventbusFromEnvWiresOverLiveRedis(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	bus := eventbusFromEnv(func(k string) string {
		switch k {
		case "AGENTD_EVENTBUS_ADDR":
			return mr.Addr()
		case "WORKSPACE_ID":
			return "ws-1"
		}
		return ""
	}, zap.NewNop())
	require.NotNil(t, bus)
	assert.Equal(t, eventbus.StreamKey("ws-1"), bus.Key())
	// The URL spelling wires the same.
	bus2 := eventbusFromEnv(func(k string) string {
		switch k {
		case "AGENTD_EVENTBUS_ADDR":
			return "redis://" + mr.Addr() + "/0"
		case "WORKSPACE_ID":
			return "ws-1"
		}
		return ""
	}, zap.NewNop())
	require.NotNil(t, bus2)
	assert.Equal(t, eventbus.StreamKey("ws-1"), bus2.Key())
}

func TestEventbusFromEnvMalformedAddr(t *testing.T) {
	bus := eventbusFromEnv(func(k string) string {
		if k == "AGENTD_EVENTBUS_ADDR" {
			return "htp://nope"
		}
		if k == "WORKSPACE_ID" {
			return "ws-1"
		}
		return ""
	}, zap.NewNop())
	assert.Nil(t, bus)
}

func TestEventbusConsumerName(t *testing.T) {
	assert.Equal(t, "overridden", eventbusConsumerName(func(k string) string {
		if k == "AGENTD_EVENTBUS_CONSUMER" {
			return " overridden "
		}
		return ""
	}))
	name := eventbusConsumerName(func(string) string { return "" })
	assert.NotEmpty(t, name)
}

// ---- the topics alias (design 0072 §6) ----

func TestSubscriptionEventsFromTopics(t *testing.T) {
	// The three accepted forms on the right broadcaster.
	evs, err := subscriptionEventsFromTopics([]string{"session.ses_B.idle"}, "ses_B")
	require.NoError(t, err)
	assert.Equal(t, []string{"idle"}, evs)

	evs, err = subscriptionEventsFromTopics([]string{"session.ses_B.*"}, "ses_B")
	require.NoError(t, err)
	assert.Equal(t, []string{"idle", "error"}, evs)

	// Phrase-form id segments resolve (the comparison is
	// post-resolution; canonicalNotifyDestination passes cold phrases
	// through, and the literal-equal arm catches the exact spelling).
	evs, err = subscriptionEventsFromTopics([]string{"session.ses_B.error", "session.ses_B.idle"}, "ses_B")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"idle", "error"}, evs)

	// Foreign families and foreign sessions refuse loudly.
	for _, bad := range []string{
		"lease.tsk-1.nudged",
		"task.tsk-1.stage_changed",
		"session.ses_OTHER.idle",
		"session.ses_B.busy",
		"session..idle",
	} {
		_, err := subscriptionEventsFromTopics([]string{bad}, "ses_B")
		assert.Error(t, err, "topic %q must refuse", bad)
	}
}

// ---- the lease sentinel's parity pin ----

// TestComposeLeaseNudgeParity: the sentinel + prose this pod composes
// must match the API nudge ladder's grammar byte-for-byte — the
// ordinal anchor (nudgeId) is what the engine's EventsAfterNudge
// question answers against; drift here wedges wrongly. The expected
// literal mirrors api/internal/app/board_liveness.go's
// leaseNudgeText.
func TestComposeLeaseNudgeParity(t *testing.T) {
	got := composeLeaseNudge("tsk-crimson-finch-blooms", "ndg-7")
	want := `<!-- lsp:agent-message-v1 {"mode":"lease-nudge","taskId":"tsk-crimson-finch-blooms","nudgeId":"ndg-7","expects":"respond|advance|fail"} -->` + "\n" +
		"BOARD LEASE NUDGE: your claim on task tsk-crimson-finch-blooms has gone quiet. " +
		"First RESPOND (call board_heartbeat once, with your claimToken) to confirm you are active — " +
		"respond before running any long command so the wedge clock clears. " +
		"Then: continue working if there is more work to do; otherwise move your task to its next stage " +
		"(board_move_stage with your claimToken), or declare it failed with a reason if you cannot complete it. " +
		"Do NOT restart — assess your current state."
	assert.Equal(t, want, got)
}

// TestLeaseBusConsumerUnknownArmsAck: the vocabulary is a convention
// — unknown lease arms and foreign shapes ack unread, never error (a
// poison refusal would wedge the group on vocabulary growth).
func TestLeaseBusConsumerUnknownArmsAck(t *testing.T) {
	lc := &leaseBusConsumer{seam: nil, logger: zap.NewNop()}
	require.NoError(t, lc.handle(context.Background(), eventbus.Message{Event: eventbus.Event{Topic: "lease.tsk-1.somenewarm"}}))
	require.NoError(t, lc.handle(context.Background(), eventbus.Message{Event: eventbus.Event{Topic: "session.ses_1.idle"}}))
	// A nudge without an assignee acks with a warn (nothing to
	// deliver — the ordinal anchor's task knows its assignee).
	require.NoError(t, lc.handle(context.Background(), eventbus.Message{Event: eventbus.Event{
		Topic:   eventbus.LeaseNudgeTopic("tsk-1"),
		Payload: map[string]string{"task": "tsk-1"},
	}}))
}

// TestRedisOptionsFromAddr: both spellings of the one knob.
func TestRedisOptionsFromAddr(t *testing.T) {
	o, err := redisOptionsFromAddr("127.0.0.1:6379")
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:6379", o.Addr)

	o, err = redisOptionsFromAddr("redis://u:p@127.0.0.1:6379/2")
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:6379", o.Addr)

	_, err = redisOptionsFromAddr("://nope")
	assert.Error(t, err)
}

// TestRunEventbusConsumersNilSafe: a disabled bus is a no-op (the v1
// path needs no consumers).
func TestRunEventbusConsumersNilSafe(t *testing.T) {
	assert.NotPanics(t, func() {
		runEventbusConsumers(context.Background(), nil, nil, nil, "", zap.NewNop())
	})
}
