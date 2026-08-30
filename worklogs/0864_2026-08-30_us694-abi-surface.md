# Worklog: US-69.4 — ABI surface: snapshot op, capability report, reference Go client

**Date:** 2026-08-30
**Session:** Epic 69 (#1134) US-69.4 (#1138): the S1 surface completed — GetSnapshot live (zero harness calls), the static capability report (provenance/D3), and the reference Go client implementing the discard rule (shared by comparator + API).
**Status:** Complete

---

## Objective

Complete the M1 surface on 4097: the stamped-snapshot stream + per-session snapshot + capability report, all generated handlers; a reference Go consumer implementing the client rule (apply in order, discard ≤ S, re-snapshot on `projection.reseeded`); proven no-loss connect ordering, zero-opencode-call snapshots, backpressure drop semantics.

---

## Work Completed

### Surface (`sessionstate/service.go`, `authority.go`)
- **GetSnapshot live**: per-session I12-complete snapshot from the projection under the lock; unknown session → NotFound; empty id → InvalidArgument. Reads are NOT rate-limited (I8 bounds deliveries/actions — the mutating ops; the comparator polls snapshots legitimately — this corrected a 69.2 over-application caught by the latency test).
- **Capability report**: `Config.Capabilities` (static per boot) served on every snapshot frame; `bootCapabilityReport` (wiring): provenance from the 0053 overlay anchor (`AGENTD_IMAGE_VOLUME=1` → PLATFORM_PINNED, else UNPINNED), harness + ONE bounded boot-time version discovery (2s, fallback "unknown" — M3.1 keeps hot paths harness-call-free), agentd version from pkg/version, parts [TEXT] (D3: file parts NotSupported on opencode), actions undeclared until US-69.9.

### Reference client (`pkg/abi/abiclient`, new)
- `Client` over the generated connect client: `Sync` (snapshot + idle-window drain), `Stream` (live fold, transparent re-snapshot on reseed notices), `GetSnapshot`, `Capabilities` (rides the snapshot frame — no sixth op), `Act`. The discard rule exists exactly once, here — US-69.5's comparator and the S2 API consumer share it.

### Tests (issue test plan)
- `TestGetSnapshot_ZeroOpencodeCalls` (counting store: 0 calls; I12 payload: status + pending question), `TestSnapshotLatencyLocal` (200 sessions, 300 calls, p99 < 250ms; the 2-CPU pod measurement is US-69.6), `TestDiscardRulePropertyFuzz` (50 random interleavings: client fold ≡ server fold), `TestSlowConsumerDropResync` (drop storm + fresh-snapshot convergence, no cursors), `TestCapabilityReportContract` (provenance/parts/actions + typed NotSupported detail on undeclared action), `TestAuthorityServesGeneratedHandlers` (conformance to the generated interface + generated mount path).

---

## Key Decisions

1. **Reads unrate-limited** (correction to 69.2): I8's rate limits bind deliveries/actions; snapshots/delivery-status are reads.
2. **Static capability report**: built once at boot (one bounded harness call allowed at boot, never on the hot path); re-reads would violate M3.1.
3. **`Sync` idle-window semantics**: a one-shot consistent fetch can't wait for stream close (streams stay open); 150ms idle window balances consistency vs latency; comparator may prefer Stream.
4. Client reseed handling: `Stream` reconnects transparently (mandatory re-snapshot, I3); `Sync` recurses once (rare generation events bound the depth).

## Assumptions (validated)

| # | Assumption | Validation |
|---|---|---|
| A1 | Snapshot reads are pure projection folds | counting-store test: 0 calls |
| A2 | 250ms budget holds locally with 200 sessions | p99 well under (local); pod-level at US-69.6 |
| A3 | The connect client's generated interface is the only surface the authority must satisfy | conformance test; zero hand-written handlers |

## Blockers

None.

## Tests Run

- `go test -count=1 ./pkg/abi/abiclient/` — PASS
- `go test -race -count=1 ./cmd/workspace-agentd/...` — PASS (all 69.2/69.3 suites + e2e)
- `golangci-lint --new-from-merge-base` — 0 issues

## Next Steps

1. US-69.5 (#1139): the API shadow consumer + comparator — subscribe the reference client per pod (opt-in pool), diff against the API's own tracker, drive the S1 scenario suite to zero divergence.
2. US-69.6 (#1140): spikes — snapshot p99 on a real 2-CPU pod, fsync-through-gVisor, resume budget, snapshot size at N sessions.

## Files Modified

- `pkg/abi/abiclient/{client,client_test}.go` (new)
- `cmd/workspace-agentd/sessionstate/{authority,service,projection}.go` (GetSnapshot, capabilities, IngestForTest)
- `cmd/workspace-agentd/sessionstate_wiring.go` (bootCapabilityReport)
