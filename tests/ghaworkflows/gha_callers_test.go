// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ghaworkflows exercises the ai-workflows onboarding contract for
// the reusable lenaxia/ai-workflows PR-review and AI-commands workflows
// (v0.2.10).
//
// The two thin callers (.github/workflows/pr-review.yml, ai-comment.yml)
// delegate review execution and command routing to centrally-pinned
// reusable workflows. Three properties must hold or the delegation silently
// degrades — each is pinned here so a regression fails `make test` / CI:
//
//   - Authorization: the caller `if:` in ai-comment.yml is the ONLY
//     authorization boundary for /fix, /implement & co. (the reusable
//     workflow and central route-command.sh contain no association check);
//     a dropped clause lets any GitHub user drive code-changing commands on
//     a runner holding contents: write + secrets: inherit.
//   - Pin consistency: `uses: …@vX` and `with.version: vX` must be equal
//     for both callers. propagate.yml bumps both together, but a partial
//     bump (e.g. a manually-merged renovate ref update touching only the
//     `@tag`) yields workflow logic at vX with scripts checked out at vY.
//   - Caller contract: job ids (required-check names), triggers, the four
//     permissions, secrets: inherit, and the prompt files the reusable
//     workflow cats / post-footer.sh reads.
//
// TestLocalScriptReferencesResolve is the regression guard for the 2026-08-17
// be3320ec incident: a direct push deleted .github/scripts/route-command.sh
// while main's ai-comment.yml still sourced it, breaking /ai command routing
// on main until the onboarding PR (#914) replaced the local router with the
// central pinned one.
package ghaworkflows

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot resolves the repo root (the directory containing go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repo root (go.mod not found)")
		}
		dir = parent
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

const (
	prReviewCaller  = ".github/workflows/pr-review.yml"
	aiCommentCaller = ".github/workflows/ai-comment.yml"
)

// commandsRoutedByAiComment is every slash command the caller `if:` admits.
// startsWith matches the command at comment start; the contains form matches
// it mid-sentence ("please /review this"). Both must exist for every
// command — dropping a guard silently stops routing that command.
var commandsRoutedByAiComment = []string{
	"/ai", "/review", "/fix", "/implement", "/analyze", "/test",
	"/security", "/explain", "/triage", "/help", "/design", "/merge",
}

// TestAiCommentAuthorizationGatePresent pins the caller `if:` as the sole
// authorization boundary. Neither the reusable ai-comment workflow nor the
// central route-command.sh checks author_association — if these clauses
// disappear from the caller, any GitHub user can trigger code-changing
// commands on a runner with contents: write and secrets: inherit.
func TestAiCommentAuthorizationGatePresent(t *testing.T) {
	body := readFile(t, filepath.Join(repoRoot(t), aiCommentCaller))
	ifIdx := strings.Index(body, "if: |")
	if ifIdx < 0 {
		t.Fatal("ai-comment.yml caller has no `if: |` gate — command authorization is GONE")
	}
	// The gate block runs to the `uses:` line of the job.
	endIdx := strings.Index(body[ifIdx:], "uses:")
	if endIdx < 0 {
		t.Fatal("ai-comment.yml caller has no `uses:` — not delegating to the reusable workflow")
	}
	gate := body[ifIdx : ifIdx+endIdx]

	for _, assoc := range []string{"OWNER", "MEMBER", "COLLABORATOR"} {
		if !strings.Contains(gate, "github.event.comment.author_association == '"+assoc+"'") {
			t.Errorf("authorization gate missing author_association %s clause", assoc)
		}
	}
	for _, cmd := range commandsRoutedByAiComment {
		if !strings.Contains(gate, "startsWith(github.event.comment.body, '"+cmd+"')") {
			t.Errorf("authorization gate missing startsWith guard for %s", cmd)
		}
		if !strings.Contains(gate, "contains(github.event.comment.body, ' "+cmd+"')") {
			t.Errorf("authorization gate missing contains guard for %s", cmd)
		}
	}
}

// callerPin extracts the `uses:` ref and the `with: version:` value from a
// thin caller workflow.
func callerPin(t *testing.T, caller string) (ref, version string) {
	t.Helper()
	body := readFile(t, filepath.Join(repoRoot(t), caller))
	m := regexp.MustCompile(`uses: lenaxia/ai-workflows/\.github/workflows/\S+?@(\S+)`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("%s does not delegate to lenaxia/ai-workflows", caller)
	}
	ref = m[1]
	if ref != "v0.2.10" && !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(ref) {
		t.Errorf("%s reusable ref must be a vX.Y.Z tag or 40-hex SHA pin, got %q", caller, ref)
	}
	m = regexp.MustCompile(`version:\s*(\S+)`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("%s missing `with: version:` input", caller)
	}
	version = m[1]
	return ref, version
}

