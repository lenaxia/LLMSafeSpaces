# Worklog — CI matrix + release train complexity audit (tsk-hazel-mint-waits)

Read-only audit session (2026-11-17, ~2h): inventory + cost assessment of the per-PR
check matrix and the release train. Single artifact: `reports/audits/ci-release-complexity.md`
(no code/workflow/Makefile/chart changes).

## Assumptions

- "Cost per PR" = runner-seconds of one green pull_request run of ci.yml (measured), plus
  sibling-workflow runs where path filters admit them; wall-clock from run timestamps.
- "Catch value" = documented incident/regression archaeology only (commit messages, worklogs,
  workflow-header comments, CHANGELOG). No counterfactual claims.
- The in-flight `fix/2092-crd-skew-convergence` branch is treated as **unmerged** (verified
  against origin/main at b1987029b) and flagged as such everywhere it is cited.

## Data sources (first-hand)

- ci.yml job durations + conclusions: Actions run `44188388905` (PR #2083 final head, green;
  37 jobs incl. 9 skipped merges — 150.1 runner-min total).
- Failure archaeology: runs 44168461790 / 44146217348 (test+test-full red —
  TestTrustedAppBotsInputMatchesCanonicalList, TestDocs_MCPTools_Parity), 44152843467 (lint),
  44061580277 (frontend+builds), 44180181647 (Build Opencode arm64, docs-only head).
- Release-train costs: runs 43983788986 (512s), 43986233145 (265s), 43987590002 (1870s,
  v0.44.15's three failed trains), 43992129196 (1722s, success), 44114269696 (1914s, v0.44.16).
- posture-gate: 44165478877 (1178s). pr-review lane durations from recent run list.
- Runbooks: /workspace/reports/release-0.44.16-runbook.md, release-0.44.17-runbook.md,
  release-0.44.17-status.md (workspace-local evidence for cut friction, canary 429
  signature 3/3 on PR #2087, picomatch scan friction blocking #2090).
- Git: `git log` archaeology for every catch citation (commits quoted inline in the audit);
  `grep -rn 'testing.Short()'` census = 43 hits / 33 files (16 under cmd/workspace-agentd).

## Decisions (keep/cut rationale summary)

- KEEP: changes, lint, test, test-full (real -short surface — its ci.yml header comment is
  stale, finding F9), frontend-test, abi-schema, origin-plugin-pin, sdk-contract, envtest,
  migration-safety, numbering-gate, security-scan, pr-review, run-watchdog.
- CUT: e2e-pr.yml e2e-frontend (functionally duplicate of frontend-test — same step set,
  different order, ubuntu-latest vs the 24.04 pin; corrected r0 per review).
- CONDITIONALIZE: the 9×2 PR build matrix (49% of spend, artifacts never pushed on PRs).
- DEMOTE: coverage-delta (informational only, never blocks).
- TUNE: sdk-canary (non-blocking + 4×sleep-65 + 429 signature), posture-gate paths.
- Top-5 ranked by value/risk with functional-equivalence arguments: audit §6.
