// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint_test

// The 2026-10-24 #1918 incident (release run 40046673282, v0.41.0): the
// npm publish leg failed with E404 on PUT — npm deliberately answers a
// publish PUT that a dead/mis-scoped token cannot see with 404 Not
// Found, NOT 401/403, so the expired/revoked repo-level NPM_TOKEN
// masqueraded as "package not found" — and the SDK leg's red painted
// the ENTIRE release run `failure` even though every container image,
// the chart, signing, and the GitHub Release had shipped fine. Release
// conclusions stopped being trustworthy exactly when three chained runs
// were needed to isolate two real gate failures.
//
// The fix (fail-soft SDK legs + a DISTINCT outcome-surfacing job + an
// npm auth preflight) is a contract, and these pins hold every leg of
// it — the same discipline as issue_opened_bot_gate_test.go (#1709):
// reverting any leg re-opens the incident class. Structural YAML reads
// throughout (the r1/r2 lessons: line-greps are evaded by respelling);
// `needs` parses as string OR list because release.yml uses both
// spellings (bare `needs: verify-changelog` in prepare/lint, flow list
// everywhere else).

import (
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type releaseSDKJob struct {
	ContinueOnError *bool  `yaml:"continue-on-error"`
	If              string `yaml:"if"`
	Needs           any    `yaml:"needs"`
	Steps           []struct {
		Name string            `yaml:"name"`
		Run  string            `yaml:"run"`
		Env  map[string]string `yaml:"env"`
	} `yaml:"steps"`
}

type releaseSDKWorkflow struct {
	Jobs map[string]releaseSDKJob `yaml:"jobs"`
}

func parseReleaseWorkflow(t *testing.T) releaseSDKWorkflow {
	t.Helper()
	var wf releaseSDKWorkflow
	if err := yaml.Unmarshal([]byte(readWorkflow(t, releasePath)), &wf); err != nil {
		t.Fatalf("%s does not parse: %v", releasePath, err)
	}
	return wf
}

// jobNeeds is spelling-proof: `needs: foo` and `needs: [foo, bar]`
// both yield the member list.
func jobNeeds(j releaseSDKJob) []string {
	switch v := j.Needs.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				continue
			}
			out = append(out, s)
		}
		return out
	default:
		return nil
	}
}

// sdkLegRe matches the SDK publish jobs (publish-python-sdk,
// publish-typescript-sdk — and any future publish-*-sdk leg, which is
// covered by these pins automatically).
var sdkLegRe = regexp.MustCompile(`^publish-.*-sdk$`)

// TestReleaseWorkflow_SDKLegsFailSoft: every SDK publish leg must carry
// job-level continue-on-error: true. A hard SDK leg re-opens #1918: a
// dead registry token would redden container releases again, washing
// out the release-conclusion signal the issue is about.
func TestReleaseWorkflow_SDKLegsFailSoft(t *testing.T) {
	wf := parseReleaseWorkflow(t)
	legs := 0
	for name, job := range wf.Jobs {
		if !sdkLegRe.MatchString(name) {
			continue
		}
		legs++
		if job.ContinueOnError == nil || !*job.ContinueOnError {
			t.Errorf("release.yml job %s lacks continue-on-error: true — a registry-side failure (dead NPM_TOKEN, #1918) would redden the whole release run again", name)
		}
	}
	if legs < 2 {
		t.Errorf("release.yml has %d publish-*-sdk legs (want ≥2: python + typescript) — a leg was renamed or removed; extend the pin, do not skip it", legs)
	}
}

// alwaysRunExpr is GitHub Actions' literal status-function spelling —
// misspell's US locale wants one "l", but the yaml must contain the
// expression VERBATIM for `Contains` to mean anything.
const alwaysRunExpr = "!cancelled()" //nolint:misspell // GitHub's own expression spelling, not prose

