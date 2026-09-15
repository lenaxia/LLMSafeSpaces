# Worklog: NNNN — epic-71 / 4b-c2: #1304 OpenAPI session-contract truth-up + x-opencode-proxy marker removal

**Date:** 2026-09-15
**Session:** Stream 4b-c2 of epic #1314 — implement #1304: remove the x-opencode-proxy coupling from the SPEC/SDK surface, document the contract `Session`, retype `getSession`, sync the four SDKs, version bump + breaking-change note (covering 4b's input-surface changes too).
**Status:** Complete

---

## Objective

Make the OpenAPI spec — the SDKs' single source of truth — describe the session surface as it actually is: every user-facing session endpoint speaks the platform-owned `pkg/session` contract (no raw agent passthrough anywhere), zero `x-opencode-proxy` markers remain (so #1305's lint can be born clean), the four hand-maintained SDKs carry typed `Session`/receipt surfaces, and the combined breaking change (4b input surface + this session surface) lands as one spec/SDK major bump (0.7.0 → 1.0.0) with a written note.

## Rule-7 assumptions (stated, then validated)

| # | Assumption | Validation | Result |
|---|---|---|---|
| A1 | `getSession` returns the contract `*session.Session` (issue evidence from 2026-09-08 may be stale — 4a/4b moved things) | Read the LIVE handler: `proxy_handlers.go:609-631` `GetSession` → `adapter.GetSession` → `c.JSON(200, s)`; adapter interface `pkg/agent/adapter.go:50` returns `*session.Session`; opencode adapter `adapter.go:175` → `ParseSessionWire` → `translateSession` (translate.go:582) always sets `WorkspaceID` + translated `Status` | **validated** — retype to `Session` is production-true |
| A2 | The 7 remaining markers are all stale (question/permission family markers already removed by 4a/4b) | `grep -c x-opencode-proxy sdks/openapi.yaml` = 7 before, at exactly the mission's sites (sendMessage/getHistory/sendPromptAsync/getSession/deleteSession/abortSession/streamEvents) | **validated** |
| A3 | Spec rows on touched endpoints match live handler output | Row-by-row against `proxy_handlers.go`: **FOUR DIVERGENCES FOUND** — (a) `deleteSession` documented 200 but handler emits `c.Status(204)` (proxy_handlers.go:688); (b) `abortSession` documented 200 but emits 204 (line 660); (c) `sendPromptAsync` 202 documented bodyless but emits `{"messageID","clientMessageID","status":"queued"}` (lines 272-276) plus an undocumented 200-duplicate arm (line 261); (d) `getHistory` description said "newest-first ordering within a page" but handler/adapter produce oldest-first chronological (proxy_handlers.go:479, adapter.go:597 "reverse into chronological order") | **falsified as assumed-true; all four fixed** — the body-divergence trap was real |
| A4 | `sendMessage`'s documented request body (`content` required) matches the handler | `extractMessageText` (proxy_handlers.go:294-306) parses ONLY `parts[].text`; a body with just `content` 400s ("text must not be empty") — `content` is never read | **falsified; fixed** — spec now requires `parts`, documents `content` as accepted-but-ignored (the `verbose`-param convention) |
| A5 | The spec's `SessionSummary` component (used by `ContractEvent.session`) correctly types the ABI frame | ABI schema `pkg/abi/llmsafespaces/abi/v1/contract.proto:263` — `Event.session` is the FULL `Session` message, not a summary; no SDK references SessionSummary | **falsified; fixed** — `ContractEvent.session` → `$ref Session`; SessionSummary deleted |
| A6 | In-repo consumers of the retyped SDK methods still compile | Canary grep: Go canary had 4 map-index sites on `Sessions.Get` + 5 single-value assignments on `SendPromptAsync` — all fixed to typed access (d-prompt-async now asserts the receipt: `status=="queued" && messageID!=""`); TS canary uses `as any` casts (unaffected); Python canary is runtime-transparent (TypedDict is a dict) | **falsified for Go canary; fixed; `go build ./... && go vet ./...` green in sdks/canary/go + sdks/canary/mcp** |
| A7 | Frontend references getSession shapes and needs a suite run | `frontend/src/api/types.ts` declares `AgentSession` but the frontend never imports the `@llmsafespaces/sdk` package (grep clean) — the stale declared-not-read fields (`parentID` casing, `share`, `"retry"`) are the #1303 extractor-cleanup family, deliberately untouched here | **validated — no frontend change needed; no suite run required (zero frontend files changed)** |
| A8 | Breaking → major bump per the documented rule | `sdks/PACKAGES.md` versioning section: "additive changes bump the minor, breaking changes the major" — no prior major existed (git log -G 'version: "1.' empty); packages at 0.5.4, spec at 0.7.0 | **validated — spec + TS/Python/Java packages → 1.0.0** (Go resolves from VCS tags) |

