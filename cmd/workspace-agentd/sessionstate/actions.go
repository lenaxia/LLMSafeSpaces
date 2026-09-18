// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// actions.go — design 0055 M1 op 5 (US-69.9): the typed actions op.
// agentd is the sole writer of session mutations, and every
// TRANSCRIPT-targeted action serializes against in-flight delivery on
// the same per-session single-flight (the US-69.6 concurrency-matrix
// decision). #1396 amends the matrix by VERB CLASSIFICATION, not a
// blanket unlock: an ANSWER_QUESTION forward targets the harness's ask
// REGISTRY (`/permission/:id/reply`, `/question/:id/reply|reject` —
// ephemeral harness-side state, #1312's ownership table), a write set
// DISJOINT from the admission's (the transcript). Answers therefore
// never take the session admission lock — an ask exists exactly when a
// session is busy, so queuing the user's reply behind the admission
// waiting on it was deadlock-by-design (the 2m05s→502 shape).
//
// Capability negotiation is the gate: a verb not declared in the boot
// capability report (Config.Capabilities) is a typed NotSupported BEFORE
// any harness call — harness differences are data, never API branches.

// Actor is the dialect seam for action execution: perform one typed
// action against the harness (localhost :4096 in production; the wiring
// layer injects the opencode implementation — same seam class as
// Admitter). The authority has already validated the union member and
// verified capability declaration. Transcript verbs (interrupt, switch,
// compact) execute UNDER the session's single-flight lock, so a slow
// action delays that session's admissions (the sole-writer contract);
// answer verbs execute lock-free under their own forward budget.
type Actor interface {
	Act(ctx context.Context, sessionID string, action *abiv1.ActionRequest) (*abiv1.ActionResult, error)
}

// actVerb maps a union member to its capability-report declaration and
// the capability string a NotSupported carries.
func actVerb(m *abiv1.ActionRequest) (abiv1.ActionType, string, bool) {
	switch m.GetAction().(type) {
	case *abiv1.ActionRequest_Interrupt:
		return abiv1.ActionType_ACTION_TYPE_INTERRUPT, "action.interrupt", true
	case *abiv1.ActionRequest_SwitchModel:
		return abiv1.ActionType_ACTION_TYPE_SWITCH_MODEL, "action.switch_model", true
	case *abiv1.ActionRequest_SwitchAgent:
		return abiv1.ActionType_ACTION_TYPE_SWITCH_AGENT, "action.switch_agent", true
	case *abiv1.ActionRequest_AnswerQuestion:
		return abiv1.ActionType_ACTION_TYPE_ANSWER_QUESTION, "action.answer_question", true
	case *abiv1.ActionRequest_Compact:
		return abiv1.ActionType_ACTION_TYPE_COMPACT, "action.compact", true
	case *abiv1.ActionRequest_CreateSession:
		return abiv1.ActionType_ACTION_TYPE_CREATE_SESSION, "action.create_session", true
	case *abiv1.ActionRequest_Send:
		return abiv1.ActionType_ACTION_TYPE_SEND, "action.send", true
	case *abiv1.ActionRequest_DeleteSession:
		return abiv1.ActionType_ACTION_TYPE_DELETE_SESSION, "action.delete_session", true
	case *abiv1.ActionRequest_RenameSession:
		return abiv1.ActionType_ACTION_TYPE_RENAME_SESSION, "action.rename_session", true
	default:
		return abiv1.ActionType_ACTION_TYPE_UNSPECIFIED, "action.unknown", false
	}
}

// answerForwardBudget bounds one answer's harness forward (#1396): the
// forward mutates the ask registry and completes in milliseconds
// (53-307ms measured on the pinned harness); 5s is 15-90x headroom.
// Overridable via Config.AnswerTimeout (the AdmitterTimeout precedent).
const answerForwardBudget = 5 * time.Second

