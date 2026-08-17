// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package renovateanalysis exercises the Renovate-PR onboarding contract for
// the reusable lenaxia/ai-workflows renovate-analysis workflow (v0.2.11).
//
// The caller (.github/workflows/renovate-analysis.yml), the forked prompt
// (.github/prompts/renovate-analysis.md), and the Renovate config
// (renovate.json) must stay locked to the reusable workflow's contract:
// schedule/dispatch-only triggers, the four write permissions, a pinned ref,
// the three prompt files the reusable job cats, and the renovate.json guards.
// A silent regression (e.g. re-adding a pull_request trigger, a propagate
// overwrite dropping the fork, or dropping an auto-merge guard) now fails
// `make test` / CI.
package renovateanalysis

import (
	"encoding/json"
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

// renovateCaller is the path of the thin caller workflow.
const renovateCaller = ".github/workflows/renovate-analysis.yml"

// TestCallerTriggersAreScheduleAndDispatchOnly pins the critical
// assertPermissions() fix: the workflow must run on schedule +
// workflow_dispatch only. A pull_request trigger makes every renovate[bot]
// run fail before the AI starts (renovate[bot] can never be a collaborator).
//
// Note the caller's header comment legitimately discusses pull_request (to
// explain why it is NOT used) — so scan the `on:` block only.
func TestCallerTriggersAreScheduleAndDispatchOnly(t *testing.T) {
	body := readFile(t, filepath.Join(repoRoot(t), renovateCaller))
	on := body[strings.Index(body, "on:"):]
	for _, banned := range []string{"pull_request", "pull_request_target", "push:"} {
		if strings.Contains(on, banned) {
			t.Errorf("caller must not trigger on %q — renovate[bot] runs fail assertPermissions()", banned)
		}
	}
	for _, required := range []string{"schedule:", "workflow_dispatch:", `cron: "0 */2 * * *"`} {
		if !strings.Contains(on, required) {
			t.Errorf("caller missing required trigger element %q", required)
		}
	}
}

// TestCallerPinsReusableWorkflow asserts the caller delegates to the reusable
// workflow at a pinned v0.2.11 ref with the documented contract: four write
// permissions, secrets: inherit, and the project_name/pr_number inputs.
func TestCallerPinsReusableWorkflow(t *testing.T) {
	body := readFile(t, filepath.Join(repoRoot(t), renovateCaller))

	ref := regexp.MustCompile(`uses: lenaxia/ai-workflows/\.github/workflows/renovate-analysis\.yml@(\S+)`).FindStringSubmatch(body)
	if ref == nil {
		t.Fatal("caller does not use lenaxia/ai-workflows/.github/workflows/renovate-analysis.yml")
	}
	pin := ref[1]
	if pin != "v0.2.11" && !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(strings.Fields(pin)[0]) {
		t.Errorf("reusable ref must be the v0.2.11 tag or a 40-hex SHA pin, got %q", pin)
	}

	for _, perm := range []string{"id-token: write", "contents: write", "issues: write", "pull-requests: write"} {
		if !strings.Contains(body, perm) {
			t.Errorf("caller missing required permission %q", perm)
		}
	}
	if !strings.Contains(body, "secrets: inherit") {
		t.Error("caller missing secrets: inherit")
	}
	if !strings.Contains(body, "project_name: LLMSafeSpaces") {
		t.Error("caller missing project_name: LLMSafeSpaces")
	}
	if !strings.Contains(body, "pr_number: ${{ inputs.pr_number }}") {
		t.Error("caller missing pr_number input pass-through")
	}
}

// TestPromptContractFilesExist asserts the three files the reusable job cats
// (context.md, core-rules.md, renovate-analysis.md) all exist. A propagate
// overwrite or an accidental rename silently breaks prompt assembly.
func TestPromptContractFilesExist(t *testing.T) {
	root := repoRoot(t)
	for _, f := range []string{"context.md", "core-rules.md", "renovate-analysis.md"} {
		path := filepath.Join(root, ".github", "prompts", f)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("reusable workflow cats .github/prompts/%s — missing", f)
		}
	}
}

