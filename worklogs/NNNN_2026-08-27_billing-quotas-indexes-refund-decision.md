# Worklog: billing quota enforcement + query indexes + refund decision (#768, #773, #767)

**Date:** 2026-08-27
**Session:** Close the remaining P1 billing cluster — atomic quota reservation with per-token gate and fail-closed enforcement (#768), the usage-query/cron index alignment (#773), and the documented refund-path decision (#767, docs-only per the issue's minimum and the unanswered design question on its thread).
**Status:** Complete

---

## Objective

Make quota enforcement actually enforce (#768's three gaps), stop the user-facing usage reads and billing-cron sweeps from scan-filtering (#773), and turn #767's emergent-looking schema into a stated design decision.

---

## Work Completed

### C1 — #773: query-index alignment (migration 000026)

Verified at HEAD: `GetUsage`/`GetUsageByWorkspace` filter `usage_events` on `(owner_id, owner_type, event_time)` while the only owner-scoped index covers `period`; the billing-cron sweeps (`ListAllWorkspaceOwners` database.go:1297, `ListAllWorkspacesForBilling` database.go:1323) filter `deleted_at IS NULL` while `idx_workspaces_deleted` is the inverted partial (`IS NOT NULL`).

- `idx_usage_owner_event_time ON usage_events(owner_id, owner_type, event_time)`
- `idx_workspaces_active ON workspaces(id) INCLUDE (user_id, storage_size) WHERE deleted_at IS NULL` — covering for both sweep shapes (index-only scans)
- Matched down migration.

### C2 — #768: quota enforcement (migration 000027 + service + gate)

Verified at HEAD: `checkProxyQuota` checked only `llm_request` via bare `CheckQuota` (read-then-return), failed open on DB error ("Quota check failed, allowing request").

Three gaps, three fixes:

- **(a) per-token gate**: `checkProxyQuota` now checks `CheckQuota(owner, "llm_tokens")` first — deny new requests once the period's accumulated token usage is at the limit. Absence of a limit row = unlimited (back-compat: deployments without token limits unaffected).
- **(b) fail-closed**: both gate legs deny with **503 "quota check unavailable"** on error (distinct from 429 quota-exceeded), counted in new `llmsafespaces_metering_quota_checks_failed_total`. Rationale: fail-open was the same silent-unenforcement class the P0 billing fixes just eliminated; during a real DB outage the API is unusable anyway (auth/session hit the DB first), so the availability cost of failing closed is near zero while the overage cost of failing open is unbounded.
- **(c) atomic reservation**: `metering.Service.ReserveQuota(ctx, owner, eventType, quantity)` — limit read, then a transaction holding `pg_advisory_xact_lock(hashtextextended(owner|type))` that sums committed `usage_events` (period-scoped, shared builder `quotaUsageQuery` with `getQuotaUsage`) plus unexpired `usage_quota_reservations`, compares, and inserts the slot. Concurrent requests serialize; the last-free-slot race is closed. TTL 2 min (pinned by test); expired rows stop counting and are reaped (`reapExpiredReservations` on the DLQ reaper cadence — reaping is size hygiene, not correctness).

Schema: `usage_quota_reservations` (migration 000027) with the same event_type/owner_type CHECKs as `usage_events`, `quantity > 0`, owner+expiry index, reaper expiry index.

Interface: `MeteringService.ReserveQuota` + `MockMeteringService`. Existing crosscutting/proxy-quota tests repinned to the two-gate flow; the explicit `FailOpen` test inverted to the fail-closed pin.

### C3 — #767: documented decision (docs-only)

The issue's thread has an **unanswered** question to the maintainer (local-ledger refunds [option 1] vs provider-delegated [per Epic 12's existing "Credits / refunds | Buy" row]). Per Rule 6 I did NOT guess the schema direction; I did the issue's stated minimum: Epic 12 README "Failure Modes" now carries a "Decision: refunds are provider-side, the local ledger is append-only" block recording that both CHECKs are deliberate, the failed-turn-billed consequence, the admin-credit path, and the deferred turn-failure→negative-ReportUsage trigger (meaningful only with a live provider).

---

## Key Decisions

- **503 not 429 for enforcement-unavailable** — clients/operators must distinguish "quota exhausted" from "cannot check".
- **Advisory-lock tx over SELECT FOR UPDATE** — no limit-row lock contention across event types, no dependency on a limit row existing for the lock target.
- **Expiry-only release (no per-request key)** — the usage event's idempotency key is generated post-response, unknowable pre-flight; correlating them would need turn-boundary tracking (same gap as #767's deferred trigger). Over-restriction while a reservation and its landed usage event coexist is ≤2 min and in the safe direction; the race it bounds allowed unbounded overage.
- **Token gate needs no reservation** — future token spend is unknowable pre-flight; the accumulated-usage gate is the enforceable approximation the issue asked for.

## Assumptions (stated + validated)

1. `hashtextextended` (PG11+) available — platform requires modern Postgres (LATERAL joins in export query already rely on it). Validated by existing query usage.
2. Advisory-lock serialization per owner+event_type is sufficient granularity — limits are per (owner, event_type) in `usage_limits`; same-key contention is exactly the scope being enforced.
3. 2-min TTL covers plausible in-flight request lifetime; turns longer than 2 min re-open the race for their tail only (bounded, and the request-slot reservation still counts the turn's start).

## Blockers

None. #767's implementation direction awaits the maintainer's answer on its thread.

## Tests Run

- `go test -race ./api/internal/services/metering/` — ok (7 new ReserveQuota/reaper tests red-first)
- `go test -race ./api/internal/handlers/` — ok (7 new gate tests red-first + repinned suites)
- `golangci-lint run` on all touched packages — 0 issues
- `go build ./...`, `gofmt -l` — clean

## Next Steps

- PR this branch; closes #768, closes #773, closes #767 (docs minimum).
- On #767's thread: prompt the maintainer for the local-vs-provider answer if the docs-only close is contested.
- Track D (incident follow-ups #935/#944/#1019-residual) next.

## Files Modified

- `api/migrations/000026_billing_query_indexes.{up,down}.sql` — new
- `api/migrations/000027_usage_quota_reservations.{up,down}.sql` — new
- `api/internal/services/metering/metering.go` — ReserveQuota, quotaUsageQuery refactor, reaper
- `api/internal/services/metering/quota_test.go` — new
- `api/internal/handlers/proxy.go` — checkProxyQuota two-gate + fail-closed
- `api/internal/handlers/proxy_quota_test.go`, `adapter_crosscutting_test.go` — repinned
- `api/internal/handlers/quota_gate_test.go` — new
- `api/internal/interfaces/interfaces.go`, `api/internal/mocks/metering.go` — ReserveQuota
- `api/internal/services/metrics/metrics.go` — quota-checks-failed metric
- `design/stories/epic-12-usage-metering-billing/README.md` — refund decision block
