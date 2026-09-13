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
		t.Errorf("answer on dropped input errored: %v", err)
		assert.True(t, v.Empty(), "row must end with zero violations: %v", v.Counts())
		return
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
		return m.LedgerDepths != nil && m.LedgerDepths["admitted"] == 0
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
		return m.LedgerDepths != nil && m.LedgerDepths["admitted"] == 0
	})
	require.True(t, ok, "the stranded row converges from status evidence alone")

	m := a.Metrics()
	assert.Equal(t, int64(0), m.ReconcilePromoted, "nothing was promoted — the TURN_ENDED arm carried this row")
	assert.GreaterOrEqual(t, m.ReconcileTurnEnded, int64(1), "the turn-ended arm is the asserted resolution")
}

// Row S5/L3 (leg-1 convergence — the 2a lease story): the harness drops
// the ask silently (truth no longer lists it); the reconcile cadence's
// pending-lease diff resolves the projected straggler; the projection
// converges to truth within the L3 bound.
func TestRow_S5_Leg1_SilentDrop_ConvergesWithinL3(t *testing.T) {
	store := NewEvidenceStore()
	store.SetState("ses-row", abiv1.SessionStatus_SESSION_STATUS_BUSY, &abiv1.InputRequest{
		Id: "in-gone", Kind: abiv1.InputKind_INPUT_KIND_QUESTION, Question: "Proceed?",
	})
	a := newAuthority(t, store, &AnswerActor{Store: store})
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))
	require.Len(t, snapshotOf(t, a, "ses-row").GetPendingInputs(), 1, "projected from harness truth at boot")

	// The fault: silent drop — truth forgets the ask, no event fires.
	store.SetState("ses-row", abiv1.SessionStatus_SESSION_STATUS_BUSY)

	v := NewViolations()
	log := NewConvergenceLog()
	bound := sessionstate.LeaseConvergenceBound
	elapsed, ok := WaitConverges(context.Background(), bound, 5*time.Millisecond, func() bool {
		a.Reconcile(context.Background())
		return len(snapshotOf(t, a, "ses-row").GetPendingInputs()) == 0
	})
	log.Record("L3", elapsed)
	if !ok {
		v.Add("S5") // projected ⊄ truth past the lease window
		v.Add("L3")
	}
	// Pin the RESOLUTION ARM: the cadence's lease diff resolved the
	// straggler — NOT the serve-path gather (snapshotOf triggers its own
	// diff; without this pin a dead cadence diff ships green, r1).
	if a.Metrics().LeaseResolved < 1 {
		v.Add("S5.arm")
	}

	assert.True(t, v.Empty(), "row must end with zero violations: %v", v.Counts())
	assert.True(t, log.Within("L3", bound))
}

// Row leg 3 (frame loss, harness→agentd direction): the harness ASKED,
// the INPUT_REQUEST event was lost — the projection misses what truth
// holds. The lease diff's live−projected arm makes it appear within L3
// (the L1-direction convergence).
func TestRow_Leg3_LostAskEvent_AppearsWithinL3(t *testing.T) {
	store := NewEvidenceStore()
	store.SetState("ses-row", abiv1.SessionStatus_SESSION_STATUS_BUSY)
	a := newAuthority(t, store, &AnswerActor{Store: store})
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))
	require.Empty(t, snapshotOf(t, a, "ses-row").GetPendingInputs(), "nothing projected at boot")

	// The fault: the ask exists in truth; its event never arrived.
	store.SetState("ses-row", abiv1.SessionStatus_SESSION_STATUS_BUSY, &abiv1.InputRequest{
		Id: "in-unseen", Kind: abiv1.InputKind_INPUT_KIND_QUESTION, Question: "Proceed?",
	})

	v := NewViolations()
	log := NewConvergenceLog()
	bound := sessionstate.LeaseConvergenceBound
	elapsed, ok := WaitConverges(context.Background(), bound, 5*time.Millisecond, func() bool {
		a.Reconcile(context.Background())
		return len(snapshotOf(t, a, "ses-row").GetPendingInputs()) == 1
	})
	log.Record("L3", elapsed)
	if !ok {
		v.Add("L1") // a live ask the projection never surfaced
		v.Add("L3")
	}
	// Pin the RESOLUTION ARM: the cadence's live−projected diff surfaced
	// the ask — not the serve-path gather (r1).
	if a.Metrics().LeaseAppeared < 1 {
		v.Add("L1.arm")
	}

	snap := snapshotOf(t, a, "ses-row")
	if len(snap.GetPendingInputs()) == 1 && snap.GetPendingInputs()[0].GetId() != "in-unseen" {
		v.Add("L1.identity")
	}

	assert.True(t, v.Empty(), "row must end with zero violations: %v", v.Counts())
	assert.True(t, log.Within("L3", bound))
}

