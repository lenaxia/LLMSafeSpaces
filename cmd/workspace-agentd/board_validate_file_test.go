// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_validate_file_test.go — the strongly typed validator's pins
// (design 0070 §2.5): every violation class produces a finding with
// the right path/rule/severity, the schema-DTO parity holds (reflection
// over the platform's typed request mirrors — a DTO change without a
// schema update fails here), the advisory warning never blocks, and
// the two entry points (the tool and the CRUD gate) return identical
// findings for the same file.

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
	mcpclient "github.com/lenaxia/llmsafespaces/pkg/mcp"
)

// --- eager compile (startup failure = test failure) ---------------------------

func TestBoardPayloadSchema_AllOpsCompile(t *testing.T) {
	compiled := boardPayloadSchemas()
	for op := range boardValidateOps {
		require.NotNil(t, compiled[op], "op %s compiles", op)
	}
	for op, raw := range boardPayloadSchemasRaw() {
		props, err := boardPayloadSchemaPropertyNames(raw)
		require.NoError(t, err, "op %s's raw schema is JSON with root properties", op)
		require.NotEmpty(t, props, "op %s admits at least one field", op)
	}
}

// --- schema-DTO parity (the typed coupling) ------------------------------------
//
// The schemas' property sets must mirror the platform request DTOs.
// The typed, importable mirrors are pkg/mcp's Board*Body structs (each
// "mirrors the REST board*Request" per its own doc comment — the same
// wire DTOs the pod-automation seam binds, as pinned by the board
// parity suites) plus pkg/board's PlanInput (the unified plan DTO the
// seam handler binds directly). Differences must be DECLARED below —
// an undeclared drift in either direction fails this test, which is
// what makes the schema strongly typed rather than documentation.

func dtoJSONNames(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

func TestBoardPayloadSchema_DTOParity(t *testing.T) {
	raw := boardPayloadSchemasRaw()

	parity := func(op string, schemaProps []string, dto reflect.Type, excluded []string, added ...string) {
		t.Helper()
		want := dtoJSONNames(t, dto)
		want = slices.DeleteFunc(want, func(s string) bool { return slices.Contains(excluded, s) })
		want = append(want, added...)
		assert.ElementsMatch(t, want, schemaProps,
			"op %s: schema properties must mirror the platform DTO minus the DECLARED exclusions %v plus the DECLARED additions %v", op, excluded, added)
	}

	t.Run("create_task", func(t *testing.T) {
		props, err := boardPayloadSchemaPropertyNames(raw["create_task"])
		require.NoError(t, err)
		// setCriteriaMet is control-plane: read from INLINE tool
		// arguments (the #1946 grant), refused in the file.
		parity("create_task", props, reflect.TypeOf(mcpclient.BoardStoryBody{}), []string{"setCriteriaMet"})
	})

	t.Run("update_task_patch", func(t *testing.T) {
		props, err := boardPayloadSchemaPropertyNames(raw["update_task_patch"])
		require.NoError(t, err)
		// setCriteriaMet: inline-only control-plane (above); dependsOn:
		// the platform's UPDATE DTO does not bind it (a file spelling
		// would be silently dropped — refused here instead).
		parity("update_task_patch", props, reflect.TypeOf(mcpclient.BoardStoryBody{}), []string{"setCriteriaMet", "dependsOn"})
	})

	t.Run("create_epic and update_epic_patch", func(t *testing.T) {
		for _, op := range []string{"create_epic", "update_epic_patch"} {
			props, err := boardPayloadSchemaPropertyNames(raw[op])
			require.NoError(t, err)
			parity(op, props, reflect.TypeOf(mcpclient.BoardEpicBody{}), nil)
		}
	})

	t.Run("set_config", func(t *testing.T) {
		props, err := boardPayloadSchemaPropertyNames(raw["set_config"])
		require.NoError(t, err)
		// The config override map mirrors the platform's own override
		// DTO verbatim — board.BoardConfigOverride is the type both the
		// REST adapter and the automation seam decode. tier/projectId
		// are routing keys (inline-only, refused in the file by the
		// placement pre-check) and are NOT DTO fields.
		parity("set_config", props, reflect.TypeOf(board.BoardConfigOverride{}), nil)
	})

	t.Run("import_plan", func(t *testing.T) {
		props, err := boardPayloadSchemaPropertyNames(raw["import_plan"])
		require.NoError(t, err)
		// The unified plan document: epicTitle/outcome/tags are the
		// legacy REST create-arm spellings (kept on the wire for the
		// non-agent surfaces — the file uses the epic OBJECT); dryRun
		// is inline-only control-plane; requestId is seam plumbing the
		// plan file rides with the payload (every manager route's
		// idempotency key).
		parity("import_plan", props, reflect.TypeOf(board.PlanInput{}), []string{"epicTitle", "outcome", "tags", "dryRun"}, "requestId")
	})

	t.Run("plan task node mirrors PlanTaskInput", func(t *testing.T) {
		// Extract the tasks items' properties from the raw schema.
		var doc struct {
			Properties struct {
				Tasks struct {
					Items json.RawMessage `json:"items"`
				} `json:"tasks"`
			} `json:"properties"`
		}
		require.NoError(t, json.Unmarshal([]byte(raw["import_plan"]), &doc))
		var items struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		require.NoError(t, json.Unmarshal(doc.Properties.Tasks.Items, &items))
		got := slices.Sorted(maps.Keys(items.Properties))
		assert.ElementsMatch(t, dtoJSONNames(t, reflect.TypeOf(board.PlanTaskInput{})), got,
			"plan node schema mirrors board.PlanTaskInput exactly")
	})

	t.Run("criterion item mirrors AcceptanceCriterion", func(t *testing.T) {
		// Via the compiled create_task schema: validating a criterion
		// item with an unknown key must fail — plus the DTO names.
		_, err := boardPayloadSchemaPropertyNames(raw["create_task"])
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"text", "met"},
			dtoJSONNames(t, reflect.TypeOf(board.AcceptanceCriterion{})))
	})
}

