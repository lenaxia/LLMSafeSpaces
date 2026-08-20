# Worklog: migrate-kek stub constructors vs staticcheck SA4023 (main Lint red)

**Date:** 2026-08-20
**Session:** Un-block main's failing Lint job so docs PR #962 (and everything behind it) can merge.
**Status:** Complete

---

## Objective

Main's CI Lint job fails with staticcheck SA4023 in `cmd/migrate-kek/main.go` (6 findings): the stub constructors (`newPgMigrationStore`, `newRedisCacheFlusher`) provably ALWAYS return errors ("not yet wired", per the rotate-kek convention), so every `if err != nil` after them is statically always-true — a checker-version bump turned latent dead branches into blocking findings. Pre-existing (main's own CI run red); not caused by any open PR.

## Work Completed

- `store.go`: extracted an explicit `errNotWired` sentinel (errors.New) from the inline message — callers can now `errors.Is` the stub condition; the constructor returns the sentinel.
- `main.go`: the two always-true checks annotated `//nolint:staticcheck // SA4023: stub constructor always errors until PG/Redis wiring lands` — the honest fix given wiring is deferred BY DESIGN; the alternative (deleting the checks) would change runtime behavior the day the stubs get wired.

## Key Decisions

1. Annotate-don't-delete: the checks are correct future code for the wired constructor; SA4023 is right that they're dead TODAY. nolint-with-reason documents both.
2. Sentinel over message-string: if anything ever needs to detect the stub programmatically, `errors.Is` beats string matching.

## Tests Run

`golangci-lint run ./cmd/migrate-kek/...` — 0 issues (was 6 SA4023). `go build` green. No behavior change (constructor messages preserved verbatim inside the sentinel).

## Files Modified

- `cmd/migrate-kek/store.go`, `cmd/migrate-kek/main.go`
- `worklogs/NNNN_2026-08-20_migrate-kek-stub-sa4023.md` (this file)
