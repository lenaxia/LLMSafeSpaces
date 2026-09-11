# Worklog: #828 batch 1 — CreateSession/ListSessions/SendMessage adapter-only

**Date:** 2026-09-11
**Session:** epic-826 / #828 batch 0 + batch 1 (claim: [#828 comment](https://github.com/lenaxia/LLMSafeSpaces/issues/828#issuecomment-5641340986)). Agent: opencode.
**Status:** Complete

---

## Objective

Migrate the first three dual-path proxy handlers to adapter-only, deleting their legacy dialect branches in the same PR, per #828's batch plan. Batch 0: enumerate every dual-path site on current main and audit fixture/behavior coverage. Batch 1: `CreateSession`, `ListSessions`, `SendMessage`.

---

## Work Completed

### Batch 0 — enumeration (as-built on this branch's base)

21 `h.adapter != nil` sites across 9 files. `proxy_handlers.go` 11: CreateSession, ListSessions, SendMessage, SendPromptAsync ×2, GetHistory, GetSession, AbortSession, DeleteSession, RenameSessionInAgent, EnqueueMessage. Others: `proxy_input.go` emitPendingInputRequests, `proxy_permissions.go` autoApprovePermission, `proxy_session_index.go` fetchAndPersistTitle + runParentBackfill, `session_parents.go` fetchSessionParent, plus the `Start`/`onPhaseChange`/`SetAdapter` runtime guards. Batch mapping against the issue's stale line numbers: batch 1 = first cluster (done here); batches 2–4 unchanged.

Coverage audit: adapter-path handler tests already exist (`adapter_path_test.go`, `adapter_crosscutting_test.go`, `e2e_adapter_test.go` — including `TestE2E_Adapter_SendMessage_Error_IncludesCredentialHint`, so enrichment was NOT a gap; initial suspicion corrected during audit). Production always sets the adapter (`app.go:259`, wrapped in `systemnotices.Wrap`); the nil-check fallbacks served only adapter-less tests, exactly as #828 states.

### Batch 1 — handler migration

- **Nil-adapter guard** (`adapterUnavailable`, proxy.go): migrated handlers return `503 {"error":"agent adapter not configured"}` instead of falling back to the raw proxy. Shared input validation (session-ID shape, files rejection) still precedes the guard — 400 beats 503.
- **CreateSession / ListSessions** (`proxy_handlers.go`): legacy `proxyToWorkspace` tails deleted; adapter bodies un-indented.
- **SendMessage** (`proxy_handlers.go`): legacy tail deleted (errBodyTransform closure, `proxyToWorkspaceWithErrBody` streaming call, status-based title fetch). Adapter path already handles enrichment inline and titles on success.

### Disk-pressure injection — single source enforced

The transport-level injector (`injectDiskPressureNotice` + `isLLMPromptPath` + the `proxyToWorkspaceWithErrBody` hook) was the LEGACY duplicate; the primary injection point since #944 is the `systemnotices.Wrap` adapter decorator (`noticingAdapter.Send`), which covers every entrypoint. Deleted the transport copy and its 12 byte-body tests; pinned the handler contract with `TestSendMessage_DiskPressure_HandlerNeverInjects` (99% disk, bare adapter receives verbatim text — guards against double-injection). Decorator coverage: `pkg/agent/systemnotices/systemnotices_test.go` (warning/critical/below-threshold).

### Test migration (61 failing rows → green)

- **Transport-behavior suites re-pointed to still-legacy routes** (`GetSession` GET): ProxiesGET, SendsBasicAuth, ForwardsQueryParameters, PasswordCached, SecretNotFound, EmptyPasswordKey, WorkspaceNotFound/NotRunning/UsesDefaults, ConnectionFailure503, RetriesOnStaleIP, ConnectionCeiling429, BackendErrorPassthrough, ConcurrentRequests, Activity ×2, G34 auth-scrub, auth-cache ×3, EndpointMapping (migrated rows removed), E2E_FullFlow (create/list legs dropped — covered by e2e_adapter).
- **`registerLegacyMessageTransport`** (proxy_test_helpers_test.go): a test-only route invoking the exact pre-batch-1 SendMessage transport call (write-op, session-scoped, bufferable, enrichment closure). Post-batch-1 the transport's write-op/ceiling/buffer/SSE-stream/error-body arms have NO production caller (verified: SendMessage was the only `bufferable=true`/streaming caller; SendPromptAsync's tail is `enqueueV2`, GetHistory's tail is bespoke) but the transport ships until #828's final batch — these suites pin it: ActiveSessionLimit, AlreadyActive, ReadOnlyBypasses, SessionLeak ×2, NoDoubleRelease, ProxyBuffer ×6, B2 ×2, US44 ×11, chat-buffering ×4, DoProxy-5xx write leg, ProxiesPOST.
- **Handler-semantics ports:** `TestProxy_CreateSessionBypassesLimit` → `TestCreateSession_AdapterPath_BypassesActiveSessionLimit`; 409-test rewritten on the adapter path (sync send admits within-limit active sessions, never 409).
- **Deleted with the dead code:** `TestMessage_NoFiles_BodyProxiedVerbatim` (verbatim-body contract was legacy-only).

