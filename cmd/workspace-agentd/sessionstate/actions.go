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
		if len(ans.GetOptionIds()) == 0 && ans.GetCustomText() == "" {
			return connect.NewError(connect.CodeInvalidArgument, errText("answer_question requires option_ids and/or custom_text"))
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