// Row leg 7 → S2 (rewritten, #1315): duplicate and out-of-order Deliver
// of the SAME entry — the exact replay shapes the 2026-09-10 incident
// rode. The keyed-upsert wire plus the authority's evidence short-circuit
// must hold transcript cardinality at ONE message per entry across every
// replay, and only ONE harness write may land (the second POST that
// never fires is the cheapness/idempotency observable).
func TestRow_Leg7_DuplicateOutOfOrderDeliver_S2(t *testing.T) {
	store := NewEvidenceStore()
	store.SetState("ses-row", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	admitter := &KeyedAdmitter{Out: store}
	a, err := sessionstate.New(sessionstate.Config{
		PlatformDir:     t.TempDir(),
		Parser:          NoopParser{},
		Store:           store,
		Passwords:       []string{"row-pw"},
		Admitter:        admitter,
		AdmitterTimeout: 2 * time.Second,
	})
	require.NoError(t, err)

	v := NewViolations()

	deliver := func(attempt uint32) {
		_, err := a.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
			SessionId: "ses-row", EntryId: "entry-1", Attempt: attempt,
			Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "proceed with the split"}}},
		}))
		require.NoError(t, err, "Deliver accepts (the ack is idempotent)")
	}

	// 1. The original send.
	deliver(1)
	elapsed, ok := WaitConverges(context.Background(), 5*time.Second, 2*time.Millisecond, func() bool {
		m := a.Metrics()
		return m.LedgerDepths != nil && m.LedgerDepths["admitted"] == 1
	})
	require.True(t, ok, "attempt 1 admits: %v", elapsed)

	// 2. THE FAULT — duplicate replay (same entry, same attempt): the
	// ledger's (entry, attempt) dedupe must accept-and-noop.
	deliver(1)

	// 3. THE FAULT — out-of-order replay (same entry, attempt+1 while
	// the entry is already admitted): the cross-attempt path must
	// resolve by the entry's existing outcome, never re-POST.
	deliver(2)

	if _, ok := WaitConverges(context.Background(), 5*time.Second, 2*time.Millisecond, func() bool {
		m := a.Metrics()
		return m.LedgerDepths != nil && m.LedgerDepths["admitted"] == 2
	}); !ok {
		v.Add("S2") // the replayed attempt never resolved idempotently
	}

	// S2 per-entry: the transcript holds exactly ONE message for this
	// entry — across the original, the duplicate, and the re-order.
	if got := store.TranscriptCount("ses-row"); got != 1 {
		v.Add("S2.cardinality")
	}
	// The idempotency observable: exactly one harness write landed —
	// the replays must have been resolved before POSTing.
	if got := admitter.Writes(); got != 1 {
		v.Add("S2.single-write")
	}

	assert.True(t, v.Empty(), "row must end with zero violations: %v", v.Counts())
	assert.Equal(t, 1, store.TranscriptCount("ses-row"), "per-entry transcript cardinality (S2 rewritten)")
	assert.Equal(t, 1, admitter.Writes(), "exactly one harness write for the entry's lifetime")
}