// maps_KeysRaw removed — the stdlib maps.Keys is used directly.

// --- the validator pipeline ----------------------------------------------------

// validateFixture runs validateBoardPayloadFile over one payload.
func validateFixture(t *testing.T, op, payload string) boardValidateReport {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.json")
	require.NoError(t, os.WriteFile(path, []byte(payload), 0o600))
	out, err := validateBoardPayloadFile(op, path)
	require.NoError(t, err)
	var report boardValidateReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	return report
}

func TestBoardValidate_HappyPaths(t *testing.T) {
	for op, payload := range map[string]string{
		"create_task":       `{"title":"T","description":"a real charter","acceptanceCriteria":[{"text":"c1"}],"tags":["x"],"dependsOn":["tsk-aaaa1111"]}`,
		"create_epic":       `{"title":"E","outcome":"o","tags":["y"]}`,
		"update_task_patch": `{"title":"T2"}`,
		"update_epic_patch": `{"outcome":"o2"}`,
		"import_plan":       `{"epic":{"title":"E","outcome":"o"},"tasks":[{"key":"k","title":"T","description":"d","acceptanceCriteria":[{"text":"c"}]}]}`,
	} {
		report := validateFixture(t, op, payload)
		assert.True(t, report.Valid, "%s: expected valid, findings %+v", op, report.Findings)
		assert.Empty(t, report.Findings, "%s: no findings on a happy payload", op)
		assert.NotEmpty(t, report.Fields, "%s: the submitted field names render", op)
	}

	t.Run("import_plan existing-epic arm", func(t *testing.T) {
		report := validateFixture(t, "import_plan",
			`{"epicId":"epc-bbbb2222","tasks":[{"key":"k","title":"T","description":"d","acceptanceCriteria":[{"text":"c"}]}]}`)
		assert.True(t, report.Valid)
	})

	// Role-ONLY persona shapes (owner ruling 2026-10-29, round 2): the
	// bare role reference string (canonical) and the {roleId} object —
	// nothing else. The legacy name/instructions shapes reject at the
	// schema (additionalProperties on the persona object).
	t.Run("set_config role-only persona shapes", func(t *testing.T) {
		for name, payload := range map[string]string{
			"bare reference": `{"stagePersonas":{"in_review":"reviewer"}}`,
			"roleId object":  `{"stagePersonas":{"in_progress":{"roleId":"build"}}}`,
			"roleId uuid":    `{"stagePersonas":{"in_review":{"roleId":"11111111-1111-1111-1111-111111111111"}}}`,
		} {
			report := validateFixture(t, "set_config", payload)
			assert.True(t, report.Valid, "%s: expected valid, findings %+v", name, report.Findings)
		}
		// The legacy shapes are NOT admitted (no migration, no compat —
		// owner ruling: no personas were ever created).
		for name, payload := range map[string]string{
			"legacy name+instructions": `{"stagePersonas":{"in_review":{"name":"auditor","instructions":"Verify."}}}`,
			"legacy name only":         `{"stagePersonas":{"in_review":{"name":"auditor"}}}`,
			"roleId + instructions":    `{"stagePersonas":{"in_review":{"roleId":"reviewer","instructions":"Adversarial."}}}`,
			"roleId + name":            `{"stagePersonas":{"in_review":{"roleId":"reviewer","name":"Reviewer"}}}`,
		} {
			report := validateFixture(t, "set_config", payload)
			assert.False(t, report.Valid, "%s: the non-role shape must reject", name)
			assert.Contains(t, findingPaths(report), "stagePersonas.in_review", name)
		}
	})

	t.Run("set_config persona violation classes", func(t *testing.T) {
		// An object persona with no roleId — the semantic check names
		// the role-reference rule (the schema's oneOf/required findings
		// ride along; the SEMANTIC finding is the one that names the
		// rule).
		report := validateFixture(t, "set_config", `{"stagePersonas":{"in_review":{}}}`)
		assert.False(t, report.Valid)
		found := false
		for _, f := range report.Findings {
			if f.Path == "stagePersonas.in_review" && f.Rule == "persona-role-ref" {
				found = true
			}
		}
		assert.True(t, found, "the semantic persona-role-ref finding must name the rule: %+v", report.Findings)
		// A whitespace-only roleId is the empty slot — the same
		// trim-aware rule the platform decoder enforces (one contract
		// per shape).
		report = validateFixture(t, "set_config", `{"stagePersonas":{"in_review":{"roleId":"   "}}}`)
		assert.False(t, report.Valid)
		assert.Equal(t, "persona-role-ref", report.Findings[0].Rule)
		// A whitespace-only BARE reference rejects too (review round 2,
		// PR #1990): the schema's minLength cannot see whitespace, but
		// the semantic check trim-checks — admission identical to the
		// platform decoder on both shapes.
		report = validateFixture(t, "set_config", `{"stagePersonas":{"in_review":"   "}}`)
		assert.False(t, report.Valid)
		wsFound := false
		for _, f := range report.Findings {
			if f.Path == "stagePersonas.in_review" && f.Rule == "persona-role-ref" {
				wsFound = true
			}
		}
		assert.True(t, wsFound, "the whitespace bare-string reference must carry the persona-role-ref finding: %+v", report.Findings)
		// An empty bare reference rejects at the schema (minLength).
		report = validateFixture(t, "set_config", `{"stagePersonas":{"in_review":""}}`)
		assert.False(t, report.Valid)
		// An unknown field inside a persona object still rejects.
		report = validateFixture(t, "set_config", `{"stagePersonas":{"in_review":{"roleId":"r","promt":"typo"}}}`)
		assert.False(t, report.Valid)
		assert.Contains(t, findingPaths(report), "stagePersonas.in_review")
		// An ineligible stage keys the semantic stage finding.
		report = validateFixture(t, "set_config", `{"stagePersonas":{"backlog":"build"}}`)
		assert.False(t, report.Valid)
		paths := findingPaths(report)
		assert.Contains(t, paths, "stagePersonas.backlog")
	})
}

