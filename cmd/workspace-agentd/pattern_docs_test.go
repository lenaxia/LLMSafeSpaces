// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pattern_docs_test.go — pins for the worked-pattern example directories
// (2026-10-30 owner ruling, tsk-rested-shanty-feels): the Sprint Board
// story patterns and the workflow DAG patterns, one file per example,
// embedded via //go:embed <dir>/*.md and materialized by init-fs under
// $runtimeDir/docs/.
//
//   - FORMAT PINS: every embedded file carries the canonical greppable
//     header — `# <title>` / `<!-- pattern: slug=…; doc=…; use-case=…;
//     nodes=…; wiring=… -->` / `Description: <one sentence>` — with
//     slug == the filename stem (grep is the catalog; no generated INDEX).
//   - CATALOG-COVERAGE PINS: the board catalog carries all six use-case
//     themes; the workflow catalog collectively spans the four node
//     types and the three trigger wirings (cron; webhook
//     envelope/body/mapped; manual) with ≥5 canonical DAGs.
//   - CONTENT PINS: the canonical sections each doc exists to carry —
//     a rewrite cannot silently drop one (the gate-doc pin pattern).
//   - DRIFT PINS: the embedded copies are byte-identical, file for
//     file, to the sources of truth under docs/automation/ — the sets
//     match exactly (no source-only file, no embed-only file).
//   - INFRA-NEUTRALITY PINS: the vendor blocklist scan over BOTH
//     directories (the board help-drawer's binding rule: artifacts and
//     verification outcomes, never tooling or vendors) plus the Sprint
//     Board vocabulary pin (never "task board", the wire-term note
//     stated once).
//   - MATERIALIZATION: ensureEmbeddedPatternDir mirrors the embedded
//     set into the destination (identical files untouched, stale files
//     replaced, files dropped from the image removed).

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// patternDocSet is one embedded directory plus its identity: the path
// inside the embed.FS, the source-of-truth path under docs/automation/,
// and the doc= value every header in it must carry.
type patternDocSet struct {
	fsys     embed.FS
	embedded string // e.g. "sprint_board_patterns"
	source   string // e.g. "../../docs/automation/sprint-board-patterns" (tests run with CWD = package dir)
	docKind  string // "board" | "workflow"
}

var patternDocSets = []patternDocSet{
	{sprintBoardPatternsFS, "sprint_board_patterns", filepath.Join("..", "..", "docs", "automation", "sprint-board-patterns"), "board"},
	{workflowPatternsFS, "workflow_patterns", filepath.Join("..", "..", "docs", "automation", "workflow-patterns"), "workflow"},
}

var (
	validUseCases = map[string]bool{
		"code": true, "research": true, "creative-writing": true,
		"data-analytics": true, "data-post-processing": true,
		"batch-content-ops": true, "mechanics": true,
	}
	validNodeTypes = map[string]bool{
		"script": true, "agent": true, "http": true, "condition": true,
	}
	validWirings = map[string]bool{
		"cron": true, "webhook-envelope": true, "webhook-body": true,
		"webhook-mapped": true, "manual": true, "none": true,
	}
)

// sixUseCaseThemes: the taxonomy the board help drawer ships (owner
// ruling 2026-10-30) — the board catalog must carry all six.
var sixUseCaseThemes = []string{
	"code", "research", "creative-writing",
	"data-analytics", "data-post-processing", "batch-content-ops",
}

// patternHeader is the parsed canonical header of one example file.
type patternHeader struct {
	slug, docKind, useCase, nodesCSV, wiring string
	description                              string
}

