# Worklog: re-execute the legacy runtime-Dockerfile deletion (#854)

**Date:** 2026-08-19
**Session:** Close #854 per the 2026-08-14 investigation: delete (not bump) the accidentally-resurrected per-language runtime trees; add the repolint resurrection guard.
**Status:** Complete

---

## Objective

The investigation on #854 (2026-08-14) established: tenants never ran Go 1.20.5 — `runtimes/{go,nodejs,python}/` were deleted by `1166b86d` (June 4, Epic 7 / US-7.8), accidentally resurrected the same evening by the stale-branch merge `c9c68684`, and are built by NO pipeline (CI/release/e2e build `runtimes/base` only; the chart and image-factory seed `base` only). The tenant Go toolchain is mise in base, pinned 1.26.6 by #853. My first attempt (#956) bumped the dead pin — correctly rejected by review; the fix is deletion.

The prior fix branch (`feat/issue-854-remove-legacy-runtime-dockerfiles`, commit 422dd467) was never pushed (same bot push-failure class as the #762 fix branch and design 0050) and its objects are unrecoverable in this workspace. This session re-executed the change from the investigation's preserved recipe.

## Work Completed

- **Deleted** `runtimes/{go,nodejs,python}/` (incl. `Dockerfile.ml`) and `runtimes/tests/` — 18 files, ~1.1k lines of dead, rotting tree.
- **repolint `ForbiddenPathsCheck`** (new, TDD): fails when any legacy path exists; `TestForbiddenPathsCheck_RepoTree` was demonstrably RED on the pre-deletion tree and GREEN after — the exact mutation proof the investigation required. Wired into `cmd/repolint` (pre-commit + CI). The report names `c9c68684` so the next person finds the resurrection mechanism in the error message.
- **renovate.json**: dropped the `runtimes/tests/requirements.txt` regexManager (target gone).
- **docs/operator/runtime-environments.md**: replaced the per-language table with the one-image reality; fixed BOTH phantom-image examples (`ghcr.io/lenaxia/llmsafespaces/python:3.11` — an image no pipeline ever published) to point at `base:<pinned-tag>`; documented the deletion + mise rationale.

## Key Decisions

1. Deletion over bump — per the investigation; a bumped dead file is still dead.
2. The guard lives in repolint (runs pre-commit + CI), not a CI-only check — the c9c68684 failure mode was a MERGE, which pre-commit on the branch wouldn't catch, but CI's repolint job does; both run it.
3. Local cleanup en route: stale untracked NNNN worklog leftovers from earlier sessions (d3-outbox, fail-closed-boot — both bot-numbered on main long ago) were removed; they were breaking `TestLive_Worklogs_NoDuplicates` locally.

## Tests Run

- `TestForbiddenPathsCheck_{CleanTree,DetectsResurrection,RepoTree}` — red pre-deletion, green post (RepoTree is the permanent live guard)
- Full `./pkg/repolint/` suite — green; `go run ./cmd/repolint` — all checks passed
- `go build ./...` — green

## Files Modified

- `runtimes/{go,nodejs,python,tests}/` (deleted, 18 files)
- `pkg/repolint/forbidden_paths.go` + `forbidden_paths_test.go` (new)
- `cmd/repolint/main.go` (wired)
- `.github/renovate.json`, `docs/operator/runtime-environments.md`
- `worklogs/NNNN_2026-08-19_runtimes-legacy-dockerfile-deletion.md` (this file)
