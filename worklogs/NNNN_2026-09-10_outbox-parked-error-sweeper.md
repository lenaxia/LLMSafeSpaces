# Worklog: Outbox parked-error sweeper + never-park-while-ledger-admits (epic 71 / 0b)

**Date:** 2026-09-10
**Session:** Implement #1316 (epic 71 #1314, stream 0b): automate the #1308 manual recovery — parked `status:error` outbox entries are re-verified against the agentd delivery ledger, and the error-park write refuses to park a row the ledger still holds.
**Status:** Complete

---

## Objective

Kill the parked-error stranding class from the 2026-09-10 ses_f73747f8 incident: entries parked on honest `context deadline exceeded` timeouts while the agentd ledger still held them admitted produced queue-UI error pills that only manual Valkey surgery cleared. Three deliverables per #1316:

1. Parked-error sweeper (periodic + on-Active-transition) reconciling parked entries against `GetDeliveryStatus`.
2. Never-park-while-ledger-admits guard at the error-park write.
3. Sweeper observability feeding the #1312 canary.

---

## Work Completed

### Outbox package (`api/internal/services/outbox/`)

- **`parked_sweeper.go` (new)** — the `LedgerProbe` seam (`SetLedgerProbe`), local ledger-state constants (ABI enum names, pinned by a handler-side parity test), `ledgerStateCompletes` (the I10 mirror), `SweepWorkspaceParkedErrors` + internal `sweepParkedErrors`/`sweepSessionParked`, `dispositionParked` (the #1316 decision table), `parkGuard`, and metrics (`llmsafespaces_outbox_parked_sweeper_outcomes_total{outcome=verified|completed|rearmed|stayed|indeterminate}`, `llmsafespaces_outbox_parked_sweeper_last_run_timestamp_seconds`).
  - Decision table: admitted/promoted/turn-ended/stalled at the last attempt → complete (remove + `onDelivered` — queue.update/sent rides the existing hook); LEDGERED → stay (agentd owns admission; #1311's state deadlines resolve the row, next sweep acts); FAILED-or-absent with budget → re-arm pending (an attempt+1 row found FAILED is adopted as a recorded attempt, mirroring the terminus's in-band bookkeeping); FAILED-and-exhausted → stay (terminal; user retry/dismiss); `lastErrUnverifiable` never blind re-arms to pending (#987 — that class stays with `SweepWorkspaceUnverifiable`'s verify-first path).
  - When no row exists at `Attempts`, the ambiguous in-flight `Attempts+1` row is probed — the crash/verify-park shape where a POST landed but no failure was recorded (completes the #1308-clearance class generically).
- **`outbox.go`** — the park guard in `deliverOne`'s failure branch: at the park threshold, probe (entry, Attempts) — completes → complete the entry (staging LRem + `onDelivered`); LEDGERED → restore as `delivering` with a bounded re-poll (`ownsAdmissionRePollBackoff`=15s; `Attempts` stays truthful so the terminus's prior-attempt resolution re-polls, never re-POSTs); FAILED/absent/probe-error → park as before. Probe wired iff terminus regime → adapter mode behavior unchanged. `Service` gains `ledgerProbe` + `parkedSweeping atomic.Bool`; `Run` drives the periodic sweep (time-gated at `ParkedSweepInterval`=60s, detached goroutine so probes never stall delivery ticks, non-reentrant via CAS, `sweepEvery` captured before the loop to avoid test-tuning races).
- **`parked_sweeper_test.go` (new)** — 14-case decision-table matrix, no-probe no-op, probe-error indeterminate, scope filter (parity with `SweepWorkspaceUnverifiable`), only-error-entries, metrics deltas, five park-guard cases (admitted completes / ledgered stays delivering with truthful attempts + backoff / failed parks / no-row parks / nil-probe parks / below-threshold unchanged), Run-loop periodic sweep, lock-defer and wait-out-short-hold lock tests.

### Handler wiring (`api/internal/handlers/`)

- **`outbox_terminus.go`** — `outboxLedgerProbe` adapter (agentdEndpoint → `ledgerLookup` over a 5s-timeout client; `not_found` → `("", nil)` so the outbox distinguishes absence from unreachability).
- **`proxy_lifecycle.go`** — `Start()` wires `SetLedgerProbe` iff `h.agentdTerminus` (before `Run`).
- **`proxy_events.go`** — the Active-transition self-heal goroutine calls `SweepWorkspaceParkedErrors` beside `SweepWorkspaceUnverifiable`, gated on the terminus regime (not the adapter — the probe's actual dependency).
- **`outbox_terminus_test.go`** — `deliverHits`/`statusHits` counters added to the shared ledgerStub.
- **`outbox_sweeper_test.go` (new)** — ABI-enum parity pin (outbox constants == generated enum == terminus constants), probe contract (state verbatim / not_found→no-row / transport error), the #1316 integration replay (2026-09-10 shape: admitted row completes without any Deliver POST — S9 asserted via deliverHits==0; failed-exhausted stays; other workspace untouched), and the full-wiring e2e (real `Start()` → seed's Active transition → sweep completes the stranded admission; recovery lookup-only).

---

## Key Decisions

1. **Probe-based, not message-based, park discrimination.** `SweepWorkspaceUnverifiable`'s single-lastError-string match is documented as a gap; the new sweeper consults ledger state exclusively.
2. **The sweeper holds the per-session delivery lock** (with a bounded 2s retry): its LRange→LSet/LRem window would race same-session `deliverOne` (or a peer replica's sweep) on a periodic cadence — an index-based LSet against a mutated list overwrites the wrong entry. Found in adversarial review (F1); the retry (not skip) matters because same-tick deliverOne routinely wins the spawn race (found as an e2e regression: deterministic skip → 60s starvation).
3. **`indeterminate` vs `stayed` outcome labels** (F2): the canary must not treat a dead pod like a confirmed terminal.
4. **Guard applies only at the park threshold.** Below it, the existing re-arm + prior-attempt resolution already prevents re-POSTing live rows (pinned by existing terminus tests); minimal diff, no new loop.
5. **Attempt-numbering truthfulness:** the guard keeps the `Attempts` increment for owns-admission timeouts; the sweeper adopts an observed attempt+1 FAILED row. Both keep the terminus's `attemptOf(Attempts)+1` contract intact so re-picks poll, never re-POST.
6. **Full LEDGERED-forever convergence needs 1b** (#1311's per-state deadlines flip stranded LEDGERED rows to FAILED, which this sweeper then dispositions). Documented in code; the streams compose per the epic's design.

## Assumptions (Rule 7) — stated and validated

- A1: park writes live at outbox.go failure branch + verifyOne paths — validated by reading every `StatusError` write.
- A2: last ledger attempt for a parked entry is `Attempts`; ambiguous in-flight row may exist at `Attempts+1` — validated from terminus `attemptOf(e.Attempts)+1` + increment-after-failure ordering.
- A3: re-armed entries with Attempts>0 never re-POST live rows — validated by `TestAgentdDeliver_RetryChecksPriorAttemptFirst` and the LEDGERED poll branch.
- A4: `agentdEndpoint(ctx, ws) (string,string,error)` — proxy_actions.go:146.
- A5: sweeps must not block the Run tick (network probes) — validated by Run's single-loop structure; sweep is a detached goroutine.
- A6: unverifiable entries must never blind re-arm (#987) — validated by `SweepWorkspaceUnverifiable`'s verify-first design; exclusion asserted in the matrix.

## Adversarial review (Rule 11) — validated findings

- **F1 (real, fixed):** per-session lock race — fixed with lock + bounded retry; regression tests added.
- **F2 (real, fixed):** outcome-label granularity — `indeterminate` split from `stayed`.
- **F1b (real, fixed):** skip-on-first-lock-miss starved the sweep for a full interval — retry-wait fix + `TestSweepParkedErrors_WaitsOutShortLockHold`.
- False alarms documented: staging dual-copy after sweeper LRem converges via Recover→verifying→verifier; unverifiable+FAILED cycle is the pre-existing verify-first path; crash-window re-POST is 0a's entry-level idempotency scope; `onDelivered` double-fire on LRem failure matches the existing success-path semantics.

---

## Blockers

None for 0b. Cross-stream findings:
- **Pre-existing main failure** (verified on a pristine main worktree): `TestEnsureOpencodeRegistryConfig_LegacySymlinkReplaced` in `cmd/workspace-agentd` (xdg_config_layer_test.go:132) — stale fixture vs. the current free-models catalog; fails on main, untouched by this PR. Flagged on #1314 for whichever agent owns that area. (The package also needs >240s under `-race` — budget, not a hang.)
- LEDGERED-row terminal convergence rides 1b (#1311) per the epic design.

---

## Tests Run

- `go test -timeout 90s -race -count=1 ./api/internal/services/outbox/` — ok (all sweeper/guard/matrix/lock/metrics tests).
- `go test -timeout 600s -race -count=1 ./api/internal/handlers/` — ok (129s; incl. new probe/replay/e2e + all pre-existing terminus/902/attachment suites).
- `go test -timeout 600s -race -count=1 ./api/...` — ok.
- `go test ./pkg/agent/... ./pkg/abi/... ./pkg/session/...` and `./cmd/workspace-agentd/sessionstate` — ok.
- `cmd/workspace-agentd` (root pkg): pre-existing fixture failure on main (see Blockers); `./api/...` and `./pkg/...` fully green.
- `golangci-lint run ./api/internal/services/outbox/... ./api/internal/handlers/...` — 0 issues; `go vet` clean; `gofmt` clean; `go build ./...` ok.

---

## Next Steps

- Merge-gate check for 0b: L9's mechanism is asserted (convergence latency tracks the configured sweep cadence, tested at 2× cadence bound); the 5-minute L9 budget itself is a deployment configuration argument (60s cadence ≪ 5min) plus the epic's post-deploy ops row — a wall-clock production measurement remains the Wave 0/1 deploy check.
- 1b lands the shared lease-clock constant: reconcile `ParkedSweepInterval`/`probeTimeout` into the #1312 budget table's single owner if conventions differ.
- Whoever owns the agentd config layer fixes the pre-existing xdg fixture failure (flagged on #1314).

---

## Review round 1 corrections (automated review on PR #1318)

Two claims in this worklog were wrong and are corrected here (discipline: corrections in-entry, visibly):

1. *"gated on the terminus regime (not the adapter)"* — was FALSE as first pushed: the transition sweep sat inside the adapter gate (`h.outbox != nil && h.adapter != nil`) and was dead in adapter-less wirings; the original e2e passed via Run's first tick, not the transition. Fixed: the parked sweep is hoisted to its own `h.outbox != nil && h.agentdTerminus` gate; the e2e was rewritten to drive `onPhaseChange` directly (no Start/Run confound) with a no-transition control, and a Start-based test now pins the periodic-path wiring separately.
2. *"L9 asserted: ✅ unit + integration"* — overstated. Corrected to the mechanism bound (convergence latency ≤ ~2× configured cadence, now asserted in `TestRun_SweepsParkedPeriodically`); the 5-min budget is a configuration argument until the post-deploy measurement.

### Review findings fixed (with regression tests)

- **Defect 1 (critical):** same-pass index-shift corruption — a completed entry's `LRem` shifted snapshot indices and a later `rearmed` `LSet` overwrote an innocent neighbor (S3 violation, duplicate-delivery exposure). Fixed with descending iteration; `TestSweepParkedErrors_MultiEntrySamePass` asserts the victim survives.
- **Defect 2 (high):** cycle-2 of an owns-admission timeout minted a phantom attempt (`Attempts++` on a poll-only cycle), parked the entry against a LEDGERED row the sweeper could never find — unrecoverable, and a later user `Retry` would re-POST against an admitted row (the ses_f73747f8 class). Fixed with `outbox.PriorAttemptPendingError` (parallel to `Ambiguous`): the terminus wraps the prior-row poll timeout; the failure branch neither increments nor parks. `TestDeliverOne_PriorPendingNeverMints` + `TestAgentdDeliver_PriorLedgeredTimeoutIsPriorPending` pin it.
- **Finding 4:** the periodic sweep goroutine now joins `Run`'s workers WaitGroup (no post-Run mutations).
- **Finding 3 (documented, no code):** `verifyOne`'s park writes remain unguarded at write time; the 60s sweeper reconciles them (except the deliberately-excluded unverifiable class) — the sweeper is the systemic guard.
- **Fault legs:** leg-4 shape (probe down → indeterminate → pod back + admitted → next pass completes) and leg-6 shape (rollover-parked unverifiable completed via the admitted in-flight `+1` row, zero Deliver POSTs) now tested; the full fault matrix rides the #1312 harness knobs (1a/1b waves).
- Commit type: follow-up commits use the conventional `feat(outbox):` prefix.

---

## Files Modified

- `api/internal/services/outbox/parked_sweeper.go` (new)
- `api/internal/services/outbox/parked_sweeper_test.go` (new)
- `api/internal/services/outbox/outbox.go` (park guard + Run sweep + Service fields)
- `api/internal/handlers/outbox_terminus.go` (probe adapter + probe client)
- `api/internal/handlers/outbox_terminus_test.go` (stub counters)
- `api/internal/handlers/outbox_sweeper_test.go` (new)
- `api/internal/handlers/proxy_lifecycle.go` (SetLedgerProbe wiring)
- `api/internal/handlers/proxy_events.go` (Active-transition trigger)
- `worklogs/NNNN_2026-09-10_outbox-parked-error-sweeper.md` (this file)
