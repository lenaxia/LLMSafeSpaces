# Worklog: PR-review runtime lever 2 — CI-first execution policy in the consumer review prompt

**Date:** 2026-10-21
**Session:** PR-review runtime-optimization lane (worker wt-perf2), evidence base LLMSafeSpaces PR #1763 comments 6306721273 / 6308423248 / 6309172907.
**Status:** Complete

---

## Objective

Cut the reviewer's measured double-execution and idle-wait waste without touching the quality floor. The revalidation table (comment 6309172907, n=11 runs) sized lever 2 at **mean 3.1 min/run (median 2.6, max 11.2) on suite-running rounds**: 1782s of reviewer-local runs of suites CI already executes on the same head (`go vet ./...`, full `-race` suite, `-short` coverage, vitest) plus 260s of `sleep N; gh pr checks` idling. The reviewer-unique residue — `-tags integration` board/agentd suites, agentd E2E, mutation/red-checks (494s + ~45s across the sample) — is NOT CI-covered and must stay mandatory. Owner constraints: no model change, no tool removal, findings quality identical.

## Change

`.github/prompts/pr-review.md` (consumer prompt) gains a `VERIFICATION EXECUTION POLICY — CI FIRST` section between the intro and the review checklist:

1. `gh pr checks <PR>` / `gh run list --commit <head SHA>` FIRST, bound to the exact head SHA under review;
2. CI-green on the same head SHA ⇒ do NOT re-run that suite locally; cite the check name as execution evidence (CI-covered list grounded at ci.yml:145/264/304/487/514 plus the NAMED `-tags integration` pins at ci.yml:617-629 — origin-plugin, permission-tier, origin E2E, #1722, harness synthetic-data-home);
3. CI red ⇒ read `--log-failed`, treat as a finding, no local re-run;
4. CI in-flight ⇒ one `gh pr checks --watch` or a single re-poll; never sleep-poll;
5. always still run locally the CI-uncovered residue: `-tags integration` beyond the named pins (board manager semantics/E2E, agentd handlers), agentd E2E beyond named pins, mutation/red-checks in a scratch worktree;
6. explicit non-waiver: CI-green is execution evidence only — it does NOT waive any TESTS-gate requirement; the diff must still ADD the mandated test levels.

Quality floor unchanged: no gate weakened, no model/tool change; the policy only changes where execution evidence comes from.

## Red-first pin

New `tests/ghaworkflows/pr_review_prompt_policy_test.go` — `TestPrReviewPromptCIFirstExecutionPolicy` pins 9 whitespace-normalized markers (gh pr checks; do NOT re-run it locally; same head SHA; `-tags integration`; agentd E2E; mutation; `--watch`; never sleep-poll; does NOT waive). Observed red against the pre-edit prompt (all 9 markers missing), green after. Sits beside the existing prompt-contract pins in tests/ghaworkflows/gha_callers_test.go (TestPromptContractFilesExist).

## Evidence

- Prompt edit + pin: this PR's diff.
- CI coverage grounding verified in-repo: `sed -n '145p;264p;304p;487p;514p;615,630p' .github/workflows/ci.yml` — vet, `-race -short`, second vet, `-race -count=1 ./...`, `npx vitest run`, named `-tags=integration` pins (TestOriginPlugin / TestPermissionTier / TestOriginE2E / TestIntegration1722 only — the broad board/agentd/handlers integration surface is NOT run by CI).
- Savings basis: PR #1763 comment 6309172907 (revalidation, n=11 runs).

## Not done here (per lane discipline)

- Lever 1 (subagent cap): SHELVED by owner ruling.
- Lever 4 (debounce): owner-approved 10-min quiet-window variant, sequenced after this + ai-workflows #69 — separate change in ai-workflows.
