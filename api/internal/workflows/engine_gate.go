// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// #1812: the routine fire's inline gate script — dispatch, return-schema
// validation, and the fire-audit writes. executeRoutine (engine.go) owns
// the sequencing; this file owns what a gate IS.

import (
	"context"
	"encoding/json"
	"fmt"

	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// routineGateNodeID is the gate leg's node identity in agentd dispatches
// (parallel to routine-script/routine-agent).
const routineGateNodeID = "routine-gate"

// runRoutineGate dispatches the trigger's inline gate script to the
// routine's workspace (the same agentd script-node sandbox workflow DAG
// script nodes run in), validates the {proceed, vars} return schema, and
// writes the gate outcome to the fire's action_result. It returns:
//
//   - (proceed, vars, true): gate OPEN — vars flow to the caller for
//     prompt substitution; execution continues.
//   - (false, nil, true): gate CLOSED — fire recorded skipped with the
//     gate payload; NO LLM call, no failure accounting (a closed gate is
//     the script working, not failing).
//   - (_, _, false): gate FAILED — fail-closed. The fire is failed with
//     the named error, auto-disable accounting runs, no LLM call. This
//     covers transport errors, agentd error envelopes, unparseable
//     output, and return-schema violations alike.
//
// Best-effort audit writes: an audit-store failure is logged and the
// decision stands (the gate itself already ran; blocking the fire on an
// observability write would turn a healthy gate into an outage).
func (s *Scheduler) runRoutineGate(ctx context.Context, logger Logger, trigger *wf.TriggerRow, fire *wf.TriggerFireRow, envelopeJSON json.RawMessage, workspaceID, podIP string) (proceed bool, vars map[string]string, done bool) {
	spec, err := json.Marshal(wf.TriggerScript{Language: trigger.ScriptLanguage, Handler: trigger.ScriptHandler})
	if err != nil {
		// Unreachable: both fields are plain strings.
		return false, nil, s.failRoutineGate(ctx, logger, trigger, fire, fmt.Sprintf("gate spec build failed: %v", err))
	}
	req := &NodeExecRequest{
		NodeID: routineGateNodeID, NodeType: "script",
		Spec: spec, Input: envelopeJSON,
		Timeout: "5m",
	}
	// #1458 parity: the gate rides the same bounded transient retry as
	// every other routine leg — a provider-blip-shaped 5xx must not
	// fail-closed a healthy gate. Deterministic failures return after
	// one attempt.
	resp, err := executeWithRetry(ctx, s.AgentdClient, workspaceID, podIP, req, nil)
	if err != nil {
		return false, nil, s.failRoutineGate(ctx, logger, trigger, fire, fmt.Sprintf("gate script failed: %v", err))
	}
	if resp.ErrorCode != "" {
		return false, nil, s.failRoutineGate(ctx, logger, trigger, fire, fmt.Sprintf("gate script failed: %s: %s", resp.ErrorCode, resp.Detail))
	}
	if len(resp.Output) == 0 {
		return false, nil, s.failRoutineGate(ctx, logger, trigger, fire, `gate script returned no output (the handler must return {"proceed": <bool>, "vars": {<str>: <str>}})`)
	}
	proceed, vars, perr := wf.ParseGateScriptOutput(resp.Output)
	if perr != nil {
		return false, nil, s.failRoutineGate(ctx, logger, trigger, fire, perr.Error())
	}

	action := gateActionPayload(proceed, vars, "")
	if aerr := s.Store.UpdateTriggerFireAction(ctx, fire.ID, action); aerr != nil {
		logger.Error(aerr, "routine: failed to record gate outcome", "triggerId", trigger.ID, "fireId", fire.ID)
	}
	if !proceed {
		if rerr := s.Store.UpdateTriggerFireResult(ctx, fire.ID, nil, "skipped"); rerr != nil {
			logger.Error(rerr, "routine: failed to record gated fire", "triggerId", trigger.ID, "fireId", fire.ID)
		}
		logger.Info("routine gated closed — no LLM call", "triggerId", trigger.ID, "fireId", fire.ID)
	}
	return proceed, vars, true
}

// failRoutineGate is the fail-closed arm: failed fire, named error in
// the action payload, auto-disable accounting, no LLM call.
func (s *Scheduler) failRoutineGate(ctx context.Context, logger Logger, trigger *wf.TriggerRow, fire *wf.TriggerFireRow, cause string) bool {
	errPayload, _ := json.Marshal(map[string]string{"error": cause})
	if aerr := s.Store.UpdateTriggerFireAction(ctx, fire.ID, gateActionPayload(false, nil, cause)); aerr != nil {
		logger.Error(aerr, "routine: failed to record gate failure", "triggerId", trigger.ID, "fireId", fire.ID)
	}
	_ = s.Store.UpdateTriggerFireResult(ctx, fire.ID, errPayload, "failed")
	s.accountTriggerFailure(ctx, trigger)
	logger.Error(fmt.Errorf("%s", cause), "routine: gate script failed closed — no LLM call", "triggerId", trigger.ID, "fireId", fire.ID)
	return false
}

// gateActionPayload renders the fire-audit's self-explaining gate
// outcome: {gate: {proceed, vars[, error]}} — written BEFORE the agent
// leg so every script-gated fire explains itself regardless of what the
// leg then does to result/status.
func gateActionPayload(proceed bool, vars map[string]string, gateErr string) json.RawMessage {
	gate := map[string]any{"proceed": proceed}
	if len(vars) > 0 {
		gate["vars"] = vars
	}
	if gateErr != "" {
		gate["error"] = gateErr
	}
	payload, _ := json.Marshal(map[string]any{"gate": gate})
	return payload
}
