# Worklog NNNN — epic-71 / 4b: the #1302 cleanup tail — SDK contract sync + frontend shim removal

**Date:** 2026-09-14
**Session:** Stream 4b of epic #1314 — the cleanup tail for #1302: refresh the four SDKs to the retyped input contract (4a-2, #1363), delete the deprecated frontend `QuestionRequest`/`PermissionRequest` shims, sweep legacy-shape references.
**Status:** Complete

---

## Objective

Close #1302's remaining scope: the SDKs pick up the retyped `openapi.yaml` (InputRequest schema on the question/permission list routes; the inbox dismiss route; the 202/409/503 response rows), the deprecated frontend shims and their fixture plumbing retire, and no legacy-shape references remain outside the opencode seam.

## Rule-7 assumptions (stated, then validated)

| # | Assumption | Validation | Result |
|---|---|---|---|
| A1 | "SDK regeneration" means hand-maintained sync — `make -C sdks generate-all` is a documented no-op (US-14.3–14.6 codegen never shipped; sdks/README.md "Keeping the SDKs in sync": sync is enforced by tests, not codegen) | Executed `make generate-all`: validates the spec, then prints "not yet implemented" per language | **validated** — the refresh is hand-editing + test enforcement; all PR/worklog wording says "sync refresh", never "regenerated" |
| A2 | The per-SDK `InputRequest` types (already present for SSE message payloads) match the contract json tags | Field-by-field against `pkg/session/contract_gen.go:242` (id/sessionId/rootSessionId/kind/question/header/options/multiple/custom/permission/patterns/always/metadata/tool) | **validated** |
| A3 | Reply routes: 200 has no body; 202 carries `InboxLateAnswerAccepted`; 409 = dismissed record; 503 = delivery unavailable | `sdks/openapi.yaml:1328-1470` (all four response rows read) | **validated** |
| A4 | No SDK/canary consumer depends on the loose `map[string]any`/`unknown[]` list returns or the loose reply bodies | Grepped `sdks/canary/{go,python,typescript}` for `InputRequests`/`inputRequests`/`input_requests` client usage: zero hits (the D scenario uses raw HTTP per 4a) | **validated** |
| A5 | The frontend shims' only remaining consumers are `contract.test.ts` + its Go fixture generator | Grep `frontend/src` + `frontend/tests` for `QuestionRequest\|PermissionRequest`: contract.test.ts imports + local test helper names only (the helpers construct contract-shaped locals; not shim consumers) | **validated** |
| A6 | `input-snapshot` (US-69.7) belongs in the SDK refresh although the task text names only list/dismiss/202 rows | It is the same input surface family (the documented way clients force a fresh pending set); adding it costs one method per SDK and closes the sync gap a reviewer would flag | **adopted** (decision, not a discovered fact — recorded as deliberate scope completion) |
| A7 | Spec/SDK version bumps are NOT this stream's scope | `sdks/PACKAGES.md` versioning section + #1304 change-item 5 owns "Breaking-change note + spec/SDK version bump"; recent epic streams (3a #1355, 4a #1363) did not bump 0.7.0 | **validated** — documented boundary, deferred to #1304 |

## Work completed (TDD — every surface red-first)

### 1. Go SDK (`sdks/go`)

- **RED:** `input_requests_test.go` written first — failed to compile against the loose surface (`map[string]any has no field ID`).
- `ListQuestions`/`ListPermissions` return `[]InputRequest` (the contract type; a legacy `questions[]` envelope decodes to empty fields — pinned by `LegacyEnvelopeIsNotTheContract`).
- `ReplyQuestion(workspaceID, requestID, answers [][]string)` / `ReplyPermission(workspaceID, requestID, reply, message)` return `(*InboxLateAnswerAccepted, error)` — status-branched via `send`: 200 → nil (live answer), 202 → decoded outbox entry; empty `message` omitted from the wire.
- Added `DismissInboxRecord` (DELETE `/workspaces/{id}/sessions/{sid}/inbox/{rid}`) and `RequestInputSnapshot` (POST `/workspaces/{id}/input-snapshot`).
- New `InboxLateAnswerAccepted` type; five superseded loose-typed route-smoke rows deleted from `client_test.go` (their routes are covered by the new typed rows — the old `{"decision":"allow"}` body was never valid vocabulary).
- Unhappy rows: 409 → `IsConflict`, 503 → `IsServiceUnavailable`.

