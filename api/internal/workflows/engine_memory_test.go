// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// #1453: under captureMode=full the stored routine result is the
// agent-node output envelope ({response, session_id, tokens, prompt,
// parts} — cmd/workspace-agentd/workflow_execute.go). The prompt field
// embeds the FULL rendered prompt of its round, so injecting the stored
// envelope verbatim into {{.prevResult}} compounds the prompt round over
// round. These tests pin the injection to {response, tokens} only.

// recordingLogger captures Info messages so skip-decisions are asserted,
// never silently swallowed.
type recordingLogger struct {
	mu    sync.Mutex
	infos []string
}

func (l *recordingLogger) Info(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, msg)
}

func (l *recordingLogger) Error(_ error, _ string, _ ...any) {}

// skipCount returns how many Info messages were injection-skip notices
// (all carry the #1453 marker; the routine's own "routine executed"
// log is not one).
func (l *recordingLogger) skipCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, msg := range l.infos {
		if strings.Contains(msg, "#1453") {
			n++
		}
	}
	return n
}

// The compounding pin: a stored envelope whose prompt field carries a
// round-1 marker must contribute ONLY its response+tokens to the next
// round's prompt. Reverting the fix injects the envelope verbatim and
// fails every assertion.
func TestExecuteRoutine_MemoryLastResult_StripsEmbeddedPrompt(t *testing.T) {
	stored := `{"response":"round-1 response","session_id":"ses_round1","tokens":{"input":11,"output":7,"total":18},"prompt":"template ROUND1-PROMPT-MARKER prev=ROUND0-RESPONSE","parts":[{"type":"text","text":"round-1 response"}]}`

	store := newMockSchedulerStore()
	store.lastRoutineResult = json.RawMessage(stored)
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"round-2 output"}`)

	wsID := "ws-1"
	trigger := &wf.TriggerRow{
		ID: "trig-mem", WorkspaceID: &wsID,
		Prompt:     "Do the thing. Previous: {{.prevResult}}",
		MemoryMode: types.MemoryLastResult, MemoryMaxRuns: 1,
		CaptureMode: types.CaptureFull,
	}
	fire := &wf.TriggerFireRow{ID: "fire-mem", TriggerID: "trig-mem", InputEnvelope: json.RawMessage(`{}`)}

	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
	logger := &recordingLogger{}
	sched.executeRoutine(context.Background(), logger, trigger, fire)

	if store.statuses["fire-mem"] != "delivered" {
		t.Fatalf("expected delivered, got %s", store.statuses["fire-mem"])
	}

	var parsed map[string]any
	if err := json.Unmarshal(agentd.sentSpecs["routine-agent"], &parsed); err != nil {
		t.Fatalf("failed to parse sent agent spec: %v", err)
	}
	renderedPrompt, _ := parsed["prompt"].(string)

	want := `Do the thing. Previous: {"response":"round-1 response","tokens":{"input":11,"output":7,"total":18}}`
	if renderedPrompt != want {
		t.Errorf("prompt mismatch:\n got: %s\nwant: %s", renderedPrompt, want)
	}
	for _, banned := range []string{"ROUND1-PROMPT-MARKER", "ses_round1", "parts"} {
		if strings.Contains(renderedPrompt, banned) {
			t.Errorf("injected prev result leaks envelope field %q: %s", banned, renderedPrompt)
		}
	}
	if logger.skipCount() != 0 {
		t.Errorf("well-formed envelope must inject without skip logs, got %d", logger.skipCount())
	}
}

// Multi-run pin: each stored result is stripped BEFORE the join, so no
// round's embedded prompt rides along.
func TestExecuteRoutine_MemoryLastResult_MultiRun_StripsEachResult(t *testing.T) {
	r2 := `{"response":"round-2 response","tokens":{"input":2,"output":2,"total":4},"prompt":"template ROUND2-PROMPT-MARKER"}`
	r1 := `{"response":"round-1 response","tokens":{"input":1,"output":1,"total":2},"prompt":"template ROUND1-PROMPT-MARKER"}`

	store := newMockSchedulerStore()
	store.recentRoutineResults = []json.RawMessage{json.RawMessage(r2), json.RawMessage(r1)}
	agentd := newMockAgentd()
	agentd.outputs["routine-agent"] = json.RawMessage(`{"response":"round-3 output"}`)

	wsID := "ws-1"
	trigger := &wf.TriggerRow{
		ID: "trig-mem", WorkspaceID: &wsID,
		Prompt:     "History: {{.prevResult}}",
		MemoryMode: types.MemoryLastResult, MemoryMaxRuns: 2,
		CaptureMode: types.CaptureFull,
	}
	fire := &wf.TriggerFireRow{ID: "fire-mem", TriggerID: "trig-mem", InputEnvelope: json.RawMessage(`{}`)}

	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
	logger := &recordingLogger{}
	sched.executeRoutine(context.Background(), logger, trigger, fire)

	var parsed map[string]any
	if err := json.Unmarshal(agentd.sentSpecs["routine-agent"], &parsed); err != nil {
		t.Fatalf("failed to parse sent agent spec: %v", err)
	}
	renderedPrompt, _ := parsed["prompt"].(string)

	want := `History: {"response":"round-2 response","tokens":{"input":2,"output":2,"total":4}}` + "\n---\n" +
		`{"response":"round-1 response","tokens":{"input":1,"output":1,"total":2}}`
	if renderedPrompt != want {
		t.Errorf("prompt mismatch:\n got: %s\nwant: %s", renderedPrompt, want)
	}
	for _, banned := range []string{"ROUND1-PROMPT-MARKER", "ROUND2-PROMPT-MARKER"} {
		if strings.Contains(renderedPrompt, banned) {
			t.Errorf("joined prev results leak embedded prompt %q: %s", banned, renderedPrompt)
		}
	}
	if logger.skipCount() != 0 {
		t.Errorf("well-formed envelopes must inject without skip logs, got %d", logger.skipCount())
	}
}

// Envelope variants: exact injected payload for well-formed envelopes.
// Response content rides intact (structured-output responses are
// arbitrary JSON); the injection is the compact encoding (json.Marshal
// compacts RawMessage values — deterministic across fires).
func TestExecuteRoutine_MemoryLastResult_EnvelopeVariants(t *testing.T) {
	tests := []struct {
		name       string
		stored     string
		wantInject string
	}{
		{"well formed", `{"response":"done","session_id":"ses_1","tokens":{"input":1,"output":2,"total":3},"prompt":"old","parts":[]}`, `{"response":"done","tokens":{"input":1,"output":2,"total":3}}`},
		{"missing tokens", `{"response":"done","prompt":"old","session_id":"ses_1"}`, `{"response":"done"}`},
		{"tokens null", `{"response":"done","tokens":null}`, `{"response":"done","tokens":null}`},
		{"empty response", `{"response":"","tokens":{"input":0,"output":0,"total":0}}`, `{"response":"","tokens":{"input":0,"output":0,"total":0}}`},
		{"null response", `{"response":null,"tokens":{"input":1}}`, `{"response":null,"tokens":{"input":1}}`},
		{"structured output object keeps content", `{"response":{"topic":  "ship"},"tokens":{"input":9}}`, `{"response":{"topic":"ship"},"tokens":{"input":9}}`},
		{"numeric response", `{"response":42}`, `{"response":42}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rendered, status := renderSinglePrev(t, tt.stored)
			if status != "delivered" {
				t.Fatalf("expected delivered, got %s", status)
			}
			want := "P<<" + tt.wantInject + ">>P"
			if rendered != want {
				t.Errorf("injection mismatch:\n got: %s\nwant: %s", rendered, want)
			}
		})
	}
}

