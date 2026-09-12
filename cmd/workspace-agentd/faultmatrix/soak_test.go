// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package faultmatrix

import (
	"context"
	"math"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// Row legs 6/7 → S2 + S9: the API-rollover replay shape — the same
// entry re-delivered (duplicate attempt, then the re-armed attempt+1)
// must yield exactly ONE transcript user message (S2: turn uniqueness)
// and idempotent acks (S9: ack exactly-once per row state).
func TestRow_Leg6_7_DeliveryReplay_S2_S9(t *testing.T) {
	store := NewEvidenceStore()
	store.SetState("ses-row", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	a := newAuthority(t, store, &AnswerActor{Store: store})

	deliver := func(attempt uint32) *abiv1.DeliveryAck {
		t.Helper()
		ack, err := a.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
			SessionId: "ses-row", EntryId: "entry-1", Attempt: attempt,
			Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "do the thing"}}},
		}))
		require.NoError(t, err)
		return ack.Msg
	}

	// Original delivery; the ladder admits it (evidence written).
	deliver(1)
	require.Eventually(t, func() bool {
		m := a.Metrics()
		return m.LedgerDepths != nil && m.LedgerDepths["admitted"] == 1
	}, 5*time.Second, 5*time.Millisecond)

	v := NewViolations()

	// The rollover replay: the same (entry, attempt) re-delivered. S9's
	// shape is "no second row, no re-admission" — the duplicate ack may
	// carry the row's CURRENT state (idempotence is not frozen state):
	// assert the ledger did not GROW and no new admission fired.
	depthsSum := func() int {
		m := a.Metrics()
		n := 0
		for _, c := range m.LedgerDepths {
			n += int(c)
		}
		return n
	}
	before := depthsSum()
	_ = deliver(1)
	if depthsSum() != before {
		v.Add("S9") // the replay minted or destroyed rows
	}

	// The re-arm: attempt 2 after a presumed failure. The admitter keys
	// the transcript write by the ENTRY-LEVEL dedupe key (attempt-
	// independent), so the re-admission overwrites — the sweep promotes
	// both rows from the same evidence and the transcript stays at ONE
	// user message (S2, the #1315 sixteen-copies class).
	_ = deliver(2)
	require.Eventually(t, func() bool {
		a.Reconcile(context.Background())
		m := a.Metrics()
		return m.LedgerDepths != nil && m.LedgerDepths["ledgered"] == 0 && m.LedgerDepths["admitted"] == 0
	}, 30*time.Second, 5*time.Millisecond, "both rows resolve from the shared evidence")
	if n := store.TranscriptCount("ses-row"); n != 1 {
		v.Add("S2") // one entry, one transcript user message — not n
	}

	assert.True(t, v.Empty(), "row must end with zero violations: %v", v.Counts())
}

// Row leg 9 → the cheapness bound: a snapshot-request storm costs
// O(gather windows), not O(serves) — the singleflight coalesces. Every
// serve still answers with the truth's pending set.
func TestRow_Leg9_ServeStorm_Cheapness(t *testing.T) {
	store := NewEvidenceStore()
	store.SetState("ses-row", abiv1.SessionStatus_SESSION_STATUS_BUSY, &abiv1.InputRequest{
		Id: "in-hot", Kind: abiv1.InputKind_INPUT_KIND_QUESTION, Question: "Proceed?",
	})
	a := newAuthority(t, store, &AnswerActor{Store: store})
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))

	const serves = 50
	var wg sync.WaitGroup
	errs := make(chan error, serves)
	start := make(chan struct{})
	for i := 0; i < serves; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := a.GetSnapshot(context.Background(), connect.NewRequest(&abiv1.GetSnapshotRequest{SessionId: "ses-row"}))
			if err != nil {
				errs <- err
				return
			}
			if len(res.Msg.GetPendingInputs()) != 1 || res.Msg.GetPendingInputs()[0].GetId() != "in-hot" {
				errs <- errText("storm serve returned the wrong pending set")
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err, "every storm serve succeeds with the truth's pending set")
	}

	// The bound: serves within (about) one gather TTL coalesce onto one
	// gather; the reseed's boot reads do not count toward the storm.
	gathers := store.PendingInputsCalls()
	assert.LessOrEqual(t, gathers, 3, "50 concurrent serves cost a handful of gathers, not 50 (singleflight + TTL cache)")

	// No Violations/ConvergenceLog gates here: this row's contract is
	// the cheapness bound and serve correctness (asserted above); the
	// L-gates belong to the convergence rows.
}