---

## Key Decisions

1. **503 typed guard instead of silent constructor enforcement** — making the adapter a required ctor parameter is the FINAL batch's move (touches every test constructor at once); per-batch, a loud typed 503 keeps each batch independently shippable while killing the silent fallback.
2. **Disk-pressure: delete the duplicate, don't port** — the decorator already owns injection on the adapter path; porting would have double-injected. The handler-never-injects pin makes that regression visible.
3. **`registerLegacyMessageTransport` rather than deleting the transport suites** — the transport is still shipped production code until the final batch; deleting its coverage now would leave it unguarded for 2+ batches. The final batch deletes code + suites together.
4. `ses_1`-shaped session IDs in the 5xx-metric row: `sanitizePathForMetric` collapses only `ses_`-prefixed segments; `s1` produced an unsanitized label (found via red assertion).

---

## Assumptions (Rule 7 — stated and validated)

- A1: production always sets the adapter → validated `app.go:259`; `SetAdapter(nil)` is a no-op (proxy.go:229).
- A2: SendMessage was the only streaming/bufferable transport caller → validated by grepping every `proxyToWorkspace*` call site; remaining callers pass `bufferable=false`/bespoke paths.
- A3: SendPromptAsync/GetHistory keep their dual paths (batch 2 scope) → untouched; `enqueueV2` tail confirmed not raw-proxy.
- A4: no frontend/SDK contract change — the three routes' response shapes are unchanged on the adapter path (e2e_adapter pins); only the nil-adapter failure mode changed (503 typed error), unreachable in production per A1.

---

## Blockers

None.

---

## Tests Run

- `go test -timeout 600s ./api/internal/handlers/` — ok (98.8s; 61 previously failing rows green)
- `go test -timeout 900s ./api/...` — ok (full api tree)
- `gofmt -l api/internal/handlers/` — clean
- `go vet ./api/internal/handlers/` — clean
- New rows red-first: `TestCreateSession/ListSessions/SendMessage_NilAdapter_Returns503TypedError` failed against the legacy fallback (200s through a working backend) before the migration.

---

## Next Steps

1. Batch 2: SendPromptAsync (both arms), GetHistory, GetSession, AbortSession, DeleteSession, RenameSessionInAgent, EnqueueMessage — same pattern; GetHistory's bespoke legacy tail (`doHistoryRequest`) needs its own seam decision.
2. Final batch: adapter as required ctor param, delete `proxyToWorkspace*`/`doProxy` + `registerLegacyMessageTransport` + its suites, repolint zero-site gate.
3. Epic-71 wave 4a (#1302) unblocks when batch 3 (input/permissions cluster) lands.

---

## Files Modified

- api/internal/handlers/proxy.go (adapterUnavailable guard; disk-pressure transport hook deleted)
- api/internal/handlers/proxy_handlers.go (3 handlers migrated, legacy branches deleted)
- api/internal/handlers/proxy_disk_pressure.go (byte injector + isLLMPromptPath deleted; header rewritten to decorator-as-single-source)
- api/internal/handlers/proxy_batch1_migration_test.go (new — 5 rows)
- api/internal/handlers/proxy_test_helpers_test.go (registerLegacyMessageTransport)
- api/internal/handlers/proxy_test.go (re-points + ports)
- api/internal/handlers/proxy_disk_pressure_test.go (rewritten: pure tests kept, byte-injector tests deleted, handler-never-injects pin)
- api/internal/handlers/proxy_terminal_events_test.go, proxy_chat_buffering_test.go, proxy_request_buffer_test.go, proxy_auth_cache_test.go, proxy_headers_allowlist_test.go, proxy_upstream_5xx_observability_test.go, proxy_attachments_test.go, contract_auth_test.go (re-points)
