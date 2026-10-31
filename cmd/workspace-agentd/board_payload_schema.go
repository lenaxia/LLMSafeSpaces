// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_payload_schema.go — the strongly typed payload validator (design
// 0070 §2.5, second amendment): ONE implementation with two entry points
// — the board_validate_file tool (advisory, pre-submit) and the CRUD
// dispatch gate (severity=error findings refuse the wire call with
// byte-identical feedback). Validate-first is UX, not correctness: an
// agent that skips validate gets the same findings at submit time.
//
// Engine: santhosh-tekuri/jsonschema/v6, the pkg/workflows/input_schema.go
// precedent — one compiled schema per op, object-rooted,
// additionalProperties:false (unknown fields, nested file keys, and the
// type/shape/cap checks all fall out of the schema), with the `kind`
// violation package surfaced as structured findings carrying JSON
// instance paths ("tasks[2].acceptanceCriteria[0].text") — never
// collapsed to "invalid payload".
//
// Coupling: the schemas' property sets mirror the platform request DTOs
// (typed mirrors: pkg/mcp's BoardTaskBody/BoardEpicBody/BoardPlanBody/
// BoardPlanTaskBody — themselves pinned to the REST request shapes by
// the board parity suites; the api-internal handler DTOs are anonymous
// structs binding the same keys). TestBoardPayloadSchema_DTOParity
// reflects over those structs' json tags and fails CI on drift. Above
// the schema run the semantic checks JSON Schema cannot express: #1849
// atomic-create (trimmed non-empty description/criteria), the
// placeholder-class scan (board.IsPlaceholderClass via
// isPlaceholderClassPayload — one vocabulary, not a fork), plan-key
// uniqueness, and the replace-semantics advisory.
//
// Unlike pkg/workflows' sanitized violations (external trigger payloads),
// these findings face the workspace's own agent reading its own file:
// expected/actual render instance-derived specifics — the caller needs
// them to fix the file.

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// boardValidateOps maps the validator's op vocabulary to the manager
// tool whose DTO the op mirrors (both entry points share it).
var boardValidateOps = map[string]string{
	"create_task":       "board_create_story",
	"create_epic":       "board_create_epic",
	"update_task_patch": "board_update_story",
	"update_epic_patch": "board_update_epic",
	"import_plan":       "board_import_plan",
	"set_config":        "board_set_config",
}

// boardOpForTool is the inverse lookup (the CRUD gate's tool → op).
func boardOpForTool(tool string) string {
	for op, t := range boardValidateOps {
		if t == tool {
			return op
		}
	}
	return ""
}

// Agent-surface caps expressed in the schemas. The design sets title at
// 200 characters; the tag/criterion caps mirror the platform's
// MaxTags/MaxTagBytes/MaxCriterionBytes in schema-expressible form —
// the platform's own byte caps stay authoritative on the wire. The
// persona role-reference cap mirrors board.PersonaRoleIDMax (same
// value, platform-authoritative on the wire).
const (
	boardPayloadTitleMaxChars = 200
	boardPayloadTagMaxItems   = 16
	boardPayloadTagMaxChars   = 64
	boardPayloadCritMaxChars  = 8 << 10
)

