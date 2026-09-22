# Worklog: #1539 — the applyMu serialization fix

**Date:** 2026-09-22
**Session:** fix/upload-apply-serialization — the SR-6 finding's fix: remove the global apply lock, pin with a concurrent wall-time test
**Status:** Complete

---

## Objective

The design-0060 §6.6 SR-6 guard fired on first full-stack contact: concurrent upload p95@4 = 891ms > 2×single = 622ms. The root cause: `applyMu sync.Mutex` at upload_apply.go serialized ALL applies globally via TryLock — an implementation defect (the design §8 item 3 was indifferent; §6.6's characterization guard exists precisely to detect this class). Remove the lock, pin the parallel behavior.

---

## Work Completed

- Removed `applyMu sync.Mutex` (the field) + the `TryLock`/`Unlock` block from `Apply`.
- Retired `TestUploadApply_BusyRejection` WITH the lock (its only producer was the TryLock; the staging-side `staging_busy` from the admission cap remains live and pinned).
- Added `TestUploadApply_ConcurrentWallTime`: 4 concurrent applies with a 50ms-per-apply injected rename delay must complete in < 3/4 of the serialized bound (~50ms parallel vs ~200ms serialized). Mutation-verified: the lock re-added at the correct insertion point (just before the staged open, not the param-validation block where the original TryLock sat) → the pin fails.

---

## Key Decisions

1. The lock's stated rationale ("fd pressure flat") was redundant: the staging admission already caps concurrent uploads at UPLOAD_STAGING_MAX_CONCURRENT (default 4). The supervisor sees at most 4 concurrent applies by construction.
2. The §3.2 `busy` enum member kept in the protocol mapping (a future supervisor could re-emit it for a real bounded queue); only the Go-level lock removed.
3. The mutation's insertion point matters: the ORIGINAL lock sat after param validation but before the staged open. The mutation verified at the `stagedPath` insertion — the correct serialization point (before any I/O).

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

- The SR-6 e2e guard auto-tightens when this lands (the known-issue skip on #1540).

---

## Files Modified

- `cmd/workspace-agentd/upload_apply.go` — applyMu + TryLock removed
- `cmd/workspace-agentd/upload_apply_test.go` — BusyRejection retired; ConcurrentWallTime added
- `worklogs/NNNN_2026-09-22_upload-apply-serialization.md` — this worklog
