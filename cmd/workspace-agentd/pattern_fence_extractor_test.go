// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pattern_fence_extractor_test.go — THE EXTRACTOR the FENCE CONVENTION
// ruling (2026-10-30, tsk-rested-shanty-feels) promises, shipped by
// tsk-surging-frontier-dines (owner ruling 2026-10-30: "tests or
// something or maybe an async AI workflow which validates that the
// examples are still valid").
//
// The contract: split each embedded example file on fences, read the
// kind= infostring, and DISPATCH kind→validator —
//
//	board-payload  → the REAL board_validate_file pipeline for its op
//	                  (decodeBoardPayloadJSON + validateBoardPayload;
//	                  ONE strict JSON document)
//	workflow-spec  → the REAL workflow spec validation workflow_create
//	                  applies (ParseSpec + ValidateSpec, nil predecessor
//	                  schemas, empty defaults — never a reimplementation)
//	input-schema   → the REAL ValidateInputSchema workflow_create
//	                  applies to the inputSchema field (review r1
//	                  finding 1: schema examples rode kind=shell — a
//	                  "type": "strng" mutation stayed green)
//	gate-script    → interpreter syntax check: python → py_compile,
//	                  node → node --check (structural checks always run;
//	                  a missing interpreter FAILS LOUD while the
//	                  catalogs ship gate-script fences — the fence
//	                  contract promises the check, so absence of the
//	                  binary is a broken pin, not a passed one)
//	shell, output  → not machine-validated (rendered for the human
//	                  reader; the format pin still checks the fence's
//	                  infostring)
//
// A failing fence is reported by FILE AND SLUG (the canonical header's
// slug == filename stem), so the failure names exactly where to fix;
// an UNCLOSED fence at EOF is an error, never a silent skip (review r1
// finding 2: a dangling opener once dropped the fence's content from
// every check). The extractor is the mechanism behind the
// payload-validation pins of Tier 1; the async Tier 2 walk (the
// platform patterns-drift validator workflow) mirrors this dispatch
// against LIVE tool descriptions.

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// fenceBlock is one extracted fenced code block: its file, slug (the
// canonical header's slug == filename stem), infostring, and body.
// location() is what every failure message names — file AND slug.
type fenceBlock struct {
	file, slug, info, body string
}

func (f fenceBlock) location() string {
	return fmt.Sprintf("%s:%s", f.file, f.slug)
}

var validFenceKinds = map[string]bool{
	"board-payload": true, "workflow-spec": true, "gate-script": true,
	"input-schema": true, "shell": true, "output": true,
}

var validFenceOps = map[string]bool{
	"create_task": true, "create_epic": true, "update_task_patch": true,
	"update_epic_patch": true, "import_plan": true, "set_config": true,
}

// extractFences walks both embedded directories and returns every fenced
// code block with its file, slug (from the canonical header), infostring,
// and body. Opening fences without an infostring are returned too (kind
// "") so the format pin can name them.
func extractFences(t *testing.T) []fenceBlock {
	t.Helper()
	var out []fenceBlock
	for _, set := range patternDocSets {
		err := fs.WalkDir(set.fsys, set.embedded, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			content, err := set.fsys.ReadFile(path)
			if err != nil {
				return err
			}
			name := filepath.Base(path)
			slug := strings.TrimSuffix(name, ".md")
			var cur *fenceBlock
			for _, line := range strings.Split(string(content), "\n") {
				s := strings.TrimRight(line, "\r")
				if cur == nil {
					if strings.HasPrefix(s, "```") {
						cur = &fenceBlock{file: name, slug: slug, info: strings.TrimSpace(s[3:])}
					}
				} else if s == "```" {
					out = append(out, *cur)
					cur = nil
				} else {
					cur.body += line + "\n"
				}
			}
			// An opener that never sees its closer is an ERROR, never a
			// silent drop: a dangling fence at EOF once escaped every
			// check with all pins green (review r1 finding 2).
			if cur != nil {
				t.Errorf("%s:%s: unclosed fence — the opening ``` (infostring %q) never sees its closer; its content escapes validation",
					name, slug, cur.info)
			}
			return nil
		})
		require.NoError(t, err)
	}
	require.NotEmpty(t, out, "the catalogs carry fenced examples")
	return out
}

