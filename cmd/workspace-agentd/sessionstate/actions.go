// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// actions.go — design 0055 M1 op 5 (US-69.9): the typed actions op.
// agentd is the sole writer of session mutations; every action serializes
// against in-flight delivery on the same per-session single-flight (the
// US-69.6 concurrency-matrix decision: no exceptions), and I7 holds by
// construction — the Act path never touches ledger/entry state.
//
// Capability negotiation is the gate: a verb not declared in the boot
// capability report (Config.Capabilities) is a typed NotSupported BEFORE
// any harness call — harness differences are data, never API branches.

// Actor is the dialect seam for action execution: perform one typed
// action against the harness (localhost :4096 in production; the wiring
// layer injects the opencode implementation — same seam class as
// Admitter). The authority has already validated the union member and
// verified capability declaration; the Actor executes UNDER the session's
// single-flight lock, so a slow action delays that session's admissions
// (the sole-writer contract) but nothing else.
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
	default:
		return abiv1.ActionType_ACTION_TYPE_UNSPECIFIED, "action.unknown", false
	}
}

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

	// Sole-writer serialization (M1/W4 + the no-exceptions matrix): the
	// action holds the session's single-flight lock across execution —
	// the SAME lock admissions take, so a delivery in flight and an
	// action can never interleave.
	lock := a.sessionLock(m.GetSessionId())
	lock.Lock()
	defer lock.Unlock()

	res, err := a.cfg.Actor.Act(ctx, m.GetSessionId(), m)
	if err != nil {
		var cerr *connect.Error
		if errors.As(err, &cerr) {
			// Resolve-by-absence (#1310 slice A, S6): a harness 404 on an
			// ANSWER is the resolution — the ask was a lease, the harness
			// already dropped it, and erroring here is what stranded the
			// ses_f73747f8 prompt. Drop the projected entry (browsers
			// clear on the resolved event) and succeed. Answer-only:
			// every other verb's NotFound stays a typed error.
			if cerr.Code() == connect.CodeNotFound {
				if ans := m.GetAnswerQuestion(); ans != nil {
					return a.resolveByAbsence(m.GetSessionId(), ans.GetInputId())
				}
			}
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

// resolveByAbsence executes S6's "absence is authoritative" half for an
// answer whose harness forward returned not-found: the projected entry
// (if any) is dropped through the standard fold — a seq-assigned
// InputResolved fans out to every browser — and the Act caller gets
// SUCCESS. An ask the projection never held resolves to SUCCESS without
// consuming a seq or minting a phantom session record (nothing was
// stranded, nothing needs clearing).
//
// Lock order: the caller holds the session single-flight lock; this path
// then takes a.mu — the reverse edge (a.mu → sessionLock) exists nowhere
// (applyLocked/observe take only promotionMu/ledger.mu under a.mu), so
// the ordering is acyclic.
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
		hasLegacy := len(ans.GetOptionIds()) > 0 || ans.GetCustomText() != ""
		switch {
		case ans.GetReply() != "" && hasLegacy:
			return connect.NewError(connect.CodeInvalidArgument, errText("answer_question reply is disjoint from option_ids/custom_text"))
		case ans.GetReply() == "" && !hasLegacy:
			return connect.NewError(connect.CodeInvalidArgument, errText("answer_question requires option_ids and/or custom_text, or reply"))
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
