// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// Run-input schema validation (#1413): the workflow run path accepted any
// input regardless of the workflow's declared inputSchema — garbage inputs
// only surfaced as confusing node failures deep in the DAG. These helpers
// validate at the write boundary using the instance's existing
// santhosh-tekuri/jsonschema dependency.
//
// #1433 tightens that boundary: run inputs are always JSON objects, so a
// schema rooted at a non-object type (string/number/array/…) compiles and
// stores fine, then 400s EVERY subsequent run ("got string, want object").
// The write gate now requires an object-rooted (or type-less) schema, and
// the JSON literal null means schema-less everywhere — same as an absent
// field — at both write time and run time (rows written before #1433 can
// carry a literal jsonb null input_schema).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

// ErrInputSchemaNotObject rejects a declared inputSchema whose root type
// no run input can satisfy: runs always carry a JSON object (an absent
// run input is validated as the JSON null document), so e.g.
// {"type":"string"} would fail validation on every run.
var ErrInputSchemaNotObject = errors.New(`inputSchema must be object-rooted (type "object", a type list containing "object", or no root type)`)

// ErrInvalidInputSchema is the sentinel for "the stored schema itself is
// broken" — malformed JSON or no-longer-compiling. A workflow defect, not
// an input defect: the fire path records a failed fire with
// {"code":"invalid_input_schema"} instead of a schema-mismatch payload
// (design 0059 D3).
var ErrInvalidInputSchema = errors.New("invalid inputSchema")

// jsonNullLiteral is the JSON document that means "declared, but as
// schema-less" — distinct from SQL NULL / a nil RawMessage only in how
// the row was written.
var jsonNullLiteral = []byte("null")

// ValidateInputSchema checks that a workflow's declared inputSchema is
// declarable: well-formed JSON that compiles as a JSON Schema AND roots
// at type "object" (or leaves the root type unset). An absent schema —
// or the explicit JSON literal null, which means the same thing (#1433)
// — accepts everything. Used at workflow create/update so a broken or
// unsatisfiable schema fails fast instead of at run time.
func ValidateInputSchema(inputSchema json.RawMessage) error {
	if isSchemalessInputSchema(inputSchema) {
		return nil
	}
	return validateInputSchemaDeclarable(inputSchema)
}

// NormalizeInputSchema collapses an explicit JSON null declaration to a
// nil (absent) schema so the stored row never carries a literal jsonb
// null: "declared null" and "not declared" are the same contract at the
// storage boundary — schema-less (#1433). Everything else passes through
// untouched.
func NormalizeInputSchema(inputSchema json.RawMessage) json.RawMessage {
	if isSchemalessInputSchema(inputSchema) {
		return nil
	}
	return inputSchema
}

// ValidateRunInput validates a run's input document against a workflow's
// inputSchema. An absent schema — or a stored JSON literal null row
// (#1433) — accepts everything; an absent input is validated as the
// JSON null document (so required properties fail).
func ValidateRunInput(inputSchema, input json.RawMessage) error {
	if isSchemalessInputSchema(inputSchema) {
		return nil
	}
	sch, err := compileInputSchema(inputSchema)
	if err != nil {
		// A stored schema that no longer compiles is a workflow defect,
		// not an input defect — fail loudly rather than skip validation.
		return fmt.Errorf("%w: %w", ErrInvalidInputSchema, err)
	}
	var inst any
	if len(input) > 0 {
		inst, err = jsonschema.UnmarshalJSON(bytes.NewReader(input))
		if err != nil {
			return fmt.Errorf("input is not valid JSON: %w", err)
		}
	}
	if verr := sch.Validate(inst); verr != nil {
		return verr
	}
	return nil
}