// The soak self-test: the driver at high fault rate over a short
// window must end with ZERO violations — the in-process shape of the
// kind row's hours-scale gate.
func TestSoak_SelfTest_HighRateZeroViolations(t *testing.T) {
	store := NewEvidenceStore()
	a := newAuthority(t, store, &AnswerActor{Store: store})
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))

	v, log, err := RunSoak(context.Background(), a, store, SoakConfig{
		Sessions:      4,
		FaultsPerTick: 0.9,
		Tick:          5 * time.Millisecond,
		Duration:      2 * time.Second,
	})
	require.NoError(t, err)
	assert.True(t, v.Empty(), "soak self-test must end with zero violations: %v", v.Counts())
	assert.NotEmpty(t, log.Max("L3"), "the soak recorded convergence samples")
	// Epsilon for the boundary drift between WaitConverges' deadline
	// check and its Since read under -race load (observed 78µs); the
	// soak gate is the bound, not the timer's reading of it.
	assert.LessOrEqual(t, log.Max("L3"), soakConvergenceBound+100*time.Millisecond)
}

// The admitter's keyed-upsert contract, pinned directly (r1 finding 2:
// the replay row resolves via the ledger's admittedAnywhere dedup
// BEFORE Admit re-fires — this unit pin is the change's own coverage).
func TestInstantAdmitter_KeyedUpsert_S2(t *testing.T) {
	store := NewEvidenceStore()
	ad := &InstantAdmitter{Out: store}

	first, err := ad.Admit(context.Background(), "s1", "entry-key-1", "text", "model")
	require.NoError(t, err)
	second, err := ad.Admit(context.Background(), "s1", "entry-key-1", "text", "model")
	require.NoError(t, err)

	assert.Equal(t, "entry-key-1", first, "the harness store id IS the dedupe key")
	assert.Equal(t, first, second, "a re-admission returns the SAME id")
	assert.Equal(t, 1, store.TranscriptCount("s1"), "the keyed upsert overwrites — never appends (the #1315 class)")
}

// Leg 7's out-of-order half: attempt 2 lands BEFORE attempt 1 (the
// replay ladder's reorder shape). Both rows resolve; the transcript
// holds ONE user message; the ledger never grows on replay.
func TestRow_Leg7_OutOfOrderDelivery_S2_S9(t *testing.T) {
	store := NewEvidenceStore()
	store.SetState("ses-row", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	a := newAuthority(t, store, &AnswerActor{Store: store})

	deliver := func(entry string, attempt uint32) {
		t.Helper()
		_, err := a.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
			SessionId: "ses-row", EntryId: entry, Attempt: attempt,
			Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "do the thing"}}},
		}))
		require.NoError(t, err)
	}

	// Out of order: the HIGHER attempt arrives first.
	deliver("entry-oo", 2)
	deliver("entry-oo", 1)

	v := NewViolations()
	require.Eventually(t, func() bool {
		a.Reconcile(context.Background())
		m := a.Metrics()
		return m.LedgerDepths != nil && m.LedgerDepths["ledgered"] == 0 && m.LedgerDepths["admitted"] == 0
	}, 30*time.Second, 5*time.Millisecond, "both out-of-order rows resolve from shared evidence")

	if n := store.TranscriptCount("ses-row"); n != 1 {
		v.Add("S2") // one entry, one user message — order cannot matter
	}
	depthsSum := func() int {
		n := 0
		for _, c := range a.Metrics().LedgerDepths {
			n += int(c)
		}
		return n
	}
	before := depthsSum()
	deliver("entry-oo", 2) // the duplicate replay
	if depthsSum() != before {
		v.Add("S9")
	}
	assert.True(t, v.Empty(), "row must end with zero violations: %v", v.Counts())
}

