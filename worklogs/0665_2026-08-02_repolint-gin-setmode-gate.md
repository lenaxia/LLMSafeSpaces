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
  files under the repo root, parses each with `go/parser`, and flags any
  `gin.SetMode` call inside a function that is neither `init()` nor
  `TestMain` **and** whose file contains `t.Parallel`. Fast-path skips
  files with neither token.
- `cmd/repolint/main.go` — new `runGinSetMode` wired into the standard
  check sequence; reports as `ok gin.SetMode only from init/TestMain (no
  parallel writes)`.
- `pkg/repolint/gin_setmode_test.go` — 5 unit tests: racy test body,
  racy helper (the actual incident shape — call in `newCallbackRouter`),
  safe `init()`, safe `TestMain`, and serial-only (allowed).

## Validation

- `make repolint` passes against the current tree (all checks, including
  the new gate).
- `go test -race ./pkg/repolint/` passes.
- `go test -race -short ./api/internal/handlers/` remains green (race
  already fixed in worklog 0663).
