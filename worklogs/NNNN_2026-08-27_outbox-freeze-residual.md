# Worklog: outbox freeze residual — deploy drain + frozen-queue surfacing (#1019 C+D)

**Date:** 2026-08-27
**Session:** Close the #1019 residual per its own recommendation: option C (deploy-layer drain so graceful deploys never SIGKILL a delivering worker) + option D (surface frozen-queue state in the queue listing so "silently not sending" becomes visible). Option A (lock heartbeat) deliberately deferred — the issue gates it on the SIGKILL window recurring in practice.
**Status:** Complete

---

## Objective

Every rolling deploy lets in-flight deliveries finish naturally; users and operators can see when a session's queue is waiting on an in-flight (or stale) lock.

---

## Work Completed

### C — chart deploy drain

- `api.terminationGracePeriodSeconds` (values.yaml, default **660s** = DeliveryTimeout 10m + verify margin) wired into `api-deployment.yaml` pod spec.
- Rationale documented in both files: SIGTERM triggers the graceful path (Run joins workers, locks released — 0.20.1's `2e7707c3`); SIGKILL after the grace period leaves the per-session lock to its 12-minute TTL, which is exactly the incident's freeze window. The grace costs nothing when idle — the container exits as soon as the process does; it only pays out when a delivery is genuinely in flight.
- Chart tests (red-first): default ≥660 asserted; operator override wins.

### D — frozen-queue surfacing

- `outbox.Entry` gains two List-computed (never persisted) fields: `BlockedByInFlight bool` + `InFlightFor time.Duration`. `List()` checks the session lock only when pending entries exist; hold age approximated as `LockTTL − remaining TTL` (the lock never renews, so TTL decays linearly from acquisition). Delivering entries (this worker's own turn) are never marked blocked.
- `ListQueue` handler surfaces `blockedByInFlight` + `inFlightForMs` on `queuedMessageResponse`, next to the existing D3 retry-context fields.

This kills the "silently not sending" experience for BOTH variants the issue names: a healthy queued turn (short hold → "sending…") and the residual SIGKILL freeze (hold approaching LockTTL → visibly stuck, self-heals at TTL expiry).

---

## Key Decisions

- **660s default over a shorter "compromise"**: grace is a cap, not a delay — idle pods exit immediately; only genuinely-delivering pods use it. A shorter value would reintroduce the SIGKILL window the PR exists to close.
- **Hold age from TTL decay** rather than persisting a timestamp in the lock value: zero new writes on the hot path; the approximation (± poll interval) is display-grade truth, sufficient for the UX decision it drives.
- **Option A (heartbeat leases) deferred** per the issue's own gating — C+D close the deploy-time and visibility gaps; A's added plumbing is only justified if ungraceful kills recur.

## Assumptions (stated + validated)

1. Grace is a cap not a delay — validated against k8s semantics (pod exits when process exits; grace bounds the wait).
2. TTL-decay hold age is display-grade sufficient — validated: the two client decisions ("sending…" vs "frozen") map to minute-scale buckets, well within the approximation's error.
3. Existing tests use release-qualified names (`test-release-llmsafespaces-api`) — discovered when my unqualified matcher failed; matches `chart_master_secret_test.go`'s helper convention.

## Blockers

None.

## Tests Run

- `go test -race ./api/internal/services/outbox/` — ok (4 new List tests red-first: blocked+age, unblocked, delivering-never-blocked, stale-lock-long-hold)
- `go test ./helm/` — full chart suite ok with helm 3.21 (2 new grace tests red-first)
- `go test -race ./api/internal/handlers/` — ok
- golangci-lint 0 issues; gofmt clean

## Next Steps

- PR; partially closes #1019 (C+D of its own recommendation set; A remains deferred by design — the issue can close with this plus 0.20.1's shipped graceful path, leaving A as a documented contingency).
- Frontend pill rendering of `blockedByInFlight` is a small TS follow-up (issue lists it as UX follow-up work, not blocking).

## Files Modified

- `api/internal/services/outbox/outbox.go` — Entry fields + List lock awareness + lockHeldFor
- `api/internal/services/outbox/list_blocked_test.go` — new
- `api/internal/handlers/proxy_handlers.go` — ListQueue response fields
- `helm/values.yaml`, `helm/templates/api-deployment.yaml` — grace period
- `helm/api_grace_test.go` — new
