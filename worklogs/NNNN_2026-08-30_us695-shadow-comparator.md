# Worklog: US-69.5 — API shadow consumer + divergence comparator (S1 machinery + scenario suite)

**Date:** 2026-08-30
**Session:** Epic 69 (#1134) US-69.5 (#1139): the S1 shadow consumer — reference-client subscription, the independent API-side dialect fold, the divergence comparator (5 classes + seq-stall observability), artifact recorder, and the committed scenario harness (streaming turn, opencode kill-9 mid-turn, agentd restart mid-turn, suspend/resume, reseed-under-streaming, CPU-starvation stall observability), each ×3 green.
**Status:** Complete (code + harness); the staged-pool soak (≥7 days / agreed turn volume) is the cluster-bound remainder — flagged below.

---

## Objective

Design 0055 S1: before any cutover, the API consumes agentd's stream in shadow and diffs it against its own derivation. Zero unexplained divergence across the failure zoo is the S1 exit. This story builds the comparator machinery and the committed scenario driver; the staged pinned pool runs it for real.

---

## Work Completed

### Shadow consumer (`api/internal/services/shadowconsumer/`, new)
- `consumer.go` — `ABISource` (reference-client subscription with reconnect/backoff — survives pod restarts), `ReferenceFold` (the API-side dialect derivation: busy from session.status, in-flight parts by distinct part ID, harness-death record drop = the tracker's onAgentDied, generation-reconcile = busy-from-store + parts cleared, mirroring `reconcileSessionState`). Deliberately independent of the pod-side translator (it consumes the dialect, not the contract) — the comparator's value is two independent derivations.
- `comparator.go` — divergence classes busy_mismatch / part_mismatch / session_set_mismatch / seq_non_monotonic / snapshot_inconsistency; checkpoint diffing (`CompareNow`) — per-observation diffing across an async pipeline manufactures phantom divergences from propagation lag, so state checks happen at scenario boundaries and quiescent polls; seq-stall detector (`SeqStalled`/`SeqStallSeconds`, threshold-tunable — the 0050 D1 progress signal realized at the API); explained-divergence marking (harness-death reseed windows); `WithDivergenceHook` for the `shadow_divergence_total{class}` Prometheus export when the staged-pool wiring lands.
- `recorder.go` — per-run artifacts: the raw dialect stream + every divergence (NDJSON).

### Scenario harness (`scenarios_test.go`, committed artifact)
Drives the REAL pod path — pinned dialect fixtures/synthetic feeds → `ABITranslator` → sessionstate authority (seq, reseed, fanout, durable cursor) over real HTTP → the reference client — against the reference fold. Pod restarts swap the authority behind a stable server with connection breaks (process-death semantics; pod IP re-resolve). Scenarios ×3 consecutive green:
- `scenario_streaming_turn` — full pinned live capture replayed; derivations agree at every boundary.
- `scenario_opencode_kill9_midturn` — busy mid-turn, feed stops, generation reseed from store truth; harness-death semantics on the reference side; both converge idle.
- `scenario_agentd_restart_midturn` — new authority mid-turn (cursor continuity, busy survives in store); turn continues.
- `scenario_suspend_resume` — hard-kill + reopen: durable cursor never rewinds, boot reseed from empty store, records rebuild.
- `scenario_reseed_active_streaming` — reseed window with feeds continuing into the buffer; client re-snapshots (I3); generation-reconcile on the reference side.
- `scenario_cpu_starvation` — stall window: `SeqStalled` observable from the consumer while the pod lives; recovery converges; stall-seconds evidence captured.

### Bugs the harness caught (fixed here)
1. **abiclient fold did not clear in-flight parts on IDLE** — reconnects showed stale parts (the 69.4 fuzz missed it: its scenarios never idled after parts).
2. **Never-statused sessions**: client fold created sessions as UNSPECIFIED vs the server's UNKNOWN — aligned.
3. Comparator self-deadlock (Debug→Report double lock) — found via harness hang.
4. Reference-fold semantics: monotonic part counting vs in-flight sets; session-set maintenance for created/updated; harness-death record drop; generation reconcile.
5. **Projection/fanout shared-pointer race (production-grade)**: the projection retained part/input objects also referenced by fanned-out frames — later mutations (PART_DELTA) raced in-flight sends. Fixed with clone-on-retain (upsertPartLocked, pending inputs, view()). Surfaced only when the burst-fsync serialization was removed.
6. **abiclient reseed-reconnect wedge + Receive/Err concurrency**: a wedged reconnect could stall the fold forever — now a first-frame budget (2s) drops and retries; s.Err() only after the receiver goroutine completed (connect streams are not goroutine-safe).
7. **repolint detector/test parity**: TestKnownLeaksStillMatchReality used exact-match imports while production prefix-matches subpackages — aligned; dated knownLeaks entry for the S1 comparator's wire import (disposable at S1/S3 exit).

Also: `FastCursor` config (scenario harnesses replay event BURSTS; fsync-per-event matches production's paced event rate — durability stays covered by the fault-injection suite with fsync on).

---

## Key Decisions

1. **Checkpoint diffing, not per-observation**: async pipelines make per-event cross-side diffing a lag detector, not a divergence detector. State equivalence is asserted at quiescent boundaries; seq monotonicity is checked per observation (ordering-valid).
2. **The reference fold models the API tracker faithfully** (onAgentDied + reconcile semantics), not a naive dialect fold — otherwise the comparator reports the tracker's own cleanup rules as divergences.
3. **The harness races deterministically**: slow stores and connection breaks, not sleep-loop races against goroutine scheduling (the first version's reseed race produced unattributable flake; the deterministic overlap tests the same property).

## Assumptions (validated)

| # | Assumption | Validation |
|---|---|---|
| A1 | The comparator can run in-process against real production components | all scenarios green ×3 |
| A2 | Authority swap + connection break models agentd restart faithfully | suspend/resume + restart scenarios: cursor continuity + convergence |
| A3 | Seq-stall observability works at the consumer | starvation scenario: stalled=true during window, false after recovery |

## Blockers

None for the code. **Cluster-bound remainder** (by design, per the issue's dependency note): the staged pinned pool deployment, the ≥7-day soak (or agreed turn volume), and the `shadow_divergence_total{class}` registration in the API's process registry are the ops task that runs THIS harness against real pods. Track on #1139 until the exit report is written.

## Tests Run

- `go test ./api/internal/services/shadowconsumer/` — all 6 scenarios ×3 green (10.5s)
- `go test ./cmd/workspace-agentd/sessionstate/ ./pkg/abi/abiclient/` — green (post client-fold fixes)
- `golangci-lint --new-from-merge-base ./api/... ./cmd/... ./pkg/...` — 0 issues

## Next Steps

1. Staged-pool ops task: deploy the pinned pool, register `shadow_divergence_total{class}`, run the soak, write the S1 exit report (closes the cluster-bound ACs on #1139).
2. US-69.6 (#1140) spikes: 2-CPU snapshot p99, fsync-through-gVisor, resume budget, snapshot size at N sessions.
3. S2 gate check: 0053-S3 (mandatory pins) status before US-69.7.

## Files Modified

- `api/internal/services/shadowconsumer/{consumer,comparator,recorder}.go` + `scenarios_test.go` (new)
- `pkg/abi/abiclient/client.go` (in-flight clearing on IDLE; UNKNOWN session convention)
- `cmd/workspace-agentd/sessionstate/authority.go` (PlatformDir accessor)
