# Worklog: US-69.7 — delivery ledger: PVC WAL, M2 state machine, single-flight, wake-only stalls

**Date:** 2026-08-31
**Session:** Epic 69 (#1134) US-69.7 (#1141, design 0055 M2, phase S2 — first post-gate story; 0053-S3 landed in #1156): the PVC-backed delivery ledger with the full M2 state machine, (entryID, attempt) dedupe, per-session single-flight admission driver, wake-only stall recovery, promotion correlation from the authority's own events, ledger-derived queue depth, and the Deliver/GetDeliveryStatus ops live on the ABI surface.
**Status:** Complete (in-repo core). Cluster-bound ACs (real suspend windows, real opencode kill -9, fsync on Longhorn/EFS per the US-69.6 rule) ride the staged pool — the crash matrix is covered in-repo by reopen-replay over the real WAL file.

---

## Objective

The exactly-once core: 202-from-agentd means "durably ledgered"; agentd dedupes per (entryID, attempt), drives admission with retry under per-session single-flight, and tracks promotion by messageID so the #1119 stranding class can never be silent (and can never be re-admitted).

---

## Work Completed

### The ledger (`sessionstate/ledger.go`)
- **WAL format v1** on `platform/ledger.wal`: JSON-lines; the first line is the format header (uncompactable, never a record); transitions are NEW lines with fresh seqs so replay's last-writer-wins merge resolves to the latest state; torn tails (crash mid-append) truncate at replay — the previous fsynced records are truth.
- **I9**: every append fsyncs before the caller proceeds — the `ledger()` ack (the 202 equivalent) implies persistence; `TestLedger_Durability202SurvivesKill` reopens the file without graceful close and finds the row.
- **I5 dedupe**: `ledger()` is idempotent per (entryID, attempt) — a duplicate returns the existing row, no new record; terminal `failed` re-arms at attempt+1 (new row).
- **State machine**: ledgered → admitted (messageID recorded) → promoted (by messageID correlation) → turn-ended (session idle boundary); `stalled` = admitted past deadline (10m default) + wake fired exactly once; `failed` per attempt with reason.
- **I6**: there is no re-admission path — `replayUnresolved` and `driveAdmission` only touch ledgered rows; admitted/stalled are structurally skipped (`TestDeliver_AdmittedNeverReadmitted`).
- **I7**: no interrupt surface exists in the ledger API (pinned by `TestLedger_InterruptPurity`).
- **Compaction**: rewrites the WAL dropping turn-ended/failed rows past retention; header preserved; live rows survive; retention independent of any cursor (none exist).
- **Queue depth**: `queueDepth(session) = ledgered ∪ admitted ∪ stalled` — ledger-derived, surfaced in SessionSnapshot.QueueDepth.

### The delivery driver
- **Per-session single-flight** (I5): a session mutex serializes admissions — `TestDeliver_SingleFlightPerSession` proves max-in-flight 1 with 8 concurrent deliveries, all admitted exactly once.
- **Async admission** (M3.1): `deliver()` returns at the durable ack; admission runs on agentd's queue with bounded retries (5, exponential) then `failed` with reason; re-reads state under the lock each attempt (crash-window resolution).
- **Promotion from the authority's own events**: `observeEvent` correlates message-start/end event IDs against admitted messageIDs (I12 stitch); `observeTurnEnded` on session idle terminates promoted rows — wired into `applyContractLocked` (no new ingestion path).
- **Replay after crash/suspend**: `ReplayUnresolvedDeliveries` drives ledgered rows on boot/resume (wiring pending a story that owns boot ordering; exposed on the Authority).

### Ops + wiring
- `Deliver` live: validation (entry/attempt/parts), D3 file-part typed `NotSupported` (wire test), text-join per D3, durable ack with state; `GetDeliveryStatus` live: typed NotFound for unknown rows, failure reasons carried.
- `opencodeAdmitter` (wiring layer): POST the V2 prompt endpoint (delivery "queue", model override) with the §D1 credential; returns the message ID for correlation.

### Tests (TDD — ledger_test.go + delivery_op_test.go written first)
Crash matrix in-repo: `TestLedger_CrashMatrixStateTransitions` (reopen resolves every intermediate state), `TestDeliver_CrashAgentdAfterAckPreAdmission` (kill semantics via reopen; exactly one admission), `TestLedger_Durability202SurvivesKill` (I9), `TestLedger_StalledDetectionAndWakeOnly` (#1119 class: stalled + exactly one wake; no re-admission), `TestLedger_QueueDepthLedgerDerived`, `TestLedger_CompactionPreservesTerminalOutcomesAndSeqMeta`, `TestLedger_InterruptPurity` (I7), `TestDeliver_SingleFlightPerSession`, `TestDeliver_ExactlyOncePerAttempt` (dedupe + re-arm at +1), `TestDeliver_AdmittedNeverReadmitted` (I6), `TestDeliver_PromotionFromProjectionEvents` (messageID promote + turn-end terminate), `TestDeliverOp_WireDedupe` / `TestDeliverOp_FilePartsNotSupported` / `TestDeliverOp_StatusLifecycle` (over the generated connect op). All green under `-race`.

---

## Key Decisions

1. **Transitions are new WAL lines with fresh seqs** — the replay merge is last-writer-wins per (entryID, attempt); this makes every state change crash-atomic without a rewrite-in-place format.
2. **Admission is queue-scoped, not request-scoped** (G118/contextcheck annotated): the 202 must never couple admission lifetime to the accepting request — M2's "asynchronous against agentd's own queue".
3. **Duplicate acks carry the row's CURRENT state** (not the state at first accept) — idempotence means "no second row", not "state frozen"; the wire test pins this.
4. **Stall wake fires exactly once per row** (in-memory wakeFired flag; the stalled state itself is WAL-persisted) — I6's wake-only recovery with no alert storms.
5. **Ledger disabled (Admitter nil) ⇒ ops return typed NotSupported** — the authority owns nothing dialect-specific; S1-style construction without the seam stays valid.

## Assumptions (validated)

| # | Assumption | Validation |
|---|---|---|
| A1 | WAL reopen-replay models kill/suspend faithfully | durability + crash-matrix tests over the real file |
| A2 | fsync-per-append acceptable at delivery rates (US-69.6 rule ≤10ms) | local ~7–10ms; Longhorn/EFS = pool artifact |
| A3 | messageID correlation via V2 prompt response ID works on 1.18.15 | the field exists (`V2PromptResponse.ID`); the admission-ID spike's exact-correlation question rides the pool |

## Blockers

None in-repo. `ReplayUnresolvedDeliveries` needs its boot-order call site (with the S2 flag wiring of US-69.8) — the outbox switch owns when replay actually fires.

## Tests Run

- `go test -race ./cmd/workspace-agentd/sessionstate/` — PASS (58s)
- `go test ./cmd/workspace-agentd/` — PASS (full package, e2e suite)
- `golangci-lint --new-from-merge-base ./cmd/...` — 0 issues

## Next Steps

1. US-69.8 (#1142): the outbox terminus switch — `outboxDeliver` POSTs `.../deliveries` with (entryID, attempt); the state-mapping table (M2) enforced in code; `v2Shadow`/`v2Pending` retire as ledger-derived queue depth goes live; boot replay wiring lands here.
2. US-69.9 (#1143): typed actions op against the surface.
3. The Epic 65 flip (#1161) before the schema freeze at US-69.8's entrance.

## Files Modified

- `cmd/workspace-agentd/sessionstate/ledger.go` + `ledger_test.go` (new)
- `cmd/workspace-agentd/sessionstate/delivery_op_test.go` (new)
- `cmd/workspace-agentd/sessionstate/{authority,service,projection}.go` (ledger ownership, ops live, queue depth, promotion hooks)
- `cmd/workspace-agentd/sessionstate_wiring.go` (opencodeAdmitter seam)
