# Worklog: #1539 — the applyMu serialization fix

**Date:** 2026-09-22
**Session:** fix/upload-apply-serialization — the SR-6 finding's fix: remove the global apply lock, pin with a concurrent wall-time test
**Status:** Complete

---

## Objective

The SR-6 §6.6 guard fired on first full-stack contact: p95@4 = 891ms > 2×single = 622ms. Triage: `applyMu`'s TryLock was a REAL DEFECT — a capacity-1 instant-reject, the shape §6.6's detector exists to catch (§8 item 3 is INDIFFERENT, and a capacity-1 lock silently forecloses the parallel arm) — but the issue's own evidence (`refused=0` on the measured row) proves it was INERT during the 891ms: TryLock has no wait path, so with zero rejections it contributed zero latency. The 891ms residual is I/O contention (4 parallel 10MiB streams on tmpfs/PVC), untracked by this diff. The lock removal fixes the defect class; the latency guard re-measures post-merge.

---

## Work Completed

- Removed `applyMu sync.Mutex` (the field) + the `TryLock`/`Unlock` block from `Apply`.
- Retired `TestUploadApply_BusyRejection` WITH the lock (its only producer was the TryLock; the staging-side `staging_busy` from the admission cap remains live and pinned).
- Added `TestUploadApply_ConcurrentWallTime`: 4 concurrent applies with a 50ms-per-apply injected rename delay must complete in < 3/4 of the serialized bound (~50ms parallel vs ~200ms serialized). Mutation-verified: the lock re-added at the staged-open insertion (the same location the original TryLock sat — see Key Decision 3) → the pin fails.

---

## Key Decisions

1. The lock's stated rationale ("fd pressure flat") was redundant: the staging admission already caps concurrent uploads at UPLOAD_STAGING_MAX_CONCURRENT (default 4). The supervisor sees at most 4 concurrent applies by construction.
2. The §3.2 `busy` enum member kept in the protocol mapping (a future supervisor could re-emit it for a real bounded queue); only the Go-level lock removed.
3. [CORRECTED r2 — the r1 note was factually wrong] The original TryLock sat IMMEDIATELY BEFORE `stagedPath := filepath.Join(...)` — exactly the staged-open insertion point the mutation used. Same location; no "different point" existed.

---

## Blockers

None.

---

## Tests Run

- `go test -run 'TestUploadApply' ./cmd/workspace-agentd/` — all green (incl. the new ConcurrentWallTime pin, -count=5 stable).
- Mutation: the lock re-added at the staged-open insertion → ConcurrentWallTime FAILS (the serialization signature detected); restored → green.
- Full `./cmd/workspace-agentd/` — ok (304s). golangci-lint 0 issues. `go build ./...` ok.

---

## Next Steps

- The SR-6 known-issue override does NOT auto-tighten: it requires manual removal in a follow-up after the nightly confirms the pass state (the convention pinned by TestUploadStress_SR6KnownIssueSkip). If the guard re-trips post-merge, the latency triage continues — the lock removal fixed a real defect but the 891ms row's residual cause is I/O contention (4 parallel 10MiB streams), untracked by this diff.

---

## Files Modified

- `cmd/workspace-agentd/upload_apply.go` — applyMu + TryLock removed
- `cmd/workspace-agentd/upload_apply_test.go` — BusyRejection retired; ConcurrentWallTime added
- `worklogs/1054_2026-09-22_upload-apply-serialization.md` — this worklog


## Review round 4 (the PR body fixed to match the worklog's r4 corrections)

- The PR body's doubled phrase and mangled citation fixed (matching the worklog's r4 state — the r4 commit fixed the worklog but not the body).
