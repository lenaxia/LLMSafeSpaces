# Worklog: Epic 71 / 1b — ledger store-evidence convergence (#1311)

**Date:** 2026-09-10
**Session:** Stream 1b of epic #1314 — make the delivery ledger converge against harness store evidence: reseed sweeps stranded rows, deadlines on every state, BUSY re-derives from truth, boot auto-heal.
**Status:** Complete

---

## Objective

Implement #1311 (epic-71 / 1b): the delivery ledger joins the reseed; every ledger state gets a bound; the resolver consults the store, never assumes; BUSY must not survive a reconcile pass that observes an idle harness; decide the `queueDepth` disposition; land abitest knobs for fault legs 4–5. Claimed per the epic's reserved-comment protocol (comment [#5626038620](https://github.com/lenaxia/LLMSafeSpaces/issues/1314#issuecomment-5626038620)).

The production incident this kills: ws `47962542` reported `SESSION_STATUS_BUSY, queueDepth 9` for hours after the turns demonstrably completed — nine admitted-unpromoted rows with no convergence path short of manual state surgery.

---

## Work Completed

### Reconcile core (`sessionstate/reconcile.go` — new)
- `Authority.Reconcile(ctx) ReconcileStats`: one store-evidence convergence pass. Serialized against `Reseed` (shared `reseedMu`); per-session row advancement under the session's single-flight lock (the SAME lock admissions take — a sweep can never interleave an in-flight admission, so a FAILED-with-landed-message duplication is impossible by construction).
- Matrix (per #1311): `LEDGERED` past admission deadline → `FAILED` (re-armable at attempt+1); `ADMITTED`/`STALLED` + message present in store → `PROMOTED`; `ADMITTED`/`STALLED` + turn ended (store idle/ERROR/absent) → `TURN_ENDED`; busy + absent → stays (turn running).
- Message-absence is consulted only from a successful read: a `MessagePresence` error counts `EvidenceFailures` and falls through with nil evidence (status evidence still converges; busy-session rows untouched); a `SessionStates` error returns with all rows untouched — never an authoritative empty.
- BUSY re-derivation (`clearBusyFromEvidence`): evidence-idle clears the view's `busy`, sets status from evidence (IDLE; ERROR keeps the error visible), clears in-flight parts. Runs on the same pass — the ledger side supplies "no un-promoted admissions" per #1310's companion change.
- Shared epic constants: `LeaseConvergenceBound = 30s` (the one lease clock; 2a consumes it), `ReconcileCadence = 15s` (half the bound — one missed tick still converges inside L4/L5).

### The ledger joins the reseed (boot auto-heal)
- `Reseed` runs `sweepAgainstEvidence` with the seeds it already read, BEFORE the projection swap — the first post-reseed snapshot serves converged queueDepth/status. Deploying this auto-heals every currently-wedged session, no operator action (S8's executable form; verified by `TestReseedSweepsLedger_BootAutoHeal` — reopen over a wedged WAL, reseed, converged).
- The stall-wake reseed now carries evidence (was "wake-and-hope": the wake reseeds the projection but never resolved ledger rows).

### Deadlines on every state (`ledger.go`)
- `admissionDeadline` (default 1m — comfortably above the ~6.2s admission retry envelope + boot replay window) bounds `LEDGERED`; `sweepSession` fails past-deadline rows in one ledger critical section, per-outcome counts returned.
- `unresolvedBySession`/`rowsForSweep` snapshots; `advanceSweepLocked` stamps + fsyncs each transition (WAL last-writer-wins replay resolves to the sweep's state).
- `PROMOTED`/`TURN_ENDED`/`FAILED` remain resolved states; the promotion deadline (10m → STALLED) is unchanged — `STALLED` rows now converge via store evidence instead of living forever.

### Fixed two unwired production paths (Rule 5 — pre-existing, in-scope)
- `observeTurnEnded` had NO production caller: `PROMOTED` rows never reached `TURN_ENDED` on the turn boundary. Wired: `observeEvent` folds `SESSION_STATUS_IDLE` → `markTurnEnded(sessionID)`.
- `ReplayUnresolvedDeliveries` had NO production caller: accepted-but-unadmitted rows never re-drove admission after an agentd restart. Wired: `startStateAuthorityReseed` invokes it after the first successful reseed (both single-container and sidecar mode).
- **Sidecar mode ran no watchdog at all** (no stall detection, no metric refresh, no convergence cadence). Wired `runSessionStateWatchdog` there, same cadence.

### Store-evidence seam
- `StoreReader` grows `MessagePresence(ctx, sessionID, messageIDs) (map[string]bool, error)`; an error means NO evidence. All in-repo fakes updated.
- Wiring (`sessionstate_wiring.go`): pages the V1 message list (`limit=50`, `X-Next-Cursor`, budget 40 pages). Absence is proven only by cursor exhaustion (the `verifydelivery` rule); budget overrun returns an error — never a false. Queried IDs pre-seed as false so the verdict map is self-describing.

### Watchdog + metrics
- `runSessionStateWatchdog` runs `Reconcile` each tick before `CheckStalls`; production cadence 1m → `sessionstate.ReconcileCadence` (15s) in both modes.
- New counters: `llmsafespaces_ledger_reconciled_total{outcome=promoted|turn_ended|failed|busy_cleared}`, `llmsafespaces_reconcile_evidence_failures_total`; `Metrics()` carries cumulative `Reconcile*` fields (single recording site — reseed-embedded sweeps count too).

### abitest knob (fault legs 3/5, epic merge-gate item)
- `Server.SuppressEventTypes(...abiv1.EventType)` + `SuppressedEventTypes()` — composable, inspectable, default-off; the Events stream omits matching event frames (leg 5 shape: harness OOM mid-turn emits nothing further). Leg 4 (agentd crash) needs no server knob — it is in-process injection, exercised by the crash-matrix test.

### queueDepth disposition (decided, documented)
- `SessionSnapshot.queue_depth` documented in `abi.proto` as **admission-internal** (ledgered ∪ admitted ∪ stalled, converged by the sweep); NOT the user-facing queue (the outbox owns that per #1312). Additive-first discipline: no wire change; the schema pass (#1304, Wave 4) picks it up from the proto comment. Regenerated stubs (`make abi-generate`); `abi-lint` + armed `abi-breaking` gates pass.

### Pre-existing test-isolation defect fixed (Rule 5)
- `xdg_config_layer_test.go` never cleared `OPENCODE_CONFIG`, which outranks `LLMSAFESPACES_AGENT_CONFIG_PATH` — on llmsafespaces-hosted dev pods the tests read the LIVE `/agentd-config/agent-config.json` and dumped its credential-bearing contents into test output. `setXDGHome` now clears it (tests hermetic; secrets-in-output leak closed).

---

## Key Decisions

1. **Evidence-idle clears BUSY even with queued LEDGERED rows** — busy/idle ground truth is the harness (#1312 ownership table: busy iff a turn runs); queued ≠ running. V1 admission is synchronous, and a queued admission's `MESSAGE_START`/`PART_START` folds re-mark busy when its turn actually starts. Documented residual: the evidence-read-to-clear window is bounded by event latency (L4's 30s dwarfs it).
2. **Message-evidence failure falls through on status evidence** (instead of skipping the session) — the turn-ended arm is truthful from status alone; only the promote-refinement is lost. Counted, retried next pass.
3. **Sweep TryLocks session single-flight locks — skip, never wait** (review r1 correction of this worklog's first draft, which claimed a long admission "blocks ITS session's sweep only": wrong — a blocking take would head-of-line-block every later-sorted session AND stall concurrent Reseeds, breaking the 30s bound). A session mid-admission is skipped this pass; its rows are being driven and the next tick (15s) converges them. Serialization where it matters is preserved: a sweep can never fail a row an admission is about to land, because it never touches a locked session at all.
4. **LEDGERED admission deadline = 1m, gated on status evidence** ("with no evidence" per the issue): a BUSY store session may be running the row's admission/turn — the sweep holds; FAILED fires only on the no-evidence arms (idle/ERROR/absent session). Residual, documented: the crash window "Admit succeeded server-side, agentd died before markAdmitted" leaves a LEDGERED row with no messageID — ID-based evidence cannot exist for it, and content matching is the retired text-oracle (design 0055 rejects it; the design accepts this residual for the attempt-driven failure path — same-clock localhost single-flight bounds it to one entry). FAILED rows are excluded from `admittedAnywhere` ("never reached opencode"), so an outbox re-arm of this residual CAN duplicate a turn — identical to the pre-existing replay-window residual (#1288 class), not a new class introduced by the sweep; the sweep's status-evidence gate removes every variant of it where the store shows live work. The {LEDGERED × message-present} matrix cell is therefore unimplementable without content matching — pinned instead as {LEDGERED × busy-holds / idle-fails / absent-fails}.
   **STALLED has no clock deadline of its own — decision:** resolution is evidence-driven (present→PROMOTED, turn-ended→TURN_ENDED); while store evidence AGREES with the row (busy session, message absent) it persists — S7's letter ("no row past deadline against CONTRARY evidence") needs no clock there, and the seq-stall/starvation alerts own a wedged harness. Pinned by the `stalled busy no message stays` matrix cell.
5. **queueDepth documented, not dropped** — removing a field is non-additive schema surgery in a frozen ABI; Wave 4 owns the schema pass.
6. **One lease clock** — `LeaseConvergenceBound` introduced here; #1310's 2a consumes it for the pending-set lease (epic convergence requirement recorded in both reserved comments).

---

## Assumptions stated and validated (Rule 7)

| # | Assumption | Validation |
|---|---|---|
| A1 | "Store shows turn ended" ⟺ session idle/ERROR in `SessionStates`, or session absent | design 0055 I3/I4 (store is truth); `opencodeStoreReader.SessionStates` maps `/session` statuses; absent-session arm pinned by matrix test |
| A2 | V1 message list answers presence by ID | `verifydelivery.go:75` — same endpoint, `X-Next-Cursor` pagination; wiring test pins paging + absence-proven-only-by-exhaustion |
| A3 | Lock order sessionLock → ledger.mu → a.mu has no reverse anywhere | audited every `a.mu`/`sessionLock` site in `authority.go`/`actions.go`/`ledger.go`/`service.go`; no path acquires sessionLock under `a.mu` |
| A4 | 1m admission deadline > retry envelope | `admissionAttempts=5`, backoff 200ms×2ⁿ → 6.2s total (`ledger.go` driveAdmission) |
| A5 | `Reconcile` with nil Store must no-op, not panic | existing watchdog e2e builds ledger-without-store authorities; guard added and covered |
| A6 | Comment-only proto change passes the armed freeze gate | `make abi-lint` + `make abi-breaking` green post-regen |

---

## Review round 1 (automated reviewer, PR #1317 — REQUEST CHANGES → remediated)

Validated real findings, all fixed with regression tests:
1. **LEDGERED sweep dropped the issue's "with no evidence" qualifier** (duplication-on-rearm hazard) → status-evidence gate (busy holds; idle/absent fails) + matrix cells; residual documented above.
2. **Stale-evidence window across the session-lock wait** (a live turn landing mid-pass could be TURN_ENDED'd on unqueried evidence; a fresh busy-mark could be clobbered) → rows whose messageID was not queried are never resolved on that evidence + `sessionRecord.lastBusySeq` gates the busy-clear (a busy-mark newer than the evidence read survives). Pinned by `TestReconcile_LiveTurnDuringEvidenceWaitNotMisresolved`.
3. **Head-of-line blocking** (blocking session-lock takes + reseedMu held across the pass could stall the sweep and Reseeds for a 3-minute admission) → TryLock-and-skip (`TestReconcile_LockedSessionSkippedNotBlocked`); pass-scoped evidence deadline (10s default, `SetReconcileTimeoutForTest`) bounds a hung store (`TestReconcile_EvidenceDeadlineBounds`).
4. **Metrics divergence on ctx-cancelled passes** → outcomes recorded before the early return (`TestReconcile_ContextCancelRecordsOutcomes`).
5. Style: `unresolvedBySession` slices were computed and discarded → `unresolvedSessions` set; `_ = key` loop idiom dropped.

False alarm, documented with evidence: "hand-inserted comment in abi.pb.go" — the comment is emitted by protoc-gen-go from the proto source; CI's "Harness ABI schema (…codegen freshness)" check passed, proving regeneration reproduces the file byte-for-byte.

Disposition on the kind-level e2e rows: the epic's merge gate routes the delivery-pool kind workflow (AC-1b..1e, F6) at this PR — those rows execute there; the in-repo executable forms committed here are the wire-level reopen/boot-heal/crash-matrix/watchdog-loop rows. No cluster is available in the authoring environment; shipping unexecuted kind scripts would violate Rule 7 (unvalidated assumptions) — the pool run is the validating step.

---

## Review round 2 (PR #1317 — REQUEST CHANGES → remediated)

r1 fixes verified by the reviewer; three residuals, all fixed:
1. **`seqAtEvidence` stamped after the evidence read** (a busy-fold DURING the `SessionStates` read would postdate nothing and get cleared — the exact class the gate exists to refuse) → the stamp now happens BEFORE the store read in BOTH paths (`reconcileLocked`, `Reseed`); pinned by `TestReconcile_BusyFoldDuringStatesReadSurvives` (gateStore hook moved to `SessionStates`).
2. **Reseed-embedded sweep outcomes never reached Prometheus** (only `Metrics()` cumulative counters; the watchdog exported only its own returns) → single export path: `recordSessionStateMetrics` now bridges cumulative-counter DELTAS (customValveDelta convention: first scrape carries the cumulative — the boot-heal pre-dates the first tick) into `llmsafespaces_ledger_reconciled_total` / `..._reconcile_evidence_failures_total`; the watchdog's direct per-pass adds removed (double-count). Pinned by `TestRecordSessionStateMetrics_ExportsReseedSweepOutcomes`.
3. **`reconcileTimeout` unsynchronized read** (test-only race) → a.mu-guarded field + accessor.

Merge gate (delivery-pool kind suite): dispatched `us-70-delivery-pool.yml` against this branch post-push (builds the commit's own artifacts — the pool's contract); recorded in the PR. Reviewer-cleared items accepted as documented: LEDGERED×message-present cell re-pinned with disposition; STALLED no-clock decision pinned; leg-4 "kill semantics" comment softened to WAL-reopen semantics.

---

## Review round 3 (PR #1317 — r1/r2 fixes verified; one item remediated)

Reviewer verified all r1+r2 fixes with empirical red/green pins. Remaining item fixed: **the reseed-embedded sweep now carries the same pass deadline the cadence path has** (`Reseed` wraps its sweep call in `reconcileTimeout()`); pinned by `TestReseedSweep_EvidenceDeadlineBounds` (hung message evidence → prompt reseed; the r2 status-evidence fall-through converges the row via the turn-ended arm under the deadline — the pin doubles as the fall-through's deadline-case documentation).

**Merge gate executed and dispositioned:** pool dispatched on this branch (34547093034) AND on unmodified main (34549292454) — identical outcomes (40 passes, same 2 failures: AC-1b XDG mismatch, F1 autopush timeout — both byte-identical on main). Zero new pool regressions from this branch; the pre-existing main-red surfaced on epic #1314 (comment 5628098598) for the owning stream. Pool flake history noted (same-branch failure/success alternation on fix/1300 runs).

---

## Blockers

None. Coordination notes: `actions.go` untouched (1a's); the shared lease clock landed here for 2a to consume; delivery-pool kind rows (AC-1b..1e, F6) ride the weekly CI workflow on the PR — the in-repo executable forms (crash matrix, incident replay, watchdog loop) are committed here.

---

## Tests Run

- `go test -timeout 300s -count=1 -race ./cmd/workspace-agentd/sessionstate/` — ok (44s), includes the new reconcile matrix, deadline expiry/no-premature-sweep, re-arm eligibility, evidence-failure never-authoritative, status re-derivation, queueDepth post-sweep (incident shape), boot auto-heal reopen, crash-injection leg 4 matrix, leg 5 harness-dead-mid-turn, sweep/admission serialization, idle-event turn-end pin, lease-constant pin.
- `go test -timeout 1500s -count=1 -race ./cmd/workspace-agentd/` — ok (264s), includes the new `MessagePresence` wiring rows (paging, cursor exhaustion, budget-is-error, transport-error-is-no-evidence) and watchdog-reconciles-ledger e2e through the real connect wire + prometheus counters.
- `go test ./pkg/abi/...` — ok (abitest knob rows: suppression omits event frames only, knob inspectable, default-off).
- `make abi-lint`, `make abi-breaking` (freeze ARMED) — pass; `make abi-generate` regenerated `abi.pb.go`/`abi_pb.ts` from the comment delta.
- `gofmt -l`, `go vet` on touched packages — clean.
- Review round 1 regression rows added: matrix cells (ledgered×busy-holds/idle-fails/absent-fails, stalled×busy-persists), live-turn-during-evidence-wait, locked-session-skip, ctx-cancel metrics, evidence deadline bound — all green under `-race` with the full sessionstate suite.

---

## Next Steps

- PR review loop (automated reviewer) → merge; update the 1b reserved comment to `landed` with the PR link.
- 2a (#1310 slice B) consumes `LeaseConvergenceBound` for the pending-set lease and may merge its reconcile pass with the ledger sweep (one loop, two diffs — structure permits: both live under `reseedMu`).
- 2b (#1312) grows the assertion harness on top of the abitest knobs; the soak row asserts S7/L4/L5 against these counters.
- #1304's schema pass picks up the `queue_depth` admission-internal disposition from the `abi.proto` comment.
- Upstream observation: `GET /session` list lacks a status field on some pinned versions (`ListSessions` defaults idle) — status evidence quality is version-dependent; worth a fixture pin when 2b builds the canary.

---

## Files Modified

- `cmd/workspace-agentd/sessionstate/reconcile.go` (new)
- `cmd/workspace-agentd/sessionstate/reconcile_test.go` (new)
- `cmd/workspace-agentd/sessionstate/ledger.go`
- `cmd/workspace-agentd/sessionstate/authority.go`
- `cmd/workspace-agentd/sessionstate/authority_test.go`, `projection_test.go`, `spike_bench_test.go` (StoreReader fakes)
- `cmd/workspace-agentd/sessionstate_wiring.go`
- `cmd/workspace-agentd/sessionstate_evidence_test.go` (new)
- `cmd/workspace-agentd/sessionstate_metrics.go`
- `cmd/workspace-agentd/main.go`, `sidecar_mode.go`
- `cmd/workspace-agentd/xdg_config_layer_test.go` (env-isolation fix)
- `pkg/abi/abitest/server.go` + `server_test.go` (new)
- `pkg/abi/llmsafespaces/abi/v1/abi.proto`, `pkg/abi/v1/abi.pb.go`, `frontend/src/abi/llmsafespaces/abi/v1/abi_pb.ts` (regenerated)
- `pkg/abi/abiclient/client_test.go` (fake)
