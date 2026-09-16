// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	"github.com/lenaxia/llmsafespaces/pkg/abi/abitest"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	abiconnect "github.com/lenaxia/llmsafespaces/pkg/abi/v1/abiconnect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// act_answer_test.go — #1396: answer actions never serialize behind the
// session admission lock. The ask exists exactly when the session is busy
// (an admission riding its 3-minute Admit context holds the lock — #1342),
// so a lock-queued answer is deadlock-by-design: the user's click waits on
// the very turn waiting on the click. The tests here pin the carve-out's
// three legs: L12 (answer ≤2s under a busy session), the typed 5s budget
// (never a 125s silent hang), and the M1/W4 matrix UNCHANGED for every
// non-answer verb (pinned by the untouched golden tests in
// actions_test.go).

// parkingActor parks inside Act for park (honoring ctx) before succeeding
// — the slow-harness shape for the budget legs.
type parkingActor struct {
	park time.Duration
}

func (p parkingActor) Act(ctx context.Context, sessionID string, req *abiv1.ActionRequest) (*abiv1.ActionResult, error) {
	timer := time.NewTimer(p.park)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return verbResult(verbOf(req), req), nil
}

// harnessDeadlineActor returns a DeadlineExceeded-WRAPPING error
// immediately — a harness-internal deadline that raced (and lost to)
// nothing: the agentd budget has NOT fired when it arrives.
type harnessDeadlineActor struct{}

func (harnessDeadlineActor) Act(ctx context.Context, sessionID string, req *abiv1.ActionRequest) (*abiv1.ActionResult, error) {
	return nil, fmt.Errorf("harness route deadline: %w", context.DeadlineExceeded)
}

// answerBudget returns the injected-answer-timeout mutator (150ms —
// deterministic budget expiry inside a multi-second caller window).
func answerBudget(d time.Duration) func(*sessionstate.Config) {
	return func(cfg *sessionstate.Config) { cfg.AnswerTimeout = d }
}

// actAsync runs one Act over the wire and reports (result, error) — the
// bounded-wait shape every L12 leg needs (a pre-fix answer parks on the
// session lock; the assertion, not a test timeout, must fail).
func actAsync(c abiclientIface, req *abiv1.ActionRequest) (<-chan *abiv1.ActionResult, <-chan error) {
	resCh := make(chan *abiv1.ActionResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := c.Act(context.Background(), connect.NewRequest(req))
		if err != nil {
			errCh <- err
			return
		}
		resCh <- res.Msg
	}()
	return resCh, errCh
}

// TestAct_AnswerUnderHeldAdmission_Completes — today's exact repro (#1396):
// a delivery admission parks inside Admit holding the session's
// single-flight lock (the #1342 3-minute window); the user answers the
// pending ask. The answer MUST complete while the admission is still in
// flight — L12: answer latency ≤ 2s under a busy session.
func TestAct_AnswerUnderHeldAdmission_Completes(t *testing.T) {
	admitter := newBlockingAdmitter()
	a := actionsAuthority(t, &recordingActor{}, allActions(), admitter)
	_, h := a.Handler()
	c := newAuthedServer(t, h)
	// Release FIRST on teardown (LIFO): a pre-fix parked answer must
	// unwind before httptest.Close waits on its connection.
	t.Cleanup(func() { close(admitter.release) })

	_, err := c.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
		SessionId: "s1", EntryId: "e-1396", Attempt: 1,
		Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "long turn"}}},
	}))
	require.NoError(t, err)
	select {
	case <-admitter.entered: // the admission holds the session lock
	case <-time.After(2 * time.Second):
		t.Fatal("admission never started")
	}

	seedPendingInput(t, a, "s1", "per_1")

	done := make(chan error, 1)
	go func() {
		_, err := c.Act(context.Background(), connect.NewRequest(answerRequest("s1", "per_1")))
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err, "the answer must succeed under a busy session")
	case <-time.After(2 * time.Second):
		t.Fatal("L12 violated: the answer is still queued behind the held session lock (#1396's 2m05s→502 shape)")
	}
}

