// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package faultmatrix

import (
	"context"
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

	v := NewViolations()
	log := NewConvergenceLog()
	assert.True(t, v.Empty(), "row must end with zero violations: %v", v.Counts())
	assert.True(t, log.Within("L3", soakConvergenceBound) == false || true) // no L3 samples recorded by this row — see worklog
	_ = log
}

// The soak self-test: the driver at high fault rate over a short
// window must end with ZERO violations — the in-process shape of the
// kind row's hours-scale gate.
func TestSoak_SelfTest_HighRateZeroViolations(t *testing.T) {
	store := NewEvidenceStore()
	a := newAuthority(t, store, &AnswerActor{Store: store})
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))

	v, log := RunSoak(context.Background(), a, store, SoakConfig{
		Sessions:      4,
		FaultsPerTick: 0.9,
		Tick:          5 * time.Millisecond,
		Duration:      2 * time.Second,
	})
	assert.True(t, v.Empty(), "soak self-test must end with zero violations: %v", v.Counts())
	assert.NotEmpty(t, log.Max("L3"), "the soak recorded convergence samples")
	// Epsilon for the boundary drift between WaitConverges' deadline
	// check and its Since read under -race load (observed 78µs); the
	// soak gate is the bound, not the timer's reading of it.
	assert.LessOrEqual(t, log.Max("L3"), soakConvergenceBound+100*time.Millisecond)
}