// parsePatternHeader validates the first three lines of an example file
// and returns the parsed header. The canonical form (owner ruling,
// binding):
//
//	# <Human title>
//	<!-- pattern: slug=<kebab-slug>; doc=<board|workflow>; use-case=<…>; nodes=<csv or none>; wiring=<…> -->
//	Description: <one sentence>.
func parsePatternHeader(t *testing.T, name, content string) patternHeader {
	t.Helper()
	lines := strings.Split(content, "\n")
	require.GreaterOrEqual(t, len(lines), 3, "%s: too short to carry the canonical header", name)
	assert.True(t, strings.HasPrefix(lines[0], "# "), "%s: line 1 must be '# <title>'", name)
	assert.NotContains(t, lines[0], "\r", "%s: no CRLF line endings", name)

	require.Regexp(t, `^<!-- pattern: `, lines[1], "%s: line 2 must be the pattern comment", name)
	fields := map[string]string{}
	for _, kv := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(lines[1], "<!-- pattern: "), " -->"), "; ") {
		parts := strings.SplitN(kv, "=", 2)
		require.Len(t, parts, 2, "%s: header field %q must be key=value", name, kv)
		fields[parts[0]] = parts[1]
	}
	h := patternHeader{
		slug: fields["slug"], docKind: fields["doc"], useCase: fields["use-case"],
		nodesCSV: fields["nodes"], wiring: fields["wiring"],
	}
	assert.NotEmpty(t, h.slug, "%s: slug required", name)
	assert.NotEmpty(t, h.docKind, "%s: doc required", name)
	assert.NotEmpty(t, h.useCase, "%s: use-case required", name)
	assert.NotEmpty(t, h.nodesCSV, "%s: nodes required (csv or 'none')", name)
	assert.NotEmpty(t, h.wiring, "%s: wiring required", name)

	assert.True(t, validUseCases[h.useCase], "%s: use-case %q not in the canonical enum", name, h.useCase)
	assert.True(t, validWirings[h.wiring], "%s: wiring %q not in the canonical enum", name, h.wiring)
	stem := strings.TrimSuffix(filepath.Base(name), ".md")
	assert.Equal(t, stem, h.slug, "%s: slug must equal the filename stem (grep is the catalog)", name)
	if h.nodesCSV != "none" {
		seen := map[string]bool{}
		for _, n := range strings.Split(h.nodesCSV, ",") {
			assert.True(t, validNodeTypes[n], "%s: node type %q not in the vocabulary", name, n)
			assert.False(t, seen[n], "%s: duplicate node type %q", name, n)
			seen[n] = true
		}
	}

	require.True(t, strings.HasPrefix(lines[2], "Description: "), "%s: line 3 must be 'Description: <one sentence>'", name)
	h.description = strings.TrimPrefix(lines[2], "Description: ")
	assert.Greater(t, len(h.description), 10, "%s: the Description line must carry a real sentence", name)
	return h
}

// readPatternDir loads an embedded directory into name → {content, header}.
func readPatternDir(t *testing.T, set patternDocSet) map[string]patternHeader {
	t.Helper()
	out := map[string]patternHeader{}
	err := fs.WalkDir(set.fsys, set.embedded, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		require.True(t, strings.HasSuffix(path, ".md"), "only .md files belong in the embedded pattern dir: %s", path)
		b, err := set.fsys.ReadFile(path)
		if err != nil {
			return err
		}
		name := filepath.Base(path)
		// NO generated INDEX: the catalog is grepped from the example
		// files themselves — a hand-maintained or generated index file
		// rots and is refused outright.
		lower := strings.ToLower(name)
		assert.NotContains(t, lower, "index", "no INDEX file — grep is the catalog: %s", name)
		assert.NotContains(t, lower, "readme", "no README file — the headers self-index: %s", name)
		out[name] = parsePatternHeader(t, name, string(b))
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, out, "%s must embed at least one example", set.embedded)
	return out
}

// TestPatternDocs_FormatPins_CanonicalHeader: every embedded example
// file carries the canonical greppable header, slug == filename stem,
// doc matches its directory, and the board/worksheet invariants hold
// (board files carry no DAG metadata; workflow DAG files carry both a
// node set and a wiring).
func TestPatternDocs_FormatPins_CanonicalHeader(t *testing.T) {
	for _, set := range patternDocSets {
		for name, h := range readPatternDir(t, set) {
			assert.Equal(t, set.docKind, h.docKind, "%s: doc= must match the directory", name)
			if set.docKind == "board" {
				assert.Equal(t, "none", h.nodesCSV, "%s: board patterns carry no DAG nodes", name)
				assert.Equal(t, "none", h.wiring, "%s: board patterns carry no trigger wiring", name)
			} else if h.useCase != "mechanics" {
				assert.NotEqual(t, "none", h.nodesCSV, "%s: a canonical DAG names its node types", name)
				assert.NotEqual(t, "none", h.wiring, "%s: a canonical DAG names its trigger wiring (or 'manual')", name)
			}
		}
	}
}

