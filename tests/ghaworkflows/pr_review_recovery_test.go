package ghaworkflows

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The #1844 dispatch-recovery probes (pr-review.yml dispatch-recovery
// job) post and deduplicate fault comments. r1 review found the dedup
// probe DEAD — `gh api` has no `--arg` flag, so the invocation errored
// on every execution and the `|| echo 0` swallowed it: EXISTING was
// always 0 and every dispatch re-commented + re-dispatched. These pins
// hold the probe semantics the r2 rewrite established:
//
//  1. no `--jq --arg` sequence anywhere in the file (the dead-probe
//     class — gh api takes env vars inside the jq program, not --arg);
//  2. every --jq program is valid jq AND semantically correct against
//     sample payloads (marker hit for this SHA, miss for another);
//  3. the marker literal is byte-consistent between the dedup probe
//     and the posted comment (dedup and emission must match);
//  4. the ci.yml re-dispatch step is coupled to the comment dedup
//     result (no re-dispatch once a marker exists);
//  5. recovery refuses non-OPEN PRs (commenting on closed PRs /
//     dispatching at deleted refs).

// faultMarkerKey is the dedup/emission key shared by both sides of the
// dispatch-recovery job. run-watchdog.yml posts the identical literal
// (its own PR pins it in pkg/runwatchdog); this tree pins the shim's
// two sides against EACH OTHER.
const faultMarkerKey = "run-watchdog: fault sha="

func prReviewBody(t *testing.T) string {
	t.Helper()
	return readFile(t, filepath.Join(repoRoot(t), prReviewCaller))
}

// TestPrReviewNoDeadProbeArgForm pins the r1 critical finding's class
// out of the tree: `--jq --arg` is not a valid gh api invocation.
func TestPrReviewNoDeadProbeArgForm(t *testing.T) {
	body := prReviewBody(t)
	if strings.Contains(body, "--jq --arg") {
		t.Error("pr-review.yml contains `--jq --arg` — gh api has no --arg flag; the probe dies on every execution (r1 critical: silent dedup failure)")
	}
	// The probes must pass their value the way gh api supports: an env
	// var read inside the jq program (gojq env access).
	probes := jqPrograms(body)
	if len(probes) == 0 {
		t.Fatal("no --jq programs found in pr-review.yml — probe extraction broken")
	}
	envProbes := 0
	for _, p := range probes {
		if strings.Contains(p, "env.SHA") {
			envProbes++
		}
	}
	if envProbes == 0 {
		t.Error("no probe uses env.SHA — SHA-scoped probes cannot work under gh api without it")
	}
}

// TestPrReviewProbeJqSemantics executes every SHA-scoped --jq program
// from pr-review.yml against sample payloads through the real jq
// binary: a hit for the probed SHA, silence for a different SHA.
// Skips when jq is not installed (CI runners ship it; the validity of
// the program text itself is still pinned by extraction succeeding).
func TestPrReviewProbeJqSemantics(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq not installed — probe semantics not executable here")
	}
	body := prReviewBody(t)

	sampleComments := `[{"body":"unrelated"},{"body":"<!-- run-watchdog: fault sha=1111111111111111111111111111111111111111 -->"},{"body":"<!-- run-watchdog: fault sha=2222222222222222222222222222222222222222 -->"},{"body":"<!-- ai-workflows-verdict: APPROVE sha=2222222222222222222222222222222222222222 -->"}]`
	sampleReviews := `[{"user":{"login":"someone"},"commit_id":"2222222222222222222222222222222222222222","state":"APPROVED"},{"user":{"login":"github-actions[bot]"},"commit_id":"2222222222222222222222222222222222222222","state":"COMMENTED"},{"user":{"login":"github-actions[bot]"},"commit_id":"2222222222222222222222222222222222222222","state":"CHANGES_REQUESTED"}]`

	for _, prog := range jqPrograms(body) {
		if !strings.Contains(prog, "env.SHA") {
			continue // the auto-detect probe takes no SHA
		}
		for _, tc := range []struct {
			name    string
			payload string
			sample  string // value of the SHA env var
			wantHit bool
		}{
			{"comments-marker-hit", sampleComments, "2222222222222222222222222222222222222222", true},
			{"comments-marker-miss", sampleComments, "3333333333333333333333333333333333333333", false},
			{"reviews-verdict-hit", sampleReviews, "2222222222222222222222222222222222222222", true},
			{"reviews-verdict-miss", sampleReviews, "1111111111111111111111111111111111111111", false},
		} {
			// Only feed payloads matching the probe's endpoint shape.
			if strings.HasPrefix(prog, ".[] | select(.user.login") && tc.name != "reviews-verdict-hit" && tc.name != "reviews-verdict-miss" {
				continue
			}
			if strings.HasPrefix(prog, ".[] | select(.body") && !strings.Contains(tc.name, "comments") {
				continue
			}
			cmd := exec.Command(jq, "-c", prog)
			cmd.Stdin = strings.NewReader(tc.payload)
			cmd.Env = append(cmd.Environ(), "SHA="+tc.sample)
			out, err := cmd.Output()
			if err != nil {
				t.Errorf("probe program invalid jq (%s): %v\nprogram: %s", tc.name, err, prog)
				continue
			}
			gotHit := strings.TrimSpace(string(out)) != ""
			if gotHit != tc.wantHit {
				t.Errorf("probe semantics wrong for %s: got hit=%v want %v\nprogram: %s", tc.name, gotHit, tc.wantHit, prog)
			}
		}
	}
}