// TestPromptIsForkNotManaged asserts renovate-analysis.md is a fork (no
// "Managed by" render banner). The renderer produces the banner for rendered
// files; a forked file must not carry it (matching the k8s-mechanic reference
// fork), or the fork-overwrite race tracking in ai-workflows#36 has regressed.
func TestPromptIsForkNotManaged(t *testing.T) {
	body := readFile(t, filepath.Join(repoRoot(t), ".github", "prompts", "renovate-analysis.md"))
	if strings.Contains(body, "Managed by") && strings.Contains(body, "do not edit") {
		t.Error("renovate-analysis.md is a forked prompt — must not carry the rendered-file banner")
	}
}

// TestPromptRetainsTemplateGuardrails asserts the fork kept the template's
// read-only guardrails: /tmp-only scratch, no-comment-when-no-renovate-PRs,
// the post-and-verify loop, no branch creation, and the conservative
// "needs manual review" default.
func TestPromptRetainsTemplateGuardrails(t *testing.T) {
	body := readFile(t, filepath.Join(repoRoot(t), ".github", "prompts", "renovate-analysis.md"))
	for _, want := range []string{
		"/tmp",                       // scratch files under /tmp only
		"DO NOT post any comment",    // no-PR path reports and stops
		"gh pr comment",              // post via gh CLI
		"persist-credentials: false", // read-only checkout
		"Needs manual review",        // conservative default
		"Any dependency flagged as security-sensitive", // security catch-all
		"toolchain bump", // template step-4 check
	} {
		if !strings.Contains(strings.ToLower(body), strings.ToLower(want)) {
			t.Errorf("forked prompt missing template guardrail %q", want)
		}
	}
	for _, banned := range []string{"use the github_merge_pull_request tool", "call github_merge_pull_request"} {
		if strings.Contains(body, banned) {
			t.Errorf("prompt must not instruct using the (nonexistent) github_merge_pull_request tool (%q)", banned)
		}
	}
}

// TestRenovateConfigGuards asserts renovate.json carries the org-standard
// guards: the github-actions branch-automerge rule, and never-auto-merge for
// anomalyco/opencode (opencode digest bumps would otherwise land on main with
// no PR and no CI — the exact assertPermissions() failure that caused #873).
func TestRenovateConfigGuards(t *testing.T) {
	root := repoRoot(t)
	raw := readFile(t, filepath.Join(root, "renovate.json"))

	var cfg struct {
		PackageRules []map[string]any `json:"packageRules"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("renovate.json is not valid JSON: %v", err)
	}

	var foundGithubActionsAutomerge, foundOpendcodeGuard, foundAiWorkflowsGuard bool
	for _, rule := range cfg.PackageRules {
		if m, _ := rule["matchManagers"].([]any); containsString(m, "github-actions") {
			if a, ok := rule["automerge"].(bool); ok && a {
				foundGithubActionsAutomerge = true
			}
		}
		if names, _ := rule["matchPackageNames"].([]any); containsString(names, "anomalyco/opencode") {
			if a, ok := rule["automerge"].(bool); ok && !a {
				foundOpendcodeGuard = true
			}
		}
		if names, _ := rule["matchPackageNames"].([]any); containsString(names, "lenaxia/ai-workflows") {
			if a, ok := rule["automerge"].(bool); ok && !a {
				foundAiWorkflowsGuard = true
			}
		}
	}
	if !foundGithubActionsAutomerge {
		t.Error("renovate.json missing the github-actions digest/patch/minor automerge rule")
	}
	if !foundOpendcodeGuard {
		t.Error("renovate.json missing the anomalyco/opencode never-auto-merge guard")
	}
	if !foundAiWorkflowsGuard {
		t.Error("renovate.json missing the lenaxia/ai-workflows never-auto-merge guard")
	}
}

func containsString(items []any, want string) bool {
	for _, it := range items {
		if s, ok := it.(string); ok && s == want {
			return true
		}
	}
	return false
}