// TestPatternDocs_CatalogCoverage: the six use-case themes all appear in
// the board catalog; the workflow catalog collectively exercises all
// four node types and the three trigger wirings (cron; webhook
// envelope/body/mapped; manual), with ≥5 canonical DAGs.
func TestPatternDocs_CatalogCoverage(t *testing.T) {
	board := readPatternDir(t, patternDocSets[0])
	useCases := map[string]bool{}
	for _, h := range board {
		useCases[h.useCase] = true
	}
	for _, theme := range sixUseCaseThemes {
		assert.True(t, useCases[theme], "board catalog: the six-theme taxonomy requires use-case=%s", theme)
	}

	wf := readPatternDir(t, patternDocSets[1])
	nodeTypes, wirings := map[string]bool{}, map[string]bool{}
	dags := 0
	for _, h := range wf {
		if h.nodesCSV != "none" {
			dags++
		}
		if h.nodesCSV != "none" {
			for _, n := range strings.Split(h.nodesCSV, ",") {
				nodeTypes[n] = true
			}
		}
		if h.wiring != "none" {
			wirings[h.wiring] = true
		}
	}
	assert.GreaterOrEqual(t, dags, 5, "workflow catalog: at least five canonical DAGs")
	for _, n := range []string{"script", "agent", "http", "condition"} {
		assert.True(t, nodeTypes[n], "workflow catalog: node type %s must appear somewhere", n)
	}
	for _, w := range []string{"cron", "webhook-envelope", "webhook-body", "webhook-mapped", "manual"} {
		assert.True(t, wirings[w], "workflow catalog: wiring %s must appear somewhere", w)
	}
}

// TestPatternDocs_ContentPins_BoardCatalog: the canonical board sections
// — the process-discipline rules every charter hand-copies, the three
// lifecycles, the wire-term note, and the reviewer-verifiable criteria
// lesson — survive any doc rewrite.
func TestPatternDocs_ContentPins_BoardCatalog(t *testing.T) {
	full := concatEmbeddedDir(t, patternDocSets[0])
	for _, want := range []string{
		// Vocabulary: Sprint Board stories, the wire term noted ONCE.
		"story (the wire/API term is 'task')", // the one-time wire-term note
		"Sprint Board",                        // the vocabulary itself
		// Process discipline (§C) — the section charters reference.
		"AI reviewer until APPROVED", // review is verdict-gated
		"Never self-merge",           // the orchestrator merges
		"make fmt-check",             // format gate before every push
		"No sleep-polling",           // push, verify, END the turn
		"evidenceUrl",                // the evidence contract on handoff
		// Lifecycles (§B): happy, fail-bounce, salvage.
		"board_record_verdict", // the reviewer's structured verdict
		"SAME pull request",    // fail-bounce iterates on the same artifact
		"salvage note",         // inheritance, not restart
		"session_read",         // where a successor reads a lapsed attempt
		// Shapes (§D): single story, epic+plan with a dependency edge,
		// reviewer-checkable criteria.
		"board_create_task", // the flat single-story shape
		"board_import_plan", // the epic+plan shape
		"dependsOn",         // the dependency edge, in the payload
		"dryRun",            // the plan-approval moment
		"verification move", // reviewer-checkable: the how-to-check named
	} {
		assert.Contains(t, full, want, "board catalog: canonical fragment %q must stay", want)
	}
}