// TestCallerPinsMatchReusableVersion asserts pin consistency per caller: the
// workflow logic ref and the scripts checkout must be the same version.
func TestCallerPinsMatchReusableVersion(t *testing.T) {
	for _, caller := range []string{prReviewCaller, aiCommentCaller} {
		ref, version := callerPin(t, caller)
		if ref != version {
			t.Errorf("%s pin mismatch: uses@%s but with.version=%s — workflow logic and pinned scripts run at different versions", caller, ref, version)
		}
	}
}

// TestPrReviewCallerContract pins the pr-review.yml caller shape: job id
// `review` (a rename changes the required-check name), pull_request
// opened+synchronize triggers, the review permissions (contents: read —
// reviewing never writes repo contents), secrets: inherit, project_name.
func TestPrReviewCallerContract(t *testing.T) {
	body := readFile(t, filepath.Join(repoRoot(t), prReviewCaller))

	if !strings.Contains(body, "uses: lenaxia/ai-workflows/.github/workflows/pr-review.yml@") {
		t.Error("pr-review.yml must delegate to the reusable pr-review workflow")
	}
	for _, required := range []string{
		"  review:", // job id — required-check name stability
		"types: [opened, synchronize]",
		"id-token: write",
		"contents: read",
		"issues: write",
		"pull-requests: write",
		"secrets: inherit",
		"project_name: LLMSafeSpaces",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("pr-review.yml caller missing %q", required)
		}
	}
}

// TestAiCommentCallerContract pins the ai-comment.yml caller shape: job id
// `respond`, comment-created triggers, the four write permissions (the
// command runner may push code), secrets: inherit, project_name.
func TestAiCommentCallerContract(t *testing.T) {
	body := readFile(t, filepath.Join(repoRoot(t), aiCommentCaller))

	if !strings.Contains(body, "uses: lenaxia/ai-workflows/.github/workflows/ai-comment.yml@") {
		t.Error("ai-comment.yml must delegate to the reusable ai-comment workflow")
	}
	for _, required := range []string{
		"  respond:", // job id — required-check name stability
		"issue_comment:",
		"pull_request_review_comment:",
		"types: [created]",
		"id-token: write",
		"contents: write",
		"issues: write",
		"pull-requests: write",
		"secrets: inherit",
		"project_name: LLMSafeSpaces",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("ai-comment.yml caller missing %q", required)
		}
	}
}

// TestPromptContractFilesExist asserts the prompt files consumed by the
// reusable workflows exist: pr-review.yml cats context.md, core-rules.md and
// pr-review.md; post-footer.sh reads commands-footer.md. A rename or
// accidental deletion breaks every review run with nothing failing.
func TestPromptContractFilesExist(t *testing.T) {
	root := repoRoot(t)
	for _, f := range []string{"context.md", "core-rules.md", "pr-review.md", "commands-footer.md"} {
		path := filepath.Join(root, ".github", "prompts", f)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("reusable workflow consumes .github/prompts/%s — missing", f)
		}
	}
}

// TestLocalRouterIsGone asserts the superseded local router stays deleted.
// The central route-command.sh at the pinned tag is the single source of
// routing truth; a resurrected local copy guarantees drift (the v0.2.10
// central script is a strict superset — POSIX classes + SHA stamping).
func TestLocalRouterIsGone(t *testing.T) {
	if _, err := os.Stat(filepath.Join(repoRoot(t), ".github", "scripts", "route-command.sh")); err == nil {
		t.Error(".github/scripts/route-command.sh must stay deleted — routing is owned by the centrally-pinned ai-workflows script")
	}
}

// TestWorkflowScriptReferencesResolve is the regression guard for the
// be3320ec incident (2026-08-17): a direct push deleted
// .github/scripts/route-command.sh while ai-comment.yml still sourced it,
// breaking /ai command routing on main until #914 landed. Every path under
// .github/scripts/ referenced by any workflow must exist on disk.
func TestWorkflowScriptReferencesResolve(t *testing.T) {
	root := repoRoot(t)
	wfDir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(wfDir)
	if err != nil {
		t.Fatalf("read workflows dir: %v", err)
	}
	refRe := regexp.MustCompile(`(\.github/scripts/[A-Za-z0-9._/-]+)`)
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml")) {
			continue
		}
		body := readFile(t, filepath.Join(wfDir, e.Name()))
		for _, m := range refRe.FindAllString(body, -1) {
			for _, line := range strings.Split(body, "\n") {
				// Only lines that execute or source a script must resolve;
				// prose comments mentioning a path historically are fine.
				if strings.Contains(line, m) && (strings.Contains(line, "source ") || strings.Contains(line, "bash ") || strings.Contains(line, "./")) {
					key := e.Name() + " -> " + m
					if seen[key] {
						continue
					}
					seen[key] = true
					if _, err := os.Stat(filepath.Join(root, m)); err != nil {
						t.Errorf("%s executes %s but the file does not exist (be3320ec regression class)", e.Name(), m)
					}
				}
			}
		}
	}
}
