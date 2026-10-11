# Worklog: #1669 — phase 1: orgContext on /auth/config + org-scoped LoginPage button (design 0064, option C)

Session: w24, branch feat/1669-org-context-phase1 from origin/main @ 653ea772
(main had advanced past the design branch's b97dd185 base — #1668 merged).

## Scope (design 0064 §7 phase 1 — released per §8 survival rule)

Additive org-scoped login surface; /auth/lookup, /auth/sso/domains, SSO
start/callback, state cookie, invitations all untouched.

Backend:
- `pkg/types/auth.go`: `AuthConfigOrgContext {Slug, Name,omitempty}` +
  `AuthConfig.OrgContext` (`omitempty`, additive).
- `services/sso`: `ResolveSSOOrg` — org by slug, returned ONLY when org
  exists AND has SSO config; (nil,nil) for both miss kinds (indistinguishable);
  error = real store failure only.
- `handlers/org_context.go`: `OrgSlugFromHost` (exact one-label-under-base,
  port-stripped, case-insensitive; "" for exact-base/deeper/wrong-suffix/
  empty-base) + `SSOHandler.OrgContextForHost` (fail-soft nil on every miss;
  nil-safe on zero-value handler — the openapi contract-test wiring).
- `server/router.go`: `RouterConfig.OrgSubdomainBaseDomain`;
  registerAuthRoutes threads it; /config closure adds OrgContext.
- `app/app.go`: wires cfg.OrgSubdomainRouting.BaseDomain.

Frontend:
- `api/types.ts`: `orgContext?: { slug; name? } | null`.
- `LoginPage.tsx`: host-org SSO button first-class ABOVE the forms when
  orgContext present (generic label when name absent — C-strict-ready);
  §6.2 suppression: email-matched verified-domain button suppressed whenever
  orgContext present (cross-org AND same-org — exactly one org button on a
  subdomain). Root-page behavior unchanged.

Contract: openapi AuthConfig schema += optional orgContext; types contract
fixtures += AuthConfigNoOrgContext; frontend contract test both shapes.

## TDD record (red-first, all red runs observed before impl)

- handlers/org_context_test.go RED (undefined symbols) → GREEN.
- server/router_auth_config_org_context_test.go RED (unknown RouterConfig
  field; one fixture bug fixed: zero-value services panic — mock the services
  like the settings fixture does) → GREEN.
- LoginPage.test.tsx +5 cases RED (4 failed / 18 passed) → GREEN 22/22.

## Verification (verified runs, this tree)

- handlers full: ok 76.9s. sso service: ok. pkg/types: ok.
- server full: ok — 580 RUN, 0 FAIL/SKIP (count=1).
- frontend full: 2061/2061 pass; tsc --noEmit clean.
- Enumeration re-pins (unchanged files, green in full runs): login_discovery
  + org_sso domains tests.
- gofmt clean after fix (two files re-formatted).

## Honest scope

Router-tier Host-header tests stand in for live wildcard ingress (per design
§10 — no live-ingress claim). The audit #1649 UNVERIFIED item (cross-subdomain
cookie behavior) remains untouched by this phase — orgContext is display-only.

## Status

Phase 1 complete on this branch; phase 2 (docs + dead-end copy) HELD pending
owner §8 ruling per mandate (no implementation beyond phase 1 without ruling).