// Row leg 8 → S9/L5: the boundary-slow turn (a test-scale stand-in for
// the 3-min admitter window) admits late but inside the window; the
// row resolves from evidence within L5 and the transcript stays at one.
func TestRow_Leg8_SlowBoundary_S9_L5(t *testing.T) {
	store := NewEvidenceStore()
	store.SetState("ses-row", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	a, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      NoopParser{},
		Store:       store,
		Passwords:   []string{"row-pw"},
		// The slow boundary: admission stalls 150ms (test-scale for the
		// 30-120s production turn) — DelayDeliverAck's admitter-side
		// twin. The admission window is widened so the stall stays
		// INSIDE it (leg 8's "turn ≈ window" shape).
		Admitter:        &slowAdmitter{delay: 150 * time.Millisecond, out: store},
		AdmitterTimeout: 5 * time.Second,
	})
	require.NoError(t, err)

	_, err = a.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
		SessionId: "ses-row", EntryId: "entry-slow", Attempt: 1,
		Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "slow turn"}}},
	}))
	require.NoError(t, err)

	v := NewViolations()
	log := NewConvergenceLog()
	elapsed, ok := WaitConverges(context.Background(), sessionstate.LeaseConvergenceBound, 5*time.Millisecond, func() bool {
		a.Reconcile(context.Background())
		m := a.Metrics()
		return m.LedgerDepths != nil && m.LedgerDepths["ledgered"] == 0 && m.LedgerDepths["admitted"] == 0
	})
	log.Record("L5", elapsed)
	if !ok {
		v.Add("S9") // the slow boundary stranded the entry
		v.Add("L5")
	}
	if n := store.TranscriptCount("ses-row"); n != 1 {
		v.Add("S2")
	}
	assert.True(t, v.Empty(), "row must end with zero violations: %v", v.Counts())
	assert.True(t, log.Within("L5", sessionstate.LeaseConvergenceBound))
}

// The soak's negative control: a truth source whose gather always
// errors (indeterminate) means the diff can NEVER converge the pending
// set — the soak MUST detect it (zero-violations is a gate, not a
// given). Runs one fault then tears down; the violation is real, not
// ctx-phantom (the ctx is alive throughout).
func TestSoak_NegativeControl_FailingGatherYieldsViolations(t *testing.T) {
	store := NewEvidenceStore()
	a := newAuthority(t, store, &AnswerActor{Store: store})
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))

	// Seed one projected pending so the gather's convergence matters.
	store.SetState("ses-soak-0", abiv1.SessionStatus_SESSION_STATUS_BUSY, &abiv1.InputRequest{
		Id: "in-seed", Kind: abiv1.InputKind_INPUT_KIND_QUESTION,
	})
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))
	store.FailPendingInputs = true

	v, _, err := RunSoak(context.Background(), a, store, SoakConfig{
		Sessions:      2,
		FaultsPerTick: 1.0, // every tick faults; the gather stays broken
		Tick:          5 * time.Millisecond,
		Duration:      300 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.False(t, v.Empty(), "a permanently indeterminate truth source must yield violations — the gate detects")
	assert.NotEmpty(t, v.Counts()["L3"])
}

// Config validation: the exported seam fails loudly, not panically.
func TestSoak_ConfigValidation(t *testing.T) {
	store := NewEvidenceStore()
	a := newAuthority(t, store, &AnswerActor{Store: store})
	for _, cfg := range []SoakConfig{
		{Sessions: 0, FaultsPerTick: 1, Tick: time.Millisecond, Duration: time.Millisecond},
		{Sessions: 1, FaultsPerTick: 2, Tick: time.Millisecond, Duration: time.Millisecond},
		{Sessions: 1, FaultsPerTick: 1, Tick: 0, Duration: time.Millisecond},
		{Sessions: 1, FaultsPerTick: 1, Tick: time.Millisecond, Duration: 0},
		{Sessions: 1, FaultsPerTick: math.NaN(), Tick: time.Millisecond, Duration: time.Millisecond},
	} {
		_, _, err := RunSoak(context.Background(), a, store, cfg)
		require.Error(t, err, "misconfig must return ErrSoakConfig, not panic or no-op")
	}
}

// Ctx teardown mid-soak records NO phantom violations (the hours-scale
// kind run is wall-clock canceled; teardown must not poison the gate).
func TestSoak_CtxTeardownNoPhantomViolations(t *testing.T) {
	store := NewEvidenceStore()
	a := newAuthority(t, store, &AnswerActor{Store: store})
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	v, log, err := RunSoak(ctx, a, store, SoakConfig{
		Sessions:      4,
		FaultsPerTick: 0.9,
		Tick:          5 * time.Millisecond,
		Duration:      10 * time.Second, // torn down well before
	})
	require.NoError(t, err)
	assert.True(t, v.Empty(), "ctx teardown is not a breach: %v", v.Counts())
	assert.NotNil(t, log)
}

// slowAdmitter is leg 8's boundary: admission takes delay (ctx-aware),
// then writes evidence under the entry-keyed id — the test-scale twin
// of the 30-120s production turn.
type slowAdmitter struct {
	delay time.Duration
	out   *EvidenceStore
}

func (ad *slowAdmitter) Admit(ctx context.Context, sessionID, messageID, text, model string) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(ad.delay):
	}
	if ad.out != nil {
		ad.out.MarkPresent(sessionID, messageID)
	}
	return messageID, nil
}

