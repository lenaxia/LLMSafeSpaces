# Worklog: #1679 — enforce Option B: all orgs require OIDC; org creation requires complete SSO config in the same step

**Date:** 2026-10-15
**Session:** w27 charter deliverable 1 of 3 (#1679 → #1686 → #1685). Owner ruling (design 0065 §4, relayed on #1679): **Option B** — orgs cannot exist without OIDC; creation must REQUIRE complete SSO configuration in the same step; no two-step bootstrap, no waiting state.
**Status:** Complete

---

## Objective

Flip the merged gap pin `TestCreateOrg_NoSSORequirementGapPin` (which documented that `POST /api/v1/orgs` creates an immediately-active org with no SSO requirement anywhere) into the Option B contract — creation without a complete SSO config → 400 with zero side effects — and implement the enforcement end-to-end: API contract, OpenAPI, frontend creation form, design/docs sync.

---

## Work Completed

### (a) RED-first pin flip — `orgs_admin_create_test.go`

Commit f6bd4666 (before any implementation): `TestCreateOrg_NoSSORequirementGapPin` → `TestCreateOrg_RequiresSSOConfigAtCreation`. Asserts the NEW contract: no `sso` block → **400** + zero side effects (no org row, no membership row — new `membersLen()` helper counts membership rows across orgs, since the rollback/no-side-effect assertions care that NO row survives). Verified RED against main @ 48fd7542: compiles clean, fails 400-vs-201 (`expected 400, got 201` with the full created-org body in the log). Mutation surface for the orchestrator: removing the Option B gate (or weakening `validateInitialSSOConfig`) re-trips this pin AND the incomplete-SSO table below.

### (b) Enforcement — `pkg/types/orgs.go`, `handlers/orgs.go`, `app.go`

- `CreateOrgRequest` gains `SSO *UpsertSSOConfigRequest` (`json:"sso"`). No `binding:"required"` on the pointer: gin's validator recurses into the nested struct's tags and would hijack the error surface (see (c)); the handler owns the gate.
- `Create` order: platform-admin guard (403, unchanged — pinned by the payload-less NonAdmin test) → bind → slug/email normalize → **Option B gate** → owner resolve (404) → D8 pre-check (409) → slug check (409) → `CreateOrgWithAdmin` → **`ApplyConfigMutation(created.ID, *req.SSO, nil, nil)`** → `UpdateOrgStatus(active)` → 201.
- **Option B gate, zero-side-effect phase:** `sso` absent → 400 with `details["sso"]` guidance; present-but-incomplete → 400 via new `validateInitialSSOConfig` with dotted `sso.<field>` detail keys (discoveryUrl required+URL, clientId required, clientSecret required — the first-config rule: no stored secret exists for the "leave unchanged" path; groupRoleMapping roles ∈ {admin,member}); provisioner unwired → **503** (a deployment that cannot persist SSO configs refuses creation rather than producing the unsupported no-SSO org).
- **Same-step persistence + rollback:** the initial config is persisted immediately after the org row exists. On failure: best-effort rollback (`RemoveOrgMember` FIRST — `GetUserOrgID` does not filter soft-deleted orgs, so a surviving membership row would permanently 409-block the owner from any future org under the D8 unique index — then `SoftDeleteOrg`), 503 for "server key not configured" (parity with the SSO handler's own mutation mapping), else 500; error text states the org was not created. On `UpdateOrgStatus` failure the pre-existing partial-state behavior is kept (org remains pending_activation) — but now WITH its SSO config, so the no-SSO invariant is unaffected and the pending-org cleaner remains the reaper.
- Seam: `orgSSOProvisioner` interface (the `ApplyConfigMutation` subset) + `SetSSOProvisioner`, mirroring the existing `SetBilling` pattern; `app.go` wires `ssoSvc` onto `orgsHandler` in the existing SSO-construction branch (nil-safe: handler constructed earlier at :917, provisioner attached at :970 when the SSO service exists).

### (c) Nested binding-error keys — `binding_errors.go`

Discovered during (b)'s test run: the validator recursed into `UpsertSSOConfigRequest`'s own tags (`required,url`), failing the bind BEFORE the manual gate with raw Go-field keys (`details={"DiscoveryURL":"This field is required"}`) — unhighlightable client-side and inconsistent with the manual gate's keys. Fixed at the shared helper: `bindingErrorResponse` now derives a dotted JSON path from `ve.StructNamespace()` (`CreateOrgRequest.SSO.DiscoveryURL` → `sso.discoveryUrl`); top-level fields produce the identical key as before (degrades to the old `jsonFieldName` on any unwalkable namespace). Both validation layers now emit identical keys; the manual gate remains authoritative for fields the tags cannot express (clientSecret-at-creation, role mapping).

### (d) Companion pins (green, same file)

- `TestCreateOrg_WithCompleteSSO_PersistsConfigInSameStep` — 201; provisioner called exactly once, with the NEW org's ID (matched against the response `id`) and the payload verbatim (incl. `autoProvision:false` pointer passthrough — nil would default to true inside `ApplyConfigMutation`; groupRoleMapping; raw claimedDomains — normalization stays inside the service).
- `TestCreateOrg_IncompleteSSO_Rejected400NoSideEffects` — table of 5 arms (missing/malformed discoveryUrl, missing clientId, missing clientSecret, bad role mapping): each 400 + the `sso.<field>` details key + zero org/membership rows + provisioner never called.
- `TestCreateOrg_SSOPersistFails_RollsBackOrgCreation` — generic persist error → 500, "not created" error text, org AND membership rows gone, exactly one provision attempt.
- `TestCreateOrg_SSOPersistFails_ServerKeyMissing_503` — same rollback, 503 mapping.
- `TestCreateOrg_SSOProvisionerUnwired_Returns503` — nil provisioner → 503, zero rows, billing never invoked.
- All 13 pre-existing Create-path tests updated to carry complete `sso` payloads (via `withSSO` splicer / inline blocks); the two binding-failure tests (`MissingOwnerEmail`, `InvalidOwnerEmail`) and the NonAdmin 403 test deliberately keep payload-less bodies — they pin that binding and the admin guard precede the SSO gate. `setupOrgTestRouter` (orgs_test.go) wires the succeeding fake provisioner so the slug-validation family reaches 201; its two tests' payloads carry `sso`.

### (e) OpenAPI — `sdks/openapi.yaml`

`CreateOrgRequest.required` += `sso`; new `InitialOrgSsoConfig` schema (`allOf: [UpsertOrgSsoRequest]` + `required: [clientSecret]`) — precise "same shape but the secret is mandatory here" without duplicating the field list. `POST /orgs` description states the Option B contract (400 keys `sso.<field>`, same-request persistence, rollback); new `503` response. `make -C sdks validate` + `sdks/validate` tests + `TestOpenAPIRouterContract` all green.

### (f) Frontend — `api/orgs.ts`, `OrgSettingsTab.tsx`, tests

`CreateOrgRequest.sso: InitialSSOConfig` (= `UpsertSSOConfigRequest & { clientSecret: string }`). `CreateOrgForm` gains the OIDC section (discovery URL, client ID, client secret, claimed domains, auto-provision checkbox defaulting on — mirroring OrgSSOTab's vocabulary), client-side required-ness gates with specific messages, backend `sso.*` detail keys mapped to friendly labels, and a one-line rationale in the form ("Every organisation requires OIDC…"). Tests: existing create tests fill the SSO fields via a `fillCreateForm` helper; new pins — payload carries the full `sso` block; submit blocked until each required OIDC field is filled (3 progressive arms: discovery → clientId → secret, `mockCreate` never called); nested `sso.discoveryUrl` detail renders as "Discovery URL: Must be a valid URL". Full vitest suite green: 188 files / 2101 tests (was 2101 → 15 in this file, +2 new). tsc/eslint clean.

### (g) Design/docs sync

`design/0065 §4`: enforcement-gap bullets → "Enforced at creation (Option B, #1679)" + history bullet recording the pin-flip deliberately; the bootstrap chicken-and-egg recorded as resolved under Option B (platform admin supplies the IdP config inline; the resolved owner keeps password/passkey login). `docs/operator/oidc-sso.md`: "SSO is required at org creation" subsection under The model; invitation-path gap bullet updated to the transition-mechanism framing.

---

## Key Decisions

- **Manual validation, not binding tags, owns the gate** — nested validator errors produced raw Go-field keys; hoisting the check to the handler gives clean `sso.<field>` keys, expresses the creation-only clientSecret rule the shared type cannot, and keeps the 400 phase provably side-effect-free. The nested-tag recursion is still fixed (c) so the two layers agree on keys.
- **Rollback = RemoveOrgMember then SoftDeleteOrg** — ordering matters: `GetUserOrgID` does not filter soft-deleted orgs, so the membership row must go first or the owner is permanently blocked from future orgs. Both are already on the `orgStore` interface; no store changes needed.
- **SSO persists before `UpdateOrgStatus`** — the Option B invariant (org must never be reportable without SSO) outranks the pre-existing pending_activation partial-state documentation; that path now strands an org WITH its SSO config, which the cleaner may still reap (unchanged semantics).
- **503 (not 500) for the unwired/server-key-missing shapes** — parity with the SSO handler's `respondSSOMutationError`; lets operators distinguish "fix the deployment" from "transient DB failure".
- **Frontend collects only the required IdP fields + domains/auto-provision at creation** — group-role mapping stays on the org-admin SSO tab (post-creation edits are config updates, not bootstrap).

---

## Blockers

None.

---

## Tests Run (all measured on this branch, commands + counts)

- RED phase: `go test ./api/internal/handlers/ -run 'TestCreateOrg_RequiresSSOConfigAtCreation' -v` → `--- FAIL ... expected 400, got 201` (compiles, true behavioral red) at commit f6bd4666.
- GREEN phase: `-run 'TestCreateOrg' -v` → **25 top-level PASS** (23 CreateOrg family incl. 5 new pins + 2 TestCreateOrgConfig), 0 FAIL; table subtests nested under their parents.
- `go test ./api/internal/handlers/` (FULL package) → ok, 80.1s.
- `go test ./api/internal/server/ ./api/internal/services/sso/ ./pkg/types/ ./api/internal/app/` → all ok.
- `make -C sdks validate` + `(cd sdks/validate && go test ./...)` + `TestOpenAPIRouterContract` → ok.
- `go build ./...` → clean. gofmt/goimports/golangci-lint via pre-commit hooks (0 issues).
- Frontend: `npx vitest run src/components/settings/OrgSettingsTab.test.tsx` → 15/15 (13 existing updated + 2 new). `npx vitest run` (full) → **188 files / 2101 tests, all pass**. `npx tsc -b` + eslint on touched files → clean.

---

## Review round 1 (CHANGES_REQUESTED → remediated)

Findings + dispositions (all addressed; commit follows 9c91959f):

1. **Alert-grade rollback-failure logging + pinned log assertion** (orchestrator addition on the #1679 issue thread — arrived after my plan message via the issue channel; disclosed that I picked it up from the review): `orgsLogger` extended with `Error(msg, err, ...kv)` (matches webHookLogger / `*logger.Logger` — my first shape `(msg, ...any)` did not compile against the real logger; fixed to the repo's established signature); `rollbackOrgCreation` now logs `ORPHAN ORG: … manual cleanup required` at Error with orgID/ownerID/cause; still deliberately NOT transactional per the ruling. Pinned by `TestCreateOrg_RollbackItselfFails_LogsAlertGrade` (2 arms: membership-removal failure, soft-delete failure; `alertCaptureLogger`; asserts the 500 still surfaces, the specific alert fires, and the orphan bookkeeping matches per arm) + `TestCreateOrg_RollbackSucceeds_NoAlertLog` (quiet side: successful rollback emits zero Error logs). Mock store gained `removeMemberErr`/`softDeleteErr` knobs.
2. **Production observability gap (reviewer robustness finding)**: `orgsHandler.SetLogger(log)` was never called in app.go — rollback alerts would have been silent in production. Wired at construction with a comment.
3. **BREAKING CHANGE changelog flag** (orchestrator addition 2): `CHANGELOG.md` `[Unreleased]` gained the org-provisioning BREAKING entry, including the SDK audit (no hand-written SDK models `POST /orgs` — their only `createOrg`-named methods are agent-role helpers on `/orgs/{id}/agent-roles`; grep-verified across sdks/{typescript,python,go,java}; openapi.yaml is the canonical contract, updated in lockstep).
4. **Progressive disclosure** (orchestrator addition 2, "not a wall"): the SSO section is now collapsed behind an "Identity provider (OIDC) — required" toggle with a live status hint ("not configured" red / "configured" green, visible while collapsed); a failed submit gate auto-opens the section.
5. **E2E coverage** (reviewer missing-cases): new `frontend/tests/e2e/org-creation-sso.spec.ts` — 3 Playwright tests against the real form with a mocked backend: (a) happy path asserts the POSTed body carries the full `sso` block and the form closes on 201; (b) unconfigured IdP blocks submit with zero POSTs and auto-opens the section; (c) backend 400 `sso.discoveryUrl` detail renders as "Discovery URL: Must be a valid URL". API adjustments discovered en route: this Playwright version exposes `getByPlaceholder` (not `getByPlaceholderText`), and the status assertion needs `{ exact: true }` (substring matched both states).

Tests run post-remediation: `TestCreateOrg*` 27 top-level PASS / 0 FAIL; full handlers package ok (79s); app package ok; vitest OrgSettingsTab 15/15; settings dir suite ok; tsc/eslint clean; Playwright org-creation-sso 3/3.

---

## Next Steps (updated r1)

- r1 remediation pushed; awaiting review round 2.
- After merge: #1686 (disk-space banner) then #1685 (busy-15-min nudge), one PR each, per charter order.
