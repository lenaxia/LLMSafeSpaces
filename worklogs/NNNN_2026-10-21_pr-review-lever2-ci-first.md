# Worklog: PR-review runtime lever 2 — CI-first execution policy in the consumer review prompt

**Date:** 2026-10-21
**Session:** PR-review runtime-optimization lane (worker wt-perf2), evidence base LLMSafeSpaces PR #1763 comments 6306721273 / 6308423248 / 6309172907.
**Status:** r1 — incorporating the PR #1833 first review (CHANGES_REQUESTED: --watch self-deadlock, secrets-integration.yml conditional coverage, skipped-check semantics, red triage, sentinel worklog naming, pin hardening).

---

## Objective

Cut the reviewer's measured double-execution and idle-wait waste without touching the quality floor. The revalidation table (comment 6309172907, n=11 runs) sized lever 2 at **mean 3.1 min/run (median 2.6, max 11.2) on suite-running rounds**: 1782s of reviewer-local runs of suites CI already executes on the same head (`go vet ./...`, full `-race` suite, `-short` coverage, vitest) plus 260s of `sleep N; gh pr checks` idling. The reviewer-unique residue — `-tags integration` board/agentd suites when CI did not run them, agentd E2E, mutation/red-checks (494s + ~45s across the sample) — stays mandatory. Owner constraints: no model change, no tool removal, findings quality identical.

## Change

`.github/prompts/pr-review.md` (consumer prompt) gains a `VERIFICATION EXECUTION POLICY — CI FIRST` section between the intro and the review checklist:

1. `gh run list --commit <head SHA>` FIRST (or `gh pr checks` for summary), bound to the exact head SHA under review;
2. CI-green (conclusion: SUCCESS) on the same head ⇒ do NOT re-run that suite locally; cite the check name as execution evidence. A SKIPPED check is not evidence — when a check was skipped and the diff touches what it would have run, execute it locally. CI-covered list grounded at ci.yml:145/264/304/487/514 plus the NAMED `-tags integration` pins at ci.yml:617-629 (origin-plugin, permission-tier incl. TestIntegrationHarness_ChildHonorsSyntheticDataHome, origin E2E in cmd/workspace-agentd, #1722 in cmd/workspace-agentd);
3. **Conditional board coverage:** the path-filtered secrets-integration.yml workflow (filters at secrets-integration.yml:65: pkg/board/, api/internal/handlers/board*, cmd/workspace-agentd/board_manager_e2e_integration_test.go; suites at :144-179) runs `-tags integration` board-manager kill-tests, worker/manager semantics, and board-manager E2E — when it RAN and is green on the head, those suites are covered; when path-skipped or red, they are the reviewer's local obligation (PR #1833 review finding 2);
4. red triage: `gh run view --log-failed`, classify infra/transient (runner eviction, network 5xx, provider 429, snapshot flake) as CI noise vs diff-related failures as review findings (finding: robustness);
5. in-flight CI: wait on the CI workflow runs ONLY — `gh run list --workflow ci.yml --commit <sha>` + `gh run watch <id>` once; NEVER `gh pr checks --watch` (watches ALL checks including THIS review job — self-deadlock until job timeout; finding 1); never sleep-poll;
6. always still run locally the CI-uncovered residue: `-tags integration` suites not covered above, agentd E2E beyond named pins, mutation/red-checks in a scratch worktree;
7. explicit non-waiver: CI-green is execution evidence only — it does NOT waive any TESTS-gate requirement; the diff must still ADD the mandated test levels.

Quality floor unchanged: no gate weakened, no model/tool change; the policy only changes where execution evidence comes from.

## Red-first pin

`tests/ghaworkflows/pr_review_prompt_policy_test.go` — `TestPrReviewPromptCIFirstExecutionPolicy`: 12 whitespace-normalized positive markers (incl. `gh run watch`, `a SKIPPED check is not evidence`, `secrets-integration.yml`, `infra/transient failure`), 4 negative inversion markers (the exact phrases an adversarial edit used to defeat a Contains-only pin in the #1833 review), a count-based deadlock guard (`gh pr checks --watch` may appear ONLY inside its NEVER-prohibition — red-checked by appending a bare instruction), and an ordering assertion (policy precedes the review checklist). Red on the pre-edit prompt (all markers missing), green at the r1 head.

## Evidence

- Prompt edit + pin: this PR's diff.
- CI grounding verified in-repo: ci.yml:145/264/304/487/514 (vet, `-race -short`, vet, `-race -count=1 ./...`, vitest); ci.yml:617-629 named `-tags=integration` pins (TestOriginPlugin / TestPermissionTier + TestIntegrationHarness_ChildHonorsSyntheticDataHome / TestOriginE2E / TestIntegration1722); secrets-integration.yml:65,144-179 path filters + board-manager `-tags integration` suites.
- Savings basis: PR #1763 comment 6309172907 (revalidation, n=11 runs).

## Conventions notes

- Worklog named with the `NNNN_` sentinel per README-LLM §Worklog Requirements (number assigned by the repolint bot post-merge; main's 1163 was already taken by 1817-phrase-id-edge at r0 time).
- The CHANGELOG [Unreleased] entry is kept per the orchestrator's standing convention for this lane (explicit lane instruction "CHANGELOG [Unreleased]"); flagged non-blocking by the r0 review (README-LLM:1006 assigns release trains to the orchestrator) — orchestrator may drop or keep at merge.

## Not done here (per lane discipline)

- Lever 1 (subagent cap): SHELVED by owner ruling.
- Lever 4 (debounce): owner-approved 10-min quiet-window variant, sequenced after this + ai-workflows #69 — separate change in ai-workflows.
