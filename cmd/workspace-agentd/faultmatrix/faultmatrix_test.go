// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package faultmatrix

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// The epic-71 / 2b assertion harness (#1312 change item 2): fault-leg
// rows driven through the REAL sessionstate authority with harness
// fakes, asserting the S-invariants via violation counters and the
// L-bounds via convergence samples — the in-memory shape the soak row
// reuses at N×M×λ. Every row ends with the two must-hold gates:
// violations empty, convergence within bound.

// rowHarness composes the authority with the evidence store and the
// answer actor (the two harness-facing seams the rows fault).
func newAuthority(t *testing.T, store *EvidenceStore, actor sessionstate.Actor) *sessionstate.Authority {
	t.Helper()
	a, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      NoopParser{},
		Store:       store,
		Passwords:   []string{"row-pw"},
		Capabilities: &abiv1.CapabilityReport{
			Provenance:       abiv1.Provenance_PROVENANCE_PLATFORM_PINNED,
			SupportedActions: []abiv1.ActionType{abiv1.ActionType_ACTION_TYPE_ANSWER_QUESTION},
		},
		Actor:    actor,
		Admitter: &InstantAdmitter{Out: store},
	})
	require.NoError(t, err)
	return a
}

func snapshotOf(t *testing.T, a *sessionstate.Authority, sessionID string) *abiv1.SessionSnapshot {
	t.Helper()
	res, err := a.GetSnapshot(context.Background(), connect.NewRequest(&abiv1.GetSnapshotRequest{SessionId: sessionID}))
	require.NoError(t, err)
	return res.Msg
}

// Row leg 2 → S6 + L2: the stale click resolves by absence — the
// harness 404s the answer, the authority converts it to SUCCESS and
// clears the projection; the click→cleared span records an L2 sample.
func TestRow_Leg2_StaleClickResolves_S6_L2(t *testing.T) {
	store := NewEvidenceStore()
	store.SetState("ses-row", abiv1.SessionStatus_SESSION_STATUS_BUSY, &abiv1.InputRequest{
		Id: "in-stale", Kind: abiv1.InputKind_INPUT_KIND_QUESTION, Question: "Proceed?",
	})
	a := newAuthority(t, store, &AnswerActor{Store: store})
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))
	require.Len(t, snapshotOf(t, a, "ses-row").GetPendingInputs(), 1, "projected from harness truth")

	// The fault: the harness silently dropped the ask (leg 1 shape) —
	// the store's truth no longer lists it.
	store.SetState("ses-row", abiv1.SessionStatus_SESSION_STATUS_BUSY)

	v := NewViolations()
	log := NewConvergenceLog()
	clickStart := time.Now()
	res, err := a.Act(context.Background(), connect.NewRequest(&abiv1.ActionRequest{
		SessionId: "ses-row",
		Action: &abiv1.ActionRequest_AnswerQuestion{AnswerQuestion: &abiv1.AnswerInputAction{
			InputId: "in-stale", OptionIds: []string{"yes"},
		}},
	}))
	if err != nil {
		v.Add("S6") // absence must resolve, never error
		t.Fatalf("answer on dropped input errored: %v", err)
	}
	require.NotNil(t, res.Msg.GetAnswerQuestion())
	clickElapsed := time.Since(clickStart)
	log.Record("L2.click", clickElapsed)

	elapsed, ok := WaitConverges(context.Background(), 2*time.Second, 2*time.Millisecond, func() bool {
		return len(snapshotOf(t, a, "ses-row").GetPendingInputs()) == 0
	})
	log.Record("L2", elapsed)
	if !ok {
		v.Add("L2")
	}

	assert.True(t, v.Empty(), "row must end with zero violations: %v", v.Counts())
	assert.True(t, log.Within("L2", 2*time.Second))
}

