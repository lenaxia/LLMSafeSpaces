# Worklog: US-69.13 — Epic 69 close-out: flip gate, dead-path disposition, docs

**Date:** 2026-09-01
**Session:** Epic 69 (#1134) US-69.13 (#1147, design 0055 S4): the authority flip gate (drain-before-flip with mode_transition park), the dead-path disposition (oracle retention audit), the runbook + docs sweep, and the epic exit review.
**Status:** Complete in-repo; the two cluster-bound ACs (rollback drill under load, resume budget p95) are pool-bound cross-notes — harness, runbook, and gate machinery committed; evidence rides the delivery pool workflow (the repo's established pattern for cluster-bound ACs, per worklog 0875).

---

## Work Completed

### The flip gate (design 0055 M4)
- **Drain signal**: `statusz.ledger_in_flight` (`pkg/agentd/types.go`; `Authority.InFlightDeliveries` = ledgered + admitted + stalled — promoted/turn-ended are resolved, failed is terminal-per-attempt). Threaded through `serverDeps.ledgerInFlight` (single-container + sidecar). No ABI change (frozen).
- **Park mechanism**: `outbox.ParkWorkspace/UnparkWorkspace` (`park.go`) — in-flight (pending/delivering/verifying) entries → `StatusParked` with the explicit `mode_transition: <reason>`; parked is inert to the delivery loop by construction (the pick skips non-pending); unpark re-arms EXACTLY mode_transition parks (genuine errors never re-armed). TDD: park scope, round-trip, reason match, empty no-op.
- **Operator surface**: `POST /api/v1/admin/authority/{park,unpark}` + `GET /api/v1/admin/authority/inflight/:workspaceId` (`admin_authority.go`), admin-guarded, wired in app.go + router + openapi.yaml (contract test green). `local/authority-flip.sh` (preflight [--park] / park / unpark / flip on|off (dry-run unless EXECUTE=1) / rollback) + `docs/runbooks/authority-flip.md` (sidecar-flip style: preconditions incl. the D4 single-regime + ValidateDeliveryFlags matrix, ordered procedure, verify steps, rollback = flag off → unpark → 0052 drain, US-69.12 alert triage).
- `flipgate_park_with_reason`: TestFlipGate_ParkWithReason + the park_test suite.

### Dead-path disposition (the S4 audit)
- **Text-scan oracle: RETAINED behind the adapter seam as the documented rollback fallback.** The S1 spike's deletion rule ("exact admission-ID correlation ⇒ delete outright", worklog 0867) never settled — the per-pinned-version pool runs are still open. Deleting now would break R8's shape (authority-off → 0052 verify-first path) on unproven ground. Audit result recorded here + on design 0055's S4 checkbox; deletion re-opens when the pool matrix lands.
- Deleted: the three stale `eventLiteralKnownLeaks` entries (the tracker file no longer exists; the other two files no longer carry the literals) — repolint clean with an empty map.
- Verified clean: no dialect remnants outside api/ (the dead-code gate's tree); agentd's own `sessionStatusTracker` is 0055's A9/A1 surface, not a remnant.

### Docs
- design 0055: Status → Implemented (US-69.1–.13 merged; US-69.14 deferred upstream); S1–S3 checkboxes ticked (pool items annotated); S4 ticked with the oracle-retention + pool annotations.
- README-LLM (session-proxy row + changelog row 1.29), README.md (/session-events description), helm values + configuration.md (AGENTD_STATE_AUTHORITY), monitoring.md (the dangling US-69.13 reference → the runbook).

## Epic exit review (design 0055 R/I vs evidence)

| Criterion | Verdict | Evidence |
|---|---|---|
| R1 agentd sole writer | ✅ | 0861/0871 (seq under authority lock; actions+delivery serialized) |
| R2 stamped snapshots | ✅ | 0864 (GetSnapshot zero-harness), 0881/0885 (frontend fold) |
| R3 scale-to-zero display streams | ✅ | contractstream manager (0881) + usage gates (US-69.11, activity-gated — review-round correction) |
| R4 on-demand consumption | ✅ | US-69.11 (tracker deleted; zero pod streams on idle fleet — Test902_E2E_ReconcilerDoesNotReArmIdleGates) |
| R5 metrics/alerts | ✅ | 0880 (5 metrics, 4 alerts, promtool) + custom_valve counter (US-69.11) |
| R6 billing continuity | ✅ | usagestream per-step billing, deterministic keys (US-69.11 — fixed the latent 2-replica double-billing) |
| R7 dialect containment (I11) | ✅ | dead-code gates: frontend (0885) + api (US-69.11); MCP cut to ABI frames (US-69.11) |
| R8 rollback | ✅ machinery / 🌊 drill under load = pool | flag-off path retained (oracle audit above), unpark drain, runbook; the under-load drill rides the pool |
| R9 (exit review) | ✅ | this section |
| I1–I12 | ✅ each pinned by its stage's tests | stamp atomicity/discard fuzz (0861/0864/0885), subscribe-before-snapshot (0861), rebuildability+reseed (0861/0864), store-is-truth, delivery idempotency (0871 stress), wake-only (0871), interrupt purity (0878), 4097 auth, ledger durability, completion mapping, containment, I12 snapshot-completeness (0885 standing-question e2e) |
| Pool-bound exceptions | 🌊 | ≥7-day zero-divergence soak; admission-ID matrix; 20k-msg full-list cost; runsc resume p95; rollback drill; final resume p95 — all carried by the delivery pool workflow (harnesses + budgets committed) |

## Tests Run

- `go test ./api/internal/services/outbox/ ./api/internal/handlers/ -run "TestPark|TestUnpark|TestFlipGate" -count=1` — ok.
- `go test ./api/internal/server/ -run TestOpenAPIRouterContract` — ok (3 new routes documented).
- `go test ./cmd/workspace-agentd/ -run "TestStatusz"` — ok (ledger_in_flight pin).
- Full suite at PR time (CI).

## Next Steps

1. Pool runs close the 🌊 items; when the admission-ID matrix lands, re-open the oracle deletion (this worklog's audit is the record).
2. US-69.14 (#1148, S5 history terminus) — deferred on upstream pagination; the worklog's map stands.
3. Epic #1134 closes when the pool evidence lands (or the owner accepts the exceptions as recorded).

## Files Modified

- `api/internal/services/outbox/park.go` (new) + `park_test.go` (new)
- `api/internal/handlers/admin_authority.go` (new) + `admin_authority_test.go` (new)
- `api/internal/handlers/proxy.go` (GetOutbox), `proxy_state_reconciler.go` (FetchStatuszPublic)
- `api/internal/server/router.go`, `api/internal/app/app.go`, `sdks/openapi.yaml`, `router_openapi_contract_test.go`
- `pkg/agentd/types.go`, `cmd/workspace-agentd/{server.go,main.go,sidecar_mode.go,main_test.go}`, `sessionstate/authority.go`
- `local/authority-flip.sh` (new), `docs/runbooks/authority-flip.md` (new), `docs/operator/{monitoring,configuration}.md`, `helm/values.yaml`, `README.md`, `README-LLM.md`, `pkg/repolint/event_literal{,_test}.go`, `design/0055_…md`
