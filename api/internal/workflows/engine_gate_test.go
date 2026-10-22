// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// #1812: the trigger's inline gate script. Every pin below counts
// agentd dispatches by node id — the no-LLM-call claim is proven by
// ZERO "routine-agent" spawns, never by a log line.

func makeDueRoutineTrigger(id string, prompt string) *wf.TriggerRow {
	now := time.Now().UTC().Add(-5 * time.Second)
	return &wf.TriggerRow{
		ID: id, OwnerType: "user", OwnerID: "u1",
		Enabled: true, SourceType: types.TriggerSourceCron,
		SourceConfig: json.RawMessage(`{"expr":"0 * * * *","tz":"UTC"}`),
		WorkspaceID:  strPtr("ws-1"),
		Prompt:       prompt,
		MemoryMode:   types.MemoryNone,
		CaptureMode:  types.CaptureErrorsOnly,
		NextFireAt:   &now,
	}
}

const gateHandlerSrc = "def handler(input):\n    return {\"proceed\": True, \"vars\": {}}\n"

func fireByTriggerID(t *testing.T, store *mockSchedulerStore, triggerID string) *wf.TriggerFireRow {
	t.Helper()
	for _, f := range store.fires {
		if f.TriggerID == triggerID {
			return f
		}
	}
	t.Fatalf("no fire row for trigger %s", triggerID)
	return nil
}

// A closed gate (proceed=false) exits with NO LLM call: exactly one
// agentd dispatch (the gate node itself), zero agent legs, a skipped
// fire carrying the gate payload, and no failure accounting.
func TestScheduler_RoutineGateClosedNoAgentCall(t *testing.T) {
	store := newMockSchedulerStore()
	trig := makeDueRoutineTrigger("trig-gate-closed", "Summarize {{topic}}")
	trig.ScriptLanguage = "python"
	trig.ScriptHandler = gateHandlerSrc
	store.triggers = []*wf.TriggerRow{trig}

	agentd := newMockAgentd()
	agentd.outputs["routine-gate"] = json.RawMessage(`{"proceed":false,"vars":{"topic":"skipped"}}`)

	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd,
		Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	if got := agentd.callNodeIDs(); len(got) != 1 || got[0] != "routine-gate" {
		t.Fatalf("a closed gate must dispatch ONLY the gate node, got %v", got)
	}
	if _, spawned := agentd.sentSpecs["routine-agent"]; spawned {
		t.Fatal("proceed=false must spawn ZERO agent (LLM) legs — sentSpecs carries routine-agent")
	}
	fire := fireByTriggerID(t, store, "trig-gate-closed")
	if fire.Status != "skipped" {
		t.Fatalf("closed gate fire must be skipped, got %q", fire.Status)
	}
	action := store.actions[fire.ID]
	if action == nil || !strings.Contains(string(action), `"proceed":false`) || !strings.Contains(string(action), `"topic":"skipped"`) {
		t.Fatalf("fire audit must carry gate outcome + vars, got %s", action)
	}
	if store.triggerFail["trig-gate-closed"] != 0 {
		t.Fatalf("a closed gate is not a failure — consecutiveFailures incremented %d times", store.triggerFail["trig-gate-closed"])
	}
}