// Unrecognized stored shapes (corruption, foreign rows — no real
// delivered routine result has ever had a non-envelope shape, #688):
// fail safe — nothing unverifiable enters the prompt. The placeholder
// stays unreplaced (same as the no-stored-result case), the fire still
// runs, and the skip is logged, never silent.
func TestExecuteRoutine_MemoryLastResult_UnrecognizedShape(t *testing.T) {
	tests := []struct {
		name   string
		stored string
	}{
		{"not json", `the previous result as plain text`},
		{"json string", `"previous result string"`},
		{"json array", `[{"response":"x"}]`},
		{"json null", `null`},
		{"json number", `42`},
		{"empty object", `{}`},
		{"object without response", `{"error":"boom","code":"script_failed"}`},
		{"trailing garbage", `{"response":"ok"} trailing`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMockSchedulerStore()
			store.lastRoutineResult = json.RawMessage(tt.stored)
			agentd := newMockAgentd()

			wsID := "ws-1"
			trigger := &wf.TriggerRow{
				ID: "trig-mem", WorkspaceID: &wsID,
				Prompt:     "P{{.prevResult}}P",
				MemoryMode: types.MemoryLastResult, MemoryMaxRuns: 1,
				CaptureMode: types.CaptureFull,
			}
			fire := &wf.TriggerFireRow{ID: "fire-mem", TriggerID: "trig-mem", InputEnvelope: json.RawMessage(`{}`)}

			sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
			logger := &recordingLogger{}
			sched.executeRoutine(context.Background(), logger, trigger, fire)

			if store.statuses["fire-mem"] != "delivered" {
				t.Fatalf("unrecognized prev shape must not fail the fire, got %s", store.statuses["fire-mem"])
			}
			var parsed map[string]any
			if err := json.Unmarshal(agentd.sentSpecs["routine-agent"], &parsed); err != nil {
				t.Fatalf("failed to parse sent agent spec: %v", err)
			}
			rendered, _ := parsed["prompt"].(string)
			if rendered != "P{{.prevResult}}P" {
				t.Errorf("unrecognized shape must leave the placeholder unreplaced, got: %s", rendered)
			}
			if logger.skipCount() == 0 {
				t.Errorf("skipping injection must be logged, got zero Info messages")
			}
		})
	}
}

