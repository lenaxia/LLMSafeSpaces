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

	output, stderr, exitCode, err := Execute(ctx, "python", handler, input)
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

	output, stderr, exitCode, err := Execute(ctx, "node", handler, input)
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

	output, stderr, exitCode, err := Execute(ctx, "python", handler, input)

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

func TestExecutePython_NonDictReturn(t *testing.T) {
	handler := `def handler(input):
    return "not a dict"
`
	input := map[string]any{"x": 1}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	output, _, _, _ := Execute(ctx, "python", handler, input)

	var result map[string]any
	err := json.Unmarshal(output, &result)
	if err == nil {
		t.Fatalf("expected JSON parse failure for string return, got valid map: %v", result)
	}
}

func TestExecutePython_ImportWorkspaceModule(t *testing.T) {
	handler := `import json
def handler(input):
    return {"echoed": input["value"], "hasJsonModule": True}
`
	input := map[string]any{"value": "hello"}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	output, stderr, exitCode, err := Execute(ctx, "python", handler, input)
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

	_, _, _, err := Execute(ctx, "python", handler, input)
	if err == nil {
		t.Fatal("expected timeout/cancel error, got nil")
	}
}