## Work completed (TDD — every surface red-first)

### 1. RED evidence (captured before any implementation)

- **Go:** `session_contract_test.go` failed to compile (`map[string]any has no field ID`).
- **TS:** `tsc` on the test file — `TS2305: no exported member 'PromptAccepted'/'Session'` (vitest strips types, so the type-level red is the compile check; src tsconfig excludes tests by design).
- **Python:** `ImportError: cannot import name 'PromptAccepted'` (both test files).
- **Java:** compilation failure (no `Session`/`PromptAccepted` models, no `get` method).
- **Hurl:** `sessions_contract.hurl` (new) — getSession key asserts failed (spec had the free-form object); `sessions.hurl` abort row pinned `HTTP 204` and failed against the spec's wrong 200.

### 2. Spec (`sdks/openapi.yaml`)

- Deleted all 7 `x-opencode-proxy` markers; `grep -c` = 0 across the spec. Section comment "Proxy (to opencode serve)" → "Sessions (agent adapter surface)".
- New components: `Session` (mirrors `pkg/session/contract_gen.go:130-143`: id/workspaceId/parentId/title/agentId/model/status enum/cost/contextUsage/time/summary/archived; required [id]), `ContextUsage`, `TimeRange`, `PromptAccepted` (messageID/clientMessageID/status enum [queued, duplicate]).
- `getSession` 200 → `$ref Session`; description rewritten ("the contract Session"); added 400/502/503 rows (explicit handler branches).
- `deleteSession`/`abortSession` 200 → **204** (the handlers' actual `c.Status(http.StatusNoContent)`); descriptions de-opencoded; added 502 rows; abort gained 400+503.
- `sendPromptAsync`: 202 body documented (`PromptAccepted`), 200-duplicate row added, 429 (queue-full/session/quota + retryAfter) and 503 rows added.
- `sendMessage`: request body truth-up — `parts` required (what the server parses), `content` accepted-but-ignored; 502+503 rows.
- `getHistory`: ordering description corrected to chronological (oldest-first) within the page; 502+503 rows.
- `streamEvents`: marker only (description already platform-events-only, US-69.11).
- Adjacent same-family row: `enqueueMessage` 202 body documented (`{messageID, status:"queued"}`) + 200-duplicate row (handler proxy_handlers.go:846,856 — same outbox receipt family; the row was body-under-documented).
- Version `0.7.0` → `1.0.0`.

### 3. SDK sync (hand-maintained; `generate-all` is a documented no-op)

- **Go:** `Session`/`ContextUsage`/`TimeRange`/`PromptAccepted` types; `SessionsService.Get` → `(*Session, error)` (was `map[string]any`); `SendPromptAsync` → `(*PromptAccepted, error)` (was `error`) — classification by payload `status`, not body presence (the 4b r1 rule). 5 test rows + `uploads_test.go` call sites updated.
- **TypeScript:** `Session`/`ContextUsage`/`TimeRange`/`PromptAccepted` types exported; `sessions.get` → `Promise<Session>`; `sendPromptAsync` → `Promise<PromptAccepted>`; 4 test rows.
- **Python:** `Session`/`ContextUsage`/`TimeRange`/`PromptAccepted` TypedDicts (exported from `__init__`); sync+async `get` → `Session`, `send_prompt_async` → `PromptAccepted`; shared fixtures `tests/session_contract_fixtures.py`; rows in the CI-invoked `test_client.py` + `test_async_client.py` (5 sync + 4 async).
- **Java:** new `Session` (nested ModelRef/Cost/ContextUsage/TimeRange, mirroring the Message nested-class convention) + `PromptAccepted` models; `SessionsService.get` added (Java had NO getSession); `sendPromptAsync` → `PromptAccepted` (both overloads); `SessionsContractTest` (4 rows).

### 4. Canary (Go) — typed-field parity

- 4 map-index sites → typed access (`sessObj.ID`, `.Status`, `.WorkspaceID`, `.Title`); d-session-get additionally pins the contract core (`workspaceId` + `status` non-empty); 5 `SendPromptAsync` call sites updated; d-prompt-async now asserts the accepted receipt (status/messageID).

### 5. Hurl contract rows (`sdks/tests/contract/`)

- New `sessions_contract.hurl`: getSession contract keys, sendMessage parts body, prompt 202 receipt keys (`Prefer: code=202`), 200-duplicate receipt, abort 204, delete 204.
- `sessions.hurl` abort row 200→204 (was pinning the spec's wrong row); `sessions_delete_seen.hurl` delete row 200→204 (same); `sessions_queue.hurl` enqueue row pinned to 202 via `Prefer` (enqueue now documents 200+202) + `$.status` assert.
- **Red-checks executed (4):** abort 204→200 mut (exit 4); getSession `$.workspaceId`→`$.workspace_id` mut (exit 4); prompt `$.messageID`→`$.message_id` mut (exit 4); plus the pre-implementation reds above.

### 6. Docs + versioning

- `sdks/README.md`: "Proxy endpoints" section replaced with "Session and input surfaces (platform contract)"; Design Decision #4 rewritten (contract-typed, no upstream tracking); stale "Regenerate SDKs with make generate-all" step replaced with the hand-sync reality; new "Versioning and breaking changes" section documenting 1.0.0 covering BOTH the 4b input-surface changes and this session surface.
- `sdks/PACKAGES.md`: republish sentence generalized to any spec bump + pointer to the breaking note; quick starts fixed (`response.content` never existed on contract `Message` — now `text`).
- Packages: TS `package.json`+lock, Python `pyproject.toml`, Java `pom.xml` → 1.0.0.
- `sdks/tests/README.md`: file table completed (was missing 8 of 13 files).
- Historical `design/stories/` mentions of the marker convention left as-is (append-only history; US-71.4a itself documents the drop).

## Key decisions

1. **Major bump to 1.0.0** (A8): PACKAGES.md's rule is explicit — breaking → major. This PR's getSession retype, 200→204 rows, SDK signature changes, plus 4b's deferred input-surface breaks, are one logical breaking change; splitting into 0.8.0 would contradict the written rule.
2. **`content` stays documented as accepted-but-ignored** on sendMessage (not deleted): all four SDKs still send it; the spec's `verbose` param set the "accepted for backwards compatibility" convention. Making `parts` required is the truth (a parts-less body 400s).
3. **SessionSummary deleted, not augmented** (A5): the ABI Event carries the full `Session` message; a reduced "summary" type under-documents the frame. One `Session` component serves both getSession and ContractEvent.
4. **Enqueue row truth-upped** despite not being a marker site: same outbox-receipt family, one-line fix, prevents the exact spec↔handler divergence this issue exists to kill (and the row sits 30 lines from `PromptAccepted`).
5. **Error-row completeness scoped to explicit handler branches** (502 adapter-failure, 503 not-ready, 429 on the prompt path): matches the 4b r3/r4 precedent on the input surface; generic 500s left undocumented per house style.
6. **Go canary upgraded, not just repaired**: d-prompt-async now asserts the receipt body (a live-pin upgrade), d-session-get pins the contract core fields.

## Blockers

None.

## Tests run

- Go SDK: `go test -timeout 120s -race ./...` in sdks/go — ok.
- TS SDK: `npx tsc --noEmit` clean; `npx vitest run` 78/78; test-file compile check green.
- Python SDK: `python -m pytest tests/test_client.py tests/test_async_client.py -q` — 133 passed.
- Java SDK: `mvn test` — 44 tests, 0 failures, BUILD SUCCESS.
- Hurl: `make contract-test-mock` equivalent — all 13 files green (hurl 5.0.1 from /tmp, Prism mock); 4 red-check mutations → exit 4.
- `make -C sdks validate` + `make sdk-check` — spec valid, router parity holds.
- `make repolint` — all checks passed.
- Canary: `go build ./... && go vet ./...` in sdks/canary/go and sdks/canary/mcp — clean; canary TS (`as any` casts) and Python (runtime-transparent) unaffected.
- Frontend: zero files changed; frontend does not import the SDK package (grep-verified) — tsc/vitest not required for this change; live canary + kind E2E legs run in CI (not available locally in this stream).

## Next steps

- Review loop on the PR (this worklog's stream: 4b-c2).
- Post-merge: #1305 (repolint spec-coupling lint) can be born with zero known-leaks — the spec is marker-free and the three banned description patterns (`tracks upstream` / `from opencode` / `opencode session object`) have zero hits.
- #1303 owns the frontend `AgentSession` dead-field cleanup (`parentID` casing, `share`, `"retry"` status) — untouched here by design.

## Files modified

- `sdks/openapi.yaml`
- `sdks/README.md`, `sdks/PACKAGES.md`, `sdks/tests/README.md`
- `sdks/go/types.go`, `sdks/go/services.go`, `sdks/go/session_contract_test.go` (new), `sdks/go/uploads_test.go`
- `sdks/typescript/src/types.ts`, `sdks/typescript/src/client.ts`, `sdks/typescript/tests/session-contract.test.ts` (new), `sdks/typescript/package.json`, `sdks/typescript/package-lock.json`
- `sdks/python/llmsafespaces/types.py`, `llmsafespaces/client.py`, `llmsafespaces/async_client.py`, `llmsafespaces/__init__.py`, `sdks/python/tests/session_contract_fixtures.py` (new), `sdks/python/tests/test_client.py`, `sdks/python/tests/test_async_client.py`, `sdks/python/pyproject.toml`
- `sdks/java/src/main/java/com/llmsafespaces/sdk/models/Session.java` (new), `.../models/PromptAccepted.java` (new), `.../services/SessionsService.java`, `sdks/java/src/test/java/com/llmsafespaces/sdk/SessionsContractTest.java` (new), `sdks/java/pom.xml`
- `sdks/canary/go/scenarios/{d-agent-input,d-prompt-async,d-session-ensure,d-session-get,d-session-limit,d-session-subtask}/main.go`
- `sdks/tests/contract/sessions_contract.hurl` (new), `sessions.hurl`, `sessions_delete_seen.hurl`, `sessions_queue.hurl`
- `worklogs/NNNN_2026-09-15_openapi-session-contract-truthup.md` (this file)

## Review r1 remediation

**f1 (HIGH — the issue's mandatory live-router conformance test):** built as `api/internal/server/router_session_contract_test.go` — production `NewRouter` + REAL `ProxyHandler` over a fake `agent.Adapter` returning canned values built from the pkg/session CONTRACT types; every captured body is validated (jsonschema v6, `santhosh-tekuri/jsonschema/v6` — already a direct dep) against the exact response-row schema compiled by pointer out of `sdks/openapi.yaml`. Covers the issue's set (getSession/listQuestions/listPermissions/getHistory/sendMessage) plus this PR's rows (prompt 202 + 200-duplicate receipt, bodyless abort/delete 204s). A missing spec row fails the compile-by-pointer (no vacuous pass), and `TestSessionContractConformance_HarnessDiscriminates` is the harness's own red-check (unknown enum value and missing required `id` both rejected).
- **The harness caught a real pre-existing spec bug on its first run:** the wire discriminator for file-change parts is `file_change` (`contract_gen.go:34`) but the spec's `Part.type` enum — and the TS/Java SDK types copying it — said `file-change`, a value the server never emits. Fixed in spec + TS + Java; added to the 1.0.0 breaking note.
- Wiring note: `NewRouter` without a variadic config applies `DefaultRouterConfig` (production `RequireHTTPS` → 301 on plain-http test URLs); the orgs/mcp wire tests pass an explicit config that replaces it — this test follows that pattern (`RouterConfig{}`).

**f2 (the prompt 429 row claimed "Retry-After is also set as a header"):** falsified against the handler — the queue-full arm (`proxy_handlers.go:263-266`) is body-only; only the connection/session-limit arms (`proxy_adapter_crosscutting.go:66-99`) set the header. Row reworded to say exactly that.

**f3 (429 truth-up stopped mid-family; duplicate-receipt body unpinned):**
- 429 rows added to all six reachable siblings: sendMessage (connection/session/quota), getHistory, getSession, deleteSession, abortSession (connection limit), enqueueMessage (queue-full body-only + the limit arms). sendPromptAsync's row reworded per f2.
- `TestOutbox_DedupeReturnsOriginal` now pins the 200-duplicate BODY at the handler level: same `messageID` as the 202 accept, `clientMessageID` echo, `status: "duplicate"` — the spec's 200 row can no longer drift from the wire.

## Tests run (r1)

- `go test -timeout 600s ./api/internal/server/ ./api/internal/handlers/` — ok (incl. the conformance suite + extended dedupe pin)
- `make -C sdks validate` + `make sdk-check` — green; TS `tsc` + vitest 78/78 (+ test-file compile check); Java `mvn test` BUILD SUCCESS; all 13 hurl files green against Prism

## Review r2 remediation

**f1 (the quota arm's 429 shape — last divergence class):** verified against `proxy.go:355,365`: the quota arm emits `{"error": "quota exceeded", "event_type": ...}` with NEITHER a `retryAfter` body hint NOR the Retry-After header (only the connection/session-limit arms set both, `proxy_adapter_crosscutting.go:66-99`; the queue-full arm is body-only). The three write-path 429 rows (sendMessage, sendPromptAsync, enqueueMessage) now spell out all causes and their exact shapes. The quota gate's fail-closed 503 (`quotaCheckFailed`, proxy.go:372-378) is folded into the 503 row descriptions of the three quota-gated routes — same class, closed with it.
**streamEvents 429:** the broker's per-workspace SSE connection cap (`proxy_stream.go:53`) now documented (plus its 503 broker-uninitialized row).

## Tests run (r2)

- `make -C sdks validate` + `make sdk-check` — green; `go test ./api/internal/server/` (conformance suite) — ok; all 13 hurl files green against Prism.

## Review r3 remediation

**f1 (enqueueMessage had NO 503 row — and the r2 worklog/commit over-claimed):** the r2 note said the fail-closed 503 was "folded into the 503 row descriptions of the three quota-gated routes", but enqueueMessage is quota-gated too (`proxy_handlers.go:833`) and never had a 503 row to fold into — its not-ready arm (`:825`) and the quota fail-closed 503 were both undocumented. Row added (same wording as the siblings). The r2 claim was corrected here rather than rewritten (worklogs are append-only).
**f2 (sendMessage 413):** `rejectMessageRouteFiles`' MaxBytesReader arm (`proxy_handlers.go:349-351`, called at `:83` before body extraction) answers 413 `{"error": "request body exceeds 10 MB limit"}` — now documented. (sendPromptAsync/enqueue cap their bodies with the same reader but surface it as 400 "failed to read request body" — verified, no 413 rows there.)

## Tests run (r3)

- `make -C sdks validate` + `make sdk-check` — green (spec valid, router parity holds).
