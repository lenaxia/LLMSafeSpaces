package ghaworkflows

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/runwatchdog"
)

// The run-watchdog seam pins. r1 review critical finding 1: the sweep
// granted no `checks:` permission, so every check-runs call 403'd under
// GITHUB_TOKEN and the failure-tolerant guard turned the watchdog
// SILENTLY INERT. r1 review test finding 5: nothing pins the JSON
// contract between the workflow's jq emission and the binary — a
// renamed jq key yields a silent zero-value (zero commit date →
// infinite age → repo-wide false faults, each firing an image-
// publishing CI dispatch).

const runWatchdogWorkflow = ".github/workflows/run-watchdog.yml"

func runWatchdogBody(t *testing.T) string {
	t.Helper()
	return readFile(t, filepath.Join(repoRoot(t), runWatchdogWorkflow))
}

// TestRunWatchdogPermissionsPinned holds the GITHUB_TOKEN surface the
// sweep needs: exactly the four grants, including the check-runs read
// the r1 sweep was missing (inert under token). Drift in EITHER
// direction (missing grant = inert/red sweep; added write grant =
// unnecessary surface) reddens this pin.
func TestRunWatchdogPermissionsPinned(t *testing.T) {
	body := runWatchdogBody(t)
	start := strings.Index(body, "permissions:")
	end := strings.Index(body, "jobs:")
	if start < 0 || end < 0 || end < start {
		t.Fatal("could not locate the permissions block in run-watchdog.yml")
	}
	block := body[start:end]
	for _, grant := range []string{
		"contents: read",
		"pull-requests: write",
		"actions: write",
		"checks: read",
	} {
		if !strings.Contains(block, grant) {
			t.Errorf("run-watchdog.yml permissions missing %q — the r1-inertness class: the call needing it 403s and the guard swallows it", grant)
		}
	}
	for _, forbidden := range []string{"contents: write", "id-token: write", "checks: write"} {
		if strings.Contains(block, forbidden) {
			t.Errorf("run-watchdog.yml grants %q — unnecessary surface for a read/comment/dispatch sweep", forbidden)
		}
	}
}

// wiringTemplate renders the workflow's jq -n emission object into a
// concrete JSON document the way bash would: the $var placeholders are
// the --arg/--argjson values. Extracted from the YAML so a renamed key
// in EITHER the workflow or the struct drifts this test, not production.
func wiringTemplate(t *testing.T) string {
	t.Helper()
	body := runWatchdogBody(t)
	start := strings.Index(body, "jq -n")
	if start < 0 {
		t.Fatal("run-watchdog.yml has no jq -n emission block")
	}
	objStart := strings.Index(body[start:], "'{")
	if objStart < 0 {
		t.Fatal("jq -n block has no object literal")
	}
	objStart += start
	objEnd := strings.Index(body[objStart:], "}'")
	if objEnd < 0 {
		t.Fatal("jq -n object literal unterminated")
	}
	tmpl := body[objStart+1 : objStart+objEnd+1]

	// Quote the keys (jq object syntax → JSON): in the template, values
	// are exclusively $var placeholders, so rewriting `key:$var` pairs
	// is exact and touches nothing else (commas, newlines, braces stay).
	tmpl = keyQuote(tmpl)

	// Substitute the placeholders exactly as the workflow passes them
	// (--argjson for number/checks/markers/noauto/minam, --arg for the
	// strings).
	samples := map[string]string{
		"$number":  "1873",
		"$sha":     `"e5c685dabdfe8c6edfcd6316643b16e31194b675"`,
		"$branch":  `"probe/ci-event-check"`,
		"$date":    `"2026-10-23T02:02:41Z"`,
		"$msg":     `"chore: probe CI event routing (throwaway)"`,
		"$now":     `"2026-10-23T03:17:35Z"`,
		"$checks":  "0",
		"$markers": "[]",
		"$noauto":  "false",
		"$minam":   "15",
	}
	for k, v := range samples {
		tmpl = strings.ReplaceAll(tmpl, k, v)
	}
	return tmpl
}

// keyQuote rewrites `key: $var` jq object members into `"key": $var`
// JSON members. In the emission template values are exclusively
// placeholders, so the pair-anchored rewrite is exact.
func keyQuote(tmpl string) string {
	return keyVarRe.ReplaceAllString(tmpl, `"$1":$2`)
}

var keyVarRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*:\s*(\$[A-Za-z_]+)`)

// TestRunWatchdogWiringContract pins the drift-prone seam end to end:
// the workflow's jq emission → the binary's stdinState →
// pkg/runwatchdog.Evaluate. Two layers:
//
//  1. key-set: the emitted keys are EXACTLY PRState's json tags plus
//     the cmd's ergonomic min_age_minutes — nothing silent, nothing
//     extra (a dropped key = zero value = false fault).
//  2. round-trip: the rendered document piped through the REAL binary
//     (`go run ./cmd/runwatchdog`) must produce the same Decision
//     Evaluate returns for the equivalent PRState.
func TestRunWatchdogWiringContract(t *testing.T) {
	doc := wiringTemplate(t)

	// Layer 1: key set vs struct tags.
	var raw map[string]any
	if err := json.Unmarshal([]byte(doc), &raw); err != nil {
		t.Fatalf("workflow jq emission does not render to valid JSON (key drift?): %v\nrendered: %s", err, doc)
	}
	// The authoritative emission surface: WorkflowEmission (PRState's
	// promoted tags + min_age_minutes), EXCLUDING PRState.min_age —
	// the in-process-only override the workflow never emits (zero is
	// safe: Evaluate falls back to DefaultMinAge; pinned by
	// TestWorkflowEmissionNormalize and TestEvaluate_MinAgeDefault).
	want := map[string]bool{"min_age_minutes": false}
	st := reflect.TypeOf(runwatchdog.WorkflowEmission{})
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if f.Anonymous {
			// Promoted PRState fields.
			ps := f.Type
			for j := 0; j < ps.NumField(); j++ {
				if tag := ps.Field(j).Tag.Get("json"); tag != "" && tag != "-" {
					want[strings.Split(tag, ",")[0]] = false
				}
			}
			continue
		}
		if tag := f.Tag.Get("json"); tag != "" && tag != "-" {
			want[strings.Split(tag, ",")[0]] = false
		}
	}
	for k := range raw {
		if _, ok := want[k]; !ok {
			t.Errorf("workflow emits key %q that PRState does not carry — typo or drift", k)
		}
		want[k] = true
	}
	// PRState.min_age is the in-process-only override the workflow
	// never emits (minutes instead); its zero value is safe — Evaluate
	// falls back to DefaultMinAge (TestEvaluate_MinAgeDefault,
	// TestWorkflowEmissionNormalize). Every OTHER missing key is a
	// silent zero-value hazard.
	delete(want, "min_age")
	for k, seen := range want {
		if !seen {
			t.Errorf("workflow emission is MISSING key %q — the binary would read a silent zero-value (zero commit date = infinite age = repo-wide false faults)", k)
		}
	}

	// Layer 2: round-trip through the real binary.
	if testing.Short() {
		t.Skip("short mode: skipping the go-run round-trip")
	}
	root := repoRoot(t)
	cmd := exec.Command("go", "run", "./cmd/runwatchdog")
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(doc)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("cmd/runwatchdog rejected the workflow's emission: %v\ninput: %s", err, doc)
	}
	var got runwatchdog.Decision
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("binary output is not a Decision: %v\noutput: %s", err, out)
	}

	// The equivalent direct evaluation (the values above: 75m-old head,
	// zero check-runs, no markers → a FAULT with both actions).
	wantDec := runwatchdog.Evaluate(runwatchdog.PRState{
		Number:            1873,
		HeadSHA:           "e5c685dabdfe8c6edfcd6316643b16e31194b675",
		HeadBranch:        "probe/ci-event-check",
		HeadCommitDate:    time.Date(2026, 10, 23, 2, 2, 41, 0, time.UTC),
		HeadCommitMessage: "chore: probe CI event routing (throwaway)",
		Now:               time.Date(2026, 10, 23, 3, 17, 35, 0, time.UTC),
		TotalCheckRuns:    0,
		MinAge:            15 * time.Minute,
	})
	if !wantDec.Fault || !wantDec.ShouldComment || !wantDec.ShouldDispatch {
		t.Fatalf("sample values no longer produce a fault — update the sample to keep this pin meaningful")
	}
	if got.Fault != wantDec.Fault || got.ShouldComment != wantDec.ShouldComment || got.ShouldDispatch != wantDec.ShouldDispatch {
		t.Errorf("round-trip drift: binary decided fault=%v comment=%v dispatch=%v, Evaluate says %v/%v/%v",
			got.Fault, got.ShouldComment, got.ShouldDispatch, wantDec.Fault, wantDec.ShouldComment, wantDec.ShouldDispatch)
	}
}

// TestRunWatchdogNoSilentProbeSwallows pins the never-a-silent-skip rule
// on the dedup register: the marker enumeration must not degrade to an
// empty default (the #1874 r1 dead-probe class — a swallowed failure
// reads as "no markers" and mints duplicate comments + dispatches).
func TestRunWatchdogNoSilentProbeSwallows(t *testing.T) {
	body := runWatchdogBody(t)
	if strings.Contains(body, "|| MARKERS_JSON='[]'") {
		t.Error("marker enumeration still degrades to '[]' on failure — a swallowed failure reads as no-markers and breaks the per-SHA dedup (the dead-probe class)")
	}
	if !strings.Contains(body, "gh api --paginate") {
		t.Error("marker enumeration is not paginated — markers hide on page 2+ of chatty PRs (r1 review robustness)")
	}
}