// TestPatternDocs_ContentPins_WorkflowCatalog: the canonical workflow
// sections — the placeholder rules with dotted-path walks, the
// gate-script cross-reference, idempotency/dedup for re-fires, and the
// inputSchema gotchas that cause validation_error fires.
func TestPatternDocs_ContentPins_WorkflowCatalog(t *testing.T) {
	full := concatEmbeddedDir(t, patternDocSets[1])
	for _, want := range []string{
		// Placeholder rules with dotted-path walks.
		"{{.body.pull_request.title}}", // the dotted walk, worked
		"stay literal",                 // unresolvable refs stay literal
		"compact JSON",                 // composite rendering stated
		"IMMEDIATE upstream",           // the input-is-the-previous-output trap
		// Gate-script cross-reference: the pod-side doc path, verbatim.
		"/sandbox-runtime/docs/gate-script-patterns.md",
		// Idempotency/dedup for re-fires.
		"at-least-once", // the delivery contract
		"signature",     // the marker-file signature idiom
		"marker",        // marker files under /workspace (durable)
		// inputSchema enforcement gotchas (validation_error fires).
		"validation_error",     // the failed-fire status named
		"additionalProperties", // the strict-schema/mapped-mode lesson
		"is enforced on",       // the enforcement map, verbatim semantics
		"manual runs",          // enforcement point 1
		"input mapping",        // enforcement point 2 (mapped fires)
	} {
		assert.Contains(t, full, want, "workflow catalog: canonical fragment %q must stay", want)
	}
	// The wiring-vocabulary fragments (each canonical wiring worked).
	for _, want := range []string{"inputFrom", "workflowId", "workflow_run"} {
		assert.Contains(t, full, want, "workflow catalog: wiring fragment %q must stay", want)
	}
}

// TestPatternDocs_InfraNeutral_VendorBlocklist: the binding rule from
// the board help drawer, applied to BOTH directories — every example
// names ARTIFACTS and VERIFICATION OUTCOMES, never tooling or vendors.
// The list pins the obvious candidates; the rule itself is broader.
func TestPatternDocs_InfraNeutral_VendorBlocklist(t *testing.T) {
	banned := []string{
		"github", "gitlab", "actions", "jenkins", "circleci",
		"jira", "slack", "notion", "excel", "google", "openai", "copilot",
	}
	for _, set := range patternDocSets {
		full := strings.ToLower(concatEmbeddedDir(t, set))
		for _, vendor := range banned {
			assert.NotContains(t, full, vendor,
				"%s: infra-neutral rule — artifacts and outcomes, never the vendor %q", set.embedded, vendor)
		}
		// Sprint Board vocabulary: never "task board".
		assert.NotContains(t, full, "task board",
			"%s: the vocabulary is Sprint Board / stories", set.embedded)
	}
}

// TestPatternDocs_DriftPins_SourceOfTruth: every embedded file is
// byte-identical to its source under docs/automation/, and the file
// SETS match exactly — go:embed cannot reach outside the package dir,
// so the embed is a checked-in copy; this pin makes silent divergence
// (either direction, including an added file only one side) impossible.
func TestPatternDocs_DriftPins_SourceOfTruth(t *testing.T) {
	for _, set := range patternDocSets {
		embedded := readEmbeddedNames(t, set)
		srcEntries, err := os.ReadDir(set.source)
		require.NoError(t, err, "%s (the source of truth) must exist", set.source)
		source := map[string]bool{}
		for _, e := range srcEntries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			source[e.Name()] = true
		}
		assert.ElementsMatch(t, keysOf(source), keysOf(embedded),
			"%s: the embedded file set must equal the source set", set.embedded)
		for name := range source {
			want, err := os.ReadFile(filepath.Join(set.source, name))
			require.NoError(t, err)
			got, err := set.fsys.ReadFile(filepath.Join(set.embedded, name))
			require.NoError(t, err)
			assert.Equal(t, string(want), string(got),
				"%s has drifted — re-copy docs/automation/… to cmd/workspace-agentd/%s", name, set.embedded)
		}
	}
}

