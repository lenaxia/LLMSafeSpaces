// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// Run-input schema validation (#1413): the workflow run path accepted any
// input regardless of the workflow's declared inputSchema — garbage inputs
// only surfaced as confusing node failures deep in the DAG. These helpers
// validate at the write boundary using the instance's existing
// santhosh-tekuri/jsonschema dependency.

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ValidateInputSchema checks that a workflow's declared inputSchema is
// well-formed JSON and compiles as a JSON Schema. Used at workflow
// create/update so a broken schema fails fast instead of at run time.
func ValidateInputSchema(inputSchema json.RawMessage) error {
	if len(inputSchema) == 0 {
		return nil
	}
	if _, err := compileInputSchema(inputSchema); err != nil {
		return fmt.Errorf("invalid inputSchema: %w", err)
	}
	return nil
}

// ValidateRunInput validates a run's input document against a workflow's
// inputSchema. An absent schema accepts everything; an absent input is
// validated as the JSON null document (so required properties fail).
func ValidateRunInput(inputSchema, input json.RawMessage) error {
	if len(inputSchema) == 0 {
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
