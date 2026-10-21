// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// Trigger inline gate script (#1812): the {proceed, vars} contract.
//
// A trigger carrying an inline script (language + handler source —
// workflow-script-node parity, NOT a pointer) runs that handler BEFORE
// the routine's agent leg. The handler receives the fire envelope as
// its input and returns {proceed: bool, vars: {str: str}}:
//
//   - proceed=false exits the fire with NO LLM call (the gate is a
//     cheap deterministic precondition — poll checks, state diffs,
//     threshold logic — so the model is only invoked when it opens);
//   - proceed=true fires the prompt with each vars entry substituted
//     jinja-style ({{key}}) and ALL entries appended as a trailing
//     metadata block;
//   - ANY script failure (transport error, agentd error envelope,
//     unparseable output, return-schema violation) is fail-closed: the
//     fire fails, the error lands in the fire audit, no LLM call.
//
// The parse/validate/substitute helpers here are pure — the engine
// (api/internal/workflows) owns dispatch and audit writes.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lenaxia/llmsafespaces/pkg/types"
	"github.com/lenaxia/llmsafespaces/pkg/workflows/scriptwrap"
)

// MaxTriggerScriptBytes caps the inline handler source at create/update
// (matches the static-input cap precedent, types.MaxTriggerStaticInputBytes).
const MaxTriggerScriptBytes = types.MaxTriggerStaticInputBytes

// MaxTriggerScriptVarsBytes caps the serialized vars dict a gate may
// return at fire time — an unbounded dict would bloat the prompt.
const MaxTriggerScriptVarsBytes = types.MaxTriggerStaticInputBytes

// TriggerScript is the inline gate script carried on a trigger — the
// DTO type (types.TriggerScript), aliased here so store/spec code names
// it from the workflows package it persists through.
type TriggerScript = types.TriggerScript

// ValidateTriggerScript enforces the create/update field contract:
// both fields together or neither, language ∈ {python, node} (the
// runtimes scriptwrap can execute), handler non-empty within the size
// cap. Errors are self-teaching (field + expected shape + rule).
func ValidateTriggerScript(s *TriggerScript) error {
	if s == nil || !s.Present() {
		return nil
	}
	if s.Language == "" {
		return errors.New("trigger script requires 'language' (python or node) alongside 'handler'")
	}
	if s.Language != string(scriptwrap.LanguagePython) && s.Language != string(scriptwrap.LanguageNode) {
		return fmt.Errorf("trigger script language %q is not supported (valid: python, node) — the script is INLINE source, not a command", s.Language)
	}
	if strings.TrimSpace(s.Handler) == "" {
		return fmt.Errorf("trigger script requires 'handler' (the inline %s source defining handler(input) -> {proceed, vars}) alongside 'language'", s.Language)
	}
	if len(s.Handler) > MaxTriggerScriptBytes {
		return fmt.Errorf("trigger script handler exceeds the %d-byte (%s) cap", MaxTriggerScriptBytes, "64 KiB")
	}
	return nil
}

// ParseGateScriptOutput validates a gate handler's returned JSON
// against the {proceed: bool, vars: {str: str}} schema. Every violation
// returns an error naming the field, the expected shape, and the rule
// (self-teaching per the #1812 documentation mandate). Unknown
// top-level keys are tolerated (forward compatibility).
func ParseGateScriptOutput(raw []byte) (proceed bool, vars map[string]string, err error) {
	var probe any
	if uerr := json.Unmarshal(raw, &probe); uerr != nil {
		return false, nil, fmt.Errorf("gate script output is not valid JSON (the handler must return {\"proceed\": <bool>, \"vars\": {<str>: <str>}}): %w", uerr)
	}
	decoded, isObject := probe.(map[string]any)
	if !isObject {
		return false, nil, errors.New(`gate script output is not a JSON object (the handler must return {"proceed": <bool>, "vars": {<str>: <str>}})`)
	}

	rawProceed, hasProceed := decoded["proceed"]
	if !hasProceed {
		return false, nil, errors.New(`gate script output is missing "proceed" (required bool: true fires the prompt, false exits with no LLM call)`)
	}
	proceed, isBool := rawProceed.(bool)
	if !isBool {
		return false, nil, fmt.Errorf(`gate script output field "proceed" must be a bool (true fires the prompt, false exits with no LLM call), got %T`, rawProceed)
	}

	if rawVars, hasVars := decoded["vars"]; hasVars && rawVars != nil {
		varsRaw, isMap := rawVars.(map[string]any)
		if !isMap {
			return false, nil, errors.New(`gate script output field "vars" must be an object of string-to-string pairs (substituted into the prompt as {{key}})`)
		}
		vars = make(map[string]string, len(varsRaw))
		for k, v := range varsRaw {
			if k == "" {
				return false, nil, errors.New(`gate script output "vars" contains an empty key (keys become {{key}} substitution tokens and must be non-empty)`)
			}
			if strings.HasPrefix(k, ".") {
				return false, nil, fmt.Errorf(`gate script output "vars" key %q starts with "." — that namespace is reserved for platform tokens ({{.input}}, {{.prevResult}}, {{.scriptResult}}); drop the leading dot`, k)
			}
			if strings.Contains(k, "{{") || strings.Contains(k, "}}") {
				return false, nil, fmt.Errorf(`gate script output "vars" key %q contains template braces — keys name {{key}} substitution tokens and must not embed braces`, k)
			}
			val, isString := v.(string)
			if !isString {
				return false, nil, fmt.Errorf(`gate script output "vars"[%q] must be a string (values substitute into the prompt as {{%s}}), got %T`, k, k, v)
			}
			vars[k] = val
		}
		if len(varsRaw) > 0 {
			// Size cap on the serialized dict — an unbounded gate return
			// would bloat the prompt it feeds.
			if reencoded, merr := json.Marshal(decoded["vars"]); merr == nil && len(reencoded) > MaxTriggerScriptVarsBytes {
				return false, nil, fmt.Errorf("gate script vars exceed the %d-byte (%s) cap", MaxTriggerScriptVarsBytes, "64 KiB")
			}
		}
	}
	return proceed, vars, nil
}

// SubstitutePromptVars replaces literal {{key}} tokens with the vars
// values in ONE left-to-right pass (strings.NewReplacer semantics): a
// value containing another token lands verbatim — never re-substituted.
// Tokens without a matching var stay literal (the #1812 v1 ruling).
// Dot-prefixed tokens ({{.input}} and family) are a different namespace
// and are never touched here.
func SubstitutePromptVars(prompt string, vars map[string]string) string {
	if len(vars) == 0 {
		return prompt
	}
	pairs := make([]string, 0, len(vars)*2)
	for k, v := range vars {
		pairs = append(pairs, "{{"+k+"}}", v)
	}
	return strings.NewReplacer(pairs...).Replace(prompt)
}

// PromptVarsMetadata renders the trailing metadata block appended to
// every gate-opened prompt: ALL vars, consumed by substitution or not
// (the #1812 owner ruling — completeness beats minimalism). Compact
// JSON with sorted keys (Go map marshaling), so the block is
// deterministic and escape-safe. Empty vars append nothing.
func PromptVarsMetadata(vars map[string]string) string {
	if len(vars) == 0 {
		return ""
	}
	payload, err := json.Marshal(vars)
	if err != nil {
		// map[string]string is always marshalable; unreachable.
		return ""
	}
	return "\n\n[trigger script vars] " + string(payload)
}
