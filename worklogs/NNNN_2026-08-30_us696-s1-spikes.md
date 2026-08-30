# Worklog: US-69.6 — S1 spikes: committed harnesses, measured baselines, recorded decisions

**Date:** 2026-08-30
**Session:** Epic 69 (#1134) US-69.6 (#1140): the seven S1 spikes — benchmark/probe harnesses committed, in-repo numbers measured, decisions recorded in design 0055 §Open items; pool-bound measurements explicitly marked for the staged pool.
**Status:** Complete (in-repo scope); pool-bound numbers (gVisor/Longhorn/EFS fsync, admission-ID matrix per pinned version, V2 full-list on a 20k-message session) are the staged-pool tasks the ACs cite.

---

## Objective

Settle design 0055's S1 open items before the S2 freeze: harnesses in-repo, numbers where measurable now, decisions recorded where the data suffices.

---

## Work Completed

### Harnesses (committed)
- `cmd/workspace-agentd/sessionstate/spike_bench_test.go` — `BenchmarkSnapshotSize100/500` (+ frame-bytes metric), `BenchmarkResumeBudget` (reopen + cursor load + boot reseed), `BenchmarkCursorFsyncLatency` + `TestSpikeNumbers` (headline numbers observable in CI).
- `local/spike-admission-id.sh` — the caller-supplied admission-ID probe (baseline / fresh-unique / duplicate-reuse matrix) against any live pinned pod; self-builds its tiny Go probe; disposition rule printed with the output.

### Measured (this environment)
| Spike | Number |
|---|---|
| Snapshot frame, 100 sessions × (4 parts + 2 pendings) | 33 KB / ~0.8 ms |
| Snapshot frame, 500 sessions | 169 KB / ~4 ms |
| Resume in-process share (reopen + reseed, cursor@500) | ~0.5 ms |
| Cursor fsync, local container fs | ~7–10 ms/op |

### Decisions recorded (design 0055 §Open items updated)
1. **Snapshot size: no cap/pagination at S1/S2** — orders of magnitude under concern; single consumer per pod (D1-B); revisit at ≥10 KB/session snapshots.
2. **fsync group-commit rule** — ≤10 ms/op ⇒ fsync-per-event stands (I9 as written); >10 ms/op (pool numbers decide) ⇒ group-commit enters US-69.7.
3. **Admission-ID disposition rule** — fresh-unique accept + duplicate 409 ⇒ delete the localhost text-match fallback outright (US-69.7/.8); else it stays, matrix documents why.
4. **Q/P semantics** — no control-plane semantics in the events; `answer_question` is the complete routing path (feeds US-69.9).
5. **statusz ↔ snapshot: keep both** — distinct charters (deep poll vs frozen I12 view); US-69.11 retires the API derivation, not statusz.
6. **Concurrency matrix: no exceptions** — all verbs serialize via single-flight; session-create stays API-side; the interrupt/admission race is settled by I7 construction.
7. **Reseed-under-streaming** — settled by the 69.2/69.5 suites; CI starvation finding tracked on #1139 for the pool soak.

---

## Key Decisions

See above — all recorded in the design doc's open-items section per the story's "findings linked from design 0055" AC.

## Assumptions (validated)

| # | Assumption | Validation |
|---|---|---|
| A1 | The authority's in-process share of resume is negligible | ~0.5 ms measured |
| A2 | Snapshot size scales linearly per session | 100→500 sessions: 33→169 KB |
| A3 | Local fsync ~10 ms/op is the upper bound of healthy storage | pool rule set at that threshold |

## Blockers

None in-repo. Pool tasks (marked POOL in the design doc): gVisor/Longhorn/EFS fsync matrix, admission-ID runs per pinned version, V2 full-list cost on ~20k messages.

## Tests Run

- `go test -run TestSpikeNumbers -v ./cmd/workspace-agentd/sessionstate/` — PASS with headline logs
- Benchmarks as listed; full package `-race` suite still green
- `bash -n local/spike-admission-id.sh`

## Next Steps

1. Staged-pool runs of the POOL-marked spikes (with the 69.5 soak); append numbers to design 0055.
2. S2 gate: 0053-S3 (base strip + mandatory pins) is NOT landed — US-69.7/.8/.9 are hard-gated on it (D4). It is the next unblocked-but-external dependency.
3. US-69.10+ (S3) sequenced behind US-69.4/.8 per the epic index.

## Files Modified

- `cmd/workspace-agentd/sessionstate/spike_bench_test.go` (new)
- `local/spike-admission-id.sh` (new, executable)
- `design/0055_2026-08-29_agentd-session-state-authority.md` (open-items: findings + decisions)