// validateInputSchemaDeclarable is the write-boundary gate: compile the
// schema (garbage and unknown keywords die here), then require an
// object-rooted root. This re-establishes the check salvaged from the
// collided #1421 round, in this round's own shape (#1433): the compiled
// schema's declared type set must be empty (type-less, or a boolean
// schema) or contain "object" — ["object","null"] is fine because an
// absent run input validates as null.
func validateInputSchemaDeclarable(inputSchema json.RawMessage) error {
	sch, err := compileInputSchema(inputSchema)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidInputSchema, err)
	}
	if types := sch.Types; types != nil && !types.IsEmpty() && !slices.Contains(types.ToStrings(), "object") {
		return fmt.Errorf("%w: declared root type %s; run inputs are always JSON objects", ErrInputSchemaNotObject, types)
	}
	return nil
}

// isSchemalessInputSchema reports whether raw carries no effective
// schema: either nothing was declared, or the declaration is the JSON
// literal null. Both mean "accept any input".
func isSchemalessInputSchema(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), jsonNullLiteral)
}

func compileInputSchema(inputSchema json.RawMessage) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(inputSchema))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(inputSchemaResourceURL, doc); err != nil {
		return nil, err
	}
	return c.Compile(inputSchemaResourceURL)
}

// inputSchemaResourceURL is the in-memory resource name the schema is
// registered under — workflows use self-contained schemas (no $ref to
// external documents).
const inputSchemaResourceURL = "https://llmsafespaces.invalid/inputSchema"

// --- Sanitized schema-violation records (design 0059 §3.5) ------------------
//
// trigger_fires.action_result for a validation_error fire is persisted and
// rendered (UI + the trigger_fires MCP tool), and under inputFrom:body the
// instance is an EXTERNAL sender's payload. The jsonschema library's
// ValidationError.Error() provably embeds instance-derived content
// (pattern failures quote the instance string, format failures render the
// received value, additionalProperties failures list instance property
// names) — a raw message would give attacker-influenced bodies a durable,
// displayed reflection surface. Violations are therefore TYPED
// {jsonPointer, keyword, message} records derived from the error's typed
// trail (instance location + keyword), with messages generated from
// keyword + schema-side fields only — never from ValidationError.Error()
// and never from instance values — and the marshaled payload is hard-capped
// at 4 KiB with recursive drop semantics.

// SchemaViolationPayloadCap is the hard cap on a marshaled schema-mismatch
// action_result document. The payload size is an invariant, never a
// best-effort.
const SchemaViolationPayloadCap = 4 << 10 // 4 KiB

// SchemaViolation is one typed, location-only record of a schema
// violation. JSONPointer locates the violation in the run input (RFC
// 6901); Keyword is the failing schema keyword; Message is generated
// from keyword + schema-side fields only (e.g. a name from the schema's
// own `required` list — never an instance value).
type SchemaViolation struct {
	JSONPointer string `json:"jsonPointer"`
	Keyword     string `json:"keyword"`
	Message     string `json:"message,omitempty"`
}

// schemaMismatchPayload is the action_result document for a
// validation_error fire. Field order fixes the {"code","inputFrom",
// "violations"} shape the design pins.
type schemaMismatchPayload struct {
	Code       string            `json:"code"`
	InputFrom  string            `json:"inputFrom"`
	Violations []SchemaViolation `json:"violations"`
}

// SchemaMismatchPayload builds the capped action_result document for a
// fire-time input/schema mismatch (status validation_error): typed,
// location-only violations extracted from verr, hard-capped at
// SchemaViolationPayloadCap with the design's recursive drop semantics:
//
//   - records are appended only while the marshaled payload stays under
//     the cap; a record that does not fit first drops its message, then
//     the record — pointer included — is dropped entirely and counted in
//     a final {"keyword":"truncated","message":"N more violations
//     omitted"} marker;
//   - if even the first record cannot fit without its message, the
//     payload degrades to the bare {"code","inputFrom"} skeleton plus
//     marker.
//
// An err that is not a *jsonschema.ValidationError (or extraction that
// yields no records) still produces a well-formed payload with a single
// static, instance-free record — the invariant is "always ≤ cap, never
// instance values", including for unexpected error shapes.
func SchemaMismatchPayload(inputFrom string, err error) json.RawMessage {
	violations := ExtractSchemaViolations(err)
	if len(violations) == 0 {
		violations = []SchemaViolation{{
			Keyword: "schema",
			Message: "input does not satisfy the workflow's inputSchema",
		}}
	}
	return capSchemaMismatchPayload(inputFrom, violations)
}