// TestEnsureEmbeddedPatternDir_MirrorsTheEmbeddedSet: materialization is
// idempotent AND mirrors — absent files land (0644), identical files are
// untouched (stable mtime), a stale file is replaced, and a file dropped
// from the image is REMOVED (no orphan stays grep-findable at the pod
// path a tool description names).
func TestEnsureEmbeddedPatternDir_MirrorsTheEmbeddedSet(t *testing.T) {
	dir := t.TempDir()
	set := patternDocSets[0] // the board dir — any embedded set proves the mechanism

	require.NoError(t, ensureEmbeddedPatternDir(set.fsys, set.embedded, dir))
	names := readEmbeddedNames(t, set)
	require.NotEmpty(t, names)
	mtimes := map[string]int64{}
	for name := range names {
		path := filepath.Join(dir, name)
		got, err := os.ReadFile(path)
		require.NoError(t, err, "%s must materialize", name)
		want, err := set.fsys.ReadFile(filepath.Join(set.embedded, name))
		require.NoError(t, err)
		assert.Equal(t, string(want), string(got))
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "%s: public documentation", name)
		mtimes[name] = info.ModTime().UnixNano()
	}

	// Identical re-run: no rewrite (stable mtimes).
	require.NoError(t, ensureEmbeddedPatternDir(set.fsys, set.embedded, dir))
	for name, mtime := range mtimes {
		info, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err)
		assert.Equal(t, mtime, info.ModTime().UnixNano(), "%s must not be rewritten", name)
	}

	// Stale content replaced; an orphan (dropped from the image) removed.
	var anyName string
	for name := range names {
		anyName = name
		break
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, anyName), []byte("# an older release's example\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "orphaned-example.md"), []byte("# gone from the image\n"), 0o644))
	require.NoError(t, ensureEmbeddedPatternDir(set.fsys, set.embedded, dir))
	want, err := set.fsys.ReadFile(filepath.Join(set.embedded, anyName))
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(dir, anyName))
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got), "stale example must be replaced by the embedded bytes")
	_, err = os.Stat(filepath.Join(dir, "orphaned-example.md"))
	assert.ErrorIs(t, err, os.ErrNotExist, "a file dropped from the image must be removed from the pod dir")
}

// TestEnsureEmbeddedPatternDir_ErrorPaths: the unhappy legs surface
// wrapped errors carrying the step's identity and strand no temp files.
func TestEnsureEmbeddedPatternDir_ErrorPaths(t *testing.T) {
	set := patternDocSets[0]

	t.Run("dst dir path is an existing file", func(t *testing.T) {
		parent := t.TempDir()
		blocker := filepath.Join(parent, "docs")
		require.NoError(t, os.WriteFile(blocker, []byte("a file where the dir belongs"), 0o644))
		err := ensureEmbeddedPatternDir(set.fsys, set.embedded, blocker)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "pattern-docs dir")
	})

	t.Run("dst file path is an existing directory", func(t *testing.T) {
		dir := t.TempDir()
		var anyName string
		for name := range readEmbeddedNames(t, set) {
			anyName = name
			break
		}
		require.NoError(t, os.MkdirAll(filepath.Join(dir, anyName), 0o755))
		err := ensureEmbeddedPatternDir(set.fsys, set.embedded, dir)
		require.Error(t, err, "rename of the temp file onto a directory must surface an error")
		assert.Contains(t, err.Error(), "pattern-docs rename")
		entries, readErr := os.ReadDir(dir)
		require.NoError(t, readErr)
		for _, e := range entries {
			assert.False(t, strings.HasPrefix(e.Name(), ".pattern-docs-"), "no temp strays: %s", e.Name())
		}
	})
}

// --- helpers ---

func readEmbeddedNames(t *testing.T, set patternDocSet) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := fs.WalkDir(set.fsys, set.embedded, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out[filepath.Base(path)] = true
		}
		return nil
	})
	require.NoError(t, err)
	return out
}

func concatEmbeddedDir(t *testing.T, set patternDocSet) string {
	t.Helper()
	var b strings.Builder
	err := fs.WalkDir(set.fsys, set.embedded, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		content, err := set.fsys.ReadFile(path)
		if err != nil {
			return err
		}
		b.Write(content)
		b.WriteByte('\n')
		return nil
	})
	require.NoError(t, err)
	return b.String()
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- FENCE CONVENTION (second owner ruling, 2026-10-30) ---
//
// Every fenced code block carries a machine-readable infostring —
// `<lang> kind=<kind> [op=<op>]` — so tests can extract and validate
// each block mechanically. Exactly ONE kind= per fence; a bare ```
// opening fence is a pinned format violation.

