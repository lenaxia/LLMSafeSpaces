# Worklog: stamp hardening — retry, back-fill, stamp_lag (RC-B S1)

Task `tsk-fuzzy-vehicle-mines` (epic: Liveness and observation: every
claim observable, every lapse visible — the 2026-10-30 incident
remediation, S1). Branch `agentd-version-visibility-skew` (the task
board's working branch for this epic wave).

## The problem (RC-B, restated from the incident)

`spawnSync` wrote `tasks.assignee_session` in ONE shot, swallowed the
error at Warn, and had zero observability. The stamp gates every
consumer: `rc1Reclaims` builds observation refs only for rows that
carry an assignee; the nudger delivers through it; the badge and
Workers folder classify on it. Three incident workers ran 49m/73m/75m
first turns (pre-#1986, stamps hostage to a 15-min-blocking spawn) and
the whole plane went blind — a lost stamp makes a live worker
observationally identical to an empty row.

## What landed

1. **Retry with backoff** — `pkg/board/stamp.go`
   `writeAssigneeStamp`: 4 attempts, 100→200→400ms doubling backoff
   (ctx-aware sleeps), each failure a loud ERROR + counter bump. The
   guarded no-rows verdict (claim token moved mid-spawn) does NOT
   retry — stamping a moved claim is wrong, and the caller's abort
   path (the merged design's spawn-stamp guard) stays exact for that
   case.

2. **Exhaustion ≠ abort on a live claim** — `spawnSync` now splits the
   `landed=false` outcomes: `claimStillLive` re-checks the token. Live
   claim + lost stamp → the worker RUNS un-stamped (Warn names the
   back-fill recovery); moved claim → the dead charter aborts as
   before. Aborting a live worker over a lost stamp would have
   replayed the incident's kill with our own hands.

3. **Back-fill (eventually-true)** — `Heartbeat` calls
   `backfillAssigneeStamp` after its claim guard: missing stamp +
   actor session identity + live token (re-checked at write time) →
   identity-only re-stamp (the lease is untouched — the heartbeat's
   own arm owns renewal). Fill-only, never overwrite. Best-effort: a
   back-fill failure never fails the heartbeat.
   Production plumbing: `PodAutomationHandler.boardActor` carries the
   calling session's identity (the manager surface's existing
   `boardSessionID` probe — query first, exact-key body second);
   heartbeat/move_stage/record_verdict pass it; the three worker MCP
   schemas document the optional `sessionId` self-identification.
   Throttle/criteria-met deliberately pass "" (one releases the claim,
   the other is not a claim-evidence channel for the assignee).

4. **stamp_lag + failure metric** —
   `llmsafespaces_board_stamp_lag_seconds{origin=spawn|backfill}`
   (histogram, buckets 10ms→1h, lag measured server-side as
   `now() - stage_entered_at` — one clock domain, and
   `stage_entered_at` is written by EVERY claim-path mint: grant,
   manual start, re-arm, persona claim, bounce, drift re-entry) and
   `llmsafespaces_board_stamp_write_failures_total{origin}`.
   Registered once in pkg/board (promauto; the family lives in exactly
   one package, so the obs two-registration hazard cannot arise).

## Tests (stamp_integration_test.go, real PG)

- `TestPin_StampRetryLandsUnderTransientFailure` — fault seam fails
  attempts 1–2; retry lands; counter +2; stamp_lag(origin=spawn) +1.
- `TestPin_StampBackfillOnClaimEvidence` — erased stamp; wrong-token
  heartbeat refuses and does not stamp; token+identity heartbeat
  re-stamps (`sess-recovered`); origin=backfill lag emitted; a later
  different-identity heartbeat does NOT churn the landed identity.
- `TestPin_StampExhaustionRunsUnstampedUntilBackfill` — failpoint
  armed for the whole episode: 4 loud failures, no stamp, aborter
  NEVER fires (claim live), then the healed next heartbeat re-stamps.

Suite status: pkg/board integration (39.8s), api/internal/handlers
(85.8s), cmd/workspace-agentd (371s) — all green. `gofmt` clean on
every touched file.

## Environment notes

- PG per the recipe: `/tmp/opencode/pgdist` cluster on :5433,
  `TEST_DATABASE_URL=postgres://postgres:testpass@127.0.0.1:5433/llmsafespaces_test?sslmode=disable`,
  migrations via `go run /tmp/opencode/pgmigrate.go $URL`.
- SHARED WORKTREE HAZARD: a parallel session is mid-flight on the
  Sprint-Board Go-surface rename (`pkg/mcp/client.go`,
  `pkg/mcp/board_tools.go`, modified 02:21–02:23 during this work —
  not mine, left untouched). `make fmt-check` flags their in-flight
  file; every file THIS task touched is gofmt-clean (verified
  scoped). Commit stays scoped to my six files.

## Files

- `pkg/board/stamp.go` (new) — retry loop, back-fill, metrics, logs.
- `pkg/board/engine.go` — spawnSync rides writeAssigneeStamp +
  claimStillLive split.
- `pkg/board/stage.go` — Heartbeat's back-fill hook.
- `api/internal/handlers/board_automation.go` — worker actor carries
  session identity on the three claim-evidence routes.
- `cmd/workspace-agentd/mcp_server.go` — optional `sessionId` on the
  three worker tool schemas.
- `pkg/board/stamp_integration_test.go` (new) — the three pins.
