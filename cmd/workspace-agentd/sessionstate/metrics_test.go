// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- US-69.12: the metrics bridge (seq stall, ledger funnel, promotion
// stall, stall stats) — sessionstate stays prometheus-free; Metrics() is
// the whole surface the wiring layer scrapes. ---

func metricsAuthority(t *testing.T, wake func(context.Context, string) error, deadline time.Duration) *sessionstate.Authority {
	t.Helper()
	cfg := sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      &fixtureParser{},
		Store:       &mapStore{},
		Passwords:   []string{"pw"},
		Admitter:    &recordingAdmitter{},
		FastCursor:  true,
		Capabilities: &abiv1.CapabilityReport{
			SupportedActions: []abiv1.ActionType{abiv1.ActionType_ACTION_TYPE_INTERRUPT},
		},
	}
	if wake != nil {
		cfg.Wake = wake
	}
	a, err := sessionstate.New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	if deadline > 0 {
		a.SetStallDeadlineForTest(deadline)
	}
	return a
}

func deliverOne(t *testing.T, a *sessionstate.Authority, sessionID, entryID string) {
	t.Helper()
	_, h := a.Handler()
	c := newAuthedServer(t, h)
	// recordingAdmitter admits instantly; the row lands admitted.
	_, err := c.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
		SessionId: sessionID, EntryId: entryID, Attempt: 1,
		Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "hi"}}},
	}))
	require.NoError(t, err)
	assert.Eventually(t, func() bool {
		m := a.Metrics()
		return m.LedgerDepths != nil && m.LedgerDepths["admitted"] > 0
	}, 3*time.Second, 10*time.Millisecond, "row reaches admitted")
}

// TestMetrics_LedgerFunnelAndPromotionStall: Depths/PerState counts, the
// stalled subset, and the oldest-admitted age all read true from one
// Metrics() snapshot.
func TestMetrics_LedgerFunnelAndPromotionStall(t *testing.T) {
	a := metricsAuthority(t, nil, 0)
	deliverOne(t, a, "s1", "e-1")
	deliverOne(t, a, "s1", "e-2")

	m := a.Metrics()
	require.NotNil(t, m.LedgerDepths)
	assert.EqualValues(t, 2, m.LedgerDepths["admitted"], "funnel: both rows admitted")
	assert.Zero(t, m.StalledEntries, "nothing stalled before the deadline")
	assert.Greater(t, m.OldestPromotionStallSeconds, 0.0, "promotion stall: admitted rows age")
}

// TestMetrics_SeqStallSignal: SecondsSinceSeqAdvance resets on advance.
func TestMetrics_SeqStallSignal(t *testing.T) {
	a := metricsAuthority(t, nil, 0)
	before := a.Metrics().SecondsSinceSeqAdvance
	require.GreaterOrEqual(t, before, 0.0)
	// An event advances the projection → the clock resets (the fixture
	// parser's "busy <id>" payload).
	a.Ingest([]byte("busy s1"))
	after := a.Metrics().SecondsSinceSeqAdvance
	assert.LessOrEqual(t, after, before+0.05, "seq advance resets the stall clock")
}

// TestCheckStalls_StatsAndWakeFailures: crossing the deadline stalls the
// row, fires the wake exactly once, and the pass reports stall + wake
// failure counts (the escalation metrics).
func TestCheckStalls_StatsAndWakeFailures(t *testing.T) {
	wakeCalls := 0
	a := metricsAuthority(t, func(context.Context, string) error {
		wakeCalls++
		return errors.New("harness unreachable")
	}, time.Nanosecond) // deadline already crossed
	deliverOne(t, a, "s1", "e-1")

	stats := a.CheckStalls(context.Background())
	assert.Equal(t, 1, stats.Stalled, "one row crossed the deadline")
	assert.Equal(t, 1, stats.WakeFailures, "the errored wake is the escalation signal")
	assert.Equal(t, 1, wakeCalls, "wake fires exactly once per row (I6)")

	m := a.Metrics()
	assert.EqualValues(t, 1, m.StalledEntries, "stalled subset visible in Metrics()")
	assert.EqualValues(t, 1, m.LedgerDepths["stalled"], "funnel carries the stalled state")

	// Idempotent pass: already-stalled rows do not re-fire the wake.
	stats2 := a.CheckStalls(context.Background())
	assert.Zero(t, stats2.Stalled)
	assert.Zero(t, stats2.WakeFailures)
	assert.Equal(t, 1, wakeCalls)
}

