# Worklog: Session archiving — API foundation (PR1 of 3)

**Date:** 2026-10-07
**Session:** #1627 PR1 — DB migration + archive/unarchive API + listing field + proxy read-only enforcement + SDK surface
**Status:** Complete

---

## Objective

Land the platform foundation for issue #1627: the archived marker on the session index, the archive/unarchive API endpoint, the archive-status listing field (ABSENT = not archived), read-only enforcement at the API proxy layer on every chat-send surface, and the four SDK clients.

---

## Work Completed

### Migration + DB layer
- `api/migrations/000035_session_archived.{up,down}.sql` (+ `helm/migrations/` mirror): `session_index.archived boolean NOT NULL DEFAULT false`. No data migration — existing rows read as not-archived; the listing field is omitempty.
- `database.Service.SetSessionArchivedStatus`: UPDATE-only by design — an unindexed session is a caller error (typed 404 via `apierrors.NewNotFoundError`), never a phantom row the #1340 reconcile pass would later reap.
- `database.Service.IsSessionArchived`: `sql.ErrNoRows` → `(false, nil)` — the index is event-fed and can lag (#1452), so an absent row must never lock a session out of chat.
- `ListSessionIndex` selects + scans `archived` into `SessionListItem.Archived` (`omitempty`).

### Interfaces + services
- `SessionIndexService.SetArchived/IsArchived` + `DatabaseService.SetSessionArchivedStatus/IsSessionArchived`; sessionindex delegate; `workspace.Service.SetSessionArchived` (verifyOwner → 404/403 semantics preserved).
- Deliberate divergence from the seen/rename nil-index no-op precedent: `SetSessionArchived` with a nil index returns an ERROR — archive is a state transition the caller relies on for enforcement; a silent no-op would report success while sends keep flowing.
- Router: `PUT /api/v1/workspaces/:id/sessions/:sessionId/archived`, body `{"archived": bool}`. The bind uses `*bool` + `required` so `archived:false` (unarchive) is a first-class call, not a missing field (the plain-bool `required` trap).

### Read-only enforcement (the proxy layer)
- `ProxyHandler.rejectIfArchived` (proxy_archived.go): typed `409 {"code":"session_archived","error":...}` before any adapter call, slot reservation, or outbox accept. Guarded sites: `SendMessage`, `SendPromptAsync` (outbox arm), `EnqueueMessage` (outbox arm), `syncSend` (covers both no-outbox fallbacks). History/GetSession deliberately unguarded.
- Fail-OPEN on archive-check errors (Warn log, send proceeds) — orchestrator-confirmed ruling: UX guard, not a security boundary (the in-pod peer path bypasses the proxy by the owner's proxy-only ruling); fail-closed would turn DB blips into chat outages.
- `outbox.Terminal` error class + `deliverOne` branch: terminal failures park as `error` on the FIRST pass — no attempt minted, no retry. The #1316 ledger park guard is deliberately skipped (no admission happened for it to hold).
- `outboxDeliver`: fresh entries (no attempt driven) are gated before any delivery; entries WITH a prior attempt keep the #987 verify-first (landed text completes — that is reconciliation, not delivery), then the re-send is gated. Orchestrator-confirmed ruling (b).

### Cross-tab announcement
- `ProxyHandler.PublishSessionArchived`: `session.status` with status `archived`/`unarchived` on workspace + user streams (#786 pattern — both streams or other tabs keep rendering live state). Published after the index write commits.

### SDK surface (hand-maintained clients; generators are placeholders)
- openapi.yaml: `setSessionArchived` path + `SessionListItem.archived` (description carries the ABSENT-means-not-archived contract). Router parity test green both directions.
- Go: `SessionsService.SetArchived` + `SessionListItem.Archived`. TypeScript: `setArchived` + `archived?`. Python: `set_archived` on sync + async clients. Java: `SessionsService.setArchived` + `ArchivedRequest`.

---

## Key Decisions

1. **Platform archive ≠ agent archive.** opencode carries its own dormant `archived` (ocSession.Archived → contract Session.Archived, translate.go:583/690). Nothing on the platform sets it. The #1627 flag is `session_index.archived` ONLY; the adapter path is untouched (validated: grep for writers found none on platform surfaces).
2. **Update-only archive (404 on unindexed).** Honesty over upsert: a phantom row would be reaped by reconcile, silently un-archiving; a 404 tells the caller the ID is wrong.
3. **Fail-open on check errors** (ruling c) and **terminal outbox refusal** (ruling b) — both orchestrator-confirmed this session.
4. **Fresh-vs-prior-attempt split in outboxDeliver** — read-only governs NEW deliveries; #987 reconciliation still completes entries whose text landed pre-archive (blocking those would strand ledger state and misreport delivered messages).

### Assumptions stated and validated (Rule 7)
- *Absent index row = not archived* — validated by design review of the event-fed index (rows created at CreateSession + RecordMessage; lag documented in #1452) and pinned by `TestIsSessionArchived_AbsentRowIsNotArchived`.
- *Sessions with archived=true remain in GET /sessions listing* — the frontend's Archived group renders FROM the listing (PR3). Validated: no filter added; listing tests assert the flag rides along.
- *409 is the house status for state-conflict refusals* — validated against the uploads D16 phase gate (`409 + phase`) and the 422/410 typed-body precedents.
- *The plain-bool `binding:"required"` trap* — caught by the unarchive router test before it could ship (gin treats false as missing on non-pointer bools).

---

## Blockers

None. (Two environment stalls: shared-pod disk hit 100% mid-commit — resolved by clearing 3GB of stale `/tmp/go-build*` corpses from OOM-killed turns + `go clean -cache/-modcache` per the standing convention; the gofmt staged-blob trap hit once — re-add after gofmt, as briefed.)

---

## Tests Run

- `go test ./api/internal/services/database/` — ok (9 new archived tests + updated list fixtures)
- `go test ./api/internal/services/workspace/` — ok (4 new SetSessionArchived tests)
- `go test ./api/internal/handlers/` — ok (8 new enforcement tests incl. fail-open, outbox-accept refusal, sync fallback, history-open, terminal delivery refusal)
- `go test ./api/internal/services/outbox/` — ok (terminal-parks-immediately + no-repick)
- `go test ./api/internal/server/` — ok (5 router tests + TestOpenAPIRouterContract)
- `go test ./sdks/go/...` — ok; `tsc --noEmit` — clean; `py_compile` — ok; `mvn compile` — ok; `make -C sdks sdk-check` — green

---

## Next Steps

PR2 (agentd): internal `/internal/v1/session-archive` + `/internal/v1/session-delete` endpoints (SA-token, pod_workspace_rename pattern; delete delegates to the existing hard-delete flow) + MCP tools `session_archive`/`delete_session` + `session_metadata` current-session-only default (lsp_injected_session + busy fallback) + plugin extension to stamp session_metadata + archived status in session_metadata output (internal GET, ~15s cache).

PR3 (frontend): kebab Archive/Unarchive, the collapsed-by-default Archived group (OrphansGroup pattern), archived badge, read-only composer on archived sessions, `session.status archived/unarchived` handling in SessionActivityProvider.

---

## Files Modified

- api/migrations/000035_session_archived.{up,down}.sql (new) + helm/migrations/ mirrors (new)
- api/internal/services/database/{database.go, session_archived_test.go (new), database_test.go, session_index_test.go, session_last_seen_test.go}
- api/internal/interfaces/interfaces.go
- api/internal/mocks/{database.go, workspace.go}
- api/internal/services/sessionindex/service.go
- api/internal/services/workspace/{workspace_service.go, workspace_session_test.go}
- api/internal/server/{router.go, router_session_archived_test.go (new), router_session_reconcile_test.go}
- api/internal/handlers/{proxy_archived.go (new), proxy_archived_guard_test.go (new), proxy_session_archived_test.go (new), proxy_handlers.go, proxy_lifecycle.go, opencode_upgrade_test.go, proxy_backfill_test.go, proxy_test.go}
- api/internal/services/outbox/{outbox.go, outbox_test.go}
- pkg/types/session.go
- sdks/openapi.yaml; sdks/go/{types.go, services.go}; sdks/typescript/src/{types.ts, client.ts}; sdks/python/llmsafespaces/{client.py, async_client.py}; sdks/java/.../SessionsService.java
- README-LLM.md (API reference row)