// An open gate (proceed=true) fires the prompt with jinja-style var
// substitution, unmatched tokens left literal, and ALL vars appended as
// a trailing metadata block — with the gate outcome recorded on the
// fire row before the agent leg.
func TestScheduler_RoutineGateOpenSubstitutesVars(t *testing.T) {
	store := newMockSchedulerStore()
	trig := makeDueRoutineTrigger("trig-gate-open", "Report on {{topic}} ({{count}} items); {{unknown}} stays; env {{.input}}")
	trig.ScriptLanguage = "node"
	trig.ScriptHandler = "exports.handler = async (input) => ({proceed: true, vars: {}});"
	store.triggers = []*wf.TriggerRow{trig}

	agentd := newMockAgentd()
	agentd.outputs["routine-gate"] = json.RawMessage(`{"proceed":true,"vars":{"count":"3","topic":"nightly"}}`)
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"done"}`)

	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd,
		Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	got := agentd.callNodeIDs()
	if len(got) != 2 || got[0] != "routine-gate" || got[1] != "routine-agent" {
		t.Fatalf("open gate must dispatch gate then agent, got %v", got)
	}
	gateSpec := agentd.sentSpecs["routine-gate"]
	var gateParsed map[string]any
	if err := json.Unmarshal(gateSpec, &gateParsed); err != nil {
		t.Fatalf("gate spec must be a script-node spec: %v", err)
	}
	if gateParsed["language"] != "node" {
		t.Fatalf("gate spec must carry the trigger's language, got %v", gateParsed)
	}
	if !strings.Contains(gateParsed["handler"].(string), "exports.handler") {
		t.Fatalf("gate spec must carry the INLINE handler source, got %v", gateParsed)
	}

	var agentSpec map[string]any
	if err := json.Unmarshal(agentd.sentSpecs["routine-agent"], &agentSpec); err != nil {
		t.Fatalf("agent spec unparseable: %v", err)
	}
	prompt, _ := agentSpec["prompt"].(string)
	if !strings.Contains(prompt, "Report on nightly (3 items)") {
		t.Fatalf("{{key}} tokens must be substituted from vars, got %q", prompt)
	}
	if !strings.Contains(prompt, "{{unknown}} stays") {
		t.Fatalf("unmatched tokens must stay literal, got %q", prompt)
	}
	if !strings.Contains(prompt, `"type":"cron"`) {
		t.Fatalf("{{.input}} envelope substitution must still run, got %q", prompt)
	}
	if !strings.HasSuffix(prompt, "\n\n[trigger script vars] {\"count\":\"3\",\"topic\":\"nightly\"}") {
		t.Fatalf("ALL vars must append as trailing metadata, got %q", prompt)
	}

	fire := fireByTriggerID(t, store, "trig-gate-open")
	action := store.actions[fire.ID]
	if action == nil || !strings.Contains(string(action), `"proceed":true`) {
		t.Fatalf("fire audit must carry the open-gate outcome, got %s", action)
	}
	if fire.Status != "delivered" {
		t.Fatalf("open gate with agent success must deliver, got %q", fire.Status)
	}
}

// Script failure is fail-closed: a deterministic gate error produces a
// failed fire with the error in the audit and ZERO agent legs.
func TestScheduler_RoutineGateErrorFailsClosed(t *testing.T) {
	store := newMockSchedulerStore()
	trig := makeDueRoutineTrigger("trig-gate-err", "Prompt")
	trig.ScriptLanguage = "python"
	trig.ScriptHandler = "def handler(input):\n    raise RuntimeError('boom')\n"
	store.triggers = []*wf.TriggerRow{trig}

	agentd := newMockAgentd()
	agentd.errors["routine-gate"] = context.DeadlineExceeded

	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd,
		Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	if got := agentd.callNodeIDs(); len(got) != 1 || got[0] != "routine-gate" {
		t.Fatalf("gate error must dispatch only the gate, got %v", got)
	}
	if _, spawned := agentd.sentSpecs["routine-agent"]; spawned {
		t.Fatal("a failed gate script must spawn ZERO agent (LLM) legs")
	}
	fire := fireByTriggerID(t, store, "trig-gate-err")
	if fire.Status != "failed" {
		t.Fatalf("gate script failure must fail the fire (fail-closed), got %q", fire.Status)
	}
	action := store.actions[fire.ID]
	if action == nil || !strings.Contains(string(action), "gate") || !strings.Contains(string(action), "error") {
		t.Fatalf("fire audit must name the gate error, got %s", action)
	}
	if store.triggerFail["trig-gate-err"] != 1 {
		t.Fatalf("gate failure must drive auto-disable accounting, got %d", store.triggerFail["trig-gate-err"])
	}
}

// The agentd errorCode envelope shape (script_failed on a 200) is
// fail-closed too — not just transport errors.
func TestScheduler_RoutineGateErrorCodeFailsClosed(t *testing.T) {
	store := newMockSchedulerStore()
	trig := makeDueRoutineTrigger("trig-gate-errcode", "Prompt")
	trig.ScriptLanguage = "python"
	trig.ScriptHandler = "def handler(input):\n    1/0\n"
	store.triggers = []*wf.TriggerRow{trig}

	agentd := newMockAgentd()
	agentd.errCodes["routine-gate"] = "script_failed"
	agentd.outputs["routine-gate"] = json.RawMessage(`{"detail":"zero division"}`)

	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd,
		Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	if _, spawned := agentd.sentSpecs["routine-agent"]; spawned {
		t.Fatal("an errorCode gate failure must spawn ZERO agent (LLM) legs")
	}
	fire := fireByTriggerID(t, store, "trig-gate-errcode")
	if fire.Status != "failed" {
		t.Fatalf("errorCode gate failure must fail the fire, got %q", fire.Status)
	}
}

// A gate output that violates the {proceed: bool, vars:{str:str}} return
// schema is a script failure: fail-closed, self-teaching error.
func TestScheduler_RoutineGateBadReturnSchemaFailsClosed(t *testing.T) {
	store := newMockSchedulerStore()
	trig := makeDueRoutineTrigger("trig-gate-schema", "Prompt")
	trig.ScriptLanguage = "python"
	trig.ScriptHandler = "def handler(input):\n    return {'ok': 1}\n"
	store.triggers = []*wf.TriggerRow{trig}

	agentd := newMockAgentd()
	agentd.outputs["routine-gate"] = json.RawMessage(`{"ok":1}`)

	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd,
		Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	if _, spawned := agentd.sentSpecs["routine-agent"]; spawned {
		t.Fatal("a schema-violating gate return must spawn ZERO agent (LLM) legs")
	}
	fire := fireByTriggerID(t, store, "trig-gate-schema")
	if fire.Status != "failed" {
		t.Fatalf("schema violation must fail the fire, got %q", fire.Status)
	}
	action := store.actions[fire.ID]
	if action == nil || !strings.Contains(string(action), "proceed") {
		t.Fatalf("the audit error must name the violated field, got %s", action)
	}
}

// Script-only triggers (gate script, no prompt): the gate runs for its
// side effects and the fire delivers WITHOUT any LLM leg even when the
// gate opens.
func TestScheduler_RoutineGateScriptOnlyNoLLMLeg(t *testing.T) {
	store := newMockSchedulerStore()
	trig := makeDueRoutineTrigger("trig-gate-only", "")
	trig.ScriptLanguage = "python"
	trig.ScriptHandler = "def handler(input):\n    return {'proceed': True, 'vars': {}}\n"
	store.triggers = []*wf.TriggerRow{trig}

	agentd := newMockAgentd()
	agentd.outputs["routine-gate"] = json.RawMessage(`{"proceed":true,"vars":{"touched":"file"}}`)

	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd,
		Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	if got := agentd.callNodeIDs(); len(got) != 1 || got[0] != "routine-gate" {
		t.Fatalf("script-only trigger must never dispatch an agent leg, got %v", got)
	}
	fire := fireByTriggerID(t, store, "trig-gate-only")
	if fire.Status != "delivered" {
		t.Fatalf("script-only open gate must deliver, got %q", fire.Status)
	}
	if action := store.actions[fire.ID]; action == nil || !strings.Contains(string(action), `"proceed":true`) {
		t.Fatalf("gate outcome must be recorded, got %s", action)
	}
}

// Backward compat: a prompt-only trigger (no script columns) dispatches
// exactly as before — one agent leg, no gate node, no gate audit write.
func TestScheduler_RoutinePromptOnlyUnchanged(t *testing.T) {
	store := newMockSchedulerStore()
	store.triggers = []*wf.TriggerRow{makeDueRoutineTrigger("trig-legacy", "Summarize {{.input}}")}

	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"ok"}`)

	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd,
		Logger: noopLogger{}, TickInterval: 30 * time.Second}
	sched.tick(context.Background(), noopLogger{}, 10)

	if got := agentd.callNodeIDs(); len(got) != 1 || got[0] != "routine-agent" {
		t.Fatalf("prompt-only trigger must dispatch only the agent leg, got %v", got)
	}
	fire := fireByTriggerID(t, store, "trig-legacy")
	if fire.Status != "delivered" {
		t.Fatalf("legacy fire must deliver, got %q", fire.Status)
	}
	if action := store.actions[fire.ID]; action != nil {
		t.Fatalf("no gate script means no gate audit write, got %s", action)
	}
}
