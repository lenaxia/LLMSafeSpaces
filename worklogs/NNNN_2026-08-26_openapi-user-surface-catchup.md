# Worklog: OpenAPI spec catch-up — Epic 64 workflows/triggers, passkeys, history pagination

**Date:** 2026-08-26
**Session:** P1 of epic #1032 — document the user-facing surfaces that drifted out of sdks/openapi.yaml (#1039, #1040, #1041) and extend the contract-test fixture to cover them (partial #1043)
**Status:** Complete

---

## Objective

Close the spec↔router gap for the three highest-traffic undocumented user-facing areas: message-history pagination (frontend `messages.ts` depends on it), the entire Epic 64 workflows/triggers/runs/hooks surface (shipped in SDKs ahead of the spec), and the Epic 59 passkey surface.

---

## Work Completed

### #1039 — history pagination documented
- `GET /workspaces/{id}/sessions/{sessionId}/message` now documents `limit` (int, 1..200, default 50, capped server-side), `before` (cursor), and the `X-Next-Cursor` response header (absent = end). Values copied from `parseHistoryLimit`/`historyPageMaxLimit`/`historyPageDefaultLimit` (`proxy_handlers.go:506-512,637-660`), not from memory.

### #1040 — Epic 64 surface documented (30 operations)
- `/me/workflows` CRUD + runs (POST 202 + GET fixed-page-20 list), `/me/runs/{runId}` (+`/nodes`, `/cancel`), `/me/triggers` CRUD + `/fires` (page 50) + `/rotate-secret` (one-time `whsec_` secret), org-scope mirrors (`/orgs/{id}/workflows{,/{workflowId},/{workflowId}/runs}`, `/orgs/{id}/triggers{,/{triggerId}}`), workspace views (`/runs/active`, `/session-origins`), and the public HMAC-authed webhook receiver `/hooks/{webhookId}` (documented explicitly — external callers need it; signature headers, dedup, rate limits, 202/200/401/403/409/429 shapes).
- Schemas (`Workflow`, `WorkflowCreateRequest`, `WorkflowUpdateRequest`, `WorkflowRun`, `WorkflowNodeRun`, `Trigger`, `TriggerCreateRequest`, `TriggerUpdateRequest`, `TriggerFire`, `SessionOrigin`) transcribed from `pkg/types/workflows.go` + handler wrappers (`{"workflows":[...]}`, `{"runs":[...]}`, `{"nodes":[...]}`, `{"triggers":[...]}`, `{"fires":[...]}`, `{"origins":[...]}`, webhook-trigger create's `{trigger, webhookUrl}` variant, 409 `Retry-After`).
- Envelope facts verified in handler code, including the non-obvious ones: run list is hardcoded page 20; fires page 50; org scope has no GET runs; webhook create response differs by sourceType.

### #1041 — passkey surface documented (11 operations)
- Ceremonies: `/auth/passkey/register/{begin,finish}` (201→200 + recovery codes one-time), `/login/{begin,finish}` (token+user+cookie), `/recover` (code ≥8 chars, `mustEnrollPasskey`).
- Account: `/account/passkeys` list (public-metadata-only `PasskeyCredential`), delete (409 last-passkey guard), `recovery-codes/regenerate`, `enroll/{begin,finish}`.
- `AuthConfig` schema updated with `passkeyEnabled` (required) + `passkeyDefaultSignup` — verified against the inline handler (`router.go:875-904`) and its settings test.
- WebAuthn payloads typed as opaque objects with `description` notes (flat options shape, no `publicKey` wrapper — pinned by `options_shape_contract_test.go`); no CBOR modeling.

### Contract fixture extended (partial #1043)
- `newContractFixture` now wires zero-value stubs for `UserWorkflowsHandler`, `OrgWorkflowsHandler`, `UserTriggersHandler`, `OrgTriggersHandler`, `WebhookReceiverHandler`, `PasskeyHandler` — the newly documented routes are now checked impl-side on every run, so future drift in these areas fails the contract test (previously they were invisible to it).
- No allowlist changes needed: every new spec route balances against a registered route.

### Housekeeping
- Spec version 0.5.4 → 0.6.0 (additive user-facing surface).
- New tags: `workflows`, `triggers`, `passkeys`. New reusable params: `OrgId`, `WorkflowId`, `RunId`, `TriggerId`.

---

## Key Decisions

1. **Webhook receiver documented (not implOnly-allowlisted)** — external systems are the intended callers; a spec entry describing the HMAC contract is strictly more useful than hiding it. The `security: []` override makes the non-JWT auth model explicit.
2. **Opaque WebAuthn objects with described top-level keys** rather than full JSON-schema of WebAuthn types — the platform forwards them verbatim; modeling the browser API would couple our spec to go-webauthn's serialization.
3. **Zero-value handler stubs in the fixture** for route presence (same pattern as the existing MCP handlers) — behavioral coverage for these routes stays in the per-handler suites; the contract test's job is presence/method drift.
4. **Hurl contract cases deferred to #1044** — the Hurl suite is non-blocking today; adding cases lands together with making it blocking so they're enforced from day one.
5. Fixed two YAML quoting bugs found by the contract test itself (`Retry-After: 30` inside unquoted description scalars).

---

## Assumptions validated

- Org-scope workflow param is `{workflowId}` while user scope is `{id}` — verified in `registerWorkflowRoutes` (router.go:1771-1835) before writing paths.
- `ssoProviders` never set by the /auth/config handler — kept (pre-existing, informational) rather than removed; removal belongs to #1049's accuracy pass.

---

## Blockers

None.

---

## Tests Run

- `go test -timeout 300s ./api/internal/server/` — ok (contract test green BOTH directions: 0 specOnly, 0 implOnly for the new areas; whole package green)
- `make openapi-validate` — ✓ spec valid (structure, $refs, operationId uniqueness)
- `go test ./...` in `sdks/validate` — ok (expectedPaths superset check + completeness)

---

## Next Steps

1. PR this branch (#1039 #1040 #1041 + partial #1043).
2. #1042 remaining spec areas (image factory, org policies/prompt/agent-roles/invitations/credentials/SSO, platform-info, queue retry, admin surface) + finish #1043 (wire ALL handlers, shrink allowlists).
3. #1044 make Hurl blocking + add workflow/passkey cases.
4. #1045 remove expectedPaths; #1049 version/claims accuracy pass.
5. #1047 SDK pagination params (now unblocked by #1039); #1046 SDK MCP-server CRUD.

---

## Files Modified

- `sdks/openapi.yaml` — pagination params, 41 new operations (30 Epic 64 + 11 passkey), 15 new schemas, AuthConfig passkey fields, 3 tags, 5 params, version 0.6.0
- `api/internal/server/router_openapi_contract_test.go` — fixture wires Epic 64 + passkey handlers