// TestAct_AnswerRacingInFlightAdmission_BothLand — the fault leg proposed
// for #1312's L12: an answer racing an admission that PROMOTES the same
// turn. The answer lands while the admission is in flight; the admission
// then completes to ADMITTED; the projection sees the harness's own
// INPUT_RESOLVED fold cleanly afterward. Neither path loses.
func TestAct_AnswerRacingInFlightAdmission_BothLand(t *testing.T) {
	admitter := newBlockingAdmitter()
	a := actionsAuthority(t, &recordingActor{}, allActions(), admitter)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(admitter.release) }) }
	_, h := a.Handler()
	c := newAuthedServer(t, h)
	// Release FIRST on teardown (LIFO): a pre-fix parked answer must
	// unwind before httptest.Close waits on its connection.
	t.Cleanup(release)
	ctx := context.Background()

	_, err := c.Deliver(ctx, connect.NewRequest(&abiv1.DeliveryRequest{
		SessionId: "s1", EntryId: "e-race", Attempt: 1,
		Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "turn blocked on the ask"}}},
	}))
	require.NoError(t, err)
	select {
	case <-admitter.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("admission never started")
	}

	seedPendingInput(t, a, "s1", "per_1")

	resCh, errCh := actAsync(c, answerRequest("s1", "per_1"))
	select {
	case res := <-resCh:
		require.NotNil(t, res.GetAnswerQuestion(), "the answer lands while the admission is still in flight")
	case err := <-errCh:
		t.Fatalf("answer failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("L12 violated: the answer is queued behind the admission it is answering for")
	}

	// The turn the answer unblocked completes; the harness folds the
	// resolved ask onto the stream (its own event, not agentd's).
	a.IngestForTest(&abiv1.Event{
		SessionId: "s1",
		Type:      abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED,
		Input:     &abiv1.InputRequest{Id: "per_1"},
	})
	assert.Equal(t, 0, pendingCount(a, "s1"), "the projection sees the resolved ask")

	// The raced admission is neither lost nor corrupted — it lands ADMITTED.
	release()
	require.Eventually(t, func() bool {
		st, err := c.GetDeliveryStatus(ctx, connect.NewRequest(&abiv1.GetDeliveryStatusRequest{EntryId: "e-race", Attempt: 1}))
		return err == nil && st.Msg.GetState() == abiv1.LedgerState_LEDGER_STATE_ADMITTED
	}, 3*time.Second, 20*time.Millisecond, "the raced admission lands ADMITTED (I7: the answer path never touches entry state)")
}

// TestAct_AnswerResolveByAbsence_UnderHeldAdmission — the 404 fold
// (resolveByAbsence) must not need the session lock either: a stale click
// while an admission is in flight resolves by absence under the projection
// lock alone (a.mu — a leaf in the lock order).
func TestAct_AnswerResolveByAbsence_UnderHeldAdmission(t *testing.T) {
	admitter := newBlockingAdmitter()
	actor := &notFoundActor{failVerbs: map[string]bool{"answer_question": true}}
	a := actionsAuthority(t, actor, allActions(), admitter)
	_, h := a.Handler()
	c := newAuthedServer(t, h)
	// Release FIRST on teardown (LIFO): a pre-fix parked answer must
	// unwind before httptest.Close waits on its connection.
	t.Cleanup(func() { close(admitter.release) })

	_, err := c.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
		SessionId: "s1", EntryId: "e-stale", Attempt: 1,
		Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "turn"}}},
	}))
	require.NoError(t, err)
	select {
	case <-admitter.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("admission never started")
	}

	seedPendingInput(t, a, "s1", "per_gone")
	require.Equal(t, 1, pendingCount(a, "s1"))

	done := make(chan error, 1)
	go func() {
		_, err := c.Act(context.Background(), connect.NewRequest(answerRequest("s1", "per_gone")))
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err, "S6: absence resolves even under a held admission")
	case <-time.After(2 * time.Second):
		t.Fatal("resolve-by-absence queued behind the held session lock")
	}
	assert.Equal(t, 0, pendingCount(a, "s1"), "the projected ask dropped through the standard fold")
}

// TestAct_AnswerBudget_TypedDeadlineError — the budget leg (#1396 change
// 2): an answer forward that exceeds its deadline budget fails FAST with a
// typed retryable DeadlineExceeded — never a silent 125s hang. Caller
// cancellation surfaces as Canceled (never masked as the budget), and the
// budget is answer-only (a compact parking past it still completes).
func TestAct_AnswerBudget_TypedDeadlineError(t *testing.T) {
	t.Run("budget expiry is typed and fast", func(t *testing.T) {
		// 150ms budget, 600ms harness park: the budget must win well
		// inside the caller's 5s window.
		a := actionsAuthority(t, parkingActor{park: 600 * time.Millisecond}, allActions(), nil, answerBudget(150*time.Millisecond))
		_, h := a.Handler()
		c := newAuthedServer(t, h)

		start := time.Now()
		_, err := c.Act(context.Background(), connect.NewRequest(answerRequest("s1", "per_slow")))
		require.Error(t, err)
		elapsed := time.Since(start)
		assert.Less(t, elapsed, 2*time.Second, "the budget fires fast — never a hang")
		var cerr *connect.Error
		require.ErrorAs(t, err, &cerr)
		assert.Equal(t, connect.CodeDeadlineExceeded, cerr.Code(), "typed deadline — the UI can retry")
		assert.Equal(t, int64(1), a.Metrics().AnswerBudgetExceeded, "the budget-expiry counter advances")
	})

	t.Run("caller cancellation is never masked as the budget", func(t *testing.T) {
		a := actionsAuthority(t, parkingActor{park: 10 * time.Second}, allActions(), nil, answerBudget(150*time.Millisecond))

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		_, err := a.Act(ctx, connect.NewRequest(answerRequest("s1", "per_cancel")))
		require.Error(t, err)
		var cerr *connect.Error
		require.ErrorAs(t, err, &cerr)
		assert.Equal(t, connect.CodeCanceled, cerr.Code(), "the CALLER's cancel surfaces — distinct from the budget deadline")
		assert.Equal(t, int64(0), a.Metrics().AnswerBudgetExceeded, "a caller cancel is not a budget expiry")
	})

	t.Run("harness-internal deadlines are not the budget", func(t *testing.T) {
		// An actor error WRAPPING DeadlineExceeded but produced before
		// the budget fired (a harness-internal deadline) must not count
		// as a budget expiry — the budget context is the classifier,
		// not the actor's error shape (exact counter semantics).
		a := actionsAuthority(t, harnessDeadlineActor{}, allActions(), nil, answerBudget(150*time.Millisecond))
		_, err := a.Act(context.Background(), connect.NewRequest(answerRequest("s1", "per_hd")))
		require.Error(t, err)
		assert.Equal(t, int64(0), a.Metrics().AnswerBudgetExceeded,
			"a harness-internal deadline is not OUR budget — the canary counter stays exact")
	})

	t.Run("non-answer verbs keep the unbounded sole-writer wait", func(t *testing.T) {
		// Compact parks 400ms past the same 150ms budget and still
		// completes: the budget bounds ANSWER forwards only.
		a := actionsAuthority(t, parkingActor{park: 400 * time.Millisecond}, allActions(), nil, answerBudget(150*time.Millisecond))
		_, h := a.Handler()
		c := newAuthedServer(t, h)

		res, err := c.Act(context.Background(), connect.NewRequest(&abiv1.ActionRequest{
			SessionId: "s1",
			Action:    &abiv1.ActionRequest_Compact{Compact: &abiv1.CompactAction{}},
		}))
		require.NoError(t, err, "the answer budget never bounds transcript verbs")
		assert.NotNil(t, res.Msg.GetCompact())
		assert.Equal(t, int64(0), a.Metrics().AnswerBudgetExceeded)
	})
}

