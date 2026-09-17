// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
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

// --- 0059 §3.5: sanitized, capped schema-violation payloads ------------------

// runFailingValidation validates input against schema and returns the
// error (nil on pass — callers require a failure).
func runFailingValidation(t *testing.T, schema, input string) error {
	t.Helper()
	err := ValidateRunInput(json.RawMessage(schema), json.RawMessage(input))
	if err == nil {
		t.Fatal("expected a validation error")
	}
	return err
}

// TestExtractSchemaViolations_NoInstanceValues is the security-load-bearing
// pin: instances whose VALUES would appear in raw jsonschema messages
// (pattern-mismatched strings, format-invalid values, extra properties)
// produce typed records carrying locations + keywords + schema-side
// messages ONLY — never the instance-derived content.
func TestExtractSchemaViolations_NoInstanceValues(t *testing.T) {
	const secretValue = "SUPER-SECRET-INSTANCE-VALUE"
	const extraProp = "attackerControlledExtraProperty"

	schema := `{
		"type": "object",
		"required": ["topic"],
		"properties": {
			"topic": {"type": "string"},
			"code": {"pattern": "^[A-Z]{3}[0-9]+$"},
			"pick": {"enum": ["a", "b"]}
		},
		"additionalProperties": false
	}`
	input := `{"code": "` + secretValue + `", "pick": "c", "` + extraProp + `": true}`

	err := runFailingValidation(t, schema, input)
	violations := ExtractSchemaViolations(err)
	if len(violations) == 0 {
		t.Fatal("expected violations")
	}

	payload := string(SchemaMismatchPayload("body", err))
	for _, forbidden := range []string{secretValue, extraProp} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("payload leaks instance-derived value %q:\n%s", forbidden, payload)
		}
	}

	// Typed record shape: every violation is {jsonPointer, keyword} with a
	// pointer that locates the failure, keyword = the failing keyword.
	byKeyword := map[string]SchemaViolation{}
	for _, v := range violations {
		if v.Keyword == "" {
			t.Fatalf("violation without keyword: %+v", v)
		}
		byKeyword[v.Keyword] = v
	}
	for _, kw := range []string{"required", "pattern", "enum", "additionalProperties"} {
		if _, ok := byKeyword[kw]; !ok {
			t.Fatalf("expected a %s violation, got %+v", kw, violations)
		}
	}
	if v := byKeyword["required"]; v.JSONPointer != "/topic" || v.Message != `missing property "topic"` {
		t.Fatalf("required record must be {\"/topic\",\"required\",missing property \"topic\"}, got %+v", v)
	}
	if v := byKeyword["pattern"]; v.JSONPointer != "/code" || v.Message == "" {
		t.Fatalf("pattern record must point at /code with a keyword-static message, got %+v", v)
	}
	if v := byKeyword["additionalProperties"]; strings.Contains(v.Message, extraProp) || v.JSONPointer != "" {
		t.Fatalf("additionalProperties record must be static + root-located, got %+v", v)
	}
}

// TestViolationsForKind_LeakVectors covers the kind branches whose library
// messages provably embed instance values — format (renders the received
// value), type (renders got), const/enum (renders got) — asserting our
// rendered records drop the instance side. These kinds are exercised
// directly because the default compiler does not assert formats.
func TestViolationsForKind_LeakVectors(t *testing.T) {
	const instanceValue = ".INSTANCE-VALUE."
	cases := []struct {
		name string
		kind jsonschema.ErrorKind
		inst []string
		want SchemaViolation
	}{
		{"format drops got", &kind.Format{Got: instanceValue, Want: "email"}, []string{"email"},
			SchemaViolation{JSONPointer: "/email", Keyword: "format", Message: "not a valid email"}},
		{"type drops got", &kind.Type{Got: "string", Want: []string{"number"}}, []string{"n"},
			SchemaViolation{JSONPointer: "/n", Keyword: "type", Message: `expected type "number"`}},
		{"enum drops got", &kind.Enum{Got: instanceValue, Want: []any{"a"}}, []string{"pick"},
			SchemaViolation{JSONPointer: "/pick", Keyword: "enum", Message: "value does not match any allowed enum value"}},
		{"const drops got", &kind.Const{Got: instanceValue, Want: "fixed"}, []string{"c"},
			SchemaViolation{JSONPointer: "/c", Keyword: "const", Message: "value does not match the required constant"}},
		{"propertyNames drops the instance property", &kind.PropertyNames{Property: instanceValue}, nil,
			SchemaViolation{JSONPointer: "", Keyword: "propertyNames", Message: "invalid property name"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := violationsForKind(&jsonschema.ValidationError{InstanceLocation: tc.inst, ErrorKind: tc.kind})
			if len(got) != 1 {
				t.Fatalf("expected one record, got %+v", got)
			}
			if got[0] != tc.want {
				t.Fatalf("got %+v, want %+v", got[0], tc.want)
			}
			if strings.Contains(got[0].Message, instanceValue) {
				t.Fatalf("record leaks the instance value: %+v", got[0])
			}
		})
	}
}