// The identity predicate's regression pin (r2 F2 / r4 D1): the pure
// set-matcher, driven DIRECTLY — wall-clock-free (a pin through
// GetSnapshot carries the serve-gather TTL as a hidden 500ms bound; the
// r3 "fresh authority" reshuffle did not close that window and was
// correctly rejected). Equal count, different identity must NOT match.
func TestIDsMatch_IdentityNotCount(t *testing.T) {
	assert.True(t, idsMatch("in-a\x00in-b", "in-a\x00in-b"), "identical sets match")
	assert.False(t, idsMatch("in-original", "in-replacement"),
		"stale-ask-projected + live-ask-missing at equal count is NOT a match — the count-based predicate's false-green")
	assert.False(t, idsMatch("in-a", "in-a\x00in-b"), "different sizes never match")
	assert.True(t, idsMatch("a,b\x00c", "a,b\x00c"), "comma-bearing ids are safe under the NUL join")
	assert.False(t, idsMatch("a,b\x00c", "a\x00b,c"), "the NUL join cannot collide sets a comma join would")
	assert.Equal(t, "in-x", liveOfNilFiltered(), "nil/empty filtered, duplicates collapsed")
}

// liveOfNilFiltered pins the helper-side filter contract: nil/empty ids
// never reach the join, duplicates collapse.
func liveOfNilFiltered() string {
	s := NewEvidenceStore()
	s.SetState("ses-f", 0, nil,
		&abiv1.InputRequest{Id: "in-x"},
		&abiv1.InputRequest{Id: "in-x"}, // duplicate collapses
		&abiv1.InputRequest{Id: ""},     // filtered
	)
	return s.livePendingIDs("ses-f")
}

// The leg-8 timeout→ladder-re-POST cell (r4: relabeled to what it
// provably exercises): the admitter stalls past the window on the
// ladder's FIRST try; iteration 2 re-POSTs and succeeds — the keyed id
// keeps the transcript at ONE user message, and the later deliver(2) is
// absorbed by the LEDGER's cross-attempt admittedAnywhere dedup (not by
// a harness write). The FAILED-evidence-absorption cell has its own row
// below.
func TestRow_Leg8_TimeoutLadderRePOST_S2(t *testing.T) {
	store := NewEvidenceStore()
	store.SetState("ses-row", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	a, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      NoopParser{},
		Store:       store,
		Passwords:   []string{"row-pw"},
		Admitter: &twoPhaseAdmitter{
			stall: 2 * time.Second, // > the window below — the timeout leg
			out:   store,
		},
		AdmitterTimeout: 100 * time.Millisecond,
	})
	require.NoError(t, err)

	deliver := func(attempt uint32) {
		t.Helper()
		_, err := a.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
			SessionId: "ses-row", EntryId: "entry-to", Attempt: attempt,
			Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "timeout turn"}}},
		}))
		require.NoError(t, err)
	}

	v := NewViolations()
	deliver(1) // stalls iteration 1 past the window; iteration 2 re-POSTs and succeeds

	_, ok := WaitConverges(context.Background(), sessionstate.LeaseConvergenceBound, 5*time.Millisecond, func() bool {
		a.Reconcile(context.Background())
		m := a.Metrics()
		return m.LedgerDepths != nil && m.LedgerDepths["ledgered"] == 0 && m.LedgerDepths["admitted"] == 0
	})
	if !ok {
		v.Add("L5")
	}
	deliver(2) // the re-arm: instant admission, keyed upsert
	_, ok = WaitConverges(context.Background(), sessionstate.LeaseConvergenceBound, 5*time.Millisecond, func() bool {
		a.Reconcile(context.Background())
		m := a.Metrics()
		return m.LedgerDepths != nil && m.LedgerDepths["ledgered"] == 0 && m.LedgerDepths["admitted"] == 0
	})
	if !ok {
		v.Add("S9") // the timeout's re-arm stranded
		v.Add("L5")
	}
	if n := store.TranscriptCount("ses-row"); n != 1 {
		v.Add("S2") // the ladder's re-POST and the original share the keyed id
	}
	assert.True(t, v.Empty(), "row must end with zero violations: %v", v.Counts())
}