// ReplaceAll semantics preserved: every occurrence of the placeholder
// is replaced with the stripped payload.
func TestExecuteRoutine_MemoryLastResult_MultiplePlaceholders(t *testing.T) {
	store := newMockSchedulerStore()
	store.lastRoutineResult = json.RawMessage(`{"response":"resp-1","tokens":{"input":1,"output":1,"total":2},"prompt":"PROMPT-MARKER"}`)
	agentd := newMockAgentd()

	wsID := "ws-1"
	trigger := &wf.TriggerRow{
		ID: "trig-mem", WorkspaceID: &wsID,
		Prompt:     "First {{.prevResult}} then {{.prevResult}} last",
		MemoryMode: types.MemoryLastResult, MemoryMaxRuns: 1,
		CaptureMode: types.CaptureFull,
	}
	fire := &wf.TriggerFireRow{ID: "fire-mem", TriggerID: "trig-mem", InputEnvelope: json.RawMessage(`{}`)}

	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	var parsed map[string]any
	if err := json.Unmarshal(agentd.sentSpecs["routine-agent"], &parsed); err != nil {
		t.Fatalf("failed to parse sent agent spec: %v", err)
	}
	rendered, _ := parsed["prompt"].(string)
	inj := `{"response":"resp-1","tokens":{"input":1,"output":1,"total":2}}`
	want := "First " + inj + " then " + inj + " last"
	if rendered != want {
		t.Errorf("prompt mismatch:\n got: %s\nwant: %s", rendered, want)
	}
	if strings.Contains(rendered, "PROMPT-MARKER") {
		t.Errorf("stripped envelope must not leak the embedded prompt: %s", rendered)
	}
}

