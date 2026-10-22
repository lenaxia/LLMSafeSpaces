// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

import (
	"encoding/json"
	"strings"
	"testing"
)

// --- ParseGateScriptOutput (#1812) ---

func TestParseGateScriptOutput_ProceedTrueWithVars(t *testing.T) {
	proceed, vars, err := ParseGateScriptOutput(json.RawMessage(`{"proceed":true,"vars":{"count":"3","topic":"nightly"}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !proceed {
		t.Fatal("expected proceed=true")
	}
	if vars["count"] != "3" || vars["topic"] != "nightly" {
		t.Fatalf("expected vars decoded, got %v", vars)
	}
}

func TestParseGateScriptOutput_ProceedFalseNoVars(t *testing.T) {
	proceed, vars, err := ParseGateScriptOutput(json.RawMessage(`{"proceed":false}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if proceed {
		t.Fatal("expected proceed=false")
	}
	if len(vars) != 0 {
		t.Fatalf("expected empty vars, got %v", vars)
	}
}

func TestParseGateScriptOutput_EmptyVarsKey(t *testing.T) {
	proceed, vars, err := ParseGateScriptOutput(json.RawMessage(`{"proceed":true,"vars":{}}`))
	if err != nil || !proceed || len(vars) != 0 {
		t.Fatalf("expected proceed=true empty vars, got %v %v %v", proceed, vars, err)
	}
}

func TestParseGateScriptOutput_UnknownKeysIgnored(t *testing.T) {
	proceed, _, err := ParseGateScriptOutput(json.RawMessage(`{"proceed":true,"extra":"ignored"}`))
	if err != nil || !proceed {
		t.Fatalf("expected unknown keys tolerated, got %v %v", proceed, err)
	}
}

func TestParseGateScriptOutput_NonObjectOutput(t *testing.T) {
	_, _, err := ParseGateScriptOutput(json.RawMessage(`[1,2]`))
	if err == nil || !strings.Contains(err.Error(), "object") {
		t.Fatalf("expected object-shape error, got %v", err)
	}
}

func TestParseGateScriptOutput_UnparseableJSON(t *testing.T) {
	_, _, err := ParseGateScriptOutput(json.RawMessage(`not json`))
	if err == nil || !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("expected JSON parse error, got %v", err)
	}
}

func TestParseGateScriptOutput_MissingProceed(t *testing.T) {
	_, _, err := ParseGateScriptOutput(json.RawMessage(`{"vars":{}}`))
	if err == nil || !strings.Contains(err.Error(), "proceed") || !strings.Contains(err.Error(), "bool") {
		t.Fatalf("expected self-teaching proceed error, got %v", err)
	}
}

func TestParseGateScriptOutput_ProceedNotBool(t *testing.T) {
	_, _, err := ParseGateScriptOutput(json.RawMessage(`{"proceed":"yes"}`))
	if err == nil || !strings.Contains(err.Error(), "proceed") || !strings.Contains(err.Error(), "bool") {
		t.Fatalf("expected self-teaching proceed error, got %v", err)
	}
}

func TestParseGateScriptOutput_VarsNotObject(t *testing.T) {
	_, _, err := ParseGateScriptOutput(json.RawMessage(`{"proceed":true,"vars":["a"]}`))
	if err == nil || !strings.Contains(err.Error(), "vars") || !strings.Contains(err.Error(), "object") {
		t.Fatalf("expected self-teaching vars error, got %v", err)
	}
}

func TestParseGateScriptOutput_VarValueNotString(t *testing.T) {
	_, _, err := ParseGateScriptOutput(json.RawMessage(`{"proceed":true,"vars":{"count":42}}`))
	if err == nil || !strings.Contains(err.Error(), "count") || !strings.Contains(err.Error(), "string") {
		t.Fatalf("expected self-teaching per-key error naming count, got %v", err)
	}
}

func TestParseGateScriptOutput_EmptyVarKey(t *testing.T) {
	_, _, err := ParseGateScriptOutput(json.RawMessage(`{"proceed":true,"vars":{"":"v"}}`))
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-key error, got %v", err)
	}
}

func TestParseGateScriptOutput_DotPrefixedVarKey(t *testing.T) {
	// A key starting with "." would mint {{.x}} tokens — the reserved
	// Go-template-style namespace ({{.input}}, {{.prevResult}},
	// {{.scriptResult}}). Rejected at parse time.
	_, _, err := ParseGateScriptOutput(json.RawMessage(`{"proceed":true,"vars":{".input":"evil"}}`))
	if err == nil || !strings.Contains(err.Error(), ".input") {
		t.Fatalf("expected dot-namespace error naming the key, got %v", err)
	}
}

func TestParseGateScriptOutput_BraceVarKey(t *testing.T) {
	_, _, err := ParseGateScriptOutput(json.RawMessage(`{"proceed":true,"vars":{"a{{b":"v"}}`))
	if err == nil || !strings.Contains(err.Error(), "a{{b") {
		t.Fatalf("expected brace-in-key error naming the key, got %v", err)
	}
}

