package exprlang

import (
	"encoding/json"
	"testing"
)

func TestCompileCondition_ValidAgainstSchema(t *testing.T) {
	tests := []struct {
		name   string
		schema map[string]any
		expr   string
	}{
		{
			name: "boolean field equality",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"skipped":   map[string]any{"type": "boolean"},
					"meetingId": map[string]any{"type": "string"},
				},
			},
			expr: "input.Skipped == true",
		},
		{
			name: "string field comparison",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"error_code": map[string]any{"type": "string"},
				},
			},
			expr: "input.ErrorCode == 'transient'",
		},
		{
			name: "numeric comparison",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"count": map[string]any{"type": "integer"},
				},
			},
			expr: "input.Count > 5",
		},
		{
			name: "nested object access",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"issue": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"number": map[string]any{"type": "integer"},
							"title":  map[string]any{"type": "string"},
						},
					},
				},
			},
			expr: "input.Issue.Number > 100 and input.Issue.Title != ''",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schemaJSON, _ := json.Marshal(tt.schema)
			err := CompileCondition(tt.expr, schemaJSON)
			if err != nil {
				t.Fatalf("expected expression %q to compile against schema, got error: %v", tt.expr, err)
			}
		})
	}
}

func TestCompileCondition_TypeErrorOnMissingField(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"skipped":   map[string]any{"type": "boolean"},
			"meetingId": map[string]any{"type": "string"},
		},
	}
	schemaJSON, _ := json.Marshal(schema)

	err := CompileCondition("input.Nonexistent == true", schemaJSON)
	if err == nil {
		t.Fatal("expected type error for missing field 'Nonexistent', got nil")
	}
}

func TestCompileCondition_TypeErrorOnWrongFieldType(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"count": map[string]any{"type": "integer"},
		},
	}
	schemaJSON, _ := json.Marshal(schema)

	tests := []struct {
		name string
		expr string
	}{
		{"string method on integer field", `input.Count.Contains("x")`},
		{"boolean operation on integer field", "input.Count && true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CompileCondition(tt.expr, schemaJSON)
			if err == nil {
				t.Fatalf("expected type error for expression %q, got nil", tt.expr)
			}
		})
	}
}

func TestCompileCondition_SyntaxError(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{}}
	schemaJSON, _ := json.Marshal(schema)

	err := CompileCondition("input. == ", schemaJSON)
	if err == nil {
		t.Fatal("expected syntax error, got nil")
	}
}

func TestSchemaToEnv(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"skipped":   map[string]any{"type": "boolean"},
			"meetingId": map[string]any{"type": "string"},
			"count":     map[string]any{"type": "integer"},
			"nested": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"flag": map[string]any{"type": "boolean"},
				},
			},
		},
	}
	schemaJSON, _ := json.Marshal(schema)

	env, err := SchemaToEnv(schemaJSON)
	if err != nil {
		t.Fatalf("SchemaToEnv error: %v", err)
	}
	if env == nil {
		t.Fatal("expected non-nil env")
	}
	if _, ok := env["input"]; !ok {
		t.Fatal("expected 'input' key in env")
	}
}

func TestCompileCondition_NestedMissingField(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"issue": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"number": map[string]any{"type": "integer"},
				},
			},
		},
	}
	schemaJSON, _ := json.Marshal(schema)

	err := CompileCondition("input.Issue.Nonexistent > 5", schemaJSON)
	if err == nil {
		t.Fatal("expected type error for nested missing field, got nil")
	}
}