// twoPhaseAdmitter stalls its FIRST admission past the caller's window
// (the stalled call writes nothing — ctx dies before any evidence);
// every later admission is instant and writes evidence under the
// entry-keyed id (the keyed upsert).
type twoPhaseAdmitter struct {
	stall time.Duration
	once  sync.Once
	out   *EvidenceStore
}

func (ad *twoPhaseAdmitter) Admit(ctx context.Context, sessionID, messageID, text, model string) (string, error) {
	ad.once.Do(func() {
		select {
		case <-ctx.Done():
		case <-time.After(ad.stall):
		}
	})
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if ad.out != nil {
		ad.out.MarkPresent(sessionID, messageID)
	}
	return messageID, nil
}

// The leg-8 FAILED-evidence-absorption cell (r4 finding 1 — the
// #1315-compatible-with-write incident shape, made reachable): the
// admitter NEVER writes and always stalls — the whole first ladder
// times out → markFailed (FAILED, re-armable); THEN the transcript
// message appears out-of-band (the incident's leftover write, keyed
// `msg_<entryID>`); the attempt-2 re-arm falls through the FAILED
// exclusion in admittedAnywhere and the PRE-POST evidence check
// resolves ADMITTED with NO further POST (call count frozen at the
// first ladder's; transcript exactly one).
func TestRow_Leg8_LadderExhaustion_EvidenceAbsorption_S2_S9(t *testing.T) {
	store := NewEvidenceStore()
	store.SetState("ses-row", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	ad := &pureTimeoutAdmitter{}
	a, err := sessionstate.New(sessionstate.Config{
		PlatformDir:     t.TempDir(),
		Parser:          NoopParser{},
		Store:           store,
		Passwords:       []string{"row-pw"},
		Admitter:        ad,
		AdmitterTimeout: 50 * time.Millisecond,
	})
	require.NoError(t, err)

	deliver := func(attempt uint32) {
		t.Helper()
		_, err := a.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
			SessionId: "ses-row", EntryId: "entry-f", Attempt: attempt,
			Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "time out, never write"}}},
		}))
		require.NoError(t, err)
	}

	deliver(1)

	v := NewViolations()
	// The ladder exhausts (every attempt: evidence absent → POST →
	// stall) and the row lands FAILED.
	require.Eventually(t, func() bool {
		a.Reconcile(context.Background())
		m := a.Metrics()
		return m.LedgerDepths != nil && m.LedgerDepths["failed"] >= 1
	}, 30*time.Second, 5*time.Millisecond, "the exhausted ladder marks the row FAILED")
	callsAtFailed := ad.calls
	require.Equal(t, 5, callsAtFailed, "the ladder is five attempts (the #1315 signature: attempts NEVER exceeded five)")

	// The out-of-band transcript write — the incident's leftover
	// message, under the entry-derived dedupe key.
	store.MarkPresent("ses-row", "msg_entry-f")

	// The re-arm: through the FAILED exclusion, absorbed by the pre-POST
	// evidence check — ADMITTED with no POST.
	deliver(2)
	_, ok := WaitConverges(context.Background(), sessionstate.LeaseConvergenceBound, 5*time.Millisecond, func() bool {
		a.Reconcile(context.Background())
		m := a.Metrics()
		return m.LedgerDepths != nil && m.LedgerDepths["ledgered"] == 0 && m.LedgerDepths["admitted"] == 0
	})
	if !ok {
		v.Add("S9") // the FAILED re-arm stranded
		v.Add("L5")
	}
	if ad.calls != callsAtFailed {
		v.Add("S9.absorb") // the evidence short-circuit must not re-POST
	}
	// Pin the ABSORPTION ARM (r5, the S7.arm pattern): with the FAILED
	// exclusion intact, the re-arm admits BY EVIDENCE (MessageID set)
	// and the sweep promotes it — `promoted >= 1`. Under the regression
	// this cell guards (FAILED folded into admittedAnywhere), the row
	// absorbs at the LEDGER with an EMPTY MessageID and resolves via the
	// turn-ended arm — promoted stays 0 and this pin fires.
	if a.Metrics().LedgerDepths["promoted"] < 1 {
		v.Add("S9.arm")
	}
	if n := store.TranscriptCount("ses-row"); n != 1 {
		v.Add("S2") // one out-of-band write, zero ladder writes — one transcript message
	}
	assert.True(t, v.Empty(), "row must end with zero violations: %v", v.Counts())
}