### 2. TypeScript SDK (`sdks/typescript`)

- **RED:** `tests/input-requests.test.ts` — 4 rows failed (`dismissInboxRecord is not a function` etc.) before the change.
- `listQuestions`/`listPermissions` → `InputRequest[]`; `replyQuestion(…, answers: string[][])` / `replyPermission(…, reply: "once"|"always"|"reject", message?)` → `InboxLateAnswerAccepted | undefined`; added `dismissInboxRecord`, `requestInputSnapshot`; new exported `InboxLateAnswerAccepted` type.

### 3. Python SDK (`sdks/python`)

- **RED:** rows failed on `ImportError: cannot import name 'InboxLateAnswerAccepted'` before the change.
- Sync + async clients: typed list returns, reply signatures (`answers: list[list[str]]` / `reply` + optional `message`, empty message omitted), 202 body return, `dismiss_inbox_record`, `request_input_snapshot`; `InboxLateAnswerAccepted` TypedDict added and exported.
- Rows live in `tests/test_client.py` + `tests/test_async_client.py` (the CI-invoked files — a standalone file would never run: CI pins `pytest tests/test_client.py tests/test_async_client.py`); shared wire fixtures in `tests/input_contract_fixtures.py`.

### 4. Java SDK (`sdks/java`)

- **RED:** `InputRequestsServiceTest` — compilation failure (no service existed; the Java SDK had NO input surface at all).
- New `InputRequestsService` (list/reply/reject/dismiss/snapshot) wired on the client; `InputRequest` promoted from a nested `Message.InputRequest` (which had zero external references) to a top-level model + `InputOption`, `ToolRef`, `InboxLateAnswerAccepted`; null-tolerant list decode (`rows == null ? List.of() : List.of(rows)`).
- Wire-body asserts run against the recorded request bodies (method+path+body recorder), not helper introspection.

### 5. Hurl contract rows (`sdks/tests/contract/inputs.hurl`)

- New file: list routes (array + contract keys), reply 200/202 (`InboxLateAnswerAccepted` keys)/409/503 rows via `Prefer: code=N`, reject, dismiss (204), input-snapshot (202).
- **Red-check executed:** mutating the dismiss row's expectation to `HTTP 200` fails the suite (`error: Assert status code`, hurl exit 4) — the file discriminates.
- `make contract-test-mock` green across all 12 files (hurl 5.0.1 installed to /tmp for the run; CI installs its own).

### 6. Frontend shim removal

- **RED:** `contract.test.ts` retargeted to `InputRequest` first — 3 failures against the legacy fixtures.
- `pkg/types/contract_test.go` now emits `InputRequestQuestion`/`InputRequestPermission` from `pkg/session.InputRequest` (the `pkg/agent` import is gone from pkg/types); fixtures regenerated (`multiple` asserts key-absence — `omitempty` drops false).
- Deleted the `@deprecated QuestionRequest`/`PermissionRequest` shims and the orphaned `QuestionInfo` from `frontend/src/api/types.ts`. Enforcement is compile-time: tsc fails on any straggler import (observed during migration).
- Full suite: **1812/1812 vitest, tsc clean**.

### 7. Legacy-shape sweep

- `pkg/mcp`: zero `agent.QuestionRequest`/`PermissionRequest` references (4a already moved the client to the generic contract + ABI InputRequest).
- `sdks`: zero references (grepped all four languages + spec + docs).
- `pkg/agent/types.go`: deleted the dead `WhileAway bool` fields from both types — zero writers/readers since 4a's D3 envelope move (grep-verified; the marker lives on `WorkspaceSSEEvent`).
- Remaining `agent.QuestionRequest`/`PermissionRequest` consumers are seam-side by design: `pkg/agent/opencode/dialect.go` (opencode wire parsing), `pkg/agent/opencode/adapter.go:720,740` (event translation → `session.InputRequest`), `cmd/workspace-agentd/sessionstate_wiring.go` (→ `abiv1.InputRequest`). Design 0049 discipline rule 2 containment holds.