// boardPayloadFinding is one finding: WHERE (JSON instance path, arrays
// indexed), WHAT rule broke, how bad it is (error blocks the CRUD gate;
// warning never blocks), the problem in prose, an actionable fix, and
// expected/actual where expressible.
type boardPayloadFinding struct {
	Path     string `json:"path"`
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Problem  string `json:"problem"`
	Fix      string `json:"fix"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
}

// boardValidateReport is the two entry points' shared output shape
// (field order fixes the wire shape the equivalence test pins).
type boardValidateReport struct {
	Valid    bool                  `json:"valid"`
	Op       string                `json:"op"`
	Fields   []string              `json:"fields"`
	Findings []boardPayloadFinding `json:"findings"`
}

// hasErrors reports whether any finding blocks the CRUD gate.
func (r boardValidateReport) hasErrors() bool {
	for _, f := range r.Findings {
		if f.Severity == "error" {
			return true
		}
	}
	return false
}

// --- the five compiled schemas -----------------------------------------------
//
// Property sets mirror the platform DTOs exactly (the parity test below
// pins the coupling). The inline-only control-plane keys are
// DELIBERATELY absent — setCriteriaMet and dryRun are read from INLINE
// tool arguments, and dependsOn is absent from update_task_patch
// because the platform's update DTO does not bind it (a file spelling
// would be silently dropped — refused here instead).

const boardPayloadSchemaCreateTask = `{
	"type": "object",
	"additionalProperties": false,
	"properties": {
		"title":              {"type": "string", "minLength": 1, "maxLength": boardTitleCap},
		"description":        {"type": "string", "minLength": 1},
		"tags":               {"type": "array", "maxItems": boardTagItems, "items": {"type": "string", "minLength": 1, "maxLength": boardTagCap}},
		"acceptanceCriteria": {"type": "array", "minItems": 1, "items": {"$ref": "#/$defs/criterion"}},
		"dependsOn":          {"type": "array", "items": {"type": "string", "minLength": 1}},
		"requestId":          {"type": "string", "minLength": 1}
	},
	"required": ["title", "description", "acceptanceCriteria"]
}`

const boardPayloadSchemaUpdateTaskPatch = `{
	"type": "object",
	"additionalProperties": false,
	"properties": {
		"title":              {"type": "string", "minLength": 1, "maxLength": boardTitleCap},
		"description":        {"type": "string", "minLength": 1},
		"tags":               {"type": "array", "maxItems": boardTagItems, "items": {"type": "string", "minLength": 1, "maxLength": boardTagCap}},
		"acceptanceCriteria": {"type": "array", "minItems": 1, "items": {"$ref": "#/$defs/criterion"}},
		"requestId":          {"type": "string", "minLength": 1}
	}
}`

const boardPayloadSchemaCreateEpic = `{
	"type": "object",
	"additionalProperties": false,
	"properties": {
		"title":     {"type": "string", "minLength": 1, "maxLength": boardTitleCap},
		"outcome":   {"type": "string"},
		"tags":      {"type": "array", "maxItems": boardTagItems, "items": {"type": "string", "minLength": 1, "maxLength": boardTagCap}},
		"requestId": {"type": "string", "minLength": 1}
	},
	"required": ["title"]
}`

const boardPayloadSchemaUpdateEpicPatch = `{
	"type": "object",
	"additionalProperties": false,
	"properties": {
		"title":     {"type": "string", "minLength": 1, "maxLength": boardTitleCap},
		"outcome":   {"type": "string"},
		"tags":      {"type": "array", "maxItems": boardTagItems, "items": {"type": "string", "minLength": 1, "maxLength": boardTagCap}},
		"requestId": {"type": "string", "minLength": 1}
	}
}`

// The unified plan document (design 0070 §1a): one epic arm — the epic
// object (create) or epicId (attach) — plus 1..N tasks. The
// exactly-one-of arm rule is SEMANTIC (both/neither → one clear
// finding; a schema oneOf would render two confusing failures).
const boardPayloadSchemaImportPlan = `{
	"type": "object",
	"additionalProperties": false,
	"properties": {
		"epic": {"type": "object", "additionalProperties": false,
			"properties": {
				"title":   {"type": "string", "minLength": 1, "maxLength": boardTitleCap},
				"outcome": {"type": "string"},
				"tags":    {"type": "array", "maxItems": boardTagItems, "items": {"type": "string", "minLength": 1, "maxLength": boardTagCap}}
			},
			"required": ["title"]},
		"epicId":    {"type": "string", "minLength": 1},
		"tasks":     {"type": "array", "minItems": 1, "items": {
			"type": "object",
			"additionalProperties": false,
			"properties": {
				"key":                {"type": "string", "minLength": 1},
				"title":              {"type": "string", "minLength": 1, "maxLength": boardTitleCap},
				"description":        {"type": "string", "minLength": 1},
				"tags":               {"type": "array", "maxItems": boardTagItems, "items": {"type": "string", "minLength": 1, "maxLength": boardTagCap}},
				"acceptanceCriteria": {"type": "array", "minItems": 1, "items": {"$ref": "#/$defs/criterion"}},
				"dependsOn":          {"type": "array", "items": {"type": "string", "minLength": 1}}
			},
			"required": ["key", "title", "description", "acceptanceCriteria"]
		}},
		"requestId": {"type": "string", "minLength": 1}
	},
	"required": ["tasks"]
}`

// The config override map (board_set_config's configFile): the pure
// key vocabulary of board.BoardConfigOverride — every key optional
// ({} is the legal full-inheritance reset), an unknown key refuses.
// tier/projectId are ROUTING keys and stay inline on the tool call
// (the placement pre-check refuses them in the file). stagePersonas
// values are ROLE REFERENCES ONLY (owner ruling 2026-10-29, round 2):
// a bare role reference string ("in_review": "reviewer" — canonical)
// or the {roleId} object carrying exactly one non-empty roleId (the
// semantic check names the rule — a schema anyOf would render two
// confusing failures). The stage-key vocabulary (persona-eligible
// stages only) is the SEMANTIC check's to name — a propertyNames enum
// would collapse the finding to "does not satisfy propertyNames"
// without naming the stage.
const boardPayloadSchemaSetConfig = `{
	"type": "object",
	"additionalProperties": false,
	"properties": {
		"reviewColumn":       {"type": "string", "enum": ["off", "manual", "auto"]},
		"autoAssignReviewer": {"type": "boolean"},
		"defaultWorkerSlots": {"type": "integer", "minimum": 1},
		"stagePersonas": {"type": "object",
			"additionalProperties": {"oneOf": [{"$ref": "#/$defs/persona"}, {"$ref": "#/$defs/personaRoleRef"}]}}
	}
}`

// boardPayloadSchemasRaw stitches the shared $defs (the criterion item
// mirrors board.AcceptanceCriterion {text, met}; the persona item is
// the ROLE-REFERENCE object — {roleId} and nothing else, mirroring
// board.PersonaConfig (the legacy name/instructions shape is not
// admitted anywhere); the personaRoleRef item is the canonical
// bare-string role reference — the schema's minLength cannot see
// whitespace, so the SEMANTIC check trim-rejects a blank string and
// admission is identical to the platform decoder on both shapes) and
// stamps the cap placeholders (the placeholders keep the caps
// single-sourced as named constants above); one raw schema per op.
func boardPayloadSchemasRaw() map[string]string {
	withDefs := func(body string) string {
		// Splice body's own keywords after $defs inside one root
		// object (drop body's opening brace, keep its keys), then
		// stamp the cap placeholders over the WHOLE document.
		stitched := `{"$defs": {"criterion": {"type": "object", "additionalProperties": false,` +
			`"properties": {"text": {"type": "string", "minLength": 1, "maxLength": boardCritCap}, "met": {"type": "boolean"}},` +
			`"required": ["text"]}, "persona": {"type": "object", "additionalProperties": false,` +
			// The role-only shape: exactly {roleId}. The trim-aware
			// non-empty rule is the SEMANTIC check's (persona-role-ref),
			// identically on both surfaces — the schema cannot see
			// whitespace.
			`"properties": {"roleId": {"type": "string", "minLength": 1, "maxLength": boardPersonaRoleCap}},` +
			`"required": ["roleId"]},` +
			`"personaRoleRef": {"type": "string", "minLength": 1, "maxLength": boardPersonaRoleCap}}, ` +
			strings.TrimSpace(strings.TrimPrefix(body, "{"))
		r := strings.NewReplacer(
			"boardTitleCap", strconv.Itoa(boardPayloadTitleMaxChars),
			"boardTagCap", strconv.Itoa(boardPayloadTagMaxChars),
			"boardTagItems", strconv.Itoa(boardPayloadTagMaxItems),
			"boardCritCap", strconv.Itoa(boardPayloadCritMaxChars),
			"boardPersonaRoleCap", strconv.Itoa(board.PersonaRoleIDMax),
		)
		return r.Replace(stitched)
	}
	return map[string]string{
		"create_task":       withDefs(boardPayloadSchemaCreateTask),
		"create_epic":       withDefs(boardPayloadSchemaCreateEpic),
		"update_task_patch": withDefs(boardPayloadSchemaUpdateTaskPatch),
		"update_epic_patch": withDefs(boardPayloadSchemaUpdateEpicPatch),
		"import_plan":       withDefs(boardPayloadSchemaImportPlan),
		"set_config":        withDefs(boardPayloadSchemaSetConfig),
	}
}

// boardPayloadSchemas compiles each op's schema once (startup cost; a
// schema that fails to compile is a programming error — the eager
// compile test turns it into a CI failure before anything ships).
var boardPayloadSchemas = sync.OnceValue(func() map[string]*jsonschema.Schema {
	out := make(map[string]*jsonschema.Schema, len(boardValidateOps))
	for op, raw := range boardPayloadSchemasRaw() {
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
		if err != nil {
			panic(fmt.Sprintf("board payload schema %s: %v", op, err))
		}
		c := jsonschema.NewCompiler()
		url := "https://llmsafespaces.invalid/board-payload/" + op
		if err := c.AddResource(url, doc); err != nil {
			panic(fmt.Sprintf("board payload schema %s: %v", op, err))
		}
		sch, err := c.Compile(url)
		if err != nil {
			panic(fmt.Sprintf("board payload schema %s: %v", op, err))
		}
		out[op] = sch
	}
	return out
})

// boardPayloadSchemaPropertyNames extracts one raw schema's ROOT
// property names (the parity test's reflection target; plain JSON walk,
// no compilation).
func boardPayloadSchemaPropertyNames(raw string) ([]string, error) {
	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, err
	}
	if doc.Properties == nil {
		return nil, fmt.Errorf("schema has no root properties")
	}
	return slices.Sorted(maps.Keys(doc.Properties)), nil
}

// --- the validator pipeline --------------------------------------------------

// validateBoardPayload runs the whole check pipeline over one decoded
// payload file and returns the report both entry points render:
//
//  1. placement pre-checks (payload-side): file-param keys and
//     routing/control keys inside the file (they are read from INLINE
//     arguments — in the file they would be silently unread);
//  2. the compiled JSON Schema (unknown fields, required, types,
//     shapes, caps — kind violations with instance paths);
//  3. semantic checks above the schema (#1849 trimmed non-empty, the
//     placeholder-class scan, plan-key uniqueness);
//  4. advisory warnings (replace-semantics) — never block.
//
// Findings are deterministically ordered: pipeline stage, then path
// (then rule) within the stage.
func validateBoardPayload(op string, payload map[string]any) boardValidateReport {
	spec := boardToolFileSpec(boardValidateOps[op])
	report := boardValidateReport{Op: op, Fields: slices.Sorted(maps.Keys(payload)), Findings: []boardPayloadFinding{}}

	// (1) placement pre-checks — sorted keys; offending keys are
	// removed from the schema-validated copy so the schema does not
	// double-report them as additionalProperties.
	prepped := make(map[string]any, len(payload))
	for k, v := range payload {
		prepped[k] = v
	}
	for _, k := range slices.Sorted(maps.Keys(payload)) {
		switch {
		case isBoardFileParamKey(k):
			report.Findings = append(report.Findings, boardPayloadFinding{
				Path: k, Rule: "file-param", Severity: "error",
				Problem: fmt.Sprintf("%q is a file param — a payload file IS the long content; one indirection level only", k),
				Fix:     "put the VALUE directly in the payload (files never reference files)",
				Actual:  k,
			})
			delete(prepped, k)
		case slices.Contains(spec.inline, k):
			report.Findings = append(report.Findings, boardPayloadFinding{
				Path: k, Rule: "inline-only", Severity: "error",
				Problem: fmt.Sprintf("%q is routing/control-plane — it stays INLINE (in the file it would be silently unread: routing and control keys are read from the inline arguments)", k),
				Fix:     fmt.Sprintf("remove %q from the file and pass it as an inline tool argument", k),
				Actual:  k,
			})
			delete(prepped, k)
		}
	}

	// (2) the compiled schema, violations surfaced with instance paths.
	if sch := boardPayloadSchemas()[op]; sch != nil {
		if verr := sch.Validate(prepped); verr != nil {
			report.Findings = append(report.Findings, boardSchemaFindings(verr)...)
		}
	}

	// (3) semantics above the schema.
	report.Findings = append(report.Findings, boardSemanticFindings(op, payload)...)

	// (4) advisories — never block.
	if op == "update_task_patch" {
		if _, ok := payload["acceptanceCriteria"]; ok {
			report.Findings = append(report.Findings, boardPayloadFinding{
				Path: "acceptanceCriteria", Rule: "replace-semantics", Severity: "warning",
				Problem:  "acceptanceCriteria in an update patch REPLACES the full criteria set (met flags included)",
				Fix:      "board_read the task first, then send the COMPLETE new criteria array",
				Expected: "the full replacement array",
			})
		}
	}

	report.Valid = !report.hasErrors()
	return report
}

// validateBoardPayloadFile is the board_validate_file tool's arm (also
// the report shape the CRUD gate's refusal embeds): read the file under
// the shared discipline, strict-decode, run the pipeline, render the
// report JSON. File-level and JSON-level failures become findings on
// "file" — the report shape never varies by failure class. NEVER
// deletes (a validate pass must leave the file for the submit).
func validateBoardPayloadFile(op, path string) (string, error) {
	if _, ok := boardValidateOps[op]; !ok {
		return "", fmt.Errorf("unknown op %q — one of create_task, create_epic, update_task_patch, update_epic_patch, import_plan, set_config", op)
	}
	report := boardValidateReport{Op: op, Fields: []string{}, Findings: []boardPayloadFinding{}}
	content, _, err := boardReadFileField("file", path, boardPayloadFileMaxBytes)
	if err != nil {
		report.Findings = append(report.Findings, boardPayloadFinding{
			Path: "file", Rule: "file", Severity: "error",
			Problem: err.Error(),
			Fix:     "write the JSON payload with the write tool (relative paths resolve against the workspace root), then pass its path",
		})
		report.Valid = false
		return boardRenderReport(report)
	}
	fields, err := decodeBoardPayloadJSON([]byte(content))
	if err != nil {
		report.Findings = append(report.Findings, boardPayloadFinding{
			Path: "file", Rule: "json", Severity: "error",
			Problem: err.Error(),
			Fix:     "emit exactly ONE JSON object — no duplicate keys, no trailing content, object at the top level",
		})
		report.Valid = false
		return boardRenderReport(report)
	}
	return boardRenderReport(validateBoardPayload(op, fields))
}

// boardValidateReportJSON renders a report for embedding (the CRUD
// gate's refusal carries the same bytes the tool would return).
func boardValidateReportJSON(r boardValidateReport) string {
	b, err := json.Marshal(r)
	if err != nil {
		return `{"valid":false}`
	}
	return string(b)
}

func boardRenderReport(r boardValidateReport) (string, error) {
	return boardValidateReportJSON(r), nil
}

// --- schema violations → findings ---------------------------------------------

// boardSchemaFindings walks one ValidationError's typed trail (the
// kind-package precedent: container nodes skipped, one finding per
// violation) and renders each with rule/problem/fix/expected/actual.
func boardSchemaFindings(verr error) []boardPayloadFinding {
	var ve *jsonschema.ValidationError
	if !errors.As(verr, &ve) {
		return []boardPayloadFinding{{
			Path: "", Rule: "schema", Severity: "error",
			Problem: "the payload does not satisfy this op's schema",
			Fix:     "compare the payload against the op's admitted fields and types",
		}}
	}
	var out []boardPayloadFinding
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if e == nil || e.ErrorKind == nil {
			return
		}
		switch e.ErrorKind.(type) {
		case *kind.Schema, *kind.Group, *kind.Reference:
			// Container wrappers — the causes carry the violations.
		default:
			out = append(out, boardFindingsForKind(e)...)
		}
		for _, cause := range e.Causes {
			walk(cause)
		}
	}
	walk(ve)
	slices.SortStableFunc(out, func(a, b boardPayloadFinding) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return strings.Compare(a.Rule, b.Rule)
	})
	return out
}

// boardFindingsForKind renders one violation into findings. Unlike the
// sanitized workflows records, expected/actual ARE rendered (the
// library's own Got values) — the reader owns this file and needs the
// specifics to fix it.
func boardFindingsForKind(e *jsonschema.ValidationError) []boardPayloadFinding {
	path := boardInstancePath(e.InstanceLocation)

	switch k := e.ErrorKind.(type) {
	case *kind.Required:
		var out []boardPayloadFinding
		for _, missing := range k.Missing {
			out = append(out, boardPayloadFinding{
				Path: boardJoinPath(path, missing), Rule: "required", Severity: "error",
				Problem:  fmt.Sprintf("missing property %q", missing),
				Fix:      "add the field to the payload file",
				Expected: "present",
			})
		}
		return out
	case *kind.Type:
		want := strings.Join(k.Want, " or ")
		return []boardPayloadFinding{{
			Path: path, Rule: "type", Severity: "error",
			Problem:  fmt.Sprintf("wrong type — expected %s", want),
			Fix:      fmt.Sprintf("convert the value to %s", want),
			Expected: want, Actual: k.Got,
		}}
	case *kind.AdditionalProperties:
		var out []boardPayloadFinding
		for _, prop := range k.Properties {
			out = append(out, boardPayloadFinding{
				Path: boardJoinPath(path, prop), Rule: "additionalProperties", Severity: "error",
				Problem:  fmt.Sprintf("unknown field %q — not part of this op's request DTO", prop),
				Fix:      "remove it, or move it to the op that admits it (the platform would silently ignore it)",
				Expected: "only this op's admitted payload fields", Actual: prop,
			})
		}
		return out
	case *kind.MinLength:
		return []boardPayloadFinding{{
			Path: path, Rule: "minLength", Severity: "error",
			Problem:  fmt.Sprintf("string is shorter than the minimum of %d characters", k.Want),
			Fix:      "write actual content (whitespace-only values fail the platform's trimmed check too)",
			Expected: ">= " + strconv.Itoa(k.Want) + " characters", Actual: strconv.Itoa(k.Got) + " characters",
		}}
	case *kind.MaxLength:
		return []boardPayloadFinding{{
			Path: path, Rule: "maxLength", Severity: "error",
			Problem:  fmt.Sprintf("string is longer than the maximum of %d characters", k.Want),
			Fix:      "shorten the value",
			Expected: "<= " + strconv.Itoa(k.Want) + " characters", Actual: strconv.Itoa(k.Got) + " characters",
		}}
	case *kind.MinItems:
		return []boardPayloadFinding{{
			Path: path, Rule: "minItems", Severity: "error",
			Problem:  fmt.Sprintf("array has fewer than the minimum of %d items", k.Want),
			Fix:      "add the missing items (#1849: Done is undefined without at least one criterion)",
			Expected: ">= " + strconv.Itoa(k.Want) + " items", Actual: strconv.Itoa(k.Got) + " items",
		}}
	case *kind.MaxItems:
		return []boardPayloadFinding{{
			Path: path, Rule: "maxItems", Severity: "error",
			Problem:  fmt.Sprintf("array has more than the maximum of %d items", k.Want),
			Fix:      "drop items down to the cap",
			Expected: "<= " + strconv.Itoa(k.Want) + " items", Actual: strconv.Itoa(k.Got) + " items",
		}}
	default:
		return []boardPayloadFinding{{
			Path: path, Rule: boardKeyword(e), Severity: "error",
			Problem: "the value does not satisfy the " + boardKeyword(e) + " rule",
			Fix:     "compare the value against the op's schema at " + path,
		}}
	}
}

// boardKeyword takes the violation's keyword path tail (the rule name).
func boardKeyword(e *jsonschema.ValidationError) string {
	if p := e.ErrorKind.KeywordPath(); len(p) > 0 {
		return p[len(p)-1]
	}
	return "schema"
}

// boardInstancePath renders the library's instance location tokens in
// the design's display form — tasks[2].acceptanceCriteria[0].text
// (all-digit tokens are array indices, object keys dot-joined; "" at
// the document root).
func boardInstancePath(tokens []string) string {
	var sb strings.Builder
	for _, tok := range tokens {
		switch {
		case sb.Len() == 0:
			sb.WriteString(tok)
		case isIndexToken(tok):
			sb.WriteByte('[')
			sb.WriteString(tok)
			sb.WriteByte(']')
		default:
			sb.WriteByte('.')
			sb.WriteString(tok)
		}
	}
	return sb.String()
}

func isIndexToken(tok string) bool {
	if tok == "" {
		return false
	}
	for _, r := range tok {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func boardJoinPath(path, token string) string {
	if path == "" {
		return token
	}
	return path + "." + token
}

// --- semantic checks above the schema -----------------------------------------

// boardSemanticFindings: #1849 trimmed non-empty seeds, the
// placeholder-class scan (board.IsPlaceholderClass via
// isPlaceholderClassPayload — ONE vocabulary with send_message and the
// engine's seed guard), and plan-key uniqueness. The schema handles
// presence/shape/caps; these handle the value-level and cross-field
// rules. Type-guarded throughout: wrong shapes are the schema's
// findings, never semantics' crashes. Sorted by path within the stage.
func boardSemanticFindings(op string, payload map[string]any) []boardPayloadFinding {
	var out []boardPayloadFinding
	addPlaceholder := func(path string, v any) {
		if s, ok := v.(string); ok && isPlaceholderClassPayload(strings.TrimSpace(s)) {
			out = append(out, boardPayloadFinding{
				Path: path, Rule: "placeholder", Severity: "error",
				Problem: "the value is ENTIRELY a placeholder-class token (#1849 rejects it outright — suspected emission-corruption substitute)",
				Fix:     "re-read the draft and re-compose; prose that MERELY mentions such a token is fine",
			})
		}
	}
	addTrimmedEmpty := func(path string, v any) {
		if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
			out = append(out, boardPayloadFinding{
				Path: path, Rule: "atomic-create", Severity: "error",
				Problem:  "the value is empty after trimming — a seedless field hands the worker nothing (#1849)",
				Fix:      "write the actual content",
				Expected: "non-empty after trimming",
			})
		}
	}

	switch op {
	case "create_task", "update_task_patch":
		addPlaceholder("title", payload["title"])
		addPlaceholder("description", payload["description"])
		addTrimmedEmpty("description", payload["description"])
		for i, f := range boardCriteriaItems(payload["acceptanceCriteria"]) {
			addPlaceholder(fmt.Sprintf("acceptanceCriteria[%d].text", i), f["text"])
			addTrimmedEmpty(fmt.Sprintf("acceptanceCriteria[%d].text", i), f["text"])
		}
	case "create_epic", "update_epic_patch":
		addPlaceholder("title", payload["title"])
	case "set_config":
		// The stage-key vocabulary check the schema deliberately leaves
		// to semantics (naming the stage, not "propertyNames"): a
		// persona key on a stage a live claim cannot exist in would
		// render an "effective" config the engine never honors.
		if personas, ok := payload["stagePersonas"].(map[string]any); ok {
			for stage := range personas {
				if stage != board.StageInProgress && stage != board.StageInReview {
					out = append(out, boardPayloadFinding{
						Path: "stagePersonas." + stage, Rule: "stage-not-eligible", Severity: "error",
						Problem:  fmt.Sprintf("stage %q cannot hold a persona (persona-eligible stages: in_progress, in_review — backlog is the claim pool, done is terminal, pending_input keeps its suspended worker)", stage),
						Fix:      "move the persona to in_progress or in_review, or drop the key",
						Expected: "in_progress | in_review",
						Actual:   stage,
					})
				}
			}
			// The role-reference rule the object schema's anyOf leaves
			// to semantics (a persona object carries exactly one
			// non-empty, trim-aware roleId — an object with a
			// whitespace-only or absent roleId is the empty persona).
			// Bare-string values carry the reference but are STILL
			// trim-checked: the platform decoder rejects a
			// whitespace-only string, and this surface must agree —
			// identical admission on both surfaces.
			for stage, v := range personas {
				if s, ok := v.(string); ok {
					if strings.TrimSpace(s) == "" {
						out = append(out, boardPayloadFinding{
							Path: "stagePersonas." + stage, Rule: "persona-role-ref", Severity: "error",
							Problem:  "the bare-string role reference is empty after trimming — a persona IS a role reference (owner ruling 2026-10-29, role-only)",
							Fix:      "set the value to an available agent role's id or slug (see GET /api/v1/auth/me/agent-roles)",
							Expected: "non-empty role reference",
						})
					}
					continue
				}
				p, ok := v.(map[string]any)
				if !ok {
					continue
				}
				roleID, _ := p["roleId"].(string)
				if strings.TrimSpace(roleID) == "" {
					out = append(out, boardPayloadFinding{
						Path: "stagePersonas." + stage, Rule: "persona-role-ref", Severity: "error",
						Problem:  "the persona object carries no non-empty roleId — a persona IS a role reference (owner ruling 2026-10-29, role-only); the board defines no behavior of its own",
						Fix:      "set roleId to an available agent role (see GET /api/v1/auth/me/agent-roles)",
						Expected: "roleId (non-empty)",
					})
				}
			}
		}
		// The two tier-dependent advisories the schema cannot see (the
		// file carries no tier): warnings, never errors — the platform
		// refuses the user-level-key shape at the project tier, and a
		// suppression here is a legitimate opt-out the caller must
		// merely make deliberately.
		if _, ok := payload["defaultWorkerSlots"]; ok {
			out = append(out, boardPayloadFinding{
				Path: "defaultWorkerSlots", Rule: "tier-scope", Severity: "warning",
				Problem: "defaultWorkerSlots is a USER-level key — it refuses at tier=project (a project's slots are fixed at create; set them via board_update_project)",
				Fix:     "send tier=user for the board-wide default, or drop the key from a project-tier set",
			})
		}
		if personas, ok := payload["stagePersonas"].(map[string]any); ok {
			if _, listed := personas["in_review"]; !listed {
				out = append(out, boardPayloadFinding{
					Path: "stagePersonas", Rule: "reviewer-suppression", Severity: "warning",
					Problem: "an explicit stagePersonas map OWNS every stage it does not list — this one has no in_review entry, so whatever in_review runs on today (an inherited persona, or the built-in auto-reviewer under the reviewColumn=auto + autoAssignReviewer composition) is suppressed",
					Fix:     fmt.Sprintf("deliberate opt-out: keep it; otherwise add an in_review entry — the default reviewer is %q (board_get_config renders it under personas.in_review when the composition is live)", board.DefaultReviewerPersona.RoleID),
				})
			}
		}
	case "import_plan":
		// The unified plan document's arm rule (§1a): exactly one of
		// the epic object (create) / epicId (attach). Schema oneOf
		// cannot say this without two confusing failures — one clear
		// finding here, naming the rule.
		_, hasEpic := payload["epic"]
		_, hasEpicID := payload["epicId"]
		switch {
		case hasEpic && hasEpicID:
			out = append(out, boardPayloadFinding{
				Path: "epic", Rule: "exactly-one-of", Severity: "error",
				Problem:  "the plan carries BOTH epic arms — a new-epic definition (epic) AND an existing epicId",
				Fix:      "keep exactly one: epic (create a new epic with these tasks) or epicId (attach these tasks to an existing epic)",
				Expected: "exactly one of epic | epicId",
			})
		case !hasEpic && !hasEpicID:
			out = append(out, boardPayloadFinding{
				Path: "", Rule: "exactly-one-of", Severity: "error",
				Problem:  "the plan carries NO epic arm — tasks need a target",
				Fix:      "add the epic object (create a new epic with these tasks) or epicId (attach these tasks to an existing epic); a single task may instead ride board_create_story's flat bodyFile",
				Expected: "exactly one of epic | epicId",
			})
		}
		if epic, ok := payload["epic"].(map[string]any); ok {
			addPlaceholder("epic.title", epic["title"])
			addTrimmedEmpty("epic.title", epic["title"])
		}
		seen := map[string]bool{}
		for i, task := range boardPlanTasks(payload["tasks"]) {
			node := fmt.Sprintf("tasks[%d]", i)
			addPlaceholder(node+".title", task["title"])
			addPlaceholder(node+".description", task["description"])
			addTrimmedEmpty(node+".description", task["description"])
			for j, f := range boardCriteriaItems(task["acceptanceCriteria"]) {
				addPlaceholder(fmt.Sprintf("%s.acceptanceCriteria[%d].text", node, j), f["text"])
				addTrimmedEmpty(fmt.Sprintf("%s.acceptanceCriteria[%d].text", node, j), f["text"])
			}
			if key, ok := task["key"].(string); ok && key != "" {
				if seen[key] {
					out = append(out, boardPayloadFinding{
						Path: node + ".key", Rule: "plan-key", Severity: "error",
						Problem: fmt.Sprintf("duplicate plan key %q — tasks address each other by key (dependsOn), so keys must be unique", key),
						Fix:     "give every node a distinct key",
						Actual:  key,
					})
				}
				seen[key] = true
			}
		}
	}

	slices.SortStableFunc(out, func(a, b boardPayloadFinding) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return strings.Compare(a.Rule, b.Rule)
	})
	return out
}

// boardCriteriaItems extracts criterion objects, type-guarded.
func boardCriteriaItems(raw any) []map[string]any {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// boardPlanTasks extracts plan node objects in order, type-guarded.
func boardPlanTasks(raw any) []map[string]any {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
