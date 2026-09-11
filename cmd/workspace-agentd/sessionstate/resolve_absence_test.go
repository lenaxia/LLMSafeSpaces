// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate_test

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// --- #1310 slice A (epic-71 / 1a): resolve-by-absence -----------------------
//
// An opencode ask is a lease, not a record: the harness drops it without
// any lifecycle event when the turn aborts, and the user's reply click
// then 404s — stranding the projection (ses_f73747f8). Absence IS the
// resolution: a harness not-found on answer_question drops the projected
// entry (InputResolved through the standard fold, browsers clear) and
// returns SUCCESS. Answer-only; every other verb's NotFound stays typed.

// notFoundActor fails the listed verbs with connect NotFound.
type notFoundActor struct {
	failVerbs map[string]bool
	innerErr  error
}

func (n *notFoundActor) Act(ctx context.Context, sessionID string, req *abiv1.ActionRequest) (*abiv1.ActionResult, error) {
	if n.failVerbs[verbOf(req)] {
		code := connect.CodeNotFound
		if n.innerErr != nil {
			var cerr *connect.Error
			if errorsAs(n.innerErr, &cerr) {
				return nil, cerr
			}
		}
		return nil, connect.NewError(code, errStringOf("harness: input not found"))
	}
	return verbResult(verbOf(req), req), nil
}

func errStringOf(s string) error { return &staticErr{s} }

type staticErr struct{ msg string }

func (e *staticErr) Error() string { return e.msg }

func errorsAs(err error, target *(*connect.Error)) bool {
	if ce, ok := err.(*connect.Error); ok {
		*target = ce
		return true
	}
	return false
}

func answerRequest(sessionID, inputID string) *abiv1.ActionRequest {
	return &abiv1.ActionRequest{
		SessionId: sessionID,
		Action: &abiv1.ActionRequest_AnswerQuestion{
			AnswerQuestion: &abiv1.AnswerInputAction{InputId: inputID, OptionIds: []string{"yes"}},
		},
	}
}

func seedPendingInput(t *testing.T, a *sessionstate.Authority, sessionID, inputID string) {
	t.Helper()
	a.IngestForTest(&abiv1.Event{
		SessionId: sessionID,
		Type:      abiv1.EventType_EVENT_TYPE_INPUT_REQUEST,
		Input:     &abiv1.InputRequest{Id: inputID, Kind: abiv1.InputKind_INPUT_KIND_PERMISSION},
	})
}

// TestAct_ResolveByAbsence_DropsProjectionAndSucceeds: a harness 404 on
// answer_question is the resolution — entry dropped, InputResolved on the
// stream (browsers clear), SUCCESS to the caller (S6).
func TestAct_ResolveByAbsence_DropsProjectionAndSucceeds(t *testing.T) {
	a := actionsAuthority(t, &notFoundActor{failVerbs: map[string]bool{"answer_question": true}}, allActions(), nil)
	stream, cancel, err := a.Stream(context.Background())
	require.NoError(t, err)
	defer cancel()
	drainFrames(stream)

	seedPendingInput(t, a, "ses-1", "per_1")
	require.Equal(t, 1, pendingCount(a, "ses-1"))

	res, err := a.Act(context.Background(), &connect.Request[abiv1.ActionRequest]{Msg: answerRequest("ses-1", "per_1")})
	require.NoError(t, err, "absence is a valid answer — never an error")
	require.NotNil(t, res.Msg.GetAnswerQuestion())
	assert.Equal(t, "per_1", res.Msg.GetAnswerQuestion().GetInputId())

	assert.Equal(t, 0, pendingCount(a, "ses-1"), "projection entry dropped")

	frames := collectFrames(stream, 2)
	require.Len(t, frames, 2, "the seed ask then exactly one resolved event")
	require.Equal(t, abiv1.EventType_EVENT_TYPE_INPUT_REQUEST, frames[0].GetEvent().GetEvent().GetType())
	evt := frames[1].GetEvent().GetEvent()
	require.Equal(t, abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED, evt.GetType())
	assert.Equal(t, "per_1", evt.GetInput().GetId())
}