// Multi-run mixed shapes: unrecognized entries are dropped from the
// join (logged); recognized ones still inject. All-unrecognized leaves
// the placeholder unreplaced.
func TestExecuteRoutine_MemoryLastResult_MultiRun_MixedShapes(t *testing.T) {
	t.Run("one envelope one garbage", func(t *testing.T) {
		store := newMockSchedulerStore()
		store.recentRoutineResults = []json.RawMessage{
			json.RawMessage(`{"response":"good response","tokens":{"input":1}}`),
			json.RawMessage(`garbage row`),
		}
		agentd := newMockAgentd()
		wsID := "ws-1"
		trigger := &wf.TriggerRow{
			ID: "trig-mem", WorkspaceID: &wsID,
			Prompt:     "H: {{.prevResult}}",
			MemoryMode: types.MemoryLastResult, MemoryMaxRuns: 2,
			CaptureMode: types.CaptureFull,
		}
		fire := &wf.TriggerFireRow{ID: "fire-mem", TriggerID: "trig-mem", InputEnvelope: json.RawMessage(`{}`)}
		sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
		logger := &recordingLogger{}
		sched.executeRoutine(context.Background(), logger, trigger, fire)

		var parsed map[string]any
		if err := json.Unmarshal(agentd.sentSpecs["routine-agent"], &parsed); err != nil {
			t.Fatalf("failed to parse sent agent spec: %v", err)
		}
		rendered, _ := parsed["prompt"].(string)
		if rendered != `H: {"response":"good response","tokens":{"input":1}}` {
			t.Errorf("expected only the recognized result injected, got: %s", rendered)
		}
		if strings.Contains(rendered, "garbage") {
			t.Errorf("unrecognized entry must not enter the prompt: %s", rendered)
		}
		if logger.skipCount() == 0 {
			t.Errorf("dropping an entry from the join must be logged")
		}
	})

	t.Run("all garbage", func(t *testing.T) {
		store := newMockSchedulerStore()
		store.recentRoutineResults = []json.RawMessage{
			json.RawMessage(`garbage one`),
			json.RawMessage(`{"error":"no response here"}`),
		}
		agentd := newMockAgentd()
		wsID := "ws-1"
		trigger := &wf.TriggerRow{
			ID: "trig-mem", WorkspaceID: &wsID,
			Prompt:     "H: {{.prevResult}}",
			MemoryMode: types.MemoryLastResult, MemoryMaxRuns: 2,
			CaptureMode: types.CaptureFull,
		}
		fire := &wf.TriggerFireRow{ID: "fire-mem", TriggerID: "trig-mem", InputEnvelope: json.RawMessage(`{}`)}
		sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
		logger := &recordingLogger{}
		sched.executeRoutine(context.Background(), logger, trigger, fire)

		var parsed map[string]any
		if err := json.Unmarshal(agentd.sentSpecs["routine-agent"], &parsed); err != nil {
			t.Fatalf("failed to parse sent agent spec: %v", err)
		}
		rendered, _ := parsed["prompt"].(string)
		if rendered != "H: {{.prevResult}}" {
			t.Errorf("all-unrecognized must leave the placeholder unreplaced, got: %s", rendered)
		}
		if logger.skipCount() == 0 {
			t.Errorf("skipping injection must be logged")
		}
	})
}

// renderSinglePrev runs one last_result fire (maxRuns=1) with the given
// stored result and template "P<<{{.prevResult}}>>P", returning the
// rendered prompt and fire status.
func renderSinglePrev(t *testing.T, stored string) (string, string) {
	t.Helper()
	store := newMockSchedulerStore()
	store.lastRoutineResult = json.RawMessage(stored)
	agentd := newMockAgentd()

	wsID := "ws-1"
	trigger := &wf.TriggerRow{
		ID: "trig-mem", WorkspaceID: &wsID,
		Prompt:     "P<<{{.prevResult}}>>P",
		MemoryMode: types.MemoryLastResult, MemoryMaxRuns: 1,
		CaptureMode: types.CaptureFull,
	}
	fire := &wf.TriggerFireRow{ID: "fire-mem", TriggerID: "trig-mem", InputEnvelope: json.RawMessage(`{}`)}

	sched := &Scheduler{Store: store, Activator: &mockActivator{}, AgentdClient: agentd, Logger: noopLogger{}}
	sched.executeRoutine(context.Background(), noopLogger{}, trigger, fire)

	var parsed map[string]any
	if err := json.Unmarshal(agentd.sentSpecs["routine-agent"], &parsed); err != nil {
		t.Fatalf("failed to parse sent agent spec: %v", err)
	}
	rendered, _ := parsed["prompt"].(string)
	return rendered, store.statuses["fire-mem"]
}
