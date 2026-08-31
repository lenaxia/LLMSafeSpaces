// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- US-69.9 test plan: union dispatch, typed NotSupported, validation,
// action↔delivery serialization (golden), and the I7 interrupt purity ---

// blockingAdmitter parks admission inside Admit until released.
type blockingAdmitter struct {
	entered chan string // one send per Admit call
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func newBlockingAdmitter() *blockingAdmitter {
	return &blockingAdmitter{entered: make(chan string, 16), release: make(chan struct{})}
}

func (b *blockingAdmitter) Admit(ctx context.Context, sessionID, text, model string) (string, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	b.entered <- sessionID + "/" + text
	<-b.release
	return "msg-1", nil
}

// recordingActor records executed verbs and optionally parks inside Act.
type recordingActor struct {
	mu     sync.Mutex
	verbs  []string
	enter  chan string
	relead chan struct{}
}

func (r *recordingActor) Act(ctx context.Context, sessionID string, req *abiv1.ActionRequest) (*abiv1.ActionResult, error) {
	verb := verbOf(req)
	r.mu.Lock()
	r.verbs = append(r.verbs, verb)
	r.mu.Unlock()
	if r.enter != nil {
		r.enter <- verb
		<-r.relead
	}
	return verbResult(verb, req), nil
}

func verbOf(m *abiv1.ActionRequest) string {
	switch m.GetAction().(type) {
	case *abiv1.ActionRequest_Interrupt:
		return "interrupt"
	case *abiv1.ActionRequest_SwitchModel:
		return "switch_model"
	case *abiv1.ActionRequest_SwitchAgent:
		return "switch_agent"
	case *abiv1.ActionRequest_AnswerQuestion:
		return "answer_question"
	case *abiv1.ActionRequest_Compact:
		return "compact"
	default:
		return "unknown"
	}
}

func verbResult(verb string, m *abiv1.ActionRequest) *abiv1.ActionResult {
	switch verb {
	case "interrupt":
		return &abiv1.ActionResult{Result: &abiv1.ActionResult_Interrupt{Interrupt: &abiv1.InterruptResult{}}}
	case "switch_model":
		return &abiv1.ActionResult{Result: &abiv1.ActionResult_SwitchModel{SwitchModel: &abiv1.SwitchModelResult{Model: m.GetSwitchModel().GetModel()}}}
	case "switch_agent":
		return &abiv1.ActionResult{Result: &abiv1.ActionResult_SwitchAgent{SwitchAgent: &abiv1.SwitchAgentResult{AgentId: m.GetSwitchAgent().GetAgentId()}}}
	case "answer_question":
		return &abiv1.ActionResult{Result: &abiv1.ActionResult_AnswerQuestion{AnswerQuestion: &abiv1.AnswerInputResult{InputId: m.GetAnswerQuestion().GetInputId()}}}
	case "compact":
		return &abiv1.ActionResult{Result: &abiv1.ActionResult_Compact{Compact: &abiv1.CompactResult{}}}
	}
	return &abiv1.ActionResult{}
}

func allActions() []abiv1.ActionType {
	return []abiv1.ActionType{
		abiv1.ActionType_ACTION_TYPE_INTERRUPT,
		abiv1.ActionType_ACTION_TYPE_SWITCH_MODEL,
		abiv1.ActionType_ACTION_TYPE_SWITCH_AGENT,
		abiv1.ActionType_ACTION_TYPE_ANSWER_QUESTION,
		abiv1.ActionType_ACTION_TYPE_COMPACT,
	}
}

func actionsAuthority(t *testing.T, actor sessionstate.Actor, actions []abiv1.ActionType, admitter sessionstate.Admitter) *sessionstate.Authority {
	t.Helper()
	cfg := sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      &fixtureParser{},
		Store:       &mapStore{},
		Passwords:   []string{"pw"},
		FastCursor:  true,
		Capabilities: &abiv1.CapabilityReport{
			SupportedActions: actions,
		},
	}
	if actor != nil {
		cfg.Actor = actor
	}
	if admitter != nil {
		cfg.Admitter = admitter
	}
	a, err := sessionstate.New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// TestActOp_UnionMembers: every frozen-union member dispatches to the
// Actor and returns its typed result (the union is closed — unknown is
// NotSupported, exercised separately).
func TestActOp_UnionMembers(t *testing.T) {
	actor := &recordingActor{}
	a := actionsAuthority(t, actor, allActions(), &recordingAdmitter{})
	_, h := a.Handler()
	c := newAuthedServer(t, h)
	ctx := context.Background()

	cases := []*abiv1.ActionRequest{
		{SessionId: "s1", Action: &abiv1.ActionRequest_Interrupt{Interrupt: &abiv1.InterruptAction{}}},
		{SessionId: "s1", Action: &abiv1.ActionRequest_SwitchModel{SwitchModel: &abiv1.SwitchModelAction{Model: &abiv1.ModelRef{Id: "m1", Provider: "p"}}}},
		{SessionId: "s1", Action: &abiv1.ActionRequest_SwitchAgent{SwitchAgent: &abiv1.SwitchAgentAction{AgentId: "plan"}}},
		{SessionId: "s1", Action: &abiv1.ActionRequest_AnswerQuestion{AnswerQuestion: &abiv1.AnswerInputAction{InputId: "q1", OptionIds: []string{"Go"}}}},
		{SessionId: "s1", Action: &abiv1.ActionRequest_Compact{Compact: &abiv1.CompactAction{}}},
	}
	for _, req := range cases {
		res, err := c.Act(ctx, connect.NewRequest(req))
		require.NoError(t, err, "verb %s", verbOf(req))
		assert.Equal(t, "s1", res.Msg.GetSessionId())
		assert.NotNil(t, res.Msg.GetResult(), "typed result set for %s", verbOf(req))
		assert.Zero(t, res.Msg.GetEffectSeq(), "effect_seq unset: not knowable before the response returns")
	}
	actor.mu.Lock()
	defer actor.mu.Unlock()
	assert.Equal(t, []string{"interrupt", "switch_model", "switch_agent", "answer_question", "compact"}, actor.verbs)
}

// TestActOp_NotSupportedTyped: an undeclared verb is a TYPED
// NotSupported from the capability report — never a 500/404 guessing game.
func TestActOp_NotSupportedTyped(t *testing.T) {
	actor := &recordingActor{}
	declared := []abiv1.ActionType{
		abiv1.ActionType_ACTION_TYPE_INTERRUPT,
		abiv1.ActionType_ACTION_TYPE_SWITCH_MODEL,
		abiv1.ActionType_ACTION_TYPE_ANSWER_QUESTION,
	}
	a := actionsAuthority(t, actor, declared, nil)
	_, h := a.Handler()
	c := newAuthedServer(t, h)

	_, err := c.Act(context.Background(), connect.NewRequest(&abiv1.ActionRequest{
		SessionId: "s1",
		Action:    &abiv1.ActionRequest_Compact{Compact: &abiv1.CompactAction{}},
	}))
	require.Error(t, err)
	var cerr *connect.Error
	require.ErrorAs(t, err, &cerr)
	assert.Equal(t, connect.CodeUnimplemented, cerr.Code())
	ns := findNotSupportedDetail(cerr)
	require.NotNil(t, ns, "typed NotSupported detail present")
	assert.Equal(t, "action.compact", ns.GetCapability())
}

// findNotSupportedDetail extracts the NotSupported detail (the
// not_supported_test convention).
func findNotSupportedDetail(err *connect.Error) *abiv1.NotSupported {
	for _, d := range err.Details() {
		v, verr := d.Value()
		if verr != nil {
			continue
		}
		if ns, ok := v.(*abiv1.NotSupported); ok {
			return ns
		}
	}
	return nil
}

// TestActOp_UnknownAction: an unset oneof is typed action.unknown.
func TestActOp_UnknownAction(t *testing.T) {
	a := actionsAuthority(t, &recordingActor{}, allActions(), nil)
	_, h := a.Handler()
	c := newAuthedServer(t, h)
	_, err := c.Act(context.Background(), connect.NewRequest(&abiv1.ActionRequest{SessionId: "s1"}))
	require.Error(t, err)
	var cerr *connect.Error
	require.ErrorAs(t, err, &cerr)
	assert.Equal(t, connect.CodeUnimplemented, cerr.Code())
}

// TestActOp_ActorNilNotSupported: no Actor wired → the surface itself is
// NotSupported (abi.actions), matching Deliver-without-ledger.
func TestActOp_ActorNilNotSupported(t *testing.T) {
	a := actionsAuthority(t, nil, allActions(), nil)
	_, h := a.Handler()
	c := newAuthedServer(t, h)
	_, err := c.Act(context.Background(), connect.NewRequest(&abiv1.ActionRequest{
		SessionId: "s1",
		Action:    &abiv1.ActionRequest_Interrupt{Interrupt: &abiv1.InterruptAction{}},
	}))
	require.Error(t, err)
	var cerr *connect.Error
	require.ErrorAs(t, err, &cerr)
	assert.Equal(t, connect.CodeUnimplemented, cerr.Code())
	ns := findNotSupportedDetail(cerr)
	require.NotNil(t, ns)
	assert.Equal(t, "abi.actions", ns.GetCapability())
}

// TestActOp_Validation: per-verb field validation rejects before the Actor
// (and before the session lock).
func TestActOp_Validation(t *testing.T) {
	actor := &recordingActor{}
	a := actionsAuthority(t, actor, allActions(), nil)
	_, h := a.Handler()
	c := newAuthedServer(t, h)
	ctx := context.Background()

	bad := []*abiv1.ActionRequest{
		{SessionId: "s1", Action: &abiv1.ActionRequest_SwitchModel{SwitchModel: &abiv1.SwitchModelAction{}}},
		{SessionId: "s1", Action: &abiv1.ActionRequest_SwitchAgent{SwitchAgent: &abiv1.SwitchAgentAction{}}},
		{SessionId: "s1", Action: &abiv1.ActionRequest_AnswerQuestion{AnswerQuestion: &abiv1.AnswerInputAction{}}},
		{SessionId: "s1", Action: &abiv1.ActionRequest_AnswerQuestion{AnswerQuestion: &abiv1.AnswerInputAction{InputId: "q1"}}},
	}
	for _, req := range bad {
		_, err := c.Act(ctx, connect.NewRequest(req))
		require.Error(t, err, "verb %s must validate", verbOf(req))
		var cerr *connect.Error
		require.ErrorAs(t, err, &cerr)
		assert.Equal(t, connect.CodeInvalidArgument, cerr.Code())
	}
	actor.mu.Lock()
	assert.Empty(t, actor.verbs, "no action reaches the Actor on validation failure")
	actor.mu.Unlock()
}

// TestActOp_SerializesAgainstDelivery (golden): actions and admissions
// share the per-session single-flight — no interleave, in BOTH directions,
// and no lost interrupt while an admission holds the lock.
func TestActOp_SerializesAgainstDelivery(t *testing.T) {
	t.Run("action waits for in-flight admission", func(t *testing.T) {
		admitter := newBlockingAdmitter()
		actor := &recordingActor{enter: make(chan string, 1), relead: make(chan struct{})}
		a := actionsAuthority(t, actor, allActions(), admitter)
		_, h := a.Handler()
		c := newAuthedServer(t, h)

		_, err := c.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
			SessionId: "s1", EntryId: "e-1", Attempt: 1,
			Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "hello"}}},
		}))
		require.NoError(t, err)
		select {
		case <-admitter.entered: // admission holds the session lock
		case <-time.After(2 * time.Second):
			t.Fatal("admission never started")
		}

		resCh := make(chan error, 1)
		go func() {
			_, err := c.Act(context.Background(), connect.NewRequest(&abiv1.ActionRequest{
				SessionId: "s1",
				Action:    &abiv1.ActionRequest_Interrupt{Interrupt: &abiv1.InterruptAction{}},
			}))
			resCh <- err
		}()

		select {
		case <-actor.enter:
			t.Fatal("action executed WHILE admission held the single-flight lock")
		case <-time.After(150 * time.Millisecond):
		}

		close(admitter.release) // admission completes, lock frees
		select {
		case <-actor.enter: // the interrupt was NOT lost
		case <-time.After(2 * time.Second):
			t.Fatal("interrupt never executed after admission released")
		}
		close(actor.relead)
		require.NoError(t, <-resCh)
	})

	t.Run("admission waits for in-flight action", func(t *testing.T) {
		admitter := newBlockingAdmitter()
		actor := &recordingActor{enter: make(chan string, 1), relead: make(chan struct{})}
		a := actionsAuthority(t, actor, allActions(), admitter)
		_, h := a.Handler()
		c := newAuthedServer(t, h)

		go func() {
			_, err := c.Act(context.Background(), connect.NewRequest(&abiv1.ActionRequest{
				SessionId: "s2",
				Action:    &abiv1.ActionRequest_Compact{Compact: &abiv1.CompactAction{}},
			}))
			assert.NoError(t, err)
		}()
		select {
		case <-actor.enter: // the action holds the session lock
		case <-time.After(2 * time.Second):
			t.Fatal("action never started")
		}

		_, err := c.Deliver(context.Background(), connect.NewRequest(&abiv1.DeliveryRequest{
			SessionId: "s2", EntryId: "e-2", Attempt: 1,
			Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "hi"}}},
		}))
		require.NoError(t, err)

		select {
		case <-admitter.entered:
			t.Fatal("admission ran WHILE the action held the single-flight lock")
		case <-time.After(150 * time.Millisecond):
		}

		close(actor.relead)
		select {
		case <-admitter.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("admission never ran after the action released")
		}
		close(admitter.release)
	})
}