// TestAct_ResolveByAbsence_NothingProjectedNoSeq: an ask the projection
// never held resolves to SUCCESS without consuming a seq or minting a
// phantom session record.
func TestAct_ResolveByAbsence_NothingProjectedNoSeq(t *testing.T) {
	a := actionsAuthority(t, &notFoundActor{failVerbs: map[string]bool{"answer_question": true}}, allActions(), nil)
	stream, cancel, err := a.Stream(context.Background())
	require.NoError(t, err)
	defer cancel()
	drainFrames(stream)

	seqBefore := a.State().Seq
	res, err := a.Act(context.Background(), &connect.Request[abiv1.ActionRequest]{Msg: answerRequest("ses-ghost", "per_none")})
	require.NoError(t, err)
	require.NotNil(t, res.Msg.GetAnswerQuestion())

	assert.Equal(t, 0, pendingCount(a, "ses-ghost"))
	assert.Equal(t, seqBefore, a.State().Seq, "no seq consumed for an unprojected ask")
	frames := collectFrames(stream, 1)
	assert.Empty(t, frames, "no seq consumed, no phantom record")
}

// TestAct_ResolveByAbsence_AnswerOnly: every other verb's NotFound stays
// a typed error — absence resolves answers, not switch_model.
func TestAct_ResolveByAbsence_AnswerOnly(t *testing.T) {
	a := actionsAuthority(t, &notFoundActor{failVerbs: map[string]bool{"switch_model": true}}, allActions(), nil)
	_, err := a.Act(context.Background(), &connect.Request[abiv1.ActionRequest]{Msg: &abiv1.ActionRequest{
		SessionId: "ses-1",
		Action:    &abiv1.ActionRequest_SwitchModel{SwitchModel: &abiv1.SwitchModelAction{Model: &abiv1.ModelRef{Id: "m1"}}},
	}})
	require.Error(t, err)
	var cerr *connect.Error
	require.ErrorAs(t, err, &cerr)
	assert.Equal(t, connect.CodeNotFound, cerr.Code(), "non-answer NotFound passes through typed")
}

// TestAct_ResolveByAbsence_OtherErrorCodesLeaveProjection: only NotFound
// resolves by absence — an Internal/Unavailable harness error keeps the
// entry projected (the click may be retried).
func TestAct_ResolveByAbsence_OtherErrorCodesLeaveProjection(t *testing.T) {
	actor := &notFoundActor{
		failVerbs: map[string]bool{"answer_question": true},
		innerErr:  connect.NewError(connect.CodeInternal, &staticErr{"harness exploded"}),
	}
	a := actionsAuthority(t, actor, allActions(), nil)
	seedPendingInput(t, a, "ses-1", "per_1")

	_, err := a.Act(context.Background(), &connect.Request[abiv1.ActionRequest]{Msg: answerRequest("ses-1", "per_1")})
	require.Error(t, err)
	assert.Equal(t, 1, pendingCount(a, "ses-1"), "non-NotFound errors never resolve by absence")
}

