# Worklog 0665 — Repolint gate: gin.SetMode must not run under t.Parallel

**Date:** 2026-08-02
**Scope:** Add a repo lint gate preventing recurrence of the 2026-08-02
`gin.SetMode` data race (worklog 0663) — a `*_test.go` file calling
`gin.SetMode` from a `t.Parallel()` test body.

## Summary

Worklog 0663 fixed the race that red-lined main CI and blocked the v0.7.1
release gate. This follow-up hardens against recurrence with a **repolint
check** so the identical bug turns CI red *at the lint stage* instead of in
the race-detector run.

## Why a lint gate instead of bulk cleanup

The bot review of PR #627 offered two options: bulk-replace ~100+ redundant
per-test `gin.SetMode` calls, or add a lint check. Bulk cleanup would touch
~110 `_test.go` files across the repo (much of it other agents' in-flight
work) with a large, noisy diff. The lint gate achieves the same protection
with a focused diff and is enforceable in pre-commit + CI forever:

- `make repolint` runs in the Lint job (`.github/workflows/ci.yml`) and in
  `.githooks/pre-commit`.
- The check is precise: it only flags `gin.SetMode` calls reachable from a
  `t.Parallel()` body — serial-only per-test calls remain allowed (they
  cannot race), so the gate does not force the bulk cleanup.

## Implementation

- `pkg/repolint/gin_setmode.go` — `GinSetModeCheck` walks `*_test.go`
  files under the repo root, parses each with `go/parser`, and flags
  `gin.SetMode` calls reachable from a `t.Parallel()` test body.
  Detection is per-function **reachability**: it computes the transitive
  closure of functions starting from those that call `t.Parallel`, and
  flags `gin.SetMode` calls (receiver `gin` only) inside that closure.
  A file where an unrelated serial test calls `gin.SetMode` while another
  test uses `t.Parallel` passes — serial bodies never overlap parallel
  ones, so the write cannot race. Fast-path skips files with neither
  token.
- `cmd/repolint/main.go` — new `runGinSetMode` wired into the standard
  check sequence; reports as `ok gin.SetMode only from init/TestMain (no
  parallel writes)`.
- `pkg/repolint/gin_setmode_test.go` — 8 unit tests: racy test body,
  racy helper (the actual incident shape — call in `newCallbackRouter`),
  transitive helper, safe `init()`, safe `TestMain`, serial-only
  (allowed), mixed serial+parallel file (allowed — reachability, not
  co-occurrence), and non-gin receiver (allowed).

## Validation

- `make repolint` passes against the current tree (all checks, including
  the new gate).
- `go test -race ./pkg/repolint/` passes (8 gin.SetMode gate tests).
- `go test -race -short ./api/internal/handlers/` remains green (race
  already fixed in worklog 0663).

## Review iteration (PR #630)

Reviewer requested changes: the initial implementation matched on
file-level co-occurrence of the `gin.SetMode`/`t.Parallel` strings and on
any `SetMode` selector, which over-flagged mixed files and non-gin
receivers. Tightened to:

- Per-function transitive reachability from `t.Parallel` call sites
  (helpers called by parallel tests are flagged; unrelated serial tests
  in the same file are not).
- Receiver check: only `gin.SetMode` (ident receiver `gin`) matches.
- Added `TestGinSetModeCheck_MixedSerialAndParallel`,
  `TestGinSetModeCheck_NonGinReceiver`, and
  `TestGinSetModeCheck_TransitiveHelper` to pin the semantics.