// parseFenceInfo validates the infostring shape: `<lang> kind=<kind>
// [op=<op>]` — a language token first, EXACTLY one kind=, and op= only
// where the kind needs it. Returns (kind, op).
func parseFenceInfo(t *testing.T, f fenceBlock) (kind, op string) {
	t.Helper()
	toks := strings.Fields(f.info)
	require.NotEmpty(t, toks, "%s: a fence's infostring needs a language token and kind=", f.location())
	assert.NotContains(t, toks[0], "=", "%s: the first infostring token is the language, not key=value", f.location())
	for _, tok := range toks {
		switch {
		case strings.HasPrefix(tok, "kind="):
			require.Empty(t, kind, "%s: exactly ONE kind= per fence", f.location())
			kind = strings.TrimPrefix(tok, "kind=")
		case strings.HasPrefix(tok, "op="):
			require.Empty(t, op, "%s: at most one op= per fence", f.location())
			op = strings.TrimPrefix(tok, "op=")
		default:
			assert.NotContains(t, tok, "=", "%s: unknown key=value token %q in the infostring", f.location(), tok)
		}
	}
	return kind, op
}

// TestPatternDocs_FencePins_InfostringOnEveryFence: no bare fences, the
// kind vocabulary is closed, board-payload fences name a valid op, and
// gate-script fences carry a supported language token — the contract the
// extractor dispatches on.
func TestPatternDocs_FencePins_InfostringOnEveryFence(t *testing.T) {
	kinds := map[string]int{}
	for _, f := range extractFences(t) {
		kind, op := parseFenceInfo(t, f)
		require.NotEmpty(t, kind, "%s: bare fence — every code fence carries kind=", f.location())
		assert.True(t, validFenceKinds[kind], "%s: fence kind %q not in the convention", f.location(), kind)
		kinds[kind]++
		if kind == "board-payload" {
			require.NotEmpty(t, op, "%s: board-payload requires op=", f.location())
			assert.True(t, validFenceOps[op], "%s: board-payload fences name a valid op (got %q)", f.location(), op)
		} else {
			assert.Empty(t, op, "%s: op= belongs to board-payload fences only", f.location())
		}
		if kind == "gate-script" {
			lang := strings.Fields(f.info)[0]
			assert.Contains(t, []string{"python", "node"}, lang,
				"%s: gate-script fences carry the interpreter as the language token (python|node), got %q", f.location(), lang)
		}
	}
	// The catalogs' machine-validated classes exist at all — and stay:
	// each class's floor keeps a fence of that kind from silently
	// disappearing (review r1 missing-case 3: the sole gate-script
	// fence once could vanish behind the combined shell/output count).
	assert.Greater(t, kinds["board-payload"], 0, "board catalog: kind=board-payload fences must exist")
	assert.GreaterOrEqual(t, kinds["workflow-spec"], 5, "workflow catalog: the canonical DAGs ride kind=workflow-spec fences")
	assert.Greater(t, kinds["input-schema"], 0, "workflow catalog: the inputSchema examples ride kind=input-schema fences")
	assert.Greater(t, kinds["gate-script"], 0, "workflow catalog: the gate-script syntax-check class must stay represented")
}

// TestPatternDocs_EveryFence_PassesItsKindValidator — THE EXTRACTOR
// WALK: every fence in both catalogs is split out, its infostring read,
// and its kind dispatched to the real validator. An example that would
// fail pasted verbatim is invalid; the failure names file AND slug.
func TestPatternDocs_EveryFence_PassesItsKindValidator(t *testing.T) {
	checked := map[string]int{}
	for _, f := range extractFences(t) {
		kind, _ := parseFenceInfo(t, f)
		checked[kind]++
		switch kind {
		case "":
			t.Fatalf("%s: bare fence — every code fence carries kind= (the format pin names it too)", f.location())
		case "board-payload":
			validateBoardPayloadFence(t, f)
		case "workflow-spec":
			validateWorkflowSpecFence(t, f)
		case "input-schema":
			validateInputSchemaFence(t, f)
		case "gate-script":
			validateGateScriptFence(t, f)
		case "shell", "output":
			// Not machine-validated: these fences render for the human
			// reader (tool-call sketches, rendered output). The format
			// pin above still checks their infostring.
		default:
			t.Fatalf("%s: no validator dispatched for kind %q — the dispatch table is incomplete", f.location(), kind)
		}
	}
	assert.Positive(t, checked["board-payload"], "the board catalogs must carry board-payload fences")
	assert.GreaterOrEqual(t, checked["workflow-spec"], 5, "the workflow catalog carries at least five canonical DAGs")
	assert.Greater(t, checked["input-schema"], 0, "the workflow catalog carries its inputSchema examples")
	assert.Greater(t, checked["gate-script"], 0, "the gate-script syntax-check class must be exercised")
	assert.NotEmpty(t, checked["shell"]+checked["output"],
		"the catalogs carry human-rendered fences too")
}