// TestActOp_InterruptAdmissionRace (I7): an admission completing during an
// interrupt lands a PRESERVED admitted row — the interrupt never mutates
// entry states (no superseded-by-interrupt exists), and the queued row is
// not lost to the interrupt.
func TestActOp_InterruptAdmissionRace(t *testing.T) {
	admitter := newBlockingAdmitter()
	actor := &recordingActor{}
	a := actionsAuthority(t, actor, allActions(), admitter)
	_, h := a.Handler()
	c := newAuthedServer(t, h)
	ctx := context.Background()

	_, err := c.Deliver(ctx, connect.NewRequest(&abiv1.DeliveryRequest{
		SessionId: "s1", EntryId: "e-1", Attempt: 1,
		Parts: []*abiv1.DeliveryPart{{Part: &abiv1.DeliveryPart_Text{Text: "hello"}}},
	}))
	require.NoError(t, err)
	select {
	case <-admitter.entered: // the admission is in flight, holding the lock
	case <-time.After(2 * time.Second):
		t.Fatal("admission never started")
	}

	// The interrupt fires mid-admission and queues on the single-flight.
	resCh := make(chan *abiv1.ActionResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := c.Act(ctx, connect.NewRequest(&abiv1.ActionRequest{
			SessionId: "s1",
			Action:    &abiv1.ActionRequest_Interrupt{Interrupt: &abiv1.InterruptAction{}},
		}))
		if err != nil {
			errCh <- err
			return
		}
		resCh <- res.Msg
	}()

	// The admission completes WHILE the interrupt is pending (the race).
	close(admitter.release)

	select {
	case res := <-resCh:
		require.NotNil(t, res.GetInterrupt(), "the pending interrupt executes — not lost")
	case err := <-errCh:
		t.Fatalf("interrupt failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("interrupt never executed")
	}

	// I7: the row landed ADMITTED and is PRESERVED through the interrupt —
	// the Act path never touched entry states.
	st, err := c.GetDeliveryStatus(ctx, connect.NewRequest(&abiv1.GetDeliveryStatusRequest{EntryId: "e-1", Attempt: 1}))
	require.NoError(t, err)
	assert.Equal(t, abiv1.LedgerState_LEDGER_STATE_ADMITTED, st.Msg.GetState(),
		"admission completing during the interrupt is preserved — no superseded-by-interrupt state exists")
}
