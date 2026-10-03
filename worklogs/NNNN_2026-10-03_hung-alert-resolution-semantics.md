# Worklog: Hung-alert resolution semantics — server-side resolved_at (#998 follow-up)

**Date:** 2026-10-03
**Session:** The hung-badge latch bug family was diagnosed as an abstraction mismatch (a log consumed as state); this change moves the truth server-side. Stacked on fix/hung-badge-stale-alert-seed (#1616).
**Status:** Complete

---

## Objective

Make "is a session hung NOW" first-party server state instead of a client-side reconstruction from an append-only log, killing the latch-bug class by construction.

---

## Work Completed

- **Migration 000034** (`alert_resolution`): `session_alerts.resolved_at timestamptz NULL` + partial index on unresolved rows (only live hangs exist in it). Round-trip verified up→down→up against Postgres 16; resolve-UPDATE semantics smoke-tested (targets only the workspace's unresolved rows).
- **DB + service**: `ResolveSessionAlerts` (UPDATE ... WHERE resolved_at IS NULL, returns count); `ListSessionAlerts` selects resolved_at; `SessionAlert.ResolvedAt` (Go + TS types). `sessionalerts.Service.ResolveWorkspace` rides the existing bounded-queue drainer (same best-effort contract as RecordAlert).
- **Sweep** (`escalateHungs`): workspaces that have alerted keep being polled THROUGH the alert cooldown (recovery detection cannot ride the cooldown — a hang ending one minute after an alert would stay "live" for the full window). On observed recovery: emit `workspace.alert_resolved` (user+workspace streams), persist the resolution, drop the cooldown entry (a fresh hang re-alerts immediately). Still-hung inside cooldown: silent both directions.
- **Frontend consumes the flag**: provider seed gates on `alert === "session_hung" && !resolvedAt`; `workspace.alert_resolved` SSE handler clears; WorkflowsPage filters unresolved and drops its sessions cross-fetch. The busySessionsRef gate + sync effect from #1616 round 2 are DELETED (net simplification — resolution is server truth; the late-fetch race and idle-clear heuristics die with it). All terminal-event clears from #1616 round 3 stay (instant UX + defense in depth).

---

## Key Decisions

1. Resolution lives where the observation lives. The D6 sweep already polls statusz per workspace; it is the only component that observes busy→idle transitions for unattended sessions. Emitting the resolution event + persisting the flag there gives one authoritative source; every consumer (provider badge, WorkflowsPage dot, future) reads it.
2. Through-cooldown polling is bounded: only workspaces with unresolved alerts pay the extra statusz fetches during cooldown, and the entry is dropped at resolution — steady-state cost is unchanged.
3. The between-recovery-and-sweep window (alert unresolved, session already idle): the seed badges it (correct — the server has not resolved it) and `workspace.alert_resolved` clears it at the next tick. Bounded by one sweep interval, closed by the same authority.
4. #1616's client-side hardening stays: SSE terminal clears are instant where the sweep is tick-lagged, and they protect against a missed alert_resolved event. Belt and suspenders, each layer now simple.

### Review round 1 — the orphaned-resolution paths (read-side heal)

The bounded-window claim was happy-path-only; three validated paths lose the resolution authority and re-introduce the latch as a regression vs the busy-gate: (a) API restart spanning recovery (in-memory busyAlerts lost; post-restart sweep sees idle + not-alerted → never resolves), (b) workspace leaving Active after alerting (sweep stops covering it; the feed serves unresolved rows unfiltered), (c) resolve-flush failure after the SSE event (clearBusyAlerted already ran; no re-arm). Fix — one mechanism covering all three:

- **Read-side heal**: `ListByWorkspace` resolves unresolved rows older than `UnresolvedStaleAfter` (1h = 2× the 30-min re-alert cooldown) before serving. Every latch path funnels through this read, so the read that would have latched is itself the reconciler; a genuine hang re-alerts every cooldown, so an old unresolved row with no successor means resolution was lost. Failure direction: a missing badge, never a latch. Best-effort — heal failure serves the read unhealed (sweep + SSE remain primary).
- `ResolveStaleSessionAlerts` DB method; integration tests pin the resolve UPDATE semantics, the resolved_at scan (NULL and set — the scan-drift class from the bases_null_scan incident), and the stale-heal cutoff; service tests pin the heal-on-read + failure-serves-read behavior.
- Resolve flush now logs the resolved count (0 rows = surfaced orphan signal).
- Registered the missing `workspace.alert` in KNOWN_EVENT_TYPES; dropped the unreachable `!alerted && busyAlertCooling` branch; `ListSessionAlerts` scans via `sql.NullTime` (convention); openapi `SessionAlert.resolvedAt` added.
- Queue-full asymmetry (a dropped resolve is worse than a dropped alert) and the wedged-pod-blocks-resolution note: both bounded by the read-side heal — a dropped/failed resolve heals on the next read past the trust window.

### Review round 1 on the recreated PR (#1620; #1618 was closed and recreated as one clean commit after GitHub Actions stopped firing runs on its branch — no platform outage)

- **Belt-test regression**: the branch recreation carried the pre-round-4 `json_duplicate_keys_test.go` (the old branch predated #1616's round-4 `%s` fix); restored main's version.
- **Non-Active resolution**: an alerted workspace with no pod (suspended/terminated) now RESOLVES at the no-pod branch of the sweep (the hang cannot outlive the pod; a resumed workspace has fresh agent state) instead of orphaning rows to the read-side heal. Also drains the busyAlerts entry at process level. Test: `TestEscalateHungs_ResolvesWhenWorkspaceLeavesActive`.
- **Claim corrected**: `UnresolvedStaleAfter` doc no longer claims "never a latch" — two bounded false-latch paths remain inside the window (orphan younger than the bound served unresolved; resolveHungs publishing the SSE clear before the persist commits), both self-correcting via the 1h heal for future reads and the reconnect re-seed for a latched tab.

---

## Blockers

None.

---

## Tests Run

- Go: `escalateHungs` resolution lifecycle (alert → recovered emits alert_resolved + persists + drops cooldown → fresh hang re-alerts immediately); still-hung-inside-cooldown silent both directions; never-alerted never resolves (no per-tick writes). `sessionalerts` drain-to-resolve + failure-not-fatal. All affected packages green (`handlers` 101s, `sessionalerts`, `database`, `auth`, `app`, `pkg/types`); `go vet ./...`, `golangci-lint` 0 issues.
- Migration: up→down→up round-trip on Postgres 16 + resolve-UPDATE smoke (2 unresolved → 1 workspace resolved, other workspace untouched).
- Frontend: provider tests reworked for resolvedAt semantics (unresolved seeds; resolved history never seeds; late unresolved seed latches then alert_resolved clears; reconnect re-seed; terminal clears incl. aborted/deleted/agent_died/phase/resync) — 150 provider/page tests green; e2e both directions green; typecheck clean (module install fixed the earlier local env gap).

---

## Next Steps

- Merge order: #1616 first, then this (stacked) — or rebase onto main after.
- Optional follow-up noted by review: workspace-granularity idle clear can drop a badge while a DIFFERENT session in the workspace is still hung (self-heals ≤ cooldown via re-alert); now that resolution is server-side, the badge could key off per-session alert state instead.

---

## Files Modified

- api/migrations/000034_alert_resolution.{up,down}.sql (new) + helm mirror
- pkg/types/session.go — SessionAlert.ResolvedAt
- api/internal/interfaces/interfaces.go — ResolveWorkspace / ResolveSessionAlerts
- api/internal/services/database/database.go — resolve + stale-heal + list resolved_at (NullTime)
- api/internal/services/database/session_alerts_integration_test.go (new)
- sdks/openapi.yaml — SessionAlert.resolvedAt
- api/internal/mocks/database.go — mock method
- api/internal/services/sessionalerts/service.go + service_test.go — resolve queue path
- api/internal/handlers/proxy_lifecycle.go — sweep restructure + resolveHungs
- api/internal/handlers/proxy_d6_test.go, proxy_d6_resolution_test.go (new)
- api/internal/app/e2e_suspend_test.go, api/internal/services/auth/auth_e2e_all_test.go, auth_e2e_secrets_test.go, auth_sessionid_test.go — fakes grow the method
- frontend/src/api/workspaces.ts — SessionAlert.resolvedAt
- frontend/src/providers/SessionActivityProvider.tsx — flag gate, alert_resolved handler, busy-ref deletion
- frontend/src/providers/SessionActivityProvider.alerts.test.tsx — resolvedAt semantics
- frontend/src/pages/WorkflowsPage.tsx + test — unresolved filter, sessions fetch dropped
- frontend/tests/e2e/hung-badge.spec.ts — resolvedAt fixtures