// TestReleaseWorkflow_SDKOutcomeJobSurfacesResults: fail-soft legs must
// come with a DISTINCT surfacing job — never-a-silent-skip (the
// run-watchdog.yml convention). sdk-outcome must run even when a leg is
// red or skipped, depend on both legs, and read each leg's RAW
// `result` (under continue-on-error, `outcome` lies "success"; `result`
// carries the failure). Without this pin the decoupling could silently
// degrade into "green run, SDK never shipped, nobody told".
func TestReleaseWorkflow_SDKOutcomeJobSurfacesResults(t *testing.T) {
	wf := parseReleaseWorkflow(t)
	job, ok := wf.Jobs["sdk-outcome"]
	if !ok {
		t.Fatalf("release.yml has no sdk-outcome job — the fail-soft SDK legs (#1918) would fail SILENTLY; the distinct outcome-surfacing job is part of the contract")
	}
	if !strings.Contains(job.If, alwaysRunExpr) && !strings.Contains(job.If, "always()") {
		t.Errorf("sdk-outcome if = %q — lacks %s/always(); a skipped-because-SDK-red outcome job leaves the failure unsurfaced (#1918)", job.If, alwaysRunExpr)
	}
	for _, leg := range []string{"publish-python-sdk", "publish-typescript-sdk"} {
		found := false
		for _, n := range jobNeeds(job) {
			if n == leg {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("sdk-outcome does not need %s — that leg's result cannot be surfaced (#1918)", leg)
		}
	}
	// The raw-result reads: env carries needs.<leg>.result into the run
	// (comment-blind Contains on the active source would miss a
	// respelling, so this pins the expression text; both spellings of
	// the env wiring — map value or inline run expression — contain it).
	src := readWorkflow(t, releasePath)
	for _, leg := range []string{"publish-python-sdk", "publish-typescript-sdk"} {
		if !strings.Contains(src, "needs."+leg+".result") {
			t.Errorf("release.yml never reads needs.%s.result — under continue-on-error the job outcome lies 'success'; only the raw result carries the failure (#1918)", leg)
		}
	}
}

// TestReleaseWorkflow_NpmLegHasAuthPreflight: the npm leg must verify
// the token BEFORE the publish PUT, naming NPM_TOKEN rotation in the
// failure message. The incident cost a run-log hunt precisely because
// E404-on-PUT reads like a missing package; the preflight converts the
// next token death into a one-line actionable error.
func TestReleaseWorkflow_NpmLegHasAuthPreflight(t *testing.T) {
	wf := parseReleaseWorkflow(t)
	job, ok := wf.Jobs["publish-typescript-sdk"]
	if !ok {
		t.Fatalf("release.yml has no publish-typescript-sdk job — renamed? extend the pin, do not skip it")
	}
	preflight, publish := -1, -1
	for i, s := range job.Steps {
		switch {
		case strings.Contains(s.Name, "Preflight npm auth"):
			preflight = i
			if !strings.Contains(s.Run, "npm access ls-packages") {
				t.Errorf("preflight step does not run `npm access ls-packages` — `npm whoami` cannot authenticate granular access tokens; ls-packages works for both token kinds and proves the scope (#1918)")
			}
			if !strings.Contains(s.Run, "NPM_TOKEN") {
				t.Errorf("preflight step's error message does not name NPM_TOKEN — the #1918 lesson: the error must say WHICH secret to rotate")
			}
			if s.Env["NODE_AUTH_TOKEN"] != "${{ secrets.NPM_TOKEN }}" {
				t.Errorf("preflight step env NODE_AUTH_TOKEN = %q — the check must authenticate with the SAME secret the publish uses, or it proves nothing (#1918)", s.Env["NODE_AUTH_TOKEN"])
			}
		case strings.Contains(s.Name, "Publish to npm"):
			publish = i
		}
	}
	if preflight == -1 {
		t.Fatalf("publish-typescript-sdk has no 'Preflight npm auth' step — the next dead token fails as an inscrutable E404 on PUT again (#1918)")
	}
	if publish == -1 {
		t.Fatalf("publish-typescript-sdk has no 'Publish to npm' step — the publish step moved? extend the pin, do not skip it")
	}
	if preflight > publish {
		t.Errorf("preflight step (index %d) runs AFTER Publish to npm (index %d) — it must fail fast BEFORE the publish PUT (#1918)", preflight, publish)
	}
}

// TestReleaseWorkflow_CreateReleaseNotGatedOnSDK: create-release must
// NOT need the SDK legs. The release object (notes, SBOM, relay
// binaries) ships with the container artifacts; wiring the SDK legs
// into its needs would resurrect #1918 one level deeper (a dead npm
// token would block the GitHub Release itself).
func TestReleaseWorkflow_CreateReleaseNotGatedOnSDK(t *testing.T) {
	wf := parseReleaseWorkflow(t)
	cr, ok := wf.Jobs["create-release"]
	if !ok {
		t.Fatalf("release.yml has no create-release job — moved? extend the pin, do not skip it")
	}
	for _, n := range jobNeeds(cr) {
		if sdkLegRe.MatchString(n) || n == "sdk-outcome" {
			t.Errorf("create-release needs %s — the GitHub Release must not be gated on SDK publish legs (#1918); container artifacts own the release conclusion", n)
		}
	}
}
