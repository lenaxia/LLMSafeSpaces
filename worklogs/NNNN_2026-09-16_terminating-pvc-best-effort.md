# Worklog: Terminating-phase PVC delete is best-effort (owner-ref GC + warning event)

**Date:** 2026-09-16
**Session:** Fix #772 — PVC delete error wedges workspace in Terminating forever; finalizer never removed on persistent failure
**Status:** Complete

---

## Objective

Make the explicit PVC `Delete` in `handleTerminating` best-effort so a persistent
failure (RBAC denial; PVC with a stuck CSI finalizer returning conflicts every
reconcile — the Longhorn node-loss case) no longer wedges the Workspace in
`Terminating` forever. Surface the delegated cleanup to operators (warning
Event + log + metric) instead of silently swallowing.

---

## Work Completed

### TDD: failing tests first (`controller/internal/workspace/phase_terminating_test.go`)

Five new tests, all red before the fix (undefined `v1.ReasonPVCCleanupDelegated`,
undefined `ctrMetrics.WorkspacePVCCleanupDelegatedTotal`, and the wedge itself):

- `TestHandleTerminating_PVCDeleteError_BestEffort_StillTerminates` — persistent
  `Conflict` on PVC delete via fake-client `interceptor.Funcs{Delete:…}`:
  reconcile returns nil, phase reaches `Terminated`, finalizer removed, warning
  Event emitted (reason + PVC name + GC fallback + residual), metric +1 with
  reason label `Conflict`.
- `TestHandleTerminating_PVCDeleteError_ReconcileDoesNotRequeueOnError` —
  persistent `Forbidden`; 3 reconciles all nil-error, finalizer gone after the
  first (no error-requeue loop burning the workqueue).
- `TestHandleTerminating_PVCDeleteSucceeds_NoWarningEvent` — happy path is
  event-silent (operator noise discipline).
- `TestHandleTerminating_PVCNotFound_SilentNoEvent` — NotFound stays silent
  (extends the existing `PVCAlreadyGone` test with the no-event assertion).
- `TestHandleTerminating_StatusUpdateError_StillReturned` — with the PVC
  present and deleting cleanly, a status-update error still propagates
  (requeue semantics unchanged by the best-effort switch).

### Fix (`controller/internal/workspace/phase_terminating.go`)

- PVC delete error (non-NotFound): `return ctrl.Result{}, err` →
  `r.reportPVCCleanupDelegated(ctx, workspace, err)` + continue to finalizer
  removal.
- `reportPVCCleanupDelegated`: zap error log (repo's warn-and-continue
  convention — logr has no Warn), `ReasonPVCCleanupDelegated` warning Event
  on the Workspace via the existing `Recorder` (nil-guarded, same as
  boot_failure.go / overlay reporting), metric increment.
- Reason constant `ReasonPVCCleanupDelegated = "PVCCleanupDelegated"` added to
  `pkg/apis/llmsafespaces/v1/workspace_types.go` alongside the other event
  reasons. Deliberately NOT "PVCOrphaned": in Kubernetes parlance orphaning
  means keeping the dependent (orphan finalizer), the opposite of the
  delegated-delete semantics here.

### Metric (`controller/internal/metrics/metrics.go` + `metrics_wiring.go`)

- `llmsafespaces_workspace_pvc_cleanup_delegated_total` CounterVec, label
  `reason` (API StatusReason: `Conflict` = likely stuck CSI finalizer,
  `Forbidden` = RBAC denial, non-status errors → `unknown`). Follows the
  existing `*Into`/package-level wiring-helper pattern; registered in
  `AllCollectors()`. Alert-worthy rate, not page-on-any.

### Drive-by (Rule 5): flaky test fix in `api/internal/handlers`

`TestOutboxDeliver_V2UnhappyPaths/admission transport cut` failed once under
full-repo parallel load (`expected 1 admits, actual 0`) and passed 5x in
isolation — the assertion reads an async goroutine's counter immediately after
`DeliverOutboxOnceForTest` returns. Replaced the immediate read with
`require.Eventually` (the same file's own pattern for the same counter, line
567). Not caused by this change (nothing in the API's import graph changed
behaviorally); fixed because Rule 5 tolerates no failing/flaky tests.

---

## Key Decisions

1. **Best-effort + GC instead of requeue-with-backoff.** The issue offers both;
  GC is the right shape: `SetControllerReference` on the PVC (verified
  `phase_pending.go:66`) makes the explicit delete an optimization, not a
  necessity. Backoff would still wedge on permanently-failing deletes.
2. **Event over condition.** The workspace is about to be deleted — a status
  condition would die with the object. A Warning Event on the Workspace
  (visible via `kubectl describe` before deletion, and in event exporters
  after) + metric is the durable operator signal.
3. **Message wording is conditional** ("reclaim CAN stay blocked IF the PVC's
  own finalizers are stuck"): in the RBAC case, cluster-authoritative GC may
  still collect the PVC promptly; the unconditional phrasing reviewed in
  adversarial pass overclaimed.
4. **Password-Secret delete block left alone.** It still propagates non-NotFound
  errors (same wedge shape in theory). Left per issue framing + task direction:
  pw-Secrets have no CSI finalizers (failure mode far rarer), the Secret also
  carries a controller owner-ref (`secrets.go:94` → GC backstop), and
  `cleanupFailedWorkspaceSecrets` re-attempts it best-effort. Flagged for a
  follow-up if operators ever see it; changing it here would be scope creep
  beyond #772's ask.
5. **Reason name avoids "orphan"** — K8s reserves that word for the
  keep-the-dependent finalizer semantics; our semantics are delegate-the-delete.

---

## Blockers

None.

---

## Tests Run

- `go test -run 'TestHandleTerminating|TestHandleDeletion' ./controller/internal/workspace/` — red pre-fix (undefined symbols), 11/11 green post-fix (6 pre-existing + 5 new).
- `go build ./...` — clean.
- `go test -timeout 900s -count=1 ./...` — all packages green (after the flake fix; one unrelated pre-existing flake in `api/internal/handlers` fixed as above).
- `go test -count=5 -run TestOutboxDeliver_V2UnhappyPaths ./api/internal/handlers/` — stable 5x.
- `make lint` (golangci-lint v2) — 0 issues.
- `gofmt -l controller/ pkg/ api/` — clean.

---

## Next Steps

- If the AI reviewer wants coherence for the pw-Secret delete block, apply the
  same best-effort pattern (3 lines + one test).
- Operators: alert on `increase(llmsafespaces_workspace_pvc_cleanup_delegated_total[1h]) > 0`
  (rate, not single occurrence).

---

## Files Modified

- `controller/internal/workspace/phase_terminating.go` — best-effort PVC delete + `reportPVCCleanupDelegated`
- `controller/internal/workspace/phase_terminating_test.go` — 5 new tests + 2 helpers
- `controller/internal/workspace/metrics_wiring.go` — `incrementPVCCleanupDelegated{,Into}`
- `controller/internal/metrics/metrics.go` — `WorkspacePVCCleanupDelegatedTotal` + registration
- `pkg/apis/llmsafespaces/v1/workspace_types.go` — `ReasonPVCCleanupDelegated`
- `api/internal/handlers/proxy_outbox_verify_test.go` — flake fix (Rule 5 drive-by)
