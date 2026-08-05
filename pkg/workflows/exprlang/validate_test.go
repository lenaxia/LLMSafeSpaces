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

func TestSchemaToEnv_MalformedJSON(t *testing.T) {
	err := CompileCondition("input.Skipped == true", []byte("not valid json"))
	if err == nil {
		t.Fatal("expected error for malformed JSON schema, got nil")
	}
}

func TestCompileCondition_DuplicateFieldNameCollision(t *testing.T) {
	// Both foo_bar and fooBar capitalize to FooBar — reflect.StructOf panics
	// on duplicate field names; the validator must catch this first.
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"foo_bar": map[string]any{"type": "boolean"},
			"fooBar":  map[string]any{"type": "boolean"},
		},
	}
	schemaJSON, _ := json.Marshal(schema)

	_, err := SchemaToEnv(schemaJSON)
	if err == nil {
		t.Fatal("expected error for duplicate CamelCased field names, got nil (would panic in reflect.StructOf)")
	}
}

func TestCompileCondition_InvalidGoIdentifier(t *testing.T) {
	tests := []struct {
		name    string
		propKey string
	}{
		{"starts with digit", "1field"},
		{"contains dollar sign", "$schema"},
		{"contains space", "with space"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema := map[string]any{
				"type": "object",
				"properties": map[string]any{
					tt.propKey: map[string]any{"type": "boolean"},
				},
			}
			schemaJSON, _ := json.Marshal(schema)
			_, err := SchemaToEnv(schemaJSON)
			if err == nil {
				t.Fatalf("expected error for invalid identifier %q, got nil", tt.propKey)
			}
		})
	}
}

// TestCompileCondition_ArrayItemsNotTyped documents the v1 limitation: array
// properties return []any{} and the items schema is NOT reflected into the
// element type. Expressions like input.Tags[0].Name compile but the .Name
// access is not type-checked. This is acceptable for v1; conditions rarely
// need element-shape checking (most branch on top-level fields).
func TestCompileCondition_ArrayItemsNotTyped(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tags": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{"type": "string"},
					},
				},
			},
		},
	}
	schemaJSON, _ := json.Marshal(schema)

	// Len() works on the array.
	if err := CompileCondition("len(input.Tags) > 0", schemaJSON); err != nil {
		t.Fatalf("expected len() on array to compile, got: %v", err)
	}
}