// validateInputSchemaFence: kind=input-schema → ONE strict JSON
// document that passes the REAL ValidateInputSchema workflow_create
// applies to the inputSchema field — a schema example that would 400
// at create time is invalid (review r1 finding 1: these examples once
// rode kind=shell, and a "type": "strng" mutation stayed green).
func validateInputSchemaFence(t *testing.T, f fenceBlock) {
	t.Helper()
	schema, err := decodeBoardPayloadJSON([]byte(f.body))
	require.NoError(t, err, "%s: fence body must be ONE strict JSON object", f.location())
	raw, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.NoError(t, wf.ValidateInputSchema(json.RawMessage(raw)),
		"%s: input-schema must pass the real ValidateInputSchema workflow_create applies", f.location())
}

// validateBoardPayloadFence: kind=board-payload → ONE strict JSON
// document that passes the REAL board_validate_file pipeline for its op
// (the same validator the tool and the CRUD gate run).
func validateBoardPayloadFence(t *testing.T, f fenceBlock) {
	t.Helper()
	_, op := parseFenceInfo(t, f)
	payload, err := decodeBoardPayloadJSON([]byte(f.body))
	require.NoError(t, err, "%s: fence body must be ONE strict JSON object", f.location())
	report := validateBoardPayload(op, payload)
	assert.True(t, report.Valid,
		"%s: board-payload (op=%s) must pass the real validator — findings: %s",
		f.location(), op, boardValidateReportJSON(report))
}

// validateWorkflowSpecFence: kind=workflow-spec → a single JSON/YAML
// document that passes the REAL spec validation workflow_create applies
// (ParseSpec + ValidateSpec, nil predecessor schemas, empty defaults).
func validateWorkflowSpecFence(t *testing.T, f fenceBlock) {
	t.Helper()
	specJSON, err := specJSONFromFence(f.body)
	require.NoError(t, err, "%s: fence body must be a single JSON/YAML spec document", f.location())
	spec, err := wf.ParseSpec(json.RawMessage(specJSON))
	require.NoError(t, err, "%s: ParseSpec", f.location())
	errs := wf.ValidateSpec(spec, nil, wf.DefaultsBlock{})
	assert.Empty(t, errs, "%s: workflow-spec must pass the real validation", f.location())
}

// validateGateScriptFence: kind=gate-script → the interpreter's own
// syntax check for the fence's language token (python → py_compile,
// node → node --check). The structural non-empty check always runs.
// The interpreter leg FAILS LOUD when the binary is absent while the
// catalogs ship gate-script fences — unlike the us70 harness precedent
// (where the python lint was optional tooling), here the fence contract
// PROMISES the syntax check for every shipped fence, so a missing
// interpreter means the pin cannot run, not that it passes; the failure
// names the binary to install.
func validateGateScriptFence(t *testing.T, f fenceBlock) {
	t.Helper()
	lang := strings.Fields(f.info)[0]
	body := strings.TrimSpace(f.body)
	assert.NotEmpty(t, body, "%s: a gate-script fence is never empty", f.location())

	var bin string
	var args []string
	switch lang {
	case "python":
		bin, args = "python3", []string{"-m", "py_compile"}
	case "node":
		bin, args = "node", []string{"--check"}
	default:
		t.Fatalf("%s: gate-script language %q has no syntax checker in the dispatch table", f.location(), lang)
	}
	if _, lookErr := exec.LookPath(bin); lookErr != nil {
		t.Errorf("%s: %s is not on PATH — the gate-script fence contract requires the interpreter's syntax check; install %s or drop the fence",
			f.location(), bin, bin)
		return
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "fence"+scriptExt(lang))
	require.NoError(t, os.WriteFile(file, []byte(f.body), 0o600))
	out, err := exec.Command(bin, append(args, file)...).CombinedOutput()
	assert.NoError(t, err, "%s: gate-script fence must pass %s %s — output: %s",
		f.location(), bin, strings.Join(args, " "), strings.TrimSpace(string(out)))
}

func scriptExt(lang string) string {
	if lang == "node" {
		return ".js"
	}
	return ".py"
}

// specJSONFromFence mirrors the API's dialect handling (workflow_create
// honors the field name specYaml): valid JSON passes through, otherwise
// a single YAML document is converted; a second document is refused.
func specJSONFromFence(body string) (string, error) {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "", fmt.Errorf("empty spec")
	}
	if json.Valid([]byte(trimmed)) {
		return trimmed, nil
	}
	dec := yaml.NewDecoder(strings.NewReader(trimmed))
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return "", fmt.Errorf("spec is neither JSON nor YAML: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return "", fmt.Errorf("spec must be a single document")
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("cannot normalize YAML spec: %w", err)
	}
	return string(out), nil
}
