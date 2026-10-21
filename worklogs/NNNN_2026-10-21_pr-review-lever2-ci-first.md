# Worklog: PR-review runtime lever 2 — CI-first execution policy in the consumer review prompt

**Date:** 2026-10-21
**Session:** PR-review runtime-optimization lane (worker wt-perf2): implement lever 2 of the #1763 drill-down in the consumer review prompt, iterate PR #1833 to approval.
**Status:** Complete

---

## Objective

Cut the reviewer's measured double-execution and idle-wait waste without touching the quality floor. Evidence: PR #1763 comments 6306721273 / 6308423248 / 6309172907 (revalidation, n=11 runs) — lever 2 sized at mean 3.1 min/run (median 2.6, max 11.2) on suite-running rounds: 1782s of reviewer-local re-runs of suites CI already executes on the same head, plus 260s of `sleep N; gh pr checks` idling. The reviewer-unique residue (board/agentd `-tags integration` suites when CI did not run them, agentd E2E, mutation/red-checks; 494s + ~45s across the sample) stays mandatory. Owner constraints: no model change, no tool removal, findings quality identical.

---

## Work Completed

### `.github/prompts/pr-review.md` — VERIFICATION EXECUTION POLICY — CI FIRST section (additive-only, all existing gates intact)

- `gh run list --commit <head SHA>` FIRST; skip-evidence bound to the exact head SHA.
- CI-green (conclusion: SUCCESS) on the same head ⇒ do NOT re-run locally; only SUCCESS counts — a SKIPPED check is not evidence; path-skipped suites are the reviewer's obligation when the diff touches them. CI-covered list grounded at ci.yml:145/264/304/487/514 + the NAMED `-tags integration` pins at ci.yml:617-629 (TestOriginPlugin, TestPermissionTier + TestIntegrationHarness_ChildHonorsSyntheticDataHome, TestOriginE2E, TestIntegration1722 — pkg/agent/opencode/ and cmd/workspace-agentd).
- Board-manager conditional coverage keys on the `pkg/secrets integration` JOB (secrets-integration.yml path filter :65, suites :144-179) concluding SUCCESS — never the workflow-level conclusion (the workflow always runs; a path-skipped job still shows a green run).
- Deadlock-safe wait: CI workflow runs only (`gh run list --workflow ci.yml --commit <sha>` + `gh run watch` once); NEVER `gh pr checks --watch` (watches THIS review job's own check — self-deadlock); never sleep-poll.
- Red triage: infra/transient failures (runner eviction, 5xx, provider 429, snapshot flake) are CI noise; only diff-related failures are findings.
- Non-waiver: CI-green is execution evidence only — does NOT waive any TESTS-gate requirement.

### Red-first pin — `tests/ghaworkflows/pr_review_prompt_policy_test.go`

`TestPrReviewPromptCIFirstExecutionPolicy`: 12 whitespace-normalized positive markers, 5 negative forbidden-phrase markers, a count guard (`gh pr checks --watch` may appear only inside its NEVER-prohibition), and a policy-before-checklist ordering assertion. Red on the pre-policy prompt (all markers missing); guards mutation-checked (bare `--watch` instruction, inversion phrases, workflow-level board condition).

---

## Key Decisions

- Job-level (not workflow-level) board coverage condition: adopted after the r1 review demonstrated a real hole — the secrets-integration workflow ALWAYS runs (changes-detect job) and a path-skipped suite job still yields a green workflow run; e.g. a `cmd/workspace-agentd/board_manager_tools.go` change is NOT matched by the job's path filter while `TestIntegrationBoardManagerE2E` (which dispatches tools from that file) IS the suite you need. Coverage keys on the check itself.
- Worklog named with the `NNNN_` sentinel (README-LLM:826-843; manual numbers race — main's 1163 was taken by 1817-phrase-id-edge mid-flight).
- CHANGELOG [Unreleased] entry kept per the orchestrator's standing lane instruction ("CHANGELOG [Unreleased]"); flagged non-blocking by both reviews (README-LLM:1006 assigns release trains to the orchestrator) — the orchestrator may drop it at merge.

---

## Blockers

None.

---

## Tests Run

- `go test ./tests/ghaworkflows/ -run TestPrReviewPromptCIFirstExecutionPolicy -count=1` — ok (12 positives / 5 negatives); red-verified against the pre-policy prompt and against each mutation (bare `--watch` instruction; workflow-level board phrasing; inversion phrases).
- `go test ./tests/ghaworkflows/ -count=1` — ok. `go vet ./tests/ghaworkflows/` — clean.
- CI on the branch head: Lint, Test (-short), Test (full suite, race), Frontend, Origin plugin freeze pin, Harness ABI, Gitleaks — SUCCESS (five docker build-push jobs fail `go mod download` — pre-existing on main at 43a7ba29, no build inputs touched).

---

## Next Steps

- PR #1833 r2 review verdict; address any further findings, then orchestrator merges.
- Lever 4 (debounce, 10-min quiet window) is committed locally on feat/pr-review-debounce in ai-workflows (c3293b7...3110c12), pushed only after this + #69 sequence completes per the owner ruling.

---

## Files Modified

- `.github/prompts/pr-review.md` (additive policy section)
- `tests/ghaworkflows/pr_review_prompt_policy_test.go` (new)
- `worklogs/NNNN_2026-10-21_pr-review-lever2-ci-first.md` (this worklog)
- `CHANGELOG.md` ([Unreleased] entry)