// TestAct_AnswerL12_WireLevel_BusySession — the integration leg (abitest
// knobs, #1312 test plan): a REAL generated connect handler over HTTP, the
// REAL delivery driver + ledger, an admission parked in its 3-minute
// window, a pending ask seeded in both the harness registry and the
// projection — and the user's answer lands within L12 (≤2s) while the
// admission is still in flight.
func TestAct_AnswerL12_WireLevel_BusySession(t *testing.T) {
	abi := abitest.New()
	srv := httptest.NewServer(abi.Handler())
	t.Cleanup(srv.Close)

	actor := &abiClientActor{client: abiconnect.NewHarnessABIServiceClient(http.DefaultClient, srv.URL)}
	admitter := newBlockingAdmitter()
	a := actionsAuthority(t, actor, allActions(), admitter)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(admitter.release) }) }
	_, h := a.Handler()
	c := newAuthedServer(t, h)
	// Release FIRST on teardown (LIFO): a pre-fix parked answer must
	// unwind before httptest.Close waits on its connection.
	t.Cleanup(release)
	ctx := context.Background()

	// The harness raised the ask; both truths hold it.
	abi.SeedPendingInput("ses-busy", &abiv1.InputRequest{Id: "per_9", Kind: abiv1.InputKind_INPUT_KIND_PERMISSION})
	seedPendingInput(t, a, "ses-busy", "per_9")

	// The admission parks in its 3-minute Admit window, holding the
	// session's single-flight lock (#1342's loop).
	_, err := c.Deliver(ctx, connect.NewRequest(&abiv1.DeliveryRequest{
		SessionId: "ses-busy", EntryId: "e-l12", Attempt: 1,
		Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "turn awaiting the user's reply"}}},
	}))
	require.NoError(t, err)
	select {
	case <-admitter.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("admission never started")
	}

	start := time.Now()
	resCh, errCh := actAsync(c, answerRequest("ses-busy", "per_9"))
	select {
	case res := <-resCh:
		require.NotNil(t, res.GetAnswerQuestion(), "the user's click must land under a busy session")
	case err := <-errCh:
		t.Fatalf("answer failed under a busy session: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("L12 violated: the answer is queued behind the admission it is answering for")
	}
	assert.Less(t, int64(time.Since(start)), int64(2*time.Second),
		"L12: answer latency ≤ 2s under a busy session")

	// The harness registry dropped the ask (the forward hit the REGISTRY,
	// not the transcript): the turn is unblocked.
	assert.Empty(t, abi.PendingInputs("ses-busy"), "the harness ask registry cleared")

	// The turn proceeds; the harness's own resolved event folds.
	a.IngestForTest(&abiv1.Event{
		SessionId: "ses-busy",
		Type:      abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED,
		Input:     &abiv1.InputRequest{Id: "per_9"},
	})
	assert.Equal(t, 0, pendingCount(a, "ses-busy"))

	// The admission completes normally — the answer did not disturb it.
	release()
	require.Eventually(t, func() bool {
		st, err := c.GetDeliveryStatus(ctx, connect.NewRequest(&abiv1.GetDeliveryStatusRequest{EntryId: "e-l12", Attempt: 1}))
		return err == nil && st.Msg.GetState() == abiv1.LedgerState_LEDGER_STATE_ADMITTED
	}, 3*time.Second, 20*time.Millisecond, "the unblocked turn's admission lands ADMITTED")
}
