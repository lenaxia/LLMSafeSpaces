# Worklog — empty board_events maxEventAt NULL-scan aborts the sweep (#2063)

**Date:** 2026-11-16
**Branch:** `fix/max-event-at-empty-table`
**Issue:** [#2063](https://github.com/lenaxia/LLMSafeSpaces/issues/2063) (found during the #2042 full-suite runs; parent remediation: #2040)

## What

`Reconciler.maxEventAt` scanned `SELECT max(created_at) FROM board_events` into a non-pointer `time.Time`. Over an EMPTY table `max()` returns NULL and pgx fails the scan (`cannot scan NULL into *time.Time`), aborting the WHOLE sweep at its watermark-capture point — before RC1 ran: no reclaims, no pending coupling, no drift, no RC4 net, no watermark advance. The backstop the phase-2 event path leans on (design §4.4) was dead on any board whose `board_events` had zero rows — exactly a fresh install's first sweep. Fixed by coalescing to the origin watermark: `SELECT COALESCE(max(created_at), to_timestamp(0))` — the same seed `readWatermark` mints for a fresh scope (`reconciler.go` INSERT `to_timestamp(0)`), so an empty event history now means "watermark at zero" on BOTH watermark paths, by construction.

## Why this shape (decisions)

- **SQL-side COALESCE over Go-side `*time.Time` scan**: matches the repo's established empty-max pattern (`api/internal/services/metering/metering.go:581` `COALESCE(MAX(id), 0)`); keeps the zero-row case in the query plan instead of a second code path; and states the semantics at the capture site — "empty history IS the origin watermark", which is precisely `readWatermark`'s seed contract.
- **`to_timestamp(0)`, not a Go `time.Time{}` zero**: the seed and the coalesce must be the SAME value for the two paths to agree by construction. `advanceWatermark` already treats `next.IsZero()`/`next.Before(cutoff)` as "hold at cutoff", and `to_timestamp(0)` == the seeded cutoff, so the first populated event later advances the cursor normally — no clamp logic needed.
- **Pin assertions compare SQL-side** (`SELECT last_event_at = to_timestamp(0)`): pgx decodes `timestamptz` into `time.Time` with varying `*Location` representations across pool connections (observed: same stored instant scanning as `time.UTC` on one read and `time.Local` on the next), and testify `Equal` (reflect.DeepEqual) compares location pointers, not instants — a representation trap I hit and removed rather than papered over. The instant is the contract; Postgres compares it.

## R3 — sibling sweep (bare max/min/sum aggregates scanned into non-pointer types)

Adjudication of every `max(`/`min(`/`sum(` hit in `pkg/board` and `api/internal` (non-test):

| Site | Aggregate | Empty-safe? |
|---|---|---|
| `pkg/board/reconciler.go:236` (`maxEventAt`) | `max(created_at)` → `Scan(&time.Time)` | **NO — the bug (#2063)**; fixed here |
| `pkg/board/engine.go:42` (candidateSQL) | `min(e.vft)` inside ORDER BY subquery | Yes — an ordering key only; never scanned into Go, NULL just sorts (Postgres ASC NULLS LAST); no abort possible |
| `api/internal/services/metering/metering.go:286,335` | `SUM(quantity)` with GROUP BY | Yes — grouped aggregates emit zero rows over empty input; the row-scan loop never runs |
| `api/internal/services/metering/metering.go:508,543,550,555` | `COALESCE(SUM(quantity), 0)` | Yes — coalesced |
| `api/internal/services/metering/metering.go:581` | `COALESCE(MAX(id), 0)` | Yes — coalesced (the pattern this fix copies) |
| `api/internal/services/metering/metering.go:638` | `SUM(quantity)`, `MAX(ue.event_time)` with GROUP BY | Yes — grouped; zero rows over empty input |

No other NULL-unsafe scan exists in the swept scope.

## Tests (TDD)

- **Born-red** (`TestPin_2063_EmptyEventsSweepCompletes`, `pkg/board/watermark_empty_2063_integration_test.go`): truncated `board_events` (the harness `SetupTest` state, zero seeds) + `Sweep()` → failed pre-fix with the exact scan error (`can't scan into dest[0] (col: max): cannot scan NULL into *time.Time`); post-fix: completes, watermark pinned at the origin seed via SQL-side equality, and a SECOND still-empty sweep repeats cleanly (the sweep writes no events on an empty board — asserted — so idempotence re-exercises the empty path).
- **R4 populated pin** (`TestPin_2063_PopulatedEventsWatermarkUnchanged`): seeded history → sweep → watermark equals the pre-captured `max(created_at)` exactly (r1-C3's pre-capture contract; the coalesce arm never fires when rows exist; the fixture's no-sweep-writes assumption is itself asserted via before/after event counts).
- Full `pkg/board` suite (unit + integration, `-race -count=1 -timeout 300s`, mirroring `secrets-integration.yml`): green. `make fmt-check`, `go vet`, `golangci-lint run`: clean.

## Observability note

The bug was observed live exactly once (#2042 run 3: `col: max` scan errors surfacing as sweep failures on unrelated pins) and was state-dependent — the suite's fixtures almost always seed events before sweeping, so the empty path only fires on a pristine board. The born-red pin makes that state a permanent fixture rather than a 3am coincidence.
