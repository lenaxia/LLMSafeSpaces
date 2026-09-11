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

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// The engine's own contract — the failure paths every future L-violation
// and soak row flows through (review r1: an undetected WaitConverges
// regression would turn every soak row vacuously green).

func TestWaitConverges_ImmediateHold(t *testing.T) {
	elapsed, ok := WaitConverges(context.Background(), time.Second, 5*time.Millisecond, func() bool { return true })
	require.True(t, ok)
	assert.Less(t, elapsed, time.Second, "an already-held predicate converges without waiting")
}

func TestWaitConverges_BreachReturnsFalse(t *testing.T) {
	start := time.Now()
	elapsed, ok := WaitConverges(context.Background(), 80*time.Millisecond, 10*time.Millisecond, func() bool { return false })
	require.False(t, ok, "a never-holding predicate breaches")
	assert.GreaterOrEqual(t, elapsed, 80*time.Millisecond, "the elapsed span carries the full bound — the L-sample's evidence")
	assert.Less(t, time.Since(start), 3*time.Second, "and returns, rather than looping forever")
}

func TestWaitConverges_CtxCancelReturnsFalse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	elapsed, ok := WaitConverges(ctx, 5*time.Second, 5*time.Millisecond, func() bool { return false })
	require.False(t, ok, "a canceled ctx is a breach-shaped return — the row adds its L-violation")
	assert.Less(t, elapsed, time.Second)
}

func TestViolations_CopySemantics(t *testing.T) {
	v := NewViolations()
	require.True(t, v.Empty())
	v.Add("S6")
	v.Add("L4")
	v.Add("S6")
	require.False(t, v.Empty())
	counts := v.Counts()
	require.Equal(t, 2, counts["S6"])
	require.Equal(t, 1, counts["L4"])
	counts["S6"] = 99 // a copy — the counter is unaffected
	assert.Equal(t, 2, v.Counts()["S6"])
}

func TestConvergenceLog_MaxAndWithin(t *testing.T) {
	l := NewConvergenceLog()
	l.Record("L2", 10*time.Millisecond)
	l.Record("L2", 250*time.Millisecond)
	l.Record("L2", 5*time.Millisecond)
	assert.Equal(t, 250*time.Millisecond, l.Max("L2"), "Max is the worst sample")
	assert.True(t, l.Within("L2", 250*time.Millisecond))
	assert.False(t, l.Within("L2", 249*time.Millisecond))
}

// TestConvergenceLog_WithinFailsWithoutSamples: an unrecorded bound is a
// row that forgot to measure — the L-gate fails CLOSED, never passes
// vacuously (review r1 finding 4).
func TestConvergenceLog_WithinFailsWithoutSamples(t *testing.T) {
	l := NewConvergenceLog()
	l.Record("L4", time.Millisecond)
	assert.False(t, l.Within("L2", time.Hour), "no samples under a bound = not within — the row failed to measure")
	assert.Equal(t, time.Duration(0), l.Max("L2"))
}

func TestAnswerActor_LiveAskAnswers(t *testing.T) {
	store := NewEvidenceStore()
	store.SetState("s1", abiv1.SessionStatus_SESSION_STATUS_BUSY, &abiv1.InputRequest{
		Id: "in-1", Kind: abiv1.InputKind_INPUT_KIND_QUESTION,
	})
	actor := &AnswerActor{Store: store}

	res, err := actor.Act(context.Background(), "s1", &abiv1.ActionRequest{
		SessionId: "s1",
		Action: &abiv1.ActionRequest_AnswerQuestion{AnswerQuestion: &abiv1.AnswerInputAction{
			InputId: "in-1", OptionIds: []string{"yes"},
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, res.GetAnswerQuestion())
	assert.Equal(t, "in-1", res.GetAnswerQuestion().GetInputId(), "the result names the answered input")
}

func TestAnswerActor_NonAnswerIsUnimplemented(t *testing.T) {
	actor := &AnswerActor{Store: NewEvidenceStore()}
	_, err := actor.Act(context.Background(), "s1", &abiv1.ActionRequest{
		SessionId: "s1",
		Action:    &abiv1.ActionRequest_Interrupt{Interrupt: &abiv1.InterruptAction{}},
	})
	require.Error(t, err)
	assert.Equal(t, connect.CodeUnimplemented, connect.CodeOf(err))
}

func TestInstantAdmitter_DistinctIDsAndNilOut(t *testing.T) {
	ad := &InstantAdmitter{}
	first, err := ad.Admit(context.Background(), "s1", "m-1", "text", "model")
	require.NoError(t, err)
	second, err := ad.Admit(context.Background(), "s1", "m-2", "text", "model")
	require.NoError(t, err)
	assert.NotEqual(t, first, second, "distinct store ids — the promotion correlation leans on uniqueness")
	assert.NotEmpty(t, first)

	nilOut := &InstantAdmitter{Out: nil}
	_, err = nilOut.Admit(context.Background(), "s1", "m-3", "text", "model")
	require.NoError(t, err, "Out==nil skips the evidence write without panicking (the turn-ended arm's lever)")
}

// TestWaitConverges_PostDeadlineSuccessIsBreach: fn flipping true JUST
// past the bound reports (elapsed > bound, ok == false) — the pin for
// the round-2 contract change (deleting that branch passes everything
// else; this is the test that catches it).
func TestWaitConverges_PostDeadlineSuccessIsBreach(t *testing.T) {
	bound := 60 * time.Millisecond
	flipAt := time.Now().Add(bound + 20*time.Millisecond)
	elapsed, ok := WaitConverges(context.Background(), bound, 2*time.Millisecond, func() bool {
		return time.Now().After(flipAt)
	})
	require.False(t, ok, "a success observed past the deadline is a breach, not a convergence")
	assert.Greater(t, elapsed, bound, "the elapsed span evidences the breach")
}