// pureTimeoutAdmitter never writes and always outlives the caller's
// window — the writeless failure ladder.
type pureTimeoutAdmitter struct {
	mu    sync.Mutex
	calls int
}

func (ad *pureTimeoutAdmitter) Admit(ctx context.Context, sessionID, messageID, text, model string) (string, error) {
	ad.mu.Lock()
	ad.calls++
	ad.mu.Unlock()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(500 * time.Millisecond): // far past every window
		return messageID, nil
	}
}

// TestSoak_Dispatch is the hours-scale soak row (#1312's soak gate):
// env-driven (LLMSAFESPACES_SOAK_DURATION / _SESSIONS / _FAULT_RATE /
// _SEED), skipped unless the duration is set — the epic71-soak workflow
// dispatches it; the self-test above stays the CI-seconds gate.
func TestSoak_Dispatch(t *testing.T) {
	raw := os.Getenv("LLMSAFESPACES_SOAK_DURATION")
	if raw == "" || testing.Short() {
		t.Skip("dispatch-only soak row: set LLMSAFESPACES_SOAK_DURATION to run")
	}
	duration, err := time.ParseDuration(raw)
	require.NoError(t, err, "LLMSAFESPACES_SOAK_DURATION must be a Go duration")
	sessions := envInt(t, "LLMSAFESPACES_SOAK_SESSIONS", 8)
	faultRate := envFloat(t, "LLMSAFESPACES_SOAK_FAULT_RATE", 0.5)
	seed := int64(envInt(t, "LLMSAFESPACES_SOAK_SEED", 1))

	store := NewEvidenceStore()
	a := newAuthority(t, store, &AnswerActor{Store: store})
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))

	start := time.Now()
	v, log, err := RunSoak(context.Background(), a, store, SoakConfig{
		Sessions:      sessions,
		FaultsPerTick: faultRate,
		Tick:          50 * time.Millisecond,
		Duration:      duration,
		Rand:          rand.New(rand.NewSource(seed)),
	})
	require.NoError(t, err)

	t.Logf("soak complete: duration=%s sessions=%d rate=%.2f seed=%d maxL3=%s",
		time.Since(start).Round(time.Second), sessions, faultRate, seed,
		log.Max("L3"))
	require.True(t, v.Empty(), "SOAK GATE: zero violations required, got %v", v.Counts())
	require.NotEmpty(t, log.Max("L3"), "SOAK GATE: convergence samples must exist")
	assert.LessOrEqual(t, log.Max("L3"), soakConvergenceBound+100*time.Millisecond,
		"SOAK GATE: worst convergence within the lease bound (+timer-drift epsilon)")
}

func envInt(t *testing.T, key string, def int) int {
	t.Helper()
	if raw := os.Getenv(key); raw != "" {
		n, err := strconv.Atoi(raw)
		require.NoError(t, err, "%s must be an integer", key)
		return n
	}
	return def
}

func envFloat(t *testing.T, key string, def float64) float64 {
	t.Helper()
	if raw := os.Getenv(key); raw != "" {
		f, err := strconv.ParseFloat(raw, 64)
		require.NoError(t, err, "%s must be a float", key)
		return f
	}
	return def
}