// TestExtractSchemaViolations_Pointers pins pointer shape for nested and
// array-nested violations (RFC 6901, ~ and / escaping).
func TestExtractSchemaViolations_Pointers(t *testing.T) {
	schema := `{"type":"object","properties":{"items":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string"}}}},"we~ird/key":{"type":"number"}}}`
	err := runFailingValidation(t, schema, `{"items":[{"name":"ok"},{"name":42}],"we~ird/key":"str"}`)
	violations := ExtractSchemaViolations(err)

	want := map[string]string{ // pointer → keyword
		"/items/1/name": "type",
		"/we~0ird~1key": "type",
	}
	found := map[string]string{}
	for _, v := range violations {
		found[v.JSONPointer] = v.Keyword
	}
	for ptr, kw := range want {
		if found[ptr] != kw {
			t.Fatalf("expected %s at %s, got %+v", kw, ptr, found)
		}
	}
}

// TestSchemaMismatchPayload_CapInvariant: many violations → payload stays
// ≤ 4 KiB, ends with the truncation marker, and retained records keep
// their pointers.
func TestSchemaMismatchPayload_CapInvariant(t *testing.T) {
	// A schema with many required fields produces many violations.
	var required []string
	for i := 0; i < 400; i++ {
		required = append(required, sprintf("field%03d", i))
	}
	schemaBytes, _ := json.Marshal(map[string]any{"type": "object", "required": required})
	err := runFailingValidation(t, string(schemaBytes), `{}`)

	payload := SchemaMismatchPayload("body", err)
	if len(payload) > SchemaViolationPayloadCap {
		t.Fatalf("payload exceeds cap: %d", len(payload))
	}

	var doc struct {
		Code       string `json:"code"`
		InputFrom  string `json:"inputFrom"`
		Violations []struct {
			JSONPointer string `json:"jsonPointer"`
			Keyword     string `json:"keyword"`
			Message     string `json:"message"`
		} `json:"violations"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if doc.Code != "schema_mismatch" || doc.InputFrom != "body" {
		t.Fatalf("skeleton mismatch: %s", payload)
	}
	if len(doc.Violations) == 0 {
		t.Fatal("no violations retained")
	}
	last := doc.Violations[len(doc.Violations)-1]
	if last.Keyword != "truncated" {
		t.Fatalf("payload must end with the truncation marker, got %+v", last)
	}
	// Retained records keep their pointers (and messages — the cap only
	// bites at the tail).
	for _, v := range doc.Violations[:len(doc.Violations)-1] {
		if !strings.HasPrefix(v.JSONPointer, "/field") || v.Message == "" {
			t.Fatalf("retained record lost its pointer/message: %+v", v)
		}
	}
}

// TestSchemaMismatchPayload_DropMessageThenRecord: a record that does not
// fit first drops its message (pointer retained), then the record itself.
// Record 2's pointer is sized programmatically so that (a) its full form
// does not fit beside record 1, (b) its messageless form fits beside
// record 1 AND the truncation marker — exercising the drop-message path
// without tripping the marker-pop path.
func TestSchemaMismatchPayload_DropMessageThenRecord(t *testing.T) {
	fitsFull := SchemaViolation{JSONPointer: "/" + strings.Repeat("a", 100), Keyword: "pattern", Message: "m"}
	longMsg := strings.Repeat("m", 100)

	// Size record 2's pointer empirically: f(p2) = len of the payload with
	// [rec1, rec2-messageless(p2), marker] grows 1 byte per pointer char;
	// record 2's FULL form adds len(longMsg)+15 bytes over its messageless
	// form (115 > marker 70 + separators), so a p2 in the upper-middle of
	// the messageless budget keeps the messageless form (+ marker) under
	// the cap while the full form crosses it.
	docLen := func(vs ...SchemaViolation) int {
		b, err := json.Marshal(schemaMismatchPayload{Code: "schema_mismatch", InputFrom: "body", Violations: vs})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return len(b)
	}
	base := docLen(fitsFull, SchemaViolation{JSONPointer: "", Keyword: "x"}, truncationMarker(1))
	p2 := SchemaViolationPayloadCap - base - 22 // middle of the drop-message window
	fitsMessageless := SchemaViolation{
		JSONPointer: "/" + strings.Repeat("b", p2-1),
		Keyword:     "pattern",
		Message:     longMsg,
	}
	// Self-validate the sizing preconditions this test exists to exercise
	// (marshal directly — the function under test already degrades).
	rec2Full := SchemaViolation{JSONPointer: fitsMessageless.JSONPointer, Keyword: "pattern", Message: longMsg}
	if docLen(fitsFull, rec2Full) <= SchemaViolationPayloadCap {
		t.Fatalf("test sizing broken: record 2's FULL form fits (%d), so the drop-message path is not exercised", docLen(fitsFull, rec2Full))
	}
	cannotFit := SchemaViolation{JSONPointer: "/" + strings.Repeat("c", 5000), Keyword: "pattern"}

	payload := capSchemaMismatchPayload("body", []SchemaViolation{fitsFull, fitsMessageless, cannotFit})
	if len(payload) > SchemaViolationPayloadCap {
		t.Fatalf("payload exceeds cap: %d", len(payload))
	}
	var doc struct {
		Violations []struct {
			JSONPointer string `json:"jsonPointer"`
			Keyword     string `json:"keyword"`
			Message     string `json:"message"`
		} `json:"violations"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, payload)
	}
	var sawFull, sawMessageless bool
	for _, v := range doc.Violations {
		if len(v.JSONPointer) < 2 {
			continue // the truncation marker
		}
		switch v.JSONPointer[1] {
		case 'a':
			sawFull = v.Message != ""
		case 'b':
			// The near-cap record survives with its pointer, message dropped.
			if v.Message != "" {
				t.Fatalf("near-cap record should have dropped its message, got %+v", v)
			}
			sawMessageless = true
		}
	}
	if !sawFull {
		t.Fatal("the fitting record was dropped entirely")
	}
	if !sawMessageless {
		t.Fatal("the messageless-fitting record was dropped entirely (pointer must be retained)")
	}
	last := doc.Violations[len(doc.Violations)-1]
	if last.Keyword != "truncated" || !strings.Contains(last.Message, "omitted") {
		t.Fatalf("expected truncation marker last, got %+v", last)
	}
}