// capSchemaMismatchPayload enforces the 4 KiB invariant.
func capSchemaMismatchPayload(inputFrom string, violations []SchemaViolation) json.RawMessage {
	skeleton := schemaMismatchPayload{Code: "schema_mismatch", InputFrom: NormalizeTriggerInputFrom(inputFrom)}
	marshalLen := func(vs []SchemaViolation) int {
		p := skeleton
		p.Violations = vs
		b, merr := json.Marshal(p)
		if merr != nil {
			// SchemaViolation marshals unconditionally; unreachable.
			return SchemaViolationPayloadCap + 1
		}
		return len(b)
	}

	kept := make([]SchemaViolation, 0, len(violations))
	omitted := 0
	for _, v := range violations {
		switch {
		case marshalLen(append(kept, v)) <= SchemaViolationPayloadCap:
			kept = append(kept, v)
		case marshalLen(append(kept, messageless(v))) <= SchemaViolationPayloadCap:
			kept = append(kept, messageless(v))
		default:
			omitted++
		}
	}
	if omitted == 0 {
		out, _ := json.Marshal(schemaMismatchPayload{Code: skeleton.Code, InputFrom: skeleton.InputFrom, Violations: kept})
		return out
	}
	// The marker must fit too: if the retained records filled the budget,
	// pop them (each pop frees far more than a marker digit costs) until
	// skeleton+kept+marker is under the cap.
	for {
		marker := truncationMarker(omitted)
		if marshalLen(append(kept, marker)) <= SchemaViolationPayloadCap || len(kept) == 0 {
			kept = append(kept, marker)
			break
		}
		kept = kept[:len(kept)-1]
		omitted++
	}
	out, _ := json.Marshal(schemaMismatchPayload{Code: skeleton.Code, InputFrom: skeleton.InputFrom, Violations: kept})
	return out
}

func messageless(v SchemaViolation) SchemaViolation {
	return SchemaViolation{JSONPointer: v.JSONPointer, Keyword: v.Keyword}
}

func truncationMarker(omitted int) SchemaViolation {
	return SchemaViolation{
		Keyword: "truncated",
		Message: strconv.Itoa(omitted) + " more violations omitted",
	}
}

// ExtractSchemaViolations walks a validation error's typed trail and
// returns location-only violation records. Never consults
// ValidationError.Error(): pointers come from InstanceLocation, keywords
// from ErrorKind.KeywordPath(), and messages from the concrete kind's
// SCHEMA-side fields only.
func ExtractSchemaViolations(err error) []SchemaViolation {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	var out []SchemaViolation
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if e == nil || e.ErrorKind == nil {
			return
		}
		switch e.ErrorKind.(type) {
		case *kind.Schema, *kind.Group, *kind.Reference:
			// Pure container nodes ("validation failed" wrappers whose
			// Causes carry the actual violations) — no record of their own.
		default:
			out = append(out, violationsForKind(e)...)
		}
		for _, cause := range e.Causes {
			walk(cause)
		}
	}
	walk(ve)
	return out
}