// TestPrReviewFaultMarkerSelfConsistency pins the marker literal
// byte-consistent between the dedup probe (what it searches for) and
// the posted comment (what it emits) — the two sides of per-head-SHA
// idempotency.
func TestPrReviewFaultMarkerSelfConsistency(t *testing.T) {
	body := prReviewBody(t)

	// Dedup side: the probe's contains() argument.
	probe := regexp.MustCompile(`contains\("` + regexp.QuoteMeta(faultMarkerKey) + `" \+ env\.SHA\)`).FindString(body)
	if probe == "" {
		t.Errorf("dedup probe does not search for the fault marker key %q (contains(\"...\" + env.SHA) form expected)", faultMarkerKey)
	}

	// Emission side: the posted comment's HTML marker skeleton.
	if !strings.Contains(body, "<!-- "+faultMarkerKey+"$SHA -->") {
		t.Errorf("posted comment does not emit the marker skeleton <!-- %s$SHA --> — emission side of the dedup contract drifted", faultMarkerKey)
	}
}

// TestPrReviewDispatchCoupledToDedup pins the r1 finding-3 fix: the
// ci.yml re-dispatch step must be gated on BOTH the verdict check and
// the comment dedup result — otherwise every invocation queues a full
// multi-arch CI run even when recovery already ran.
func TestPrReviewDispatchCoupledToDedup(t *testing.T) {
	body := prReviewBody(t)
	stepStart := strings.Index(body, "- name: Re-dispatch CI on the branch")
	if stepStart < 0 {
		t.Fatal("pr-review.yml has no ci re-dispatch step")
	}
	runRel := strings.Index(body[stepStart:], "run: |")
	if runRel < 0 {
		t.Fatal("re-dispatch step malformed (no run block)")
	}
	// The gate lives in the step header (name/comments/if:) before the
	// run block.
	gate := body[stepStart : stepStart+runRel]
	for _, required := range []string{
		"steps.verdict.outputs.delivered == 'false'",
		"steps.comment.outputs.duplicate != 'true'",
	} {
		if !strings.Contains(gate, required) {
			t.Errorf("re-dispatch gate missing %q — dispatch is not coupled to the comment dedup (r1 finding 3)", required)
		}
	}
	// The comment step must actually EMIT the duplicate flag on the
	// dedup path.
	if !strings.Contains(body, `echo "duplicate=true" >> "$GITHUB_OUTPUT"`) {
		t.Error("comment step never emits duplicate=true — the re-dispatch coupling has no input")
	}
}

// TestPrReviewRecoveryOpenOnly pins the r1 robustness guard: recovery
// refuses closed/merged PRs (no fault comments on closed PRs, no ci
// dispatch at possibly-deleted refs).
func TestPrReviewRecoveryOpenOnly(t *testing.T) {
	body := prReviewBody(t)
	if !strings.Contains(body, `if [ "$STATE" != "OPEN" ]; then`) {
		t.Error("resolve step does not guard on PR state OPEN — recovery would comment on closed PRs and dispatch at deleted branch refs")
	}
}

// jqPrograms extracts every single-quoted --jq program from a workflow
// body. Multi-line programs are collapsed (the probes are one-liners;
// the extraction failing loudly is itself the pin).
func jqPrograms(body string) []string {
	re := regexp.MustCompile(`(?s)--jq\s+'([^']*)'`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		out = append(out, strings.Join(strings.Fields(m[1]), " "))
	}
	return out
}
