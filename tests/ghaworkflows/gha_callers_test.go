// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ghaworkflows exercises the ai-workflows onboarding contract for
// the reusable lenaxia/ai-workflows PR-review and AI-commands workflows
// (v0.2.16).
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
//   - Trusted-bot allowlist (#1710): app bots structurally carry
//     author_association NONE (they are neither owners nor collaborators),
//     so an association-only gate silently drops every command they post —
//     during the 2026-10-16 event-delivery outage the orchestrator's /review
//     fallback via agentic-actor-coder[bot] concluded `skipped` twice with
//     zero PR-visible trace (runs 38836736982/38850671818 on #1708). Every
//     gate filtering on author_association must also carry the ONE shared
//     exact-login allowlist arm (ai-workflows v0.2.16 README §4 caller
//     contract): each entry is a deliberate trust decision, never a blanket
//     association == 'NONE' pass.
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
	if ref != "v0.2.16" && !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(ref) {
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
// routing truth; a resurrected local copy guarantees drift (the v0.2.16
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

// canonicalTrustedAppBots is THE shared trusted-app-bot allowlist for this
// repo (#1710 ruling item 2: single source). Every caller gate that filters
// on author_association must carry this exact list, verbatim, as
// contains(fromJson('<canonicalTrustedAppBots as JSON>'),
// github.event.comment.user.login). App bots always present
// author_association NONE, so an exact-login arm is the only gate that can
// authorize them; each entry is a deliberate trust decision (adding one
// means editing this list AND every gate copy — the drift test below fails
// until all copies match).
var canonicalTrustedAppBots = []string{"agentic-actor-coder[bot]"}

// trustedBotArmRe matches the allowlist arm in the canonical ai-workflows
// v0.2.16 README §4 form, capturing the raw JSON array literal so copies
// can be compared byte-for-byte (quoting/whitespace drift included).
var trustedBotArmRe = regexp.MustCompile(
	`contains\(fromJson\('(\[[^\r\n]*?\])'\), github\.event\.comment\.user\.login\)`)

// exactBotLoginRe is the exact-bot-login shape every allowlist entry must
// have: a GitHub app-bot login (alphanumerics/hyphens + the literal [bot]
// suffix). Rejects wildcards ('*'), globs, and human/app logins without
// the [bot] marker — the allowlist may never grow a pattern entry.
var exactBotLoginRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*\[bot\]$`)

// workflowYAMLs lists every workflow file in .github/workflows.
func workflowYAMLs(t *testing.T) []string {
	t.Helper()
	wfDir := filepath.Join(repoRoot(t), ".github", "workflows")
	entries, err := os.ReadDir(wfDir)
	if err != nil {
		t.Fatalf("read workflows dir: %v", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml")) {
			continue
		}
		files = append(files, filepath.Join(wfDir, e.Name()))
	}
	if len(files) == 0 {
		t.Fatal("no workflow files found under .github/workflows")
	}
	return files
}

// associationGatedWorkflows returns every workflow file whose gate filters
// on github.event.*.author_association. Those are exactly the gates the
// #1710 trusted-bot allowlist arm must ride along with — a future gate
// added with an association filter is automatically covered (it fails the
// arm test until it carries the shared allowlist).
func associationGatedWorkflows(t *testing.T) map[string]string {
	t.Helper()
	gates := map[string]string{}
	for _, f := range workflowYAMLs(t) {
		body := readFile(t, f)
		if strings.Contains(body, "author_association") {
			gates[filepath.Base(f)] = body
		}
	}
	if len(gates) == 0 {
		t.Fatal("no author_association-filtered gates found — the ai-comment caller gate is missing entirely (authorization is GONE)")
	}
	return gates
}

// gateAllowlistArms extracts the raw JSON array literal from every
// trusted-bot allowlist arm in the given workflow body.
func gateAllowlistArms(t *testing.T, name, body string) []string {
	t.Helper()
	var arms []string
	for _, m := range trustedBotArmRe.FindAllStringSubmatch(body, -1) {
		arms = append(arms, m[1])
	}
	return arms
}

// TestTrustedBotAllowlistArmOnEveryAssociationGate pins #1710 ruling item
// 1(a): every gate filtering on author_association must ALSO carry the
// trusted-bot allowlist arm. Removing the arm from any gate (the exact
// 2026-10-16 outage no-op: /review from agentic-actor-coder[bot] concluded
// `skipped`, runs 38836736982/38850671818) fails here.
func TestTrustedBotAllowlistArmOnEveryAssociationGate(t *testing.T) {
	for name, body := range associationGatedWorkflows(t) {
		if len(gateAllowlistArms(t, name, body)) == 0 {
			t.Errorf("%s filters on author_association but has no trusted-bot allowlist arm "+
				"(contains(fromJson(...), github.event.comment.user.login)) — app bots carry association NONE "+
				"by construction and are silently skipped by this gate (#1710 outage no-op class)", name)
		}
	}
}

// TestTrustedBotAllowlistCopiesDoNotDrift pins #1710 ruling items 1(b)/2:
// the allowlist is ONE shared definition. Every gate copy's JSON literal
// must be byte-identical to canonicalTrustedAppBots rendered as compact
// JSON — a gate edited independently (drift), a missing entry, or an extra
// entry all fail until every copy matches the single source again.
func TestTrustedBotAllowlistCopiesDoNotDrift(t *testing.T) {
	canonical, err := json.Marshal(canonicalTrustedAppBots)
	if err != nil {
		t.Fatalf("marshal canonical allowlist: %v", err)
	}
	for name, body := range associationGatedWorkflows(t) {
		arms := gateAllowlistArms(t, name, body)
		if len(arms) == 0 {
			continue // covered by TestTrustedBotAllowlistArmOnEveryAssociationGate
		}
		for _, arm := range arms {
			if string(arm) != string(canonical) {
				t.Errorf("%s allowlist arm %s does not match the shared canonical list %s — "+
					"the allowlist is ONE definition (canonicalTrustedAppBots in gha_callers_test.go); "+
					"update every gate copy together", name, arm, canonical)
			}
		}
	}
}

// TestTrustedBotAllowlistExactLoginsOnly pins #1710 ruling item 1(c): the
// allowlist stays exact-login. Wildcards ('*'), glob fragments, empty
// entries, or non-bot logins (an association-shaped accident — a human
// login without the [bot] marker) all fail. Each entry must name ONE app
// bot deliberately.
func TestTrustedBotAllowlistExactLoginsOnly(t *testing.T) {
	if len(canonicalTrustedAppBots) == 0 {
		t.Fatal("canonicalTrustedAppBots must not be empty — an empty allowlist means the arm exists but trusts nobody, which is dead config")
	}
	for _, login := range canonicalTrustedAppBots {
		if !exactBotLoginRe.MatchString(login) {
			t.Errorf("canonical allowlist entry %q is not an exact app-bot login (want e.g. 'agentic-actor-coder[bot]') — wildcards and non-bot patterns are forbidden (#1710 ruling)", login)
		}
	}
	// The same shape rule holds for every gate copy, not just the canonical
	// list (a drifted copy with '*' must fail here even before drift is fixed).
	for name, body := range associationGatedWorkflows(t) {
		for _, arm := range gateAllowlistArms(t, name, body) {
			var entries []string
			if err := json.Unmarshal([]byte(arm), &entries); err != nil {
				t.Errorf("%s allowlist arm %s is not a JSON array of strings: %v", name, arm, err)
				continue
			}
			for _, login := range entries {
				if !exactBotLoginRe.MatchString(login) {
					t.Errorf("%s allowlist entry %q is not an exact app-bot login — wildcards and non-bot patterns are forbidden (#1710 ruling)", name, login)
				}
			}
		}
	}
}

// TestNoBlanketAssociationNonePass pins the #1710 security ruling: NEVER a
// blanket association == 'NONE' pass. An association-NONE arm would admit
// EVERY app bot and outside contributor (association is NONE for both),
// converting a deliberate per-bot trust decision into an open door. The
// exact-login allowlist is the only sanctioned way to admit an app bot.
func TestNoBlanketAssociationNonePass(t *testing.T) {
	for _, f := range workflowYAMLs(t) {
		name := filepath.Base(f)
		body := readFile(t, f)
		for _, blanket := range []string{
			"author_association == 'NONE'",
			"author_association != 'NONE'",
		} {
			if strings.Contains(body, blanket) {
				t.Errorf("%s contains blanket gate %q — association NONE is shared by every app bot and outside contributor; admit bots via the exact-login allowlist only (#1710 ruling)", name, blanket)
			}
		}
	}
}
