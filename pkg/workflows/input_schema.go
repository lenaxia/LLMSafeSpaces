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

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ErrInputSchemaNotObject rejects a declared inputSchema whose root type
// no run input can satisfy: runs always carry a JSON object (an absent
// run input is validated as the JSON null document), so e.g.
// {"type":"string"} would fail validation on every run.
var ErrInputSchemaNotObject = errors.New(`inputSchema must be object-rooted (type "object", a type list containing "object", or no root type)`)

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
		return fmt.Errorf("invalid inputSchema: %w", err)
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
		return fmt.Errorf("invalid inputSchema: %w", err)
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