// violationsForKind renders one ValidationError into records whose every
// byte is instance-free. Messages carry only keyword + schema-side
// values: kind.Required.Missing names come from the schema's own
// `required` list; kind.Type/kind.Format expose only the schema-declared
// want; kinds whose only specifics are instance-derived
// (additionalProperties, propertyNames, pattern-mismatch got-values,
// enum/const got-values) degrade to keyword-static text.
func violationsForKind(e *jsonschema.ValidationError) []SchemaViolation {
	ptr := instancePointer(e.InstanceLocation)
	path := e.ErrorKind.KeywordPath()
	keyword := "schema"
	if len(path) > 0 {
		keyword = path[len(path)-1]
	}

	switch k := e.ErrorKind.(type) {
	case *kind.Required:
		// One record per missing property, pointer extended to where the
		// property would sit — the design's {"/topic","required",
		// `missing property "topic"`} shape. Missing names are the
		// schema's own required entries minus the instance; the names
		// themselves are schema-side.
		var out []SchemaViolation
		for _, missing := range k.Missing {
			out = append(out, SchemaViolation{
				JSONPointer: extendPointer(ptr, missing),
				Keyword:     keyword,
				Message:     `missing property ` + strconv.Quote(missing),
			})
		}
		return out
	case *kind.Type:
		// k.Got (the instance type) is dropped — want only.
		return []SchemaViolation{{JSONPointer: ptr, Keyword: keyword,
			Message: "expected type " + quoteJoin(k.Want)}}
	case *kind.Format:
		// k.Got (the received value) is dropped — format name only.
		return []SchemaViolation{{JSONPointer: ptr, Keyword: keyword,
			Message: "not a valid " + k.Want}}
	case *kind.AdditionalProperties:
		// k.Properties are INSTANCE property names — never rendered.
		return []SchemaViolation{{JSONPointer: ptr, Keyword: keyword,
			Message: "additional properties not allowed"}}
	case *kind.PropertyNames:
		// k.Property is an instance property name — never rendered.
		return []SchemaViolation{{JSONPointer: ptr, Keyword: keyword,
			Message: "invalid property name"}}
	case *kind.Enum:
		// k.Got is the instance value — never rendered; the allowed set
		// is schema-side but unbounded in size, so keep it static.
		return []SchemaViolation{{JSONPointer: ptr, Keyword: keyword,
			Message: "value does not match any allowed enum value"}}
	case *kind.Const:
		return []SchemaViolation{{JSONPointer: ptr, Keyword: keyword,
			Message: "value does not match the required constant"}}
	case *kind.MinLength:
		return []SchemaViolation{{JSONPointer: ptr, Keyword: keyword,
			Message: "string is shorter than minimum length " + strconv.Itoa(k.Want)}}
	case *kind.MaxLength:
		return []SchemaViolation{{JSONPointer: ptr, Keyword: keyword,
			Message: "string is longer than maximum length " + strconv.Itoa(k.Want)}}
	case *kind.MinItems:
		return []SchemaViolation{{JSONPointer: ptr, Keyword: keyword,
			Message: "array has fewer than the minimum " + strconv.Itoa(k.Want) + " items"}}
	case *kind.MaxItems:
		return []SchemaViolation{{JSONPointer: ptr, Keyword: keyword,
			Message: "array has more than the maximum " + strconv.Itoa(k.Want) + " items"}}
	case *kind.MinProperties:
		return []SchemaViolation{{JSONPointer: ptr, Keyword: keyword,
			Message: "object has fewer than the minimum " + strconv.Itoa(k.Want) + " properties"}}
	case *kind.MaxProperties:
		return []SchemaViolation{{JSONPointer: ptr, Keyword: keyword,
			Message: "object has more than the maximum " + strconv.Itoa(k.Want) + " properties"}}
	case *kind.FalseSchema:
		return []SchemaViolation{{JSONPointer: ptr, Keyword: "schema",
			Message: "schema is false"}}
	default:
		return []SchemaViolation{{JSONPointer: ptr, Keyword: keyword,
			Message: "does not satisfy " + keyword}}
	}
}

// instancePointer renders an InstanceLocation token list as an RFC 6901
// JSON pointer ("" for the root document), mirroring the library's own
// pointer rendering while keeping the record shape under this package's
// control.
func instancePointer(tokens []string) string {
	var sb strings.Builder
	for _, tok := range tokens {
		sb.WriteByte('/')
		sb.WriteString(escapePointerToken(tok))
	}
	return sb.String()
}

func extendPointer(ptr, token string) string {
	if ptr == "" {
		return "/" + escapePointerToken(token)
	}
	return ptr + "/" + escapePointerToken(token)
}

func escapePointerToken(tok string) string {
	tok = strings.ReplaceAll(tok, "~", "~0")
	return strings.ReplaceAll(tok, "/", "~1")
}

func quoteJoin(vals []string) string {
	quoted := make([]string, len(vals))
	for i, v := range vals {
		quoted[i] = strconv.Quote(v)
	}
	return strings.Join(quoted, " or ")
}
