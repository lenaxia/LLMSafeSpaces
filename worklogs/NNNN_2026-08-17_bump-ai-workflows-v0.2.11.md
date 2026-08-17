# Worklog: Bump ai-workflows pins to v0.2.11

**Date:** 2026-08-17
**Status:** Complete

---

## Objective

Coordinate-bump all three ai-workflows caller pins from v0.2.10 to v0.2.11
(pr-review.yml, ai-comment.yml, renovate-analysis.yml) together with the
version literals in both contract-test packages.

## Work Completed

1. All three `uses:` refs + both `with.version:` inputs bumped to v0.2.11.
2. `tests/ghaworkflows/gha_callers_test.go` and
   `tests/renovateanalysis/renovate_analysis_test.go` version literals
   updated in the same change (the coordinated-bump convention the #914
   round-2 review called out).
3. Verified the v0.2.11 delta before bumping: the only reusable-workflow
   change is pr-review.yml `persist-credentials: false → true` (ai-workflows
   #40) — opencode's STARTUP `git fetch origin --depth=20 <branch>` died
   with `could not read Username` (exit 128) on private consumers
   (v0.2.2–v0.2.10 regression; public repos immune). The persisted
   credential is safe: job token is `contents: read`, so the end-of-run
   snapshot push still fails — the tolerated Bug A failure the Verify step
   was built for. ai-comment.yml and renovate-analysis.yml are byte-
   identical between v0.2.10 and v0.2.11.

## Key Decisions

- Bump the unchanged workflows too (single fleet pin) rather than splitting
  versions across callers — matches the propagate convention and keeps the
  contract tests' single-literal pattern.

## Record correction (per #914 round-2 review nit)

The onboarding worklog's red-proof evidence says the be3320ec red run
reports "3 dangling references in ai-comment.yml" — incorrect: the `seen`
map dedupes 3 textual mentions of the same path into **1** execution-
reference error. The red proof holds (the test fails at be3320ec); only the
stated count was wrong.

## Tests Run

- `go test -race -count=1 ./tests/ghaworkflows/ ./tests/renovateanalysis/`
  → PASS at v0.2.11 pins (would fail if any literal/ref pair diverged).
- `gofmt -l tests/` → clean. `make repolint` → all checks passed.
- Diffed reusable workflows v0.2.10 ↔ v0.2.11 at the tags before bumping.

## Next Steps

1. Salvage step + bounded retry remain open for ai-workflows itself.
2. Watch first post-bump review run (persist-credentials change) — expect
   behavior unchanged for this public repo.

## Files Modified

- `.github/workflows/pr-review.yml`
- `.github/workflows/ai-comment.yml`
- `.github/workflows/renovate-analysis.yml`
- `tests/ghaworkflows/gha_callers_test.go`
- `tests/renovateanalysis/renovate_analysis_test.go`
- `worklogs/NNNN_2026-08-17_bump-ai-workflows-v0.2.11.md`