func TestParseGateScriptOutput_VarsOverSizeCap(t *testing.T) {
	big := strings.Repeat("x", MaxTriggerScriptVarsBytes)
	_, _, err := ParseGateScriptOutput(json.RawMessage(`{"proceed":true,"vars":{"blob":"` + big + `"}}`))
	if err == nil || !strings.Contains(err.Error(), "64 KiB") {
		t.Fatalf("expected size-cap error, got %v", err)
	}
}

// --- SubstitutePromptVars (#1812) ---

func TestSubstitutePromptVars_LiteralTokens(t *testing.T) {
	out := SubstitutePromptVars("Report on {{topic}} ({{count}} items)", map[string]string{"topic": "nightly", "count": "3"})
	if out != "Report on nightly (3 items)" {
		t.Fatalf("unexpected substitution: %q", out)
	}
}

func TestSubstitutePromptVars_UnmatchedLeftLiteral(t *testing.T) {
	out := SubstitutePromptVars("{{known}} {{unknown}}", map[string]string{"known": "v"})
	if out != "v {{unknown}}" {
		t.Fatalf("unmatched token must stay literal, got %q", out)
	}
}

func TestSubstitutePromptVars_SinglePassNoResubstitution(t *testing.T) {
	// A var value containing another token must land verbatim — one
	// left-to-right pass, never recursive substitution.
	out := SubstitutePromptVars("{{a}}", map[string]string{"a": "{{b}}", "b": "inner"})
	if out != "{{b}}" {
		t.Fatalf("single-pass substitution expected, got %q", out)
	}
}

func TestSubstitutePromptVars_DotTokensUntouched(t *testing.T) {
	out := SubstitutePromptVars("{{.input}} {{.prevResult}}", map[string]string{"input": "x"})
	if out != "{{.input}} {{.prevResult}}" {
		t.Fatalf("dot-namespace tokens are not var substitutions, got %q", out)
	}
}

func TestSubstitutePromptVars_EmptyVars(t *testing.T) {
	if out := SubstitutePromptVars("plain", nil); out != "plain" {
		t.Fatalf("nil vars must be a no-op, got %q", out)
	}
}

// --- PromptVarsMetadata (#1812) ---

func TestPromptVarsMetadata_DeterministicSortedJSON(t *testing.T) {
	m := PromptVarsMetadata(map[string]string{"zebra": "z", "alpha": "a\nb", "mid": "1"})
	if m != "\n\n[trigger script vars] {\"alpha\":\"a\\nb\",\"mid\":\"1\",\"zebra\":\"z\"}" {
		t.Fatalf("unexpected metadata block: %q", m)
	}
	// Deterministic across calls (map iteration order must not leak).
	if PromptVarsMetadata(map[string]string{"zebra": "z", "alpha": "a\nb", "mid": "1"}) != m {
		t.Fatal("metadata must be deterministic")
	}
}

func TestPromptVarsMetadata_Empty(t *testing.T) {
	if PromptVarsMetadata(nil) != "" {
		t.Fatalf("empty vars append nothing, got %q", PromptVarsMetadata(nil))
	}
}

// --- ValidateTriggerScript (#1812, create/update-path field validation) ---

func TestValidateTriggerScript_Valid(t *testing.T) {
	if err := ValidateTriggerScript(&TriggerScript{Language: "python", Handler: "def handler(input):\n    return {\"proceed\": True}\n"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ValidateTriggerScript(nil); err != nil {
		t.Fatalf("nil script (absent) is valid: %v", err)
	}
}

func TestValidateTriggerScript_EmptyIsNilEquivalent(t *testing.T) {
	// The cleared state (both fields empty) is the stored legacy shape —
	// valid, meaning "no script".
	if err := ValidateTriggerScript(&TriggerScript{}); err != nil {
		t.Fatalf("zero-value script must validate as absent: %v", err)
	}
}

func TestValidateTriggerScript_BadLanguage(t *testing.T) {
	err := ValidateTriggerScript(&TriggerScript{Language: "bash", Handler: "x"})
	if err == nil || !strings.Contains(err.Error(), "language") || !strings.Contains(err.Error(), "python, node") {
		t.Fatalf("expected language error naming valid values, got %v", err)
	}
}

func TestValidateTriggerScript_MissingHandler(t *testing.T) {
	err := ValidateTriggerScript(&TriggerScript{Language: "python"})
	if err == nil || !strings.Contains(err.Error(), "handler") || !strings.Contains(err.Error(), "source") {
		t.Fatalf("expected handler error, got %v", err)
	}
}

func TestValidateTriggerScript_MissingLanguage(t *testing.T) {
	err := ValidateTriggerScript(&TriggerScript{Handler: "def handler(input): pass"})
	if err == nil || !strings.Contains(err.Error(), "language") {
		t.Fatalf("expected language-required error, got %v", err)
	}
}

func TestValidateTriggerScript_HandlerOverCap(t *testing.T) {
	err := ValidateTriggerScript(&TriggerScript{Language: "node", Handler: "exports.handler = " + strings.Repeat("x", MaxTriggerScriptBytes+1)})
	if err == nil || !strings.Contains(err.Error(), "64 KiB") {
		t.Fatalf("expected size-cap error, got %v", err)
	}
}