// TestValidateAction_AnswerForms: reply (permission vocabulary) and
// option_ids/custom_text (question answers) are disjoint forms; at least
// one is required.
func TestValidateAction_AnswerForms(t *testing.T) {
	a := actionsAuthority(t, &recordingActor{}, allActions(), nil)
	tests := []struct {
		name    string
		ans     *abiv1.AnswerInputAction
		wantErr bool
	}{
		{"reply alone", &abiv1.AnswerInputAction{InputId: "per_1", Reply: strPtr("once")}, false},
		{"options alone", &abiv1.AnswerInputAction{InputId: "que_1", OptionIds: []string{"a"}}, false},
		{"custom_text alone", &abiv1.AnswerInputAction{InputId: "que_1", CustomText: strPtr("free text")}, false},
		{"reply plus options", &abiv1.AnswerInputAction{InputId: "x", Reply: strPtr("always"), OptionIds: []string{"a"}}, true},
		{"reply plus custom_text", &abiv1.AnswerInputAction{InputId: "x", Reply: strPtr("reject"), CustomText: strPtr("t")}, true},
		{"no answer form", &abiv1.AnswerInputAction{InputId: "x"}, true},
		{"no input id", &abiv1.AnswerInputAction{Reply: strPtr("once")}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := a.Act(context.Background(), &connect.Request[abiv1.ActionRequest]{Msg: &abiv1.ActionRequest{
				SessionId: "ses-1",
				Action:    &abiv1.ActionRequest_AnswerQuestion{AnswerQuestion: tt.ans},
			}})
			if tt.wantErr {
				require.Error(t, err)
				var cerr *connect.Error
				require.ErrorAs(t, err, &cerr)
				assert.Equal(t, connect.CodeInvalidArgument, cerr.Code())
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func strPtr(s string) *string { return &s }

// pendingCount reads one session's projected pending set (0 when the
// session record does not exist).
func pendingCount(a *sessionstate.Authority, sessionID string) int {
	v := a.State().Sessions[sessionID]
	if v == nil {
		return 0
	}
	return len(v.PendingInputs)
}

func drainFrames(ch <-chan *abiv1.StreamFrame) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// collectFrames gathers up to n EVENT frames, skipping the stream's
// initial stamped-snapshot frame (not an event).
func collectFrames(ch <-chan *abiv1.StreamFrame, n int) []*abiv1.StreamFrame {
	var out []*abiv1.StreamFrame
	deadline := time.After(3 * time.Second)
	for len(out) < n {
		select {
		case f := <-ch:
			if f.GetEvent() != nil {
				out = append(out, f)
			}
		case <-deadline:
			return out
		}
	}
	return out
}

// abiClientActor adapts the real generated connect client to the Actor
// seam — the wire-level replay exercises the exact production transport.
type abiClientActor struct {
	client abiconnect.HarnessABIServiceClient
}

func (c *abiClientActor) Act(ctx context.Context, sessionID string, req *abiv1.ActionRequest) (*abiv1.ActionResult, error) {
	req.SessionId = sessionID
	res, err := c.client.Act(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// TestAct_ResolveByAbsence_WireLevelIncidentReplay: the ses_f73747f8
// shape end-to-end over the real ABI transport — the ask is projected,
// the harness silently drops it (leg 1), the user's click 404s at the
// harness (leg 2), and the projection clears with SUCCESS to the caller.
func TestAct_ResolveByAbsence_WireLevelIncidentReplay(t *testing.T) {
	abi := abitest.New()
	srv := httptest.NewServer(abi.Handler())
	t.Cleanup(srv.Close)

	actor := &abiClientActor{client: abiconnect.NewHarnessABIServiceClient(http.DefaultClient, srv.URL)}
	a := actionsAuthority(t, actor, allActions(), nil)
	stream, cancel, err := a.Stream(context.Background())
	require.NoError(t, err)
	defer cancel()

	// The harness raised the ask; the authority projected it.
	abi.SeedPendingInput("ses-f737", &abiv1.InputRequest{Id: "per_08c8", Kind: abiv1.InputKind_INPUT_KIND_PERMISSION})
	seedPendingInput(t, a, "ses-f737", "per_08c8")
	require.Equal(t, 1, pendingCount(a, "ses-f737"))

	// The turn aborted; the harness dropped the ask with NO event (leg 1).
	abi.DropAskSilently("ses-f737", "per_08c8")

	// The user clicks; the harness 404s (leg 2) — and absence resolves.
	res, err := a.Act(context.Background(), connect.NewRequest(answerRequest("ses-f737", "per_08c8")))
	require.NoError(t, err, "S6: the stale click resolves, never errors")
	require.NotNil(t, res.Msg.GetAnswerQuestion())
	assert.Equal(t, 0, pendingCount(a, "ses-f737"), "projection cleared")

	frames := collectFrames(stream, 2)
	require.Len(t, frames, 2)
	assert.Equal(t, abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED, frames[1].GetEvent().GetEvent().GetType(),
		"browsers clear on the resolved event")
}
