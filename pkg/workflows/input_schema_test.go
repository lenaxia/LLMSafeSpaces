// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidateInputSchema(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		// wantErr is a required substring of the error; empty means the
		// schema must be declarable.
		wantErr string
		// wantNotObject additionally requires errors.Is(err,
		// ErrInputSchemaNotObject) — the named rejection (#1433).
		wantNotObject bool
	}{
		{"absent schema passes", ``, "", false},
		{"explicit JSON null is schema-less", `null`, "", false},
		{"whitespace-padded null is schema-less", " null\n", "", false},
		{"object-rooted passes", `{"type":"object","required":["topic"],"properties":{"topic":{"type":"string"}}}`, "", false},
		{"type-less passes", `{"properties":{"topic":{"type":"string"}}}`, "", false},
		{"object-or-null union passes", `{"type":["object","null"]}`, "", false},
		{"string-rooted rejected", `{"type":"string"}`, "object-rooted", true},
		{"number-rooted rejected", `{"type":"number"}`, "object-rooted", true},
		{"integer-rooted rejected", `{"type":"integer"}`, "object-rooted", true},
		{"array-rooted rejected", `{"type":"array","items":{"type":"string"}}`, "object-rooted", true},
		{"boolean-rooted rejected", `{"type":"boolean"}`, "object-rooted", true},
		{"null-only type list rejected", `{"type":["null"]}`, "object-rooted", true},
		{"malformed JSON fails", `{"type":"object",}`, "invalid inputSchema", false},
		{"unknown type keyword fails", `{"type":"crayon"}`, "invalid inputSchema", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateInputSchema(json.RawMessage(tc.raw))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected pass, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
			if tc.wantNotObject && !errors.Is(err, ErrInputSchemaNotObject) {
				t.Fatalf("error %v is not ErrInputSchemaNotObject", err)
			}
		})
	}
}

func TestNormalizeInputSchema(t *testing.T) {
	const valid = `{"type":"object"}`
	cases := []struct {
		name string
		in   string
		want string // empty means the normalized form is absent (nil)
	}{
		{"absent stays absent", ``, ""},
		{"null collapses to absent", `null`, ""},
		{"padded null collapses to absent", "\t null ", ""},
		{"real schema passes through", valid, valid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeInputSchema(json.RawMessage(tc.in))
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("expected absent, got %s", got)
				}
				return
			}
			if string(got) != tc.want {
				t.Fatalf("expected %s, got %s", tc.want, got)
			}
		})
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
		// #1433: a stored row carrying the JSON literal null is
		// schema-less at run time, not a compile failure.
		{"null stored schema row is schema-less", json.RawMessage(`null`), `{"anything":"goes"}`, ""},
		{"null stored schema row, absent input", json.RawMessage(`null`), ``, ""},
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