// TestCheckStalls_NilWakeCountsAsNoFailure: a nil Wake (not wired) is a
// silent no-op — stalls surface, failures do not.
func TestCheckStalls_NilWakeCountsAsNoFailure(t *testing.T) {
	a := metricsAuthority(t, nil, time.Nanosecond)
	deliverOne(t, a, "s1", "e-1")
	stats := a.CheckStalls(context.Background())
	assert.Equal(t, 1, stats.Stalled)
	assert.Zero(t, stats.WakeFailures)
}

// TestCheckStalls_NoLedgerIsNoOp: without an Admitter there is no ledger —
// the watchdog pass is a clean zero.
func TestCheckStalls_NoLedgerIsNoOp(t *testing.T) {
	cfg := sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      &fixtureParser{},
		Passwords:   []string{"pw"},
		FastCursor:  true,
	}
	a, err := sessionstate.New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	assert.Zero(t, a.CheckStalls(context.Background()).Stalled)
	m := a.Metrics()
	assert.Nil(t, m.LedgerDepths, "no funnel without a ledger")
}

// customPartEvent builds a part-carrying event of the given part type.
func customPartEvent(t abiv1.EventType, sid, pid string, pt abiv1.PartType) *abiv1.Event {
	evt := &abiv1.Event{Type: t, SessionId: sid, PartId: pid}
	switch pt {
	case abiv1.PartType_PART_TYPE_CUSTOM:
		evt.Part = &abiv1.Part{Id: pid, Type: pt,
			Payload: &abiv1.Part_Custom{Custom: &abiv1.CustomPart{Kind: "ext.marker"}}}
	default:
		evt.Part = &abiv1.Part{Id: pid, Type: pt, Payload: &abiv1.Part_Text{Text: "text"}}
	}
	return evt
}

// TestMetrics_CustomValveCounter (US-69.11): every CUSTOM part application
// through the projection counts once — PART_START/PART_END upserts and
// message-carried parts alike — while named types do not. The retired
// API-side unknown-taxonomy counter's agentd successor.
func TestMetrics_CustomValveCounter(t *testing.T) {
	a, p := newEventAuthority(t, nil)

	feed(t, a, p, customPartEvent(abiv1.EventType_EVENT_TYPE_PART_START, "s1", "p1", abiv1.PartType_PART_TYPE_CUSTOM))
	assert.EqualValues(t, 1, a.Metrics().CustomValveEvents, "PART_START of a custom part counts")

	feed(t, a, p, customPartEvent(abiv1.EventType_EVENT_TYPE_PART_END, "s1", "p1", abiv1.PartType_PART_TYPE_CUSTOM))
	assert.EqualValues(t, 2, a.Metrics().CustomValveEvents, "PART_END re-application counts again")

	feed(t, a, p, customPartEvent(abiv1.EventType_EVENT_TYPE_PART_START, "s1", "p2", abiv1.PartType_PART_TYPE_TEXT))
	assert.EqualValues(t, 2, a.Metrics().CustomValveEvents, "named taxonomy types never count")

	feed(t, a, p, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_MESSAGE_END, SessionId: "s1", MessageId: "m1",
		Message: &abiv1.Message{Id: "m1", Parts: []*abiv1.Part{
			{Id: "p3", Type: abiv1.PartType_PART_TYPE_TEXT, Payload: &abiv1.Part_Text{Text: "hi"}},
			{Id: "p4", Type: abiv1.PartType_PART_TYPE_CUSTOM, Payload: &abiv1.Part_Custom{Custom: &abiv1.CustomPart{Kind: "ext.other"}}},
		}}})
	assert.EqualValues(t, 3, a.Metrics().CustomValveEvents, "message-carried custom parts count via the same upsert")
}