// act is the Act op core (service.go keeps only rate limiting).
func (a *Authority) act(ctx context.Context, m *abiv1.ActionRequest) (*abiv1.ActionResult, error) {
	if a.cfg.Actor == nil {
		return nil, notSupported("abi.actions", "action surface not wired on this authority")
	}
	verb, capability, known := actVerb(m)
	if !known {
		// The schema-frozen union is closed; an unset oneof is a client
		// bug, an unknown member is a schema change — both are typed
		// NotSupported from the capability report's point of view.
		return nil, notSupported(capability, "no action set or action not in the frozen union")
	}
	if !actionDeclared(a.cfg.Capabilities, verb) {
		return nil, notSupported(capability, "not declared in this authority's capability report")
	}
	if err := validateAction(m); err != nil {
		return nil, err
	}

	// Verb classification (#1396 M1/W4 amendment + #1372 r2/r4): three
	// verbs execute OUTSIDE the session single-flight, each with its own
	// recorded rationale:
	//
	//   - answer_question (#1396): forwards target the ask REGISTRY —
	//     harness-side ephemeral state disjoint from the transcript an
	//     admission writes; the answer COMMUTES with any in-flight
	//     admission, and a user's reply must never queue behind the
	//     turn waiting on it (own forward-budget context).
	//   - interrupt (#1372 r2): mutates no projected records (I7), and
	//     its purpose is to preempt the lock HOLDER's in-flight turn —
	//     queueing it behind that turn would make abort a delayed no-op
	//     (the flag-off adapter abort stops the live turn within
	//     seconds, live-verified).
	//   - send (#1372 r4): the harness itself serializes per-session
	//     message writes (a busy session blocks incoming messages, B1)
	//     and S2 (#1315) dedupes admissions at the harness write — the
	//     single-flight adds no write protection here, and holding it
	//     across a full LLM turn DEADLOCKS the ask-answer cycle the
	//     #1396 answer carve-out exists to keep open.
	if m.GetAnswerQuestion() != nil {
		return a.actAnswer(ctx, m)
	}
	if m.GetInterrupt() == nil && m.GetSend() == nil {
		lock := a.sessionLock(m.GetSessionId())
		lock.Lock()
		defer lock.Unlock()
	}

	res, err := a.cfg.Actor.Act(ctx, m.GetSessionId(), m)
	if err != nil {
		var cerr *connect.Error
		if errors.As(err, &cerr) {
			return nil, cerr // harness seams may return typed errors; pass through
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if res == nil {
		res = &abiv1.ActionResult{}
	}
	res.SessionId = m.GetSessionId()
	// effect_seq stays unset: the effect event lands on the stream after
	// the harness call returns (the design-0055 open item — causal
	// linkage is the consumer's seq observe, not a synchronous promise).
	return res, nil
}

// actAnswer executes an ANSWER_QUESTION without the session admission
// lock (#1396). The forward targets the ask registry under a short
// deadline budget — never the caller's unbounded context — and the only
// authority state the path can touch is the projection fold inside
// resolveByAbsence (a.mu alone; see its lock-order note). I7 holds by
// construction: no ledger or entry state is read or written here, so an
// answer racing an in-flight admission leaves that admission's outcome
// untouched (pinned by TestAct_AnswerRacingInFlightAdmission_BothLand).
func (a *Authority) actAnswer(ctx context.Context, m *abiv1.ActionRequest) (*abiv1.ActionResult, error) {
	budget := a.cfg.AnswerTimeout
	if budget <= 0 {
		budget = answerForwardBudget
	}
	fctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	res, err := a.cfg.Actor.Act(fctx, m.GetSessionId(), m)
	if err != nil {
		// The caller's own cancellation surfaces as Canceled — never
		// masked as the budget's deadline (the UI must tell "I gave up"
		// from "the harness is busy"). Checked before the 404 fold: a
		// NotFound racing the cancel skips resolveByAbsence and the
		// lease diff's next tick converges the projected ask (bounded).
		if ctx.Err() != nil {
			return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
		}
		if fctx.Err() == context.DeadlineExceeded {
			// The budget context — not the actor's error shape —
			// classifies the expiry: a harness-INTERNAL deadline that
			// merely wraps DeadlineExceeded keeps its own error below
			// and the canary counter exact. The budget fired: fast,
			// typed, retryable — never the 125s silent hang this path
			// used to inherit from the caller's context.
			a.mu.Lock()
			a.answerBudgetExceeded++
			a.mu.Unlock()
			return nil, connect.NewError(connect.CodeDeadlineExceeded, errText("answer forward exceeded its deadline budget; the harness did not respond in time — retry"))
		}
		var cerr *connect.Error
		if errors.As(err, &cerr) {
			// Resolve-by-absence (#1310 slice A, S6): a harness 404 on an
			// ANSWER is the resolution — the ask was a lease, the harness
			// already dropped it, and erroring here is what stranded the
			// ses_f73747f8 prompt. Drop the projected entry (browsers
			// clear on the resolved event) and succeed. Answer-only:
			// every other verb's NotFound stays a typed error.
			if cerr.Code() == connect.CodeNotFound {
				return a.resolveByAbsence(m.GetSessionId(), m.GetAnswerQuestion().GetInputId())
			}
			return nil, cerr // harness seams may return typed errors; pass through
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if res == nil {
		res = &abiv1.ActionResult{}
	}
	res.SessionId = m.GetSessionId()
	// effect_seq stays unset: the harness's INPUT_RESOLVED event (or the
	// lease diff) lands on the stream and clears every browser — the
	// answer's success itself writes no projection state.
	return res, nil
}

// resolveByAbsence executes S6's "absence is authoritative" half for an
// answer whose harness forward returned not-found: the projected entry
// (if any) is dropped through the standard fold — a seq-assigned
// InputResolved fans out to every browser — and the Act caller gets
// SUCCESS. An ask the projection never held resolves to SUCCESS without
// consuming a seq or minting a phantom session record (nothing was
// stranded, nothing needs clearing).
//
// Lock order: this path takes a.mu ALONE (a leaf — the edges
// sessionLock → a.mu exist in lease/reconcile/serialized actions; the
// reverse edge exists nowhere). Since #1396 the answer path calls it
// WITHOUT the session single-flight; the fold is idempotent under a.mu
// and composes with the lease diff's own resolve half (both check
// rec.pending before folding, and diffSessionLease's resolvedSeq gate
// never resurrects a just-resolved ask).
func (a *Authority) resolveByAbsence(sessionID, inputID string) (*abiv1.ActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if rec := a.sessions[sessionID]; rec != nil {
		if _, projected := rec.pending[inputID]; projected {
			a.applyLocked(&abiv1.Event{
				SessionId: sessionID,
				Type:      abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED,
				Input:     &abiv1.InputRequest{Id: inputID},
			})
		}
	}
	return &abiv1.ActionResult{
		SessionId: sessionID,
		Result: &abiv1.ActionResult_AnswerQuestion{
			AnswerQuestion: &abiv1.AnswerInputResult{InputId: inputID},
		},
	}, nil
}

func validateAction(m *abiv1.ActionRequest) error {
	switch a := m.GetAction().(type) {
	case *abiv1.ActionRequest_SwitchModel:
		if a.SwitchModel.GetModel().GetId() == "" {
			return connect.NewError(connect.CodeInvalidArgument, errText("switch_model requires model.id"))
		}
	case *abiv1.ActionRequest_SwitchAgent:
		if a.SwitchAgent.GetAgentId() == "" {
			return connect.NewError(connect.CodeInvalidArgument, errText("switch_agent requires agent_id"))
		}
	case *abiv1.ActionRequest_AnswerQuestion:
		ans := a.AnswerQuestion
		if ans.GetInputId() == "" {
			return connect.NewError(connect.CodeInvalidArgument, errText("answer_question requires input_id"))
		}
		// Disjoint answer forms (#1302): reply carries the permission
		// vocabulary; option_ids/custom_text carry question answers.
		// 4a D2: message is deny-feedback — only meaningful with reply.
		hasLegacy := len(ans.GetOptionIds()) > 0 || ans.GetCustomText() != ""
		switch {
		case ans.GetReply() != "" && hasLegacy:
			return connect.NewError(connect.CodeInvalidArgument, errText("answer_question reply is disjoint from option_ids/custom_text"))
		case ans.GetReply() == "" && !hasLegacy:
			return connect.NewError(connect.CodeInvalidArgument, errText("answer_question requires option_ids and/or custom_text, or reply"))
		case ans.GetMessage() != "" && ans.GetReply() == "":
			return connect.NewError(connect.CodeInvalidArgument, errText("answer_question message requires reply (deny feedback)"))
		}
	case *abiv1.ActionRequest_Send:
		// #1372: the session-scoped sessions verbs require their target;
		// create_session is exempt (the harness mints the id).
		if m.GetSessionId() == "" {
			return connect.NewError(connect.CodeInvalidArgument, errText("send requires session_id"))
		}
		if a.Send.GetText() == "" {
			return connect.NewError(connect.CodeInvalidArgument, errText("send requires text"))
		}
	case *abiv1.ActionRequest_DeleteSession:
		if m.GetSessionId() == "" {
			return connect.NewError(connect.CodeInvalidArgument, errText("delete_session requires session_id"))
		}
	case *abiv1.ActionRequest_RenameSession:
		if m.GetSessionId() == "" {
			return connect.NewError(connect.CodeInvalidArgument, errText("rename_session requires session_id"))
		}
		if a.RenameSession.GetTitle() == "" {
			return connect.NewError(connect.CodeInvalidArgument, errText("rename_session requires title"))
		}
	}
	return nil
}

// actionDeclared reports whether the boot capability report declares the
// verb. A nil report declares nothing (every action is NotSupported).
func actionDeclared(report *abiv1.CapabilityReport, verb abiv1.ActionType) bool {
	if report == nil {
		return false
	}
	for _, s := range report.GetSupportedActions() {
		if s == verb {
			return true
		}
	}
	return false
}