// fenceBlock is one extracted fenced code block.
type fenceBlock struct {
	file, slug, info, body string
}

// fenceKind is the kind= token; fenceOp the op= token.
func (f fenceBlock) location() string {
	return fmt.Sprintf("%s:%s", f.file, f.slug)
}

var validFenceKinds = map[string]bool{
	"board-payload": true, "workflow-spec": true, "gate-script": true,
	"shell": true, "output": true,
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
			name := filepath.Base(path)
			content, err := set.fsys.ReadFile(path)
			if err != nil {
				return err
			}
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
// kind vocabulary is closed, and board-payload fences name a valid op —
// the contract the follow-up extractor dispatches on.
func TestPatternDocs_FencePins_InfostringOnEveryFence(t *testing.T) {
	kinds := map[string]int{}
	for _, f := range extractFences(t) {
		kind, op := parseFenceInfo(t, f)
		require.NotEmpty(t, kind, "%s: bare fence — every code fence carries kind=", f.location())
		assert.True(t, validFenceKinds[kind], "%s: fence kind %q not in the convention", f.location(), kind)
		kinds[kind]++
		if kind == "board-payload" {
			assert.True(t, validFenceOps[op], "%s: board-payload fences name a valid op (got %q)", f.location(), op)
			require.NotEmpty(t, op, "%s: board-payload requires op=", f.location())
		} else {
			assert.Empty(t, op, "%s: op= belongs to board-payload fences only", f.location())
		}
	}
	// The catalogs' machine-validated classes exist at all: board
	// payloads to validate, workflow specs to validate.
	assert.Greater(t, kinds["board-payload"], 0, "board catalog: kind=board-payload fences must exist")
	assert.GreaterOrEqual(t, kinds["workflow-spec"], 5, "workflow catalog: the canonical DAGs ride kind=workflow-spec fences")
}

// TestPatternDocs_BoardPayloadFences_PassRealValidator: every
// kind=board-payload fence decodes as ONE strict JSON document and
// passes the REAL board_validate_file pipeline for its op — the
// examples are pasteable-verbatim valid (same validator the tool and
// the CRUD gate run).
func TestPatternDocs_BoardPayloadFences_PassRealValidator(t *testing.T) {
	checked := 0
	for _, f := range extractFences(t) {
		kind, op := parseFenceInfo(t, f)
		if kind != "board-payload" {
			continue
		}
		checked++
		payload, err := decodeBoardPayloadJSON([]byte(f.body))
		require.NoError(t, err, "%s: fence body must be ONE strict JSON object", f.location())
		report := validateBoardPayload(op, payload)
		assert.True(t, report.Valid,
			"%s: board-payload (op=%s) must pass the real validator — findings: %s",
			f.location(), op, boardValidateReportJSON(report))
	}
	assert.Positive(t, checked, "the board catalogs must carry board-payload fences")
}

// TestPatternDocs_WorkflowSpecFences_PassRealValidation: every
// kind=workflow-spec fence parses (JSON or YAML, single document) and
// passes the REAL spec validation workflow_create applies
// (ParseSpec + ValidateSpec, nil predecessor schemas, empty defaults).
func TestPatternDocs_WorkflowSpecFences_PassRealValidation(t *testing.T) {
	checked := 0
	for _, f := range extractFences(t) {
		kind, _ := parseFenceInfo(t, f)
		if kind != "workflow-spec" {
			continue
		}
		checked++
		specJSON, err := specJSONFromFence(f.body)
		require.NoError(t, err, "%s: fence body must be a single JSON/YAML spec document", f.location())
		spec, err := wf.ParseSpec(json.RawMessage(specJSON))
		require.NoError(t, err, "%s: ParseSpec", f.location())
		errs := wf.ValidateSpec(spec, nil, wf.DefaultsBlock{})
		assert.Empty(t, errs, "%s: workflow-spec must pass the real validation", f.location())
	}
	assert.GreaterOrEqual(t, checked, 5, "the workflow catalog carries at least five canonical DAGs")
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