// Row leg 8 → S2 + L5 (boundary slowness, the incident's driving fault):
// the synchronous turn's write LANDS but its outcome is LOST (response
// hangs past the admitter window, client aborts) while the entry is
// re-driven — the exact loop that manufactured sixteen copies. With the
// entry key, the next retry's evidence check finds the landed write and
// resolves ADMITTED-by-evidence with NO second POST; keyless (the
// pre-fix wire), every retry appends again.
func TestRow_Leg8_HungTurnReDrive_S2_L5(t *testing.T) {
	store := NewEvidenceStore()
	store.SetState("ses-row", abiv1.SessionStatus_SESSION_STATUS_BUSY)
	hang := make(chan struct{})
	admitter := &KeyedAdmitter{Out: store, Hang: hang}
	a, err := sessionstate.New(sessionstate.Config{
		PlatformDir:     t.TempDir(),
		Parser:          NoopParser{},
		Store:           store,
		Passwords:       []string{"row-pw"},
		Admitter:        admitter,
		AdmitterTimeout: 150 * time.Millisecond, // the window (shrunk from 3min)
	})
	require.NoError(t, err)
	defer close(hang) // release any straggler after the row

	v := NewViolations()
	log := NewConvergenceLog()

	deliver := func(attempt uint32) {
		_, err := a.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
			SessionId: "ses-row", EntryId: "entry-1", Attempt: attempt,
			Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "proceed with the split"}}},
		}))
		require.NoError(t, err)
	}

	// 1. The original send — its write lands, its outcome hangs.
	deliver(1)
	// 2. THE FAULT — the out-of-order re-drive while the turn hangs
	// (the terminus's retryable path re-arms the entry at attempt+1).
	deliver(2)

	// 3. The ladders retry against the lost outcome; with the entry
	// key, the first evidence hit ends the loop (no further writes);
	// the sweep then converges the stranded rows inside L5.
	elapsed, ok := WaitConverges(context.Background(), 30*time.Second, 5*time.Millisecond, func() bool {
		a.Reconcile(context.Background())
		m := a.Metrics()
		if m.LedgerDepths == nil {
			return false
		}
		return m.LedgerDepths["ledgered"] == 0 && m.LedgerDepths["failed"] == 0
	})
	log.Record("L5", elapsed)
	if !ok {
		v.Add("L5") // the lost-outcome entry must converge, not strand
	}

	// S2 per-entry through the hung window: ONE transcript message.
	if got := store.TranscriptCount("ses-row"); got != 1 {
		v.Add("S2.cardinality")
	}
	// The idempotency observable: the re-POST loop never restarted —
	// exactly one write for the entry's lifetime.
	if got := admitter.Writes(); got != 1 {
		v.Add("S2.single-write")
	}

	assert.True(t, v.Empty(), "row must end with zero violations: %v", v.Counts())
	assert.True(t, log.Within("L5", 30*time.Second))
	assert.Equal(t, 1, store.TranscriptCount("ses-row"), "per-entry cardinality through the hung turn")
	assert.Equal(t, 1, admitter.Writes(), "the landed write ends the re-POST loop (evidence)")
}

// F3 pin: the keyless branch of the wire fake — two keyless POSTs append
// two DISTINCT transcript messages. This is the distinction that gives
// the leg-8 row its mutation sensitivity; a future edit collapsing the
// keyless branch to an upsert would silently neuter it with every row
// staying green.
func TestKeyedAdmitter_KeylessAppends_KeyedUpserts(t *testing.T) {
	store := NewEvidenceStore()
	ad := &KeyedAdmitter{Out: store}
	ctx := context.Background()

	id1, err := ad.Admit(ctx, "ses-pin", "msg_key", "a", "")
	require.NoError(t, err)
	id2, err := ad.Admit(ctx, "ses-pin", "msg_key", "a", "")
	require.NoError(t, err)
	assert.Equal(t, "msg_key", id1, "keyed: the harness echoes the key")
	assert.Equal(t, "msg_key", id2, "keyed: re-admission upserts the same id")
	assert.Equal(t, 1, store.TranscriptCount("ses-pin"), "one message for repeated keyed writes")
	assert.Equal(t, 2, ad.Writes(), "both POSTs landed (the upsert is the HARNESS's)")

	k1, err := ad.Admit(ctx, "ses-pin", "", "a", "")
	require.NoError(t, err)
	k2, err := ad.Admit(ctx, "ses-pin", "", "a", "")
	require.NoError(t, err)
	assert.NotEqual(t, k1, k2, "keyless: every POST is a NEW transcript message (the pre-0a wire)")
	assert.Equal(t, 3, store.TranscriptCount("ses-pin"), "the append branch — the duplication class S2 forbids")
	assert.Equal(t, 4, ad.Writes())
}
