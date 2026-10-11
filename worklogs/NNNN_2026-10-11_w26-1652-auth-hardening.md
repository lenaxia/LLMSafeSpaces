# Worklog: #1652 — auth-audit low-severity hardening (passkey issuance gates, per-route caps, D4 assessment) + owner-ruling pins

**Date:** 2026-10-11
**Session:** w26 deliverable 3 of 3 — the #1652 low-severity items (passkey LoginFinish issuance checks, per-route rate caps, /auth/lookup D4 body-oracle assessment) plus the scope added mid-flight by the owner rulings relayed on PR #1676 (org-creation SSO-requirement gap pin).
**Status:** Complete

---

## Objective

Close #1652 items 1–3 red-first: (a) mirror the password-login account-state gates at passkey token issuance, (b) per-route rate caps on /auth/register, /auth/password-reset/*, /auth/verify-email/* following the existing PerRouteRateLimit pattern, (c) the D4 body-oracle mitigation assessment (accepted-risk vs uniform-response), and record the no-SSO-org enforcement gap the §4 ruling ("All orgs require OIDC") exposes.

---

## Work Completed

### (a) Passkey issuance-side account-state gates — `passkey.go`

`rejectIssuanceForAccountState` helper applies the password-login gates (suspended → 401 "account suspended"; inactive → masked 401; unverified email → 403 + `emailVerified:false` in the exact `auth.ErrEmailNotVerified` shape, message taken from the auth package's exported error for parity) before `IssueTokenAndUnlockDEK`, called from **both** LoginFinish and Recover (a recovery code is the same class of credential presentation minting the same session token — Recover had the identical gap; disclosed as a deliberate scope call). RED-first: 4 new tests failed pre-implementation (tokens were issued for suspended/inactive/unverified); green after. `fakePasskeyAuth` gained a `calls` counter so the pins assert issuance NEVER runs, not just that no token appears in the body. 3 legacy fixtures modernized to healthy users (Active/Status/EmailVerified) — they predate the gates and would otherwise fail; Register() creates Active users so the fixtures now match reality.

### (b) Per-route rate caps — `router.go` DefaultRouterConfig

Five new entries following the G41/login/recover comment pattern: `/auth/register` 10/min/burst-5 (DB-write + email per call; account-spam vector with Turnstile off), `/auth/password-reset/request` 5/5 (token row + email spray), `/auth/password-reset/confirm` 5/5 (bearer-token verification), `/auth/verify-email` 5/5 (single-use bearer token), `/auth/verify-email/resend` 5/5 (email spray). RED-first via `router_1652_rate_limit_test.go` (table-driven, G41 pattern: DefaultRouterConfig asserted to CARRY each entry, tightened locally to 3, 5 fired, 3 pass / 2 × 429; /register exercised through the real handler chain — always registered — the four conditional routes via exact-path stubs). All five subtests failed pre-implementation on the missing-config assertion.

### (c) D4 /auth/lookup body-oracle assessment — `docs/operator/oidc-sso.md`

Security-controls row + "Accepted risk" assessment: bounded exposure (no session material; personal-only users indistinguishable from unknown; SSO-org slugs derivable via the accepted `start` oracle; verified domains already public); **uniform-response rejected** (it deletes the email→org routing that IS the endpoint's function — the existing-member journey); the issue's middle option (generic redirect for no-SSO orgs) **not implemented** (adds a new SSO-ness oracle dimension for a cosmetic gain); standing mitigations (global 100/min/IP, design-0064 posture); revisit trigger = observed spray → per-route cap as the designed cheap lever.

### (d) Owner-ruling scope: org-creation SSO gap pin — `orgs_admin_create_test.go`

Per the §4 ruling ("All orgs require OIDC"), investigated enforcement: org creation is platform-admin-only (`isPlatformAdmin` gate) with **no SSO requirement anywhere**, and a created org is immediately active/usable. Added `TestCreateOrg_NoSSORequirementGapPin` documenting + locking current behavior with the ruling context and the bootstrap chicken-and-egg (first owner must be a password user — SSO users are provisioned by the org's own IdP, which can only exist after the org). Enforcement mechanism (creation gate vs not-usable-until-SSO state) needs an owner decision on bootstrap-window semantics — flagged as follow-up in the report, NOT smuggled in here (both shapes are breaking changes to platform-admin/invitation surfaces).

---

## Key Decisions

- **Recover gated alongside LoginFinish** — same issuance pattern, same file, same audit class; disclosed in the PR body for reviewer trimming.
- **Inactive → masked 401 "passkey login failed"** — mirrors password login's masking of inactive; the assertion already proved credential possession, but account state fails login.
- **Verify-email 403 reuses `auth.ErrEmailNotVerified`'s message via the exported var** — exact parity with /auth/login's mapping so frontend verify-email UX routing behaves identically.
- **Gap pin instead of enforcement fix** — the ruling's enforcement shapes need an owner decision (bootstrap semantics); a pin + follow-up flag is the honest scoped move inside a hardening PR.

---

## Blockers

None. Follow-up flagged: no-SSO-org enforcement mechanism (owner decision).

---

## Tests Run

- RED observed pre-implementation: passkey 4/4 fail (tokens issued); rate-limit 5/5 subtests fail (missing DefaultRouterConfig entries).
- `go test -count=1 ./api/internal/handlers/` — ok (78s full package, vet on)
- `go test -count=1 -v ./api/internal/server/` — ok, **586 RUN** (main baseline 580 + my 1 parent + 5 subtests)
- Targeted: `TestLoginFinish_*|TestRecover_*|TestRegisterFinish_` ok; `TestRouter_1652` ok (all 5 subtests); `TestCreateOrg_*` ok (incl. gap pin)
- gofmt/goimports/golangci-lint via pre-commit (scoped, new-from-merge-base)

---

## Next Steps

- Poll AI review on the #1652 PR to verdict; report merge-readiness (never self-merge).
- design 0065 rulings update rides #1676 (separate branch) per the orchestrator's relay.

---

## Files Modified

- api/internal/handlers/passkey.go
- api/internal/handlers/passkey_test.go
- api/internal/server/router.go
- api/internal/server/router_1652_rate_limit_test.go (new)
- api/internal/handlers/orgs_admin_create_test.go (gap pin)
- docs/operator/oidc-sso.md (D4 assessment; note — this branch is off main, D1's other oidc-sso edits ride #1676; the two edits touch different sections and merge cleanly)
- worklogs/NNNN_2026-10-11_w26-1652-auth-hardening.md (this file)
- COORDINATE.md (claim status)
