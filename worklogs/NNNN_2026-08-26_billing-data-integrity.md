# Worklog: billing data-integrity fixes (#766, #758, #759)

**Date:** 2026-08-26
**Session:** Fix the three P0 billing bugs: metering Record() silent drops (#766), Stripe export cursor advancing on failure (#758), and restart double-billing of cumulative input tokens (#759). All TDD — red tests first, verified against HEAD `128a429b` before fixing.
**Status:** Complete

---

## Objective

Eliminate the three production billing-correctness failures: lost events under burst, permanently un-reported usage windows on Stripe hiccups, and overcharging users on every API deploy.

---

## Work Completed

### B1 — #766: Record() lossless back-pressure (`metering.go`)

Verified at HEAD: `Record`'s `select`/`default` silently dropped events on a full 4096-slot channel; the TOCTOU `recover()` path dropped events racing `Stop`'s `close(s.ch)`; post-stop events were dropped unconditionally.

Fix:
- New `recordDirect(event)`: synchronous `insertEventSQL` write with its own bounded timeout, derived from a dedicated `billCtx` (never cancelled — a billing write must not die with a caller's cancellable request ctx, and must keep working during shutdown unlike `stopCtx`).
- Buffer-full → `recordDirect` (lossless back-pressure).
- Stop-race panic → `recordDirect` inside the existing recover (event salvaged, not dropped).
- Post-stop → `recordDirect` instead of drop.
- Drain-on-Stop already existed (`run`'s `!ok` branch flushes); unchanged.

Tests (red-first): buffer-full writes directly (0 drops), direct-write-failure counts a drop, after-stop writes directly, deterministic send-on-closed-channel salvage. Rewrote `TestRecord_NonBlocking` (pinned the old drop behavior) to pin the fallback.

### B2 — #758: cursor held on Stripe failure + window-bound idempotency (`metering.go`)

Verified at HEAD: `ExportUsage` advanced the cursor to `maxID` regardless of `exportToStripe` outcome; `ReportUsage`'s partial-success return (`ids[:i]`) was discarded; idempotency key bound only `maxID`.

Fix:
- Cursor advances ONLY on full success; failure returns an error carrying cursor context (`stripe export failed, cursor held at %d`), increments new `llmsafespaces_metering_export_failures_total`.
- `reportWithRetry`: same-cycle bounded retry (`exportMaxAttempts = 3`, 200ms backoff) consuming `ReportUsage`'s returned ids — only groups after the first failure re-attempt, with unchanged keys.
- Idempotency key now `meter-{customer}-{type}-{lastID}-{maxID}` — BOTH window bounds; a held-cursor retry of the same window is airtight at Stripe's idempotency layer even across process restarts.
- `exportLoop` no longer resets the lag gauge to 0 on failure (was masking held exports).
- Rewrote `TestExportUsage_ReporterFailure_AdvancesCursorAndContinues` — it pinned the exact bug #758 reports — into `..._HoldsCursorAndSurfacesError`.

Tests (red-first): failure holds cursor (ExpectationsWereMet proves no INSERT), success advances, partial failure retries only failed groups (A reported exactly once, B retried until success), retry-exhausted holds cursor with A never re-attempted, key binds both bounds, noop paths unchanged.

### B3 — #759: persistent token dedup (`sse/tracker.go`, new `token_seen_redis.go`)

Verified at HEAD: `sessionTokenSeen`/`sessionCostSeen` in-memory only; restart reset them; first post-restart `session.updated` re-billed full cumulative input. Same failure via `StopWatching`'s in-memory cleanup on suspend/resume.

Fix:
- New `TokenSeenStore` interface + `RedisTokenSeenStore` (`metering:tseen:{ws}:{ses}`, JSON `{output, cost}`, 30d TTL — must exceed any billing-emitting session lifetime incl. suspend/resume gaps).
- `handleSessionUpdated`: `priorUsage()` consults the store ONLY on in-memory miss (hot path unchanged), caches the loaded value; re-read under the write lock corrects concurrent-advance races. Write-through `persistSessionUsage` on every accepted event (best-effort).
- Store errors degrade to today's in-memory behavior — warn-once + metric `llmsafespaces_sse_token_seen_store_errors_total`, billing never stalls.
- Wiring: `ProxyHandler.SetTokenSeenStore` (panic-after-Start, mirrors `SetStateStore`) → `newSSETracker`; `app.go` constructs the Redis store alongside `redisStateStore` when the cache service is available; nil store = legacy behavior (no-Redis deployments unchanged).

Tests (red-first): two-tracker restart scenario (input billed exactly once, deltas correct), first-event bills input, GET failure falls back in-memory, SET failure tolerated, StopWatching/suspend-resume does not re-bill, no-store legacy behavior, 32-way concurrent same-session (input billed exactly once under `-race`), Redis round-trip/TTL/corrupt-entry/outage via miniredis.

---

## Key Decisions

- **Dedicated `billCtx` instead of request/stop ctx**: billing writes must outlive cancellable request contexts AND `Stop`'s cancellation of `stopCtx`; `contextcheck` satisfied without interface churn (repo precedent: `flushBatch`'s `stopCtx` pattern; `nolint:contextcheck` at the two ctx-less `Record` chains, mirroring `workspace_service.go:205`).
- **Cursor-hold + same-window retry over a reconciliation job**: the issue offers (a) OR (b); (a) is the minimal correct fix — both bounds in the idempotency key make held-cursor retries exactly-once at Stripe. Residual edge (window GROWS between cycles after partial success) is bounded by the 5-min cycle and documented; per-group high-water marks would need schema work — follow-up material, not P0.
- **`inputTokens = 0 when prevOutput > 0` semantics preserved** — only the source of `prevOutput` changed (store on miss).
- **30d TTL** over shorter: expired entries re-bill input on the next cumulative event; month-long workspaces are real.

## Assumptions (stated + validated)

1. Events racing `close(s.ch)` panic rather than buffer — validated: sends after close panic; the drain-on-close FIFO covers pre-close sends; recover salvages post-close attempts. Lossless both sides.
2. Store writes are idempotent (cumulative overwrites) — validated: values only increase; stale overwrite impossible because write-through happens after the map update under lock re-read.
3. Stripe idempotency keys live ≥24h (Stripe default) > 5-min export cycle — standard Stripe behavior; same-window retries always land inside the key window.

## Blockers

None.

## Tests Run

- `go test -timeout 600s -race ./api/internal/services/metering/ ./api/internal/services/sse/` — ok
- `go test -timeout 600s -race ./api/internal/handlers/ ./api/internal/app/` — ok (wiring unaffected paths)
- `golangci-lint run` (v2.13.1, matching CI) on all four touched packages — 0 issues
- `go build ./...`, `gofmt -l` — clean

## Next Steps

- PR this branch; closes #766, #758, #759.
- Follow-up issue to file: per-group export high-water marks (schema) to close the window-growth-after-partial-failure edge; plus the Phase-5 reconciliation job the old comment promised.
- Monitor PR #1052 (Track A) CI: lint fix pushed (`b7b9ea85`), AI reviewer already APPROVED.

## Files Modified

- `api/internal/services/metering/metering.go` — B1 + B2
- `api/internal/services/metering/metering_test.go` — B1 tests + fixture
- `api/internal/services/metering/export_test.go` — B2 tests (+rewritten failure-contract test)
- `api/internal/services/sse/tracker.go` — B3 store integration
- `api/internal/services/sse/tracker_token_seen_test.go` — new, B3 tests
- `api/internal/services/sse/token_seen_redis.go` — new, Redis impl
- `api/internal/services/sse/token_seen_redis_test.go` — new, miniredis tests
- `api/internal/handlers/proxy.go`, `proxy_lifecycle.go` — SetTokenSeenStore seam + tracker wiring
- `api/internal/app/app.go` — Redis store construction