// TestSchemaMismatchPayload_SkeletonFloor: when even the first record
// cannot fit without its message, the payload is the bare
// {"code","inputFrom"} skeleton plus the marker — never an empty or
// oversized document.
func TestSchemaMismatchPayload_SkeletonFloor(t *testing.T) {
	huge := SchemaViolation{JSONPointer: "/" + strings.Repeat("x", 8192), Keyword: "pattern", Message: "m"}
	payload := capSchemaMismatchPayload("mapped", []SchemaViolation{huge})
	if len(payload) > SchemaViolationPayloadCap {
		t.Fatalf("payload exceeds cap: %d", len(payload))
	}
	var doc struct {
		Code       string `json:"code"`
		InputFrom  string `json:"inputFrom"`
		Violations []struct {
			JSONPointer string `json:"jsonPointer"`
			Keyword     string `json:"keyword"`
			Message     string `json:"message"`
		} `json:"violations"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, payload)
	}
	if doc.Code != "schema_mismatch" || doc.InputFrom != "mapped" {
		t.Fatalf("skeleton floor must keep code+inputFrom, got %s", payload)
	}
	if len(doc.Violations) != 1 || doc.Violations[0].Keyword != "truncated" {
		t.Fatalf("floor payload must be skeleton + marker only, got %s", payload)
	}
	if doc.Violations[0].Message != "1 more violations omitted" {
		t.Fatalf("marker must count the omitted record, got %s", payload)
	}
}

// TestSchemaMismatchPayload_EmptyInputFromNormalizes: the recorded
// inputFrom is the effective mode ("" reads as envelope).
func TestSchemaMismatchPayload_EmptyInputFromNormalizes(t *testing.T) {
	payload := SchemaMismatchPayload("", runFailingValidation(t, `{"required":["t"]}`, `{}`))
	if !strings.Contains(string(payload), `"inputFrom":"envelope"`) {
		t.Fatalf("empty inputFrom must normalize to envelope: %s", payload)
	}
}

// TestSchemaMismatchPayload_NonValidationError: an err that carries no
// typed trail still yields a well-formed, instance-free payload.
func TestSchemaMismatchPayload_NonValidationError(t *testing.T) {
	payload := SchemaMismatchPayload("body", errors.New("input is not valid JSON: oops"))
	var doc struct {
		Code       string `json:"code"`
		Violations []struct {
			Keyword string `json:"keyword"`
		} `json:"violations"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if doc.Code != "schema_mismatch" || len(doc.Violations) != 1 || doc.Violations[0].Keyword == "" {
		t.Fatalf("unexpected fallback payload: %s", payload)
	}
	if strings.Contains(string(payload), "oops") {
		t.Fatalf("fallback payload must not embed the raw error: %s", payload)
	}
}

// TestValidateRunInput_InvalidSchemaSentinel: a stored schema that no
// longer compiles is distinguishable from an input mismatch via
// errors.Is(err, ErrInvalidInputSchema) — the fire path's contract.
func TestValidateRunInput_InvalidSchemaSentinel(t *testing.T) {
	// An object-rooted schema with a broken $ref compiles at the write
	// gate's structural level only in some shapes; use a schema that
	// compiles as a document but fails jsonschema compilation.
	err := ValidateRunInput(json.RawMessage(`{"$ref":"#/definitions/missing"}`), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected compile failure")
	}
	if !errors.Is(err, ErrInvalidInputSchema) {
		t.Fatalf("expected ErrInvalidInputSchema, got %v", err)
	}
	if !strings.Contains(err.Error(), "invalid inputSchema") {
		t.Fatalf("message must keep the invalid inputSchema prefix, got %v", err)
	}
}

func sprintf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}
