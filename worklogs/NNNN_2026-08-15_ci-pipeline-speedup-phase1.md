# Worklog: CI PR pipeline speed-up (phase 1)

**Date:** 2026-08-15
**Session:** Cut PR CI wall-time (measured 24–29 min) by parallelizing the test jobs past lint, cancelling superseded runs, moving the coverage-delta re-run off the critical path, and dropping a vestigial full-history fetch.
**Status:** Complete

---

## Objective

Reduce `ci.yml` PR wall-clock time (~25–30 min per run) without losing merge-gate strength. Phase 1 scope, agreed after a stress-test of trade-offs: (1) move the base-coverage re-run out of the critical path, (3) remove `needs: [lint]` from test jobs, (4) add `concurrency` with PR-only cancel-in-progress, (6) shallow clones where full history is unused. Deliberately NOT in this phase: removing `test-full`, per-component path filters, registry build cache, docs-only job-level filtering (phase 2 candidates).

---

## Work Completed

### Measured the baseline (data, not vibes)

Run `31871142694` (`fix/floating-tag-default-image`, 28.6 min): critical path was Lint 3.5m → Test 15.9m → Build Runtime 7.4m ≈ 27 min. The `test` job contained a serial re-run of the whole suite at the merge-base commit (~5m inside the job). Rapid main pushes queued 20-job runs on top of each other (37–42 min runs observed) because no workflow had a `concurrency` stanza.

### Change 1 — coverage delta moved to a parallel `coverage-delta` job

- `test` now uploads `coverage.out` and ends (kept: floor gate, coverage summary, artifact, vet, openapi-validate).
- New `coverage-delta` job: `needs: [test]`, PR-only, downloads the artifact, re-runs the suite at merge-base in a worktree, posts/updates the same PR comment (same comment markers, so existing threads keep updating).
- Base run drops `-race` (race detector does not change coverage percentages; ~2x faster) and keeps `-short` (same test selection as the PR run, apples-to-apples).
- Robustness fix found during the move: previously, if the BASE run flaked hard enough that `coverage-base.out` was unusable, `go tool cover -func` failed inside `test` and could block merge on a flake in code the PR didn't even touch. Now the job degrades to a "base unavailable" comment and stays green.
- `test` no longer needs `pull-requests: write`.

### Change 3 — removed `needs: [lint]` from `test`, `test-full`, `frontend-test`