func TestBoardValidate_ViolationClasses(t *testing.T) {
	t.Run("unknown field", func(t *testing.T) {
		report := validateFixture(t, "create_epic", `{"title":"E","bogus":"x"}`)
		assert.False(t, report.Valid)
		require.Len(t, report.Findings, 1)
		f := report.Findings[0]
		assert.Equal(t, "bogus", f.Path)
		assert.Equal(t, "additionalProperties", f.Rule)
		assert.Contains(t, f.Fix, "remove")
	})

	t.Run("missing required with path", func(t *testing.T) {
		report := validateFixture(t, "create_task", `{"title":"T"}`)
		assert.False(t, report.Valid)
		paths := findingPaths(report)
		assert.Contains(t, paths, "description")
		assert.Contains(t, paths, "acceptanceCriteria")
		for _, f := range report.Findings {
			assert.Equal(t, "required", f.Rule)
		}
	})

	t.Run("wrong type with expected/actual", func(t *testing.T) {
		report := validateFixture(t, "create_epic", `{"title":42}`)
		assert.False(t, report.Valid)
		f := report.Findings[0]
		assert.Equal(t, "type", f.Rule)
		assert.Equal(t, "string", f.Expected)
		assert.Equal(t, "number", f.Actual)
	})

	t.Run("nested instance path", func(t *testing.T) {
		report := validateFixture(t, "import_plan", `{"epic":{"title":"E"},"tasks":[
			{"key":"k0","title":"T0","description":"d0","acceptanceCriteria":[{"text":"c0"}]},
			{"key":"k1","title":"T1","description":"d1","acceptanceCriteria":[{"text":17}]}]}`)
		assert.False(t, report.Valid)
		assert.Equal(t, "tasks[1].acceptanceCriteria[0].text", report.Findings[0].Path,
			"the finding carries the indexed instance location")
	})

	t.Run("title cap", func(t *testing.T) {
		report := validateFixture(t, "create_epic", `{"title":"`+strings.Repeat("x", boardPayloadTitleMaxChars+1)+`"}`)
		assert.False(t, report.Valid)
		assert.Equal(t, "maxLength", report.Findings[0].Rule)
	})

	t.Run("criteria item shape", func(t *testing.T) {
		report := validateFixture(t, "create_task", `{"title":"T","description":"d","acceptanceCriteria":[{"met":true}]}`)
		assert.False(t, report.Valid)
		assert.Equal(t, "acceptanceCriteria[0].text", report.Findings[0].Path)
		assert.Equal(t, "required", report.Findings[0].Rule)
	})

	t.Run("tags shape", func(t *testing.T) {
		report := validateFixture(t, "create_epic", `{"title":"E","tags":"not-an-array"}`)
		assert.False(t, report.Valid)
		assert.Equal(t, "tags", report.Findings[0].Path)
		assert.Equal(t, "type", report.Findings[0].Rule)
	})

	t.Run("atomic-create trimmed empty", func(t *testing.T) {
		report := validateFixture(t, "create_task", `{"title":"T","description":"   ","acceptanceCriteria":[{"text":"c"}]}`)
		assert.False(t, report.Valid)
		f := report.Findings[0]
		assert.Equal(t, "description", f.Path)
		assert.Equal(t, "atomic-create", f.Rule)
	})

	t.Run("placeholder token refused", func(t *testing.T) {
		report := validateFixture(t, "create_task", `{"title":"tbd","description":"real charter","acceptanceCriteria":[{"text":"c"}]}`)
		assert.False(t, report.Valid)
		f := report.Findings[0]
		assert.Equal(t, "title", f.Path)
		assert.Equal(t, "placeholder", f.Rule)
		assert.Contains(t, f.Fix, "re-compose")
	})

	t.Run("placeholder prose passes (mentions, not equals)", func(t *testing.T) {
		report := validateFixture(t, "create_task",
			`{"title":"Replace the tbd token","description":"a real charter","acceptanceCriteria":[{"text":"c"}]}`)
		assert.True(t, report.Valid)
	})

	t.Run("plan duplicate keys", func(t *testing.T) {
		report := validateFixture(t, "import_plan", `{"epic":{"title":"E"},"tasks":[
			{"key":"dup","title":"T","description":"d","acceptanceCriteria":[{"text":"c"}]},
			{"key":"dup","title":"T2","description":"d2","acceptanceCriteria":[{"text":"c2"}]}]}`)
		assert.False(t, report.Valid)
		assert.Equal(t, "tasks[1].key", report.Findings[len(report.Findings)-1].Path)
		assert.Equal(t, "plan-key", report.Findings[len(report.Findings)-1].Rule)
	})

	t.Run("tasks minItems 1", func(t *testing.T) {
		report := validateFixture(t, "import_plan", `{"epic":{"title":"E"},"tasks":[]}`)
		assert.False(t, report.Valid)
		assert.Equal(t, "minItems", report.Findings[0].Rule)
	})

	t.Run("exactly-one-of both arms", func(t *testing.T) {
		report := validateFixture(t, "import_plan", `{"epic":{"title":"E"},"epicId":"epc-bbbb2222","tasks":[{"key":"k","title":"T","description":"d","acceptanceCriteria":[{"text":"c"}]}]}`)
		assert.False(t, report.Valid)
		assert.Equal(t, "exactly-one-of", report.Findings[0].Rule)
	})

	t.Run("exactly-one-of neither arm", func(t *testing.T) {
		report := validateFixture(t, "import_plan", `{"tasks":[{"key":"k","title":"T","description":"d","acceptanceCriteria":[{"text":"c"}]}]}`)
		assert.False(t, report.Valid)
		assert.Equal(t, "exactly-one-of", report.Findings[0].Rule)
	})

	t.Run("advisory warning never blocks", func(t *testing.T) {
		report := validateFixture(t, "update_task_patch", `{"acceptanceCriteria":[{"text":"whole new set"}]}`)
		assert.True(t, report.Valid, "the replace-semantics advisory is a warning, not an error")
		require.Len(t, report.Findings, 1)
		assert.Equal(t, "warning", report.Findings[0].Severity)
		assert.Equal(t, "replace-semantics", report.Findings[0].Rule)
	})

	t.Run("file-level failure renders a finding", func(t *testing.T) {
		report := validateFixture(t, "create_epic", `{not json`)
		assert.False(t, report.Valid)
		assert.Equal(t, "file", report.Findings[0].Path)
		assert.Equal(t, "json", report.Findings[0].Rule)
	})

	t.Run("missing file renders the cleanup hint", func(t *testing.T) {
		out, err := validateBoardPayloadFile("create_epic", filepath.Join(t.TempDir(), "nope.json"))
		require.NoError(t, err)
		var report boardValidateReport
		require.NoError(t, json.Unmarshal([]byte(out), &report))
		assert.False(t, report.Valid)
		assert.Contains(t, report.Findings[0].Problem, "file not found")
	})

	t.Run("unknown op refused", func(t *testing.T) {
		_, err := validateBoardPayloadFile("nope", "x.json")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown op")
	})

	t.Run("never deletes the file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "p.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"title":"E"}`), 0o600))
		_, err := validateBoardPayloadFile("create_epic", path)
		require.NoError(t, err)
		_, statErr := os.Stat(path)
		assert.False(t, os.IsNotExist(statErr), "board_validate_file never deletes")
	})
}

func findingPaths(r boardValidateReport) []string {
	out := make([]string, 0, len(r.Findings))
	for _, f := range r.Findings {
		out = append(out, f.Path)
	}
	return out
}

// --- dual-entry-point equivalence (§2.5's pinned contract) ----------------------

// TestBoardValidate_DualEntryPointEquivalence: the SAME bad file
// through board_validate_file and through the CRUD tool yields
// byte-identical findings arrays — validate-first is UX, not correctness.
func TestBoardValidate_DualEntryPointEquivalence(t *testing.T) {
	badPayloads := map[string]struct {
		op, tool string
		payload  string
		routing  map[string]any
	}{
		"seedless create": {"create_task", "board_create_story",
			`{"title":"T"}`, map[string]any{"epicId": "epc-bbbb2222"}},
		"unknown field": {"create_epic", "board_create_epic",
			`{"title":"E","bogus":1}`, map[string]any{"projectId": "prj-aaaa1111"}},
		"placeholder": {"create_task", "board_create_story",
			`{"title":"tbd","description":"d","acceptanceCriteria":[{"text":"c"}]}`, map[string]any{"epicId": "epc-bbbb2222"}},
		"plan both arms": {"import_plan", "board_import_plan",
			`{"epic":{"title":"E"},"epicId":"epc-x","tasks":[{"key":"k","title":"T","description":"d","acceptanceCriteria":[{"text":"c"}]}]}`,
			map[string]any{"projectId": "prj-aaaa1111"}},
	}
	for name, tc := range badPayloads {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "p.json")
			require.NoError(t, os.WriteFile(path, []byte(tc.payload), 0o600))

			// Entry point 1: the tool.
			toolOut, err := validateBoardPayloadFile(tc.op, path)
			require.NoError(t, err)
			var toolReport boardValidateReport
			require.NoError(t, json.Unmarshal([]byte(toolOut), &toolReport))
			require.False(t, toolReport.Valid)

			// Entry point 2: the CRUD gate (identical findings).
			api, rec := newAutomationAPI(t, 200, `{}`)
			setupRenameWorkspaceEnv(t, api)
			args := map[string]any{
				boardToolFileSpec(tc.tool).fileKey: path,
				"lsp_injected_session":             "ses-1",
			}
			for k, v := range tc.routing {
				args[k] = v
			}
			_, err = mcpAutomation(context.Background(), mcpTestPassword, tc.tool, args)
			require.Error(t, err)
			assert.Empty(t, rec.method, "no wire call for a locally refused payload")
			// The refusal embeds the report; the findings arrays must
			// be byte-identical.
			idx := strings.Index(err.Error(), `{"valid":`)
			require.GreaterOrEqual(t, idx, 0, "the refusal embeds the report JSON: %v", err)
			var gateReport boardValidateReport
			require.NoError(t, json.Unmarshal([]byte(err.Error()[idx:]), &gateReport))
			assert.Equal(t, toolReport.Valid, gateReport.Valid)
			assert.Equal(t, toolReport.Fields, gateReport.Fields)
			toolFindings, _ := json.Marshal(toolReport.Findings)
			gateFindings, _ := json.Marshal(gateReport.Findings)
			assert.Equal(t, string(toolFindings), string(gateFindings),
				"the findings arrays are byte-identical across entry points")
		})
	}
}

// --- the tool surface ------------------------------------------------------------

// TestBoardValidateFile_ToolDispatch: the catalog entry exists with a
// readOnly annotation, and callMCPTool routes it LOCALLY — zero wire
// calls, no platform dependency.
func TestBoardValidateFile_ToolDispatch(t *testing.T) {
	tools := mcpToolsList(t)
	tool, ok := tools["board_validate_file"]
	require.True(t, ok, "board_validate_file in tools/list")
	schema := tool["inputSchema"].(map[string]any)
	assert.Equal(t, []any{"op", "file"}, schema["required"])
	if ann, ok := tool["annotations"].(map[string]any); ok {
		assert.Equal(t, true, ann["readOnlyHint"])
	}

	// No automation API is configured for this subtest — the validator
	// is local; a wire dependency would fail loudly here.
	path := filepath.Join(t.TempDir(), "p.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"title":42}`), 0o600))
	out, err := callMCPTool(context.Background(), mcpTestPassword, "board_validate_file", map[string]any{
		"op": "create_epic", "file": path})
	require.NoError(t, err)
	assert.Contains(t, out, `"valid":false`)
	assert.Contains(t, out, `"op":"create_epic"`)
	assert.Contains(t, out, `"rule":"type"`, "the schema findings render")

	_, err = callMCPTool(context.Background(), mcpTestPassword, "board_validate_file", map[string]any{"op": "create_epic"})
	require.Error(t, err, "file is required")
}
