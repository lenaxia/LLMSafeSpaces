// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateInputSchema(t *testing.T) {
	if err := ValidateInputSchema(nil); err != nil {
		t.Fatalf("absent schema must pass, got %v", err)
	}
	if err := ValidateInputSchema(json.RawMessage(`{"type":"object","required":["topic"],"properties":{"topic":{"type":"string"}}}`)); err != nil {
		t.Fatalf("well-formed schema must pass, got %v", err)
	}
	if err := ValidateInputSchema(json.RawMessage(`{"type":"object",}`)); err == nil {
		t.Fatal("malformed JSON schema must fail")
	}
	if err := ValidateInputSchema(json.RawMessage(`{"type":"crayon"}`)); err == nil {
		t.Fatal("unknown type keyword value must fail compilation")
	}
}

func TestValidateRunInput(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["topic"],"properties":{"topic":{"type":"string"},"urgency":{"enum":["high","low"]}}}`)

	cases := []struct {
		name    string
		schema  json.RawMessage
		input   string
		wantErr string
	}{
		{"valid", schema, `{"topic":"ship","urgency":"high"}`, ""},
		{"missing required", schema, `{"wrong":true}`, "missing property 'topic'"},
		{"wrong type", schema, `{"topic":42}`, "got number, want string"},
		{"enum violation", schema, `{"topic":"x","urgency":"medium"}`, "urgency"},
		{"null input vs required", schema, ``, "got null, want object"},
		{"no schema accepts anything", nil, `{"anything":"goes"}`, ""},
		{"input not json", schema, `{nope`, "input is not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var in json.RawMessage
			if tc.input != "" {
				in = json.RawMessage(tc.input)
			}
			err := ValidateRunInput(tc.schema, in)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected pass, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
