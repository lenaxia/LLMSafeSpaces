# Worklog: #1847 PR1 — heartbeat server-clock clamp/reject (C1 incident class)

**Date:** 2026-10-22
**Session:** lane-1847 (platform-derived lease liveness), PR1 of the slicing.
**Status:** Complete — MERGE-READY

---

## Objective

Issue #1847 PR1: the one-line-class hotfix named in the issue. A
hallucinated FUTURE `observedProgressAt` (09:05Z at server 01:36Z — C1
gen-1 in the evidence table) was accepted as progress evidence, stored
as the lease's progress watermark, and made the claim unrenewable by
construction: every later honest heartbeat failed
`observed.After(stored)` — the live worker was locked out of its own
lease. Red-first: a future-stamped heartbeat must NOT extend the lease
watermark.

## Work Completed

### Red pins (committed first, dc6ac81d)

`pkg/board/heartbeat_1847_test.go` (integration tag, real-PG harness),
three pins against main's `Heartbeat` (stage.go:352):

- `TestKill_HeartbeatFutureStampRejected` — +8h stamp must error
  `GuardInput`, extend nothing, write no audit event. RED: "An error is
  expected but got nil".
- `TestKill_HeartbeatMildSkewClampedToServerClock` — +90s (within the
  2m tolerance) still extends, but the STORED watermark must read at-or-
  behind the server clock. RED: stored = client future verbatim
  (22:27:47 vs server 22:26:17).
- `TestKill_HeartbeatPoisonedWatermarkSelfHeals` — C1's persisted state
  (watermark poisoned +8h, lease expired): an honest present-t
  observation must extend again and REPLACE the poison. RED: no extend
  (the lockout reproduced).

Real failure output quoted in the red commit message.

### Fix

`pkg/board/stage.go` `Heartbeat`:

- `heartbeatFutureTolerance = 2 * time.Minute` (package const).
- Beyond-tolerance future stamp → `GuardError{GuardInput}` naming both
  clocks; no watermark write, no lease extension, no audit event.
- Within-tolerance future → clamp to `serverNow` (a FUTURE stamp is
  never stored — the poison direction only; past-skewed stamps remain
  client-trusted until the rework retires worker-declared stamps
  entirely).
- Poison self-heal: evidence gate reads a stored watermark reading
  future as STALE (`t.LastProgressAt.After(serverNow)` forces
  `extends`), so the honest observation overwrites the poison — ONE
  evidence-free extend, bounded (the heal overwrites the watermark; a
  re-sent identical stamp does not extend again).

### Review remediation (PR #1862, review d86cb7d8)

- Finding 1 (wording overclaim): "all stored watermark stamps are
  server-clock" was false — only the FUTURE direction is clamped.
  Corrected in CHANGELOG, the stage.go comments, and this worklog.
- Finding 2 (missing edge pin): added
  `TestKill_HeartbeatHealIsOneShot` — a poison-FREE row whose watermark
  reads future (DB-ahead/app-behind clock skew) gets exactly ONE
  evidence-free extend on the first honest heartbeat; a second
  heartbeat with the SAME stamp does NOT extend.

Existing heartbeat callers verified: the evidence-gate pin
(`now+1s`, within tolerance) stays green unmodified;
reconciler/remediation tests use `+1s` likewise.

### Verification

- Full `pkg/board` integration suite (real PG 16 via TEST_DATABASE_URL):
  `ok 18.3s` on a fresh database, fix applied.
- `go build ./...`, `go test ./pkg/board/ ./api/internal/handlers/`:
  green.
- RED→GREEN arc captured (red output in dc6ac81d's message).
- Pre-existing main flake found, NOT mine: `TestMintUniqueOwnerWideLadder`
  (ids_phrase_arbitration_integration_test.go:47) seeds a FIXED
  workspace uuid and its cleanup never truncates `workspaces` — it
  passes on a fresh DB and fails on any re-run against a used one.
  Reproduced with this branch's changes stashed. Reported to the
  orchestrator for routing (left untouched here — not this lane's
  file).

## Notes for the rework PRs (PR2+)

- The hallucination CLASS dies only when worker-declared timestamps
  stop being the primary liveness input (spec item 1) — PR1 is the
  protective clamp while that lands.
- Session observability inventory for PR2: `session_index`
  (last_message_at/message_count) is message-level only — no busy/open-
  turn truth in PG; busy truth lives in the usagestream consumer
  (in-memory, per-replica) and in agentd's `sessionStatusTracker`
  (in-pod). Platform-derived liveness therefore needs an agentd→API
  push (the board pressure-reporter precedent) into a durable table the
  RC1 sweep can JOIN, with a staleness bound so a dead pusher's latched
  busy=true cannot hold a lease forever (the wedged-turn acceptance).
- The existing tests' `now+1s` heartbeat idiom implies any future-
  tolerance below 1s would break pins — 2m chosen, documented.

## CHANGELOG

[Unreleased] → "Fixed — board heartbeat: server clock bounds
client-observed stamps (issue #1847 PR1…)" (title reworded in the
review remediation: the guarantee is the future direction, not all
stamps).
