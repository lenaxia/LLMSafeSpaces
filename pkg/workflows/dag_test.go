package workflows

import (
	"encoding/json"
	"testing"
)

func mustJSON(t *testing.T, v string) json.RawMessage {
	t.Helper()
	return json.RawMessage(v)
}

func simpleSpec(nodes []SpecNode, edges ...SpecEdge) *Spec {
	return &Spec{Nodes: nodes, Edges: edges}
}

func TestValidateSpec_ValidLinear(t *testing.T) {
	spec := simpleSpec(
		[]SpecNode{
			{ID: "start", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"def handler(i): return {}"}`)},
			{ID: "end", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"def handler(i): return {}"}`)},
		},
		SpecEdge{Source: "start", Target: "end"},
	)
	if errs := ValidateSpec(spec, nil, DefaultsBlock{}); len(errs) != 0 {
		t.Fatalf("expected no errors, got: %v", errs)
	}
}

func TestValidateSpec_ValidWithCondition(t *testing.T) {
	condData, _ := json.Marshal(ConditionNodeData{
		Conditions: []ConditionCase{
			{ID: "skip", Expression: "input.Skipped == true"},
		},
	})
	spec := simpleSpec(
		[]SpecNode{
			{ID: "start", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
			{ID: "choice", Type: "condition", Data: condData},
			{ID: "skip-path", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
			{ID: "normal-path", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
		},
		SpecEdge{Source: "start", Target: "choice"},
		SpecEdge{Source: "choice", Target: "skip-path", SourceHandle: "skip"},
		SpecEdge{Source: "choice", Target: "normal-path", SourceHandle: "otherwise"},
	)
	if errs := ValidateSpec(spec, nil, DefaultsBlock{}); len(errs) != 0 {
		t.Fatalf("expected no errors, got: %v", errs)
	}
}

func TestValidateSpec_CycleDetected(t *testing.T) {
	spec := simpleSpec(
		[]SpecNode{
			{ID: "a", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
			{ID: "b", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
		},
		SpecEdge{Source: "a", Target: "b"},
		SpecEdge{Source: "b", Target: "a"},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	if len(errs) == 0 {
		t.Fatal("expected cycle error, got none")
	}
	if errs[0].Code != "cycle" {
		t.Errorf("expected error code 'cycle', got %q", errs[0].Code)
	}
}

func TestValidateSpec_DanglingEdgeSource(t *testing.T) {
	spec := simpleSpec(
		[]SpecNode{
			{ID: "a", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
		},
		SpecEdge{Source: "nonexistent", Target: "a"},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	if len(errs) == 0 {
		t.Fatal("expected dangling-ref error")
	}
}

func TestValidateSpec_DanglingEdgeTarget(t *testing.T) {
	spec := simpleSpec(
		[]SpecNode{
			{ID: "a", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
		},
		SpecEdge{Source: "a", Target: "nonexistent"},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	if len(errs) == 0 {
		t.Fatal("expected dangling-ref error")
	}
}

func TestValidateSpec_UnreachableNode(t *testing.T) {
	spec := simpleSpec(
		[]SpecNode{
			{ID: "start", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
			{ID: "orphan", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
		},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	if len(errs) == 0 {
		t.Fatal("expected unreachable-node error")
	}
}

func TestValidateSpec_MultipleStarts(t *testing.T) {
	spec := simpleSpec(
		[]SpecNode{
			{ID: "a", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
			{ID: "b", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
			{ID: "c", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
		},
		SpecEdge{Source: "a", Target: "c"},
		SpecEdge{Source: "b", Target: "c"},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	found := false
	for _, e := range errs {
		if e.Code == "multiple_starts" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected multiple_starts error, got: %v", errs)
	}
}

func TestValidateSpec_NoNodes(t *testing.T) {
	spec := &Spec{}
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	if len(errs) == 0 {
		t.Fatal("expected error for empty spec")
	}
}

func TestValidateSpec_DuplicateNodeIDs(t *testing.T) {
	spec := simpleSpec(
		[]SpecNode{
			{ID: "dup", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
			{ID: "dup", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
		},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	found := false
	for _, e := range errs {
		if e.Code == "duplicate_node_id" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected duplicate_node_id error, got: %v", errs)
	}
}

func TestValidateSpec_InvalidNodeType(t *testing.T) {
	spec := simpleSpec(
		[]SpecNode{
			{ID: "a", Type: "transform", Data: mustJSON(t, `{}`)},
		},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	found := false
	for _, e := range errs {
		if e.Code == "invalid_node_type" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected invalid_node_type error, got: %v", errs)
	}
}

func TestValidateSpec_ConditionMissingOtherwiseEdge(t *testing.T) {
	condData, _ := json.Marshal(ConditionNodeData{
		Conditions: []ConditionCase{
			{ID: "skip", Expression: "input.Skipped == true"},
		},
	})
	spec := simpleSpec(
		[]SpecNode{
			{ID: "start", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
			{ID: "choice", Type: "condition", Data: condData},
			{ID: "skip-path", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
		},
		SpecEdge{Source: "start", Target: "choice"},
		SpecEdge{Source: "choice", Target: "skip-path", SourceHandle: "skip"},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	found := false
	for _, e := range errs {
		if e.Code == "missing_branch_edge" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected missing_branch_edge for 'otherwise', got: %v", errs)
	}
}

func TestValidateSpec_ConditionMissingBranchEdge(t *testing.T) {
	condData, _ := json.Marshal(ConditionNodeData{
		Conditions: []ConditionCase{
			{ID: "skip", Expression: "input.Skipped == true"},
			{ID: "retry", Expression: "input.ErrorCode == 'transient'"},
		},
	})
	spec := simpleSpec(
		[]SpecNode{
			{ID: "start", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
			{ID: "choice", Type: "condition", Data: condData},
			{ID: "skip-path", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
			{ID: "else-path", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
		},
		SpecEdge{Source: "start", Target: "choice"},
		SpecEdge{Source: "choice", Target: "skip-path", SourceHandle: "skip"},
		SpecEdge{Source: "choice", Target: "else-path", SourceHandle: "otherwise"},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	found := false
	for _, e := range errs {
		if e.Code == "missing_branch_edge" && e.NodeID == "choice" {
			if contains(e.Detail, "retry") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected missing_branch_edge for 'retry', got: %v", errs)
	}
}

func TestValidateSpec_ConditionExprTypeCheck(t *testing.T) {
	// Condition expression references a field that won't exist in the
	// predecessor's outputSchema — but without a schema provided, the
	// type-check is skipped (schemas are optional). With a schema, it should fail.
	condData, _ := json.Marshal(ConditionNodeData{
		Conditions: []ConditionCase{
			{ID: "skip", Expression: "input.NonExistentField == true"},
		},
	})
	spec := simpleSpec(
		[]SpecNode{
			{ID: "start", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
			{ID: "choice", Type: "condition", Data: condData},
			{ID: "skip-path", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
			{ID: "else-path", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
		},
		SpecEdge{Source: "start", Target: "choice"},
		SpecEdge{Source: "choice", Target: "skip-path", SourceHandle: "skip"},
		SpecEdge{Source: "choice", Target: "else-path", SourceHandle: "otherwise"},
	)

	// With predecessor schema: should fail type-check.
	predSchema := json.RawMessage(`{"type":"object","properties":{"skipped":{"type":"boolean"}}}`)
	errs := ValidateSpec(spec, map[string]json.RawMessage{"start": predSchema}, DefaultsBlock{})
	found := false
	for _, e := range errs {
		if e.Code == "expr_type_error" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected expr_type_error for missing field, got: %v", errs)
	}
}

func TestValidateSpec_DefaultsMerging(t *testing.T) {
	defAttempts := 3
	defaults := DefaultsBlock{MaxAttempts: &defAttempts, Timeout: "5m"}

	spec := simpleSpec(
		[]SpecNode{
			{ID: "a", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},                 // no maxAttempts → gets default
			{ID: "b", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`), MaxAttempts: 1}, // explicit → keeps
		},
		SpecEdge{Source: "a", Target: "b"},
	)
	errs := ValidateSpec(spec, nil, defaults)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got: %v", errs)
	}
	if spec.Nodes[0].MaxAttempts != 3 {
		t.Errorf("expected node 'a' to get default maxAttempts=3, got %d", spec.Nodes[0].MaxAttempts)
	}
	if spec.Nodes[1].MaxAttempts != 1 {
		t.Errorf("expected node 'b' to keep maxAttempts=1, got %d", spec.Nodes[1].MaxAttempts)
	}
	if spec.Nodes[0].Timeout != "5m" {
		t.Errorf("expected node 'a' to get default timeout=5m, got %q", spec.Nodes[0].Timeout)
	}
}

func TestValidateSpec_ScriptNodeMissingHandler(t *testing.T) {
	spec := simpleSpec(
		[]SpecNode{
			{ID: "a", Type: "script", Data: mustJSON(t, `{"language":"python"}`)}, // missing handler
		},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	found := false
	for _, e := range errs {
		if e.Code == "invalid_node_data" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected invalid_node_data for missing handler, got: %v", errs)
	}
}

func TestValidateSpec_AgentNodeMissingPrompt(t *testing.T) {
	spec := simpleSpec(
		[]SpecNode{
			{ID: "a", Type: "agent", Data: mustJSON(t, `{"agent":"build"}`)}, // missing prompt
		},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	found := false
	for _, e := range errs {
		if e.Code == "invalid_node_data" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected invalid_node_data for missing prompt, got: %v", errs)
	}
}

func TestValidateSpec_HTTPNodeMissingURL(t *testing.T) {
	spec := simpleSpec(
		[]SpecNode{
			{ID: "a", Type: "http", Data: mustJSON(t, `{"method":"GET"}`)}, // missing url
		},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	found := false
	for _, e := range errs {
		if e.Code == "invalid_node_data" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected invalid_node_data for missing url, got: %v", errs)
	}
}

func TestValidateSpec_SelfLoop(t *testing.T) {
	spec := simpleSpec(
		[]SpecNode{
			{ID: "a", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
		},
		SpecEdge{Source: "a", Target: "a"},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	found := false
	for _, e := range errs {
		if e.Code == "cycle" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected cycle error for self-loop, got: %v", errs)
	}
}

func TestParseSpec_ValidJSON(t *testing.T) {
	raw := json.RawMessage(`{
		"nodes": [
			{"id": "a", "type": "script", "data": {"language": "python", "handler": "x"}},
			{"id": "b", "type": "script", "data": {"language": "python", "handler": "y"}}
		],
		"edges": [{"source": "a", "target": "b"}]
	}`)
	spec, err := ParseSpec(raw)
	if err != nil {
		t.Fatalf("ParseSpec error: %v", err)
	}
	if len(spec.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(spec.Nodes))
	}
	if spec.Nodes[0].ID != "a" {
		t.Errorf("expected first node id 'a', got %q", spec.Nodes[0].ID)
	}
	if len(spec.Edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(spec.Edges))
	}
}

func TestParseSpec_InvalidJSON(t *testing.T) {
	_, err := ParseSpec(json.RawMessage(`{invalid json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestValidateSpec_ConditionExprTypeCheckHappy(t *testing.T) {
	// Expression references fields that DO exist in predecessor schema → no error.
	condData, _ := json.Marshal(ConditionNodeData{
		Conditions: []ConditionCase{
			{ID: "skip", Expression: "input.Skipped == true"},
		},
	})
	spec := simpleSpec(
		[]SpecNode{
			{ID: "start", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
			{ID: "choice", Type: "condition", Data: condData},
			{ID: "skip-path", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
			{ID: "else-path", Type: "script", Data: mustJSON(t, `{"language":"python","handler":"x"}`)},
		},
		SpecEdge{Source: "start", Target: "choice"},
		SpecEdge{Source: "choice", Target: "skip-path", SourceHandle: "skip"},
		SpecEdge{Source: "choice", Target: "else-path", SourceHandle: "otherwise"},
	)

	predSchema := json.RawMessage(`{"type":"object","properties":{"skipped":{"type":"boolean"},"meetingId":{"type":"string"}}}`)
	errs := ValidateSpec(spec, map[string]json.RawMessage{"start": predSchema}, DefaultsBlock{})
	for _, e := range errs {
		if e.Code == "expr_type_error" {
			t.Fatalf("expected no expr_type_error for valid field reference, got: %v", e)
		}
	}
}

func TestValidateSpec_ScriptNodeMissingLanguage(t *testing.T) {
	spec := simpleSpec(
		[]SpecNode{
			{ID: "a", Type: "script", Data: mustJSON(t, `{"handler":"x"}`)}, // missing language
		},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	hasCode(t, errs, "invalid_node_data")
}

func TestValidateSpec_ScriptNodeMalformedData(t *testing.T) {
	spec := simpleSpec(
		[]SpecNode{
			{ID: "a", Type: "script", Data: mustJSON(t, `"not an object"`)},
		},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	hasCode(t, errs, "invalid_node_data")
}

func TestValidateSpec_AgentNodeMalformedData(t *testing.T) {
	spec := simpleSpec(
		[]SpecNode{
			{ID: "a", Type: "agent", Data: mustJSON(t, `42`)},
		},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	hasCode(t, errs, "invalid_node_data")
}

func TestValidateSpec_HTTPNodeMalformedData(t *testing.T) {
	spec := simpleSpec(
		[]SpecNode{
			{ID: "a", Type: "http", Data: mustJSON(t, `"string not object"`)},
		},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	hasCode(t, errs, "invalid_node_data")
}

func TestValidateSpec_ConditionNodeMalformedData(t *testing.T) {
	spec := simpleSpec(
		[]SpecNode{
			{ID: "a", Type: "condition", Data: mustJSON(t, `null`)},
		},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	hasCode(t, errs, "invalid_node_data")
}

func TestValidateSpec_ConditionCaseMissingID(t *testing.T) {
	condData, _ := json.Marshal(ConditionNodeData{
		Conditions: []ConditionCase{
			{ID: "", Expression: "input.Skipped == true"},
		},
	})
	spec := simpleSpec(
		[]SpecNode{
			{ID: "a", Type: "condition", Data: condData},
		},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	hasCode(t, errs, "invalid_node_data")
}

func TestValidateSpec_ConditionCaseMissingExpression(t *testing.T) {
	condData, _ := json.Marshal(ConditionNodeData{
		Conditions: []ConditionCase{
			{ID: "skip", Expression: ""},
		},
	})
	spec := simpleSpec(
		[]SpecNode{
			{ID: "a", Type: "condition", Data: condData},
		},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	hasCode(t, errs, "invalid_node_data")
}

func TestValidateSpec_ConditionEmptyConditions(t *testing.T) {
	condData, _ := json.Marshal(ConditionNodeData{Conditions: []ConditionCase{}})
	spec := simpleSpec(
		[]SpecNode{
			{ID: "a", Type: "condition", Data: condData},
		},
	)
	errs := ValidateSpec(spec, nil, DefaultsBlock{})
	hasCode(t, errs, "invalid_node_data")
}

func hasCode(t *testing.T, errs []ValidationError, code string) {
	t.Helper()
	for _, e := range errs {
		if e.Code == code {
			return
		}
	}
	t.Fatalf("expected error with code %q, got: %+v", code, errs)
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
