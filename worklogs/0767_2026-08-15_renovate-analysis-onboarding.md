# Worklog: Onboard to the reusable renovate-analysis workflow (ai-workflows@v0.2.10)

**Date:** 2026-08-15
**PR:** #875
**Issue:** #873

---

## Context

Renovate PRs (opened by `renovate[bot]`) were not AI-analyzed before merge.
The org now ships a reusable analysis workflow in `lenaxia/ai-workflows`
(ai-workflows#33, tag `v0.2.10`): it posts a `## Renovate PR Analysis` comment
on every open `renovate[bot]` PR and auto-merges only PRs it recommends as
"Safe to merge".

The reusable workflow must be triggered by `schedule` + `workflow_dispatch`
only — never `pull_request` — because the opencode action's
`assertPermissions()` requires the event actor to be a repo collaborator with
write access, and `renovate[bot]` (an app bot account) can never be one. The
old `.github/workflows/renovate-analysis.yml` here triggered on
`pull_request`, so every renovate-pr run died before the AI could analyze.

## Changes

1. `.github/workflows/renovate-analysis.yml` — replaced the broken
   `pull_request`-triggered workflow with the thin caller (schedule
   `0 */2 * * *` + `workflow_dispatch` with optional `pr_number`, the four
   write permissions, `secrets: inherit`,
   `uses: lenaxia/ai-workflows/.github/workflows/renovate-analysis.yml@v0.2.10`,
   `project_name`).
2. `.github/prompts/renovate-analysis.md` — fork of the v0.2.10 template
   (verified against the actual tag's template, since `ai-sync render` isn't
   runnable here) with LLMSafeSpaces context (Go K8s platform areas
   `api/`/`controller/`/`pkg/`/`cmd/`/`frontend/`/`helm/`/`runtimes/`, single
   maintainer, README-LLM.md pointer) and exclusions: `controller-runtime`,
   `k8s.io/*`/`sigs.k8s.io/*`, `mark3labs/mcp-go`, LLM/AI SDKs, auth/crypto
   packages, core data-path deps (gin, pgx, go-redis), `bitnami/*`, any
   dependency the analysis flags as security-sensitive, major bumps, plus the
   template's toolchain-bump check. Template guardrails kept verbatim
   (post-and-verify loop, `/tmp`-only scratch, read-only checkout, no-branch
   rule, "when in doubt → Needs manual review").
3. `renovate.json` — added `:disableRateLimiting`/`:dependencyDashboard`,
   the github-actions digest/patch/minor branch-automerge rule, and
   never-auto-merge guards for both `anomalyco/opencode` and
   `lenaxia/ai-workflows` (opencode/ai-workflows bumps change permission
   semantics — the exact `assertPermissions()` failure class behind this
   issue). All existing npm/go grouping and kubernetes-coordination rules
   preserved.
4. `tests/renovateanalysis/renovate_analysis_test.go` — contract-guard suite
   (new package under `tests/`, run by `make test`/CI, following the
   `tests/gharouter` precedent). Pins: schedule/dispatch-only triggers (never
   `pull_request`), pinned reusable ref + `secrets: inherit` + input
   pass-through, all four permissions, the three prompt files the reusable
   job cats, prompt fork/guardrail invariants, and the renovate.json guards.

## Assumptions stated and validated

- Reusable workflow inputs/contract (`project_name` required, prompt files
  read from the consumer's `.github/prompts/`, three cat'ed files) —
  validated by fetching the actual v0.2.10 workflow source.
- `renovate.json` parses with `encoding/json` — validated by the
  contract-guard test reading `packageRules`.

## Verification

- `go test ./tests/renovateanalysis/` — 6/6 pass.
- `go test ./tests/...` — pass; `gofmt`/`go vet` clean.

## Out of scope (tracked elsewhere)

- ai-workflows#36: `renovate-analysis.md` must be listed under `forked:` in
  `consumers/llmsafespaces.yaml` upstream before the next propagate run,
  otherwise the fork gets overwritten by the generic template.
- Post-merge step 5: dispatch the workflow once; with 0 open renovate PRs it
  should run green and post no comment (per the prompt).