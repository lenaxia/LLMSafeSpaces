# Worklog: docs-only CI fast path

**Date:** 2026-08-28
**Session:** PR #1117 (design doc + worklog) fired the full ~20-job suite for a two-markdown-file change. User asked for docs-only PRs to skip the heavy suites. Added a `changes` detection job to the three per-PR workflows and gated the expensive jobs on its output.
**Status:** Complete (PR open)

---

## Objective

A change confined to `*.md` / `docs/` / `design/` / `worklogs/` cannot affect Go code, builds, or migrations — running the full test/build matrix on it wastes runner minutes on every docs PR.

## Changes

- `.github/workflows/ci.yml` — new `changes` job (full-fetch checkout, `git diff --name-only origin/main...HEAD | grep -qvE '\.md$|^docs/|^design/|^worklogs/'`); `test`, `test-full`, `frontend-test`, `prepare` gain `needs: [changes]` + `if: github.event_name != 'pull_request' || needs.changes.outputs.docs_only != 'true'`. Everything heavier (coverage-delta, sdk-contract, sdk-canary, all build-*/merge-*) skips transitively via the needs graph.
- `.github/workflows/secrets-integration.yml` — same `changes` job; `pkg-secrets-integration` gated.
- `.github/workflows/e2e-pr.yml` — same `changes` job; `e2e-frontend` gated (PR-only workflow, no event bypass needed).

## Design decisions

- **Job-level skip, not workflow-level `paths`**: skipped jobs keep reporting their stable names as "skipped" on the PR, satisfying any future required-check config (the test job's comment documents job names are kept stable for exactly that); workflow-level filters would leave future required checks hanging as "expected". Also confirmed in that comment: the branch protection ruleset has NO required-status-checks rule today, so nothing can hang either way.
- **Fail safe**: detection failure (missing merge-base, git error) resolves to `docs_only=false` → full suite.
- **push/tag/dispatch always run everything** — main is the safety net that catches misdetection.
- **Still runs on docs-only PRs**: `lint` (repolint validates worklog numbering — exactly what docs PRs add), `security-scan` (gitleaks should scan markdown for pasted secrets; trivy is 16s), `pr-review` (AI review).
- `envtest` and `migration-safety` already path-filtered — untouched.

## Verification

- Detection logic simulated locally against representative file lists (md-only → skip; md+go/yaml/ts → run; empty → run/fail-safe). All cases correct.
- No YAML tooling in sandbox (no python/go/node); structural verification by review; GitHub's workflow parser is the authoritative check on the PR.
- This PR itself changes workflow YAML → not docs-only → full suite runs on it (correct).

---

## Follow-up (same PR): content-type gates for e2e-pr and secrets-integration

**Session:** Reviewer pointed out the docs-only gate leaves savings on the table for every non-docs PR too: a Go PR was still running the Playwright suite, a frontend PR was still spinning Postgres/Redis service containers.

**Changes:**

- `e2e-pr.yml` — `changes` now outputs `frontend_changed` (matches `^frontend/` + the workflow file) instead of `docs_only`; `e2e-frontend` skips on explicit `false` only. A Go/helm/SDK/docs PR no longer runs Playwright.
- `secrets-integration.yml` — `changes` now outputs `secrets_changed` with a path set derived from the workflow's own `go test` lines: `pkg/secrets/`, `pkg/workflows/`, `api/internal/services/database/`, `api/internal/services/passkey/`, `api/internal/testharness/`, `api/migrations/`, `go.mod`/`go.sum`, and the workflow file. A frontend/SDK/helm PR no longer runs the live-Postgres suite.

**Design decisions:**

- Gate direction inverted vs the docs gate: these are "should-run" flags, so the job runs unless the detector PROVES irrelevance (`!= 'false'`). Empty or missing output → run. A hard detection failure fails the `changes` job visibly (retryable) rather than silently skipping.
- Each workflow file matches its own filter, so edits to a workflow always run that workflow — a new test package added to secrets-integration.yml can't be stranded behind a stale filter.
- ci.yml's `changes` stays docs-only deliberately: its jobs (test/lint/builds) legitimately consume the whole repo, and finer splits there are a separate decision.
- Fail-safe note on the base commit's claim: under `set -euo pipefail`, a failed `git diff` fails the `changes` job and downstream jobs are skipped as failed-needs — loud, not silent. The "resolves to docs_only=false" wording overstated it; behavior is unchanged (this was already true of the base commit's script).

**Verification:**

- Path sets cross-checked against the workflow files' own `go test`/`working-directory` lines and against `grep -rln 'pkg/secrets'` importer lists.
- This commit edits both workflow files → both are in their own path sets → both run on this PR (correct).