## Key decisions

1. **Hand-maintained sync, honestly labeled** (A1): the PR says "SDK sync refresh", not "regeneration" — the make targets are no-ops and the README documents the tests-not-codegen contract.
2. **`input-snapshot` included** (A6): same input-surface family; closes the sync gap cheaply.
3. **Version bumps deferred to #1304** (A7): its change-item 5 owns the breaking-change note + bump; no epic stream since US-69.7 has bumped 0.7.0.
4. **Python rows in the CI-invoked files** rather than a new standalone file (CI pins the two file names; a file CI never runs is decoration).
5. **Java `InputRequest` promoted to top-level** rather than referencing `Message.InputRequest`: the nested class had zero external references; a first-class service deserves a first-class model (no duplication — the nested copy was deleted).

## Blockers

None.

## Tests run

- Go SDK: `go test -timeout 60s -race ./...` in `sdks/go` — ok (18 new rows).
- TS SDK: `npx tsc --noEmit` + `npx vitest run` — 72/72 (10 new rows).
- Python SDK: `python -m pytest tests/test_client.py tests/test_async_client.py -q` — 122 passed (16 new rows).
- Java SDK: `mvn test` — 39 tests, 0 failures (9 new rows), BUILD SUCCESS.
- Hurl: `make contract-test-mock` — all files green incl. new `inputs.hurl`; red-check (mutated expectation → exit 4) executed.
- Frontend: full `npx vitest run` **1812/1812**; `npx tsc --noEmit` clean.
- Platform Go: `go test -timeout 120s ./pkg/types/ ./pkg/agent/` — ok; affected-package `-race` batch below in the gate run.
- `make -C sdks validate` + `make sdk-check` (validate tests + router parity) — green; `go test ./sdks/canary/mcp/` — ok.

## Next steps

- Review loop on the PR; then the delivery-pool dispatch on the branch and on main post-merge (walk-away row must stay green).
- Post-merge: close #1302 with the acceptance-criteria evidence table; update the 4b reserved comment to `landed`.
- #1304 owns: remaining session-surface `x-opencode-proxy` markers, the spec/SDK version bump + breaking-change note, getSession retyping.

## Files modified

- `sdks/go/services.go`, `sdks/go/types.go`, `sdks/go/input_requests_test.go` (new), `sdks/go/client_test.go` (superseded rows removed)
- `sdks/typescript/src/client.ts`, `sdks/typescript/src/types.ts`, `sdks/typescript/tests/input-requests.test.ts` (new)
- `sdks/python/llmsafespaces/client.py`, `sdks/python/llmsafespaces/async_client.py`, `sdks/python/llmsafespaces/types.py`, `sdks/python/llmsafespaces/__init__.py`, `sdks/python/tests/test_client.py`, `sdks/python/tests/test_async_client.py`, `sdks/python/tests/input_contract_fixtures.py` (new)
- `sdks/java/src/main/java/com/llmsafespaces/sdk/services/InputRequestsService.java` (new), `sdks/java/src/main/java/com/llmsafespaces/sdk/models/InputRequest.java` (new), `sdks/java/src/main/java/com/llmsafespaces/sdk/models/InputOption.java` (new), `sdks/java/src/main/java/com/llmsafespaces/sdk/models/ToolRef.java` (new), `sdks/java/src/main/java/com/llmsafespaces/sdk/models/InboxLateAnswerAccepted.java` (new), `sdks/java/src/main/java/com/llmsafespaces/sdk/models/Message.java` (nested copies removed), `sdks/java/src/main/java/com/llmsafespaces/sdk/LLMSafeSpacesClient.java` (wiring), `sdks/java/src/test/java/com/llmsafespaces/sdk/InputRequestsServiceTest.java` (new)
- `sdks/tests/contract/inputs.hurl` (new)
- `frontend/src/api/types.ts`, `frontend/src/api/contract.test.ts`, `frontend/src/api/contract-fixtures.json` (regenerated)
- `pkg/types/contract_test.go`, `pkg/agent/types.go`
- `worklogs/NNNN_2026-09-14_epic71-4b-sdk-cleanup.md` (this file)