// Row leg 5 → S7 + L4: the turn-end status event is lost — the store
// shows the message (evidence), the projection says BUSY. One reconcile
// pass re-derives idle; the loss→idle span records an L4 sample.
func TestRow_Leg5_StatusEventLost_S7_L4(t *testing.T) {
	store := NewEvidenceStore()
	store.SetState("ses-row", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	store.MarkPresent("ses-row", "msg-end-1")
	a := newAuthority(t, store, &AnswerActor{Store: store})
	a.IngestForTest(&abiv1.Event{
		SessionId: "ses-row",
		Type:      abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
		Status:    abiv1.SessionStatus_SESSION_STATUS_BUSY,
	})
	require.Equal(t, abiv1.SessionStatus_SESSION_STATUS_BUSY, snapshotOf(t, a, "ses-row").GetStatus())

	v := NewViolations()
	log := NewConvergenceLog()
	rec := a.Reconcile(context.Background())
	if rec.BusyCleared < 1 {
		v.Add("S7") // evidence says idle; BUSY past the window is a violation
	}
	elapsed, ok := WaitConverges(context.Background(), 30*time.Second, time.Millisecond, func() bool {
		return snapshotOf(t, a, "ses-row").GetStatus() == abiv1.SessionStatus_SESSION_STATUS_IDLE
	})
	log.Record("L4", elapsed)
	if !ok {
		v.Add("L4")
	}

	assert.True(t, v.Empty(), "row must end with zero violations: %v", v.Counts())
	assert.True(t, log.Within("L4", 30*time.Second))
}

// Row leg 4 → S7 + S8 + L5: the crash-restart shape — an admitted row
// never promoted (its turn events never arrived), the store holds the
// transcript evidence. The boot reseed's embedded sweep converges it
// with no operator action, and the reconstituted state still satisfies
// S7 on a second reseed.
func TestRow_Leg4_CrashRestart_S7_S8_L5(t *testing.T) {
	store := NewEvidenceStore()
	store.SetState("ses-row", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	a := newAuthority(t, store, &AnswerActor{Store: store})

	ack, err := a.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
		SessionId: "ses-row", EntryId: "entry-1", Attempt: 1,
		Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "do the thing"}}},
	}))
	require.NoError(t, err)
	_ = ack.Msg
	// The admission ladder runs asynchronously — wait for the stranded
	// state (ADMITTED with the promotion event never arriving).
	require.Eventually(t, func() bool {
		m := a.Metrics()
		return m.LedgerDepths != nil && m.LedgerDepths["admitted"] == 1
	}, 5*time.Second, 5*time.Millisecond, "row reaches ADMITTED (InstantAdmitter succeeds)")

	v := NewViolations()
	log := NewConvergenceLog()

	// Restart: a fresh reseed embeds the sweep (boot auto-heal); a
	// session whose admission ladder still holds the single-flight lock
	// is SKIPPED that pass (correct — never wait behind a live turn),
	// so convergence is the watchdog's cadence contract, not the first
	// pass: tick Reconcile until the stranded row clears, inside L5.
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))
	var reconciled bool
	elapsed, ok := WaitConverges(context.Background(), 30*time.Second, 5*time.Millisecond, func() bool {
		a.Reconcile(context.Background())
		reconciled = true
		m := a.Metrics()
		return m.LedgerDepths == nil || m.LedgerDepths["admitted"] == 0
	})
	log.Record("L5", elapsed)
	if !ok {
		v.Add("S7") // stranded admitted row against present evidence, past L5
		v.Add("L5")
	}
	require.True(t, reconciled)
	// Pin the RESOLUTION ARM: evidence present → PROMOTED (not the
	// turn-ended fallback) — the admitter wrote the transcript message
	// the sweep found (review r1 finding 2).
	if m := a.Metrics(); m.LedgerDepths["promoted"] < 1 && m.ReconcilePromoted < 1 {
		v.Add("S7.arm")
	}

	// S8: reconstitutability — a second boot reseed keeps S7 holding.
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))
	m2 := a.Metrics()
	if m2.LedgerDepths != nil && m2.LedgerDepths["admitted"] != 0 {
		v.Add("S8")
	}

	assert.True(t, v.Empty(), "row must end with zero violations: %v", v.Counts())
	assert.True(t, log.Within("L5", 30*time.Second))
}

// Row leg 4, evidence-absent variant → S7 via the TURN_ENDED arm: the
// admission never wrote the transcript (the harness OOM-mid-turn shape)
// — the sweep resolves the stranded row from STATUS evidence alone and
// must NOT report it promoted.
func TestRow_Leg4_EvidenceAbsent_TurnEndedArm(t *testing.T) {
	store := NewEvidenceStore()
	store.SetState("ses-row", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	a, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      NoopParser{},
		Store:       store,
		Passwords:   []string{"row-pw"},
		Admitter:    &InstantAdmitter{Out: nil}, // no evidence write — the lever
	})
	require.NoError(t, err)

	_, err = a.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
		SessionId: "ses-row", EntryId: "entry-1", Attempt: 1,
		Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "do the thing"}}},
	}))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		m := a.Metrics()
		return m.LedgerDepths != nil && m.LedgerDepths["admitted"] == 1
	}, 5*time.Second, 5*time.Millisecond, "row reaches ADMITTED")

	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))
	_, ok := WaitConverges(context.Background(), 30*time.Second, 5*time.Millisecond, func() bool {
		a.Reconcile(context.Background())
		m := a.Metrics()
		return m.LedgerDepths == nil || m.LedgerDepths["admitted"] == 0
	})
	require.True(t, ok, "the stranded row converges from status evidence alone")

	m := a.Metrics()
	assert.Equal(t, int64(0), m.ReconcilePromoted, "nothing was promoted — the TURN_ENDED arm carried this row")
	assert.GreaterOrEqual(t, m.ReconcileTurnEnded, int64(1), "the turn-ended arm is the asserted resolution")
}
