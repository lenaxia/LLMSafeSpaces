# Worklog: OpenAPI org/admin surface + full contract-fixture wiring

**Date:** 2026-08-26
**Session:** P1/P2 of epic #1032 — document the remaining org + admin + image-factory surface (#1042) and finish #1043: the contract-test fixture now wires EVERY production handler
**Status:** Complete

---

## Objective

Make the router↔spec contract test authoritative: no route may hide behind an unwired fixture handler, and every user/frontend-facing route is documented.

---

## Work Completed

### #1043 (complete) — fixture wires every handler
- `newContractFixture` now stubs all ~40 `RouterConfig` handlers (was ~10). The impl→spec diff previously missed ~84 routes; wiring surfaced exactly those 84, all triaged below. Zero allowlist entries added for coverage gaps — only genuinely-internal routes (rationale in-file).

### #1042 — remaining surface documented (~80 operations)
- **Org SSO (9):** public domains/start/callback (302 + cookie semantics, error reasons), org config CRUD, domain verify (PascalCase response keys documented as-is), token rotate.
- **Invitations (8):** org list/create (bare arrays, 429 limit body), revoke, resend (replacement row), verify-user (422 no_account_for_email), public token read, accept (403 email-mismatch, 410 expired), decline.
- **Org credentials (8):** CRUD + probe + auto-apply (no-body POST, PascalCase rule list, idempotent 204 delete, `bindWarning` partial-success).
- **Org policies (3):** raw-JSON-scalar body documented explicitly; 402 FeatureGuard; 10 policy keys with value types.
- **Org prompt (2) + agent-roles (5) + audit (1):** member-read prompt with allowUserPrompt; 409 role-in-use; paginated audit.
- **Image factory (17):** consumer catalog/configs CRUD/PATCH-rename/org+platform-scoped creates + full admin portal (platform-config, bases upsert w/ 409 default-move race, extensions publish/retire, known-failures).
- **Sessions/workspaces (3):** queue retry (documented — removed from implOnly; the frontend calls it), bulk agent reload (NDJSON stream), dev-preview-bootstrap ×2 (302 + one-time token).
- **Admin (17):** platform-info, cross-org audit, force-abort, platform-admin orgs/users list+suspend (409 last-admin + force), email test (429/502), usage report, billing status (snake_case keys documented as-is) + DLQ list/retry/discard, relay fleet setup/status/creds/deploy/rotate/pause/resume.
- **Stripe webhook (1):** public, Stripe-Signature header, handled event types.

### Spec bugs fixed along the way (audit findings)
- `CreateProviderCredentialRequest`: added `modelAllowlist`/`modelContextLimits`/`modelOutputLimits` (present in code, missing from spec).
- `ProviderCredential`: added `orgId` + `bindWarning`.
- `CreateAgentRoleRequest`: `description` was wrongly required.
- Name collision: new audit-log schema is `AuditLogEntry` (the pre-existing `AuditEntry` is the secrets-audit shape — different fields).

### Allowlist end state (implOnly, each with rationale)
metrics, pprof ×9, `/health` alias, `/events` SSE, internal org status (X-Internal-Token), pod-bootstrap (TokenReview), image-factory build callback (per-build token), dev-preview wildcard (path-shape mismatch; spec documents `{port}` form), org core CRUD ×13 (pre-existing deliberate exclusion — see Next Steps).

---

## Key Decisions

1. **Admin surface documented rather than excluded** — the validator's old comment called admin "operator surfaces, not SDK-facing", but the spec already documented admin settings/agent-roles/prompt/provider-credentials/mcp-servers, and the frontend consumes platform-info/relay/admin-audit/platform-admin. Completeness beat the stale exclusion note; #1045 will reconcile the validator.
2. **Internal endpoints stay undocumented with rationale** — service-to-service auth (internal token, TokenReview, per-build token) is not client surface.
3. **Org core CRUD (13 routes) left allowlisted** — pre-existing deliberate exclusion, out of #1042's enumerated scope; flagged for an explicit follow-up decision (document vs. formalize exclusion).
4. Documented wire quirks verbatim (PascalCase SSO/auto-apply keys, snake_case billing keys, bare-array lists, `{status:"ok"}` acks) rather than idealizing them — the spec describes the API as built.

## Incidents (self-inflicted, fixed)

- A `head -N`-based file rebuild dropped the entire `components:` section (parameters/responses/schemas) — detected via the contract test failing to find AuthConfig, restored from HEAD via git. Lesson: rebuild files by anchored `sed` ranges, not line counts.
- Two duplicate schema definitions (PaginationMetadata, AuditEntry) from the same rebuild — caught by the validator's YAML duplicate-key check; AuditEntry collision resolved by renaming to AuditLogEntry (different semantics).

---

## Blockers

None.

---

## Tests Run

- `TestOpenAPIRouterContract` green BOTH directions with ALL handlers wired (84 undocumented → 0; no new coverage allowlist entries)
- `make openapi-validate` ✓; `sdks/validate` suite ✓
- Full `api/internal/server` suite ✓

---

## Next Steps

1. PR this branch (closes #1042, closes #1043).
2. #1044 make Hurl contract tests blocking + add workflow/passkey/pagination cases; #1045 remove `expectedPaths` (contract test is now the superset gate — verify equivalence first); #1049 PACKAGES.md accuracy (path count changed again).
3. #1047 SDK pagination params; #1046 SDK MCP-server CRUD.
4. Decide org-core-CRUD spec inclusion (new small issue if kept excluded).

---

## Files Modified

- `sdks/openapi.yaml` — ~80 new operations, 20+ schemas, 3 existing-schema fixes, spec 0.6.0
- `api/internal/server/router_openapi_contract_test.go` — fixture wires all handlers; implOnly allowlist: −queue-retry, +3 internal entries, +dev-preview wildcard rationale
