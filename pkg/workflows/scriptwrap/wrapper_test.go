package scriptwrap

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestExecutePython_RoundTrip(t *testing.T) {
	handler := `def handler(input):
    return {"meetingId": input["meetingId"], "processed": True, "count": len(input.get("items", []))}
`
	input := map[string]any{"meetingId": "mtg_123", "items": []any{"a", "b", "c"}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	output, stderr, exitCode, err := Execute(ctx, LanguagePython, handler, input)
	if err != nil {
		t.Fatalf("Execute failed: %v\nstderr: %s", err, stderr)
	}
	if exitCode != 0 {
		t.Fatalf("non-zero exit code %d\nstderr: %s", exitCode, stderr)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output as JSON: %v\noutput: %s", err, string(output))
	}
	if result["meetingId"] != "mtg_123" {
		t.Errorf("expected meetingId=mtg_123, got %v", result["meetingId"])
	}
	if result["processed"] != true {
		t.Errorf("expected processed=true, got %v", result["processed"])
	}
	if result["count"] != float64(3) {
		t.Errorf("expected count=3, got %v", result["count"])
	}
}

func TestExecuteNode_RoundTrip(t *testing.T) {
	handler := `function handler(input) {
    return { meetingId: input.meetingId, processed: true, count: (input.items || []).length };
}
module.exports = { handler };
`
	input := map[string]any{"meetingId": "mtg_456", "items": []any{"x", "y"}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	output, stderr, exitCode, err := Execute(ctx, LanguageNode, handler, input)
	if err != nil {
		t.Fatalf("Execute failed: %v\nstderr: %s", err, stderr)
	}
	if exitCode != 0 {
		t.Fatalf("non-zero exit code %d\nstderr: %s", exitCode, stderr)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output as JSON: %v\noutput: %s", err, string(output))
	}
	if result["meetingId"] != "mtg_456" {
		t.Errorf("expected meetingId=mtg_456, got %v", result["meetingId"])
	}
	if result["processed"] != true {
		t.Errorf("expected processed=true, got %v", result["processed"])
	}
	if result["count"] != float64(2) {
		t.Errorf("expected count=2, got %v", result["count"])
	}
}

func TestExecutePython_Exception(t *testing.T) {
	handler := `def handler(input):
    raise ValueError("intentional error for spike test")
`
	input := map[string]any{"x": 1}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	output, stderr, exitCode, err := Execute(ctx, LanguagePython, handler, input)

	if exitCode == 0 {
		t.Fatalf("expected non-zero exit code, got 0\noutput: %s", string(output))
	}
	if err == nil {
		t.Fatal("expected error on exception, got nil")
	}
	if !strings.Contains(stderr, "intentional error") {
		t.Errorf("expected stderr to contain exception message, got: %s", stderr)
	}
}

// TestExecutePython_NonDictReturn verifies that the scriptwrap layer does NOT
// enforce dict returns — a handler returning a JSON-serializable non-dict
// succeeds, and the caller is responsible for validating the shape. This is a
// documented contract: Execute returns json.RawMessage; US-64.7 must enforce
// dict shape on top.
func TestExecutePython_NonDictReturn(t *testing.T) {
	handler := `def handler(input):
    return "not a dict"
`
	input := map[string]any{"x": 1}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	output, _, exitCode, err := Execute(ctx, LanguagePython, handler, input)
	if err != nil {
		t.Fatalf("Execute returned unexpected error: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("expected exit 0 for valid JSON serialization, got %d", exitCode)
	}
	// The wrapper successfully serialized a JSON string. Validate the CALLER
	// catches the shape mismatch by unmarshaling into a map.
	var result map[string]any
	if unmarshalErr := json.Unmarshal(output, &result); unmarshalErr == nil {
		t.Fatalf("expected caller's json.Unmarshal to fail for non-dict, got valid map: %v", result)
	}
}

// TestExecutePython_BuiltinImport confirms the handler can use Python builtins
// (json, sys, os). Workspace-module import (a real A3 concern — modules
// installed under the workspace via pip/mise) is not exercised here; it's an
// integration concern deferred to US-64.7 against a real workspace.
func TestExecutePython_BuiltinImport(t *testing.T) {
	handler := `import json
def handler(input):
    return {"echoed": input["value"], "hasJsonModule": True}
`
	input := map[string]any{"value": "hello"}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	output, stderr, exitCode, err := Execute(ctx, LanguagePython, handler, input)
	if err != nil {
		t.Fatalf("Execute failed: %v\nstderr: %s", err, stderr)
	}
	if exitCode != 0 {
		t.Fatalf("non-zero exit code %d\nstderr: %s", exitCode, stderr)
	}

	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}
	if result["echoed"] != "hello" {
		t.Errorf("expected echoed=hello, got %v", result["echoed"])
	}
}

func TestExecute_ContextCancellation(t *testing.T) {
	handler := `import time
def handler(input):
    time.sleep(30)
    return {"done": True}
`
	input := map[string]any{"x": 1}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, _, _, err := Execute(ctx, LanguagePython, handler, input)
	if err == nil {
		t.Fatal("expected timeout/cancel error, got nil")
	}
}

func TestExecute_UnsupportedLanguage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _, _, err := Execute(ctx, Language("perl"), "sub handler { return {} }", map[string]any{})
	if err == nil {
		t.Fatal("expected error for unsupported language, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported language") {
		t.Errorf("expected 'unsupported language' in error, got: %v", err)
	}
}

func TestExecute_InputMarshalFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Channel cannot be JSON-marshaled.
	_, _, _, err := Execute(ctx, LanguagePython, "def handler(i): return {}", make(chan int))
	if err == nil {
		t.Fatal("expected marshal error for channel input, got nil")
	}
}