All three now start at t≈0 alongside lint. (Correction, post-review: this originally said "merge is still gated on Lint via branch protection" — verified 2026-08-15 via the rulesets API that the repo's "Main Branch Protection" ruleset contains only `deletion` + `non_fast_forward`, i.e. **no required status checks exist**; the merge gate is process — green checks + reviewer APPROVE before squash merge — and was equally un-mechanical before this change.) Accepted trade-off: a lint-failing push now wastes one test-run's compute instead of delaying every run by 3.5 min.

### Review fixes (round 1 → 6dadf080; round 2 nits → docs pass)

Round-1 REQUEST CHANGES (AI reviewer, PR #866) fixed in `6dadf080`:
- **F1** — my original comment claimed scheduled and main-push runs "never cancel each other"; wrong: with `cancel-in-progress: false` GitHub still replaces a *pending* run when a newer run joins the group. Fixed in comments everywhere AND semantically: `security-scan.yml`/`envtest.yml` (the two scheduled workflows) now key the group on `${{ github.workflow }}-${{ github.event_name }}-${{ github.ref }}` so nightlies are un-droppable.
- **F3** — base `go test` exit status is now gated; a partway-failed base run (partial profile → understated base → optimistic delta) forces the degraded comment instead.
- **F4** — the sha-image consequence of pending-run replacement (replaced pending runs never publish `sha-<commit>` images; pin rollbacks by `ts-`/`dev`) is now spelled out in ci.yml.

Round-2 APPROVE notes fixed in the docs pass:
- **W1** — this addendum (the worklog previously described the pre-fix state and mis-attributed the F3 gating to the original move).
- **W2** — `envtest.yml` comment said "main-push run"; envtest has no push trigger. Corrected to nightly/dispatch.
- Branch-protection assumption (Key Decisions 5a) corrected above after API verification.

Live evidence gathered on the PR itself: parallel start at t≈0; `coverage-delta` posted the base/new/delta comment (base 67.4% / PR 67.4% / +0%); superseded-run cancellation demonstrated twice (run 31873628311 → cancelled with 16 jobs killed on push; a duplicate same-sha run pair deduplicated by the concurrency group).

### Change 4 — `concurrency` stanzas (PR-only cancel-in-progress)

Added to `ci.yml`, `security-scan.yml`, `secrets-integration.yml`, `envtest.yml`, `migration-safety.yml`:

```yaml
concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}
```

- PR ref groups (`refs/pull/N/merge`) → new push cancels the superseded run.
- Main/tags: `cancel-in-progress` evaluates false → a main push interrupted mid-image-push (incomplete ghcr tag set) is impossible; rapid main pushes serialize newest-wins instead of piling up.
- Deliberately NOT added to `pr-review.yml`, `renovate-analysis.yml`, `ai-comment.yml` (they write to PRs/issues; cancelling mid-write is riskier than the runs they waste).

### Change 6 — shallow fetch where full history was unused

- `test` checkout: dropped `fetch-depth: 0` (merge-base consumer moved to `coverage-delta`, the only job that now needs history).
- `security-scan.yml` gitleaks: dropped `fetch-depth: 0` — vestigial since the scan runs `--no-git` (working tree only); the old comment claimed history-walking that the command never did.
- `pr-review.yml` keeps its full fetch (AI review consumes git history for diff context; out of scope).

---

## Key Decisions

1. **Artifact-based delta instead of caching base coverage from main runs.** Cache misses are common (merge-base of a stale branch typically never ran on main as a push event). Artifact download keeps the comparison exact at the true merge-base. Cost: the base suite still runs once per PR — but in a job nothing depends on.
2. **`cancel-in-progress` as an expression, PR-only.** Protects ghcr tag-set integrity on main/tag pushes (an interrupted manifest merge would publish an incomplete multi-arch image).
3. **Keep `test-full` untouched in phase 1** (it was flagged for removal but only wastes compute, not wall-time — it finishes before `test`). Avoids conflating compute savings with latency savings in one PR; makes the verification measurements cleaner.
4. **No path filters in this phase.** The docs-only-PR trap (worklog-numbering PRs skipping repolint → incident 0552/0553 class) requires job-level `dorny/paths-filter` design, deferred to phase 2.
5. **Assumptions recorded per Rule 4:** (a) ~~branch protection requires job names, which are unchanged — no required-check breakage~~ — corrected post-review: no required-status-checks rule exists; job-name stability is still kept so any future protection config picks the names up unchanged; (b) `cancel-in-progress` accepts expressions (GitHub-documented, actionlint-validated); (c) fork PRs get the same group per PR number (fork PRs run `pull_request`, not `pull_request_target`, in all five workflows).

---

## Blockers

None.

---

## Tests Run

- `actionlint` v1.7.12 on all five edited workflow files: clean (also validates YAML + expression contexts).
- `make repolint`: all checks passed.
- Python YAML parse attempt: skipped (no pyyaml in sandbox; actionlint parse supersedes it).
- Full validation = the PR run itself: `pull_request` events execute the workflow file from the PR branch, so this PR exercises the new pipeline pre-merge. Verification plan: (a) jobs start at t≈0 parallel to lint; (b) coverage-delta posts the comment; (c) second push to the PR cancels the first run; (d) post-merge main push completes without cancellation and measurably faster.

---

## Next Steps

1. After merge: pull run timings from the API (jobs' started_at deltas) and compare against the 28.6-min baseline; record actual numbers in the follow-up worklog.
2. Phase 2 candidates (each needs its own stress-test): remove `test-full` (~11m compute/PR, 0 wall-time), per-component build path filters + amd64-only PR builds, `type=registry` buildx cache (fixes the 10-scope gha-cache eviction), docs-only PR fast path with job-level filtering, pin `golangci-lint` version.
3. Watch for the first superseded-PR-cancel in practice and confirm no orphaned state. (Observed during review, 2026-08-15: cancellation was demonstrated twice and left no shared state behind. Note on `always()`: per GitHub docs `always()` evaluates true even for cancelled jobs, but a cancelled job's in-flight steps are terminated regardless — either way the canary's `if: always()` kind-cleanup does not complete on cancel; acceptable because the kind cluster is runner-local and dies with the runner.)

---

## Files Modified

- `.github/workflows/ci.yml` — concurrency stanza; `test` restructure (no lint dep, shallow clone, read-only perms); new `coverage-delta` job; `test-full`/`frontend-test` lint dep removed.
- `.github/workflows/security-scan.yml` — concurrency stanza; gitleaks shallow checkout + corrected comment.
- `.github/workflows/secrets-integration.yml` — concurrency stanza.
- `.github/workflows/envtest.yml` — concurrency stanza.
- `.github/workflows/migration-safety.yml` — concurrency stanza.
- `worklogs/NNNN_2026-08-15_ci-pipeline-speedup-phase1.md` — this worklog.
- (docs pass, review round 2): `.github/workflows/ci.yml` comment-only correction (branch-protection claim), `.github/workflows/envtest.yml` comment-only correction (W2), this worklog's review-fix addendum.
