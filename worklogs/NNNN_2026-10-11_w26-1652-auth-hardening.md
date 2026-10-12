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

---

## Post-push addendum: follow-ups from the ruling-relay window

- Owner ruling "All orgs require OIDC" (design 0065 §4) enforcement follow-up filed as **#1679** (org-creation/SSO gating mechanism; owner decision on bootstrap semantics; this PR's gap pin must be flipped deliberately when implemented).
- Anomaly noted: PR #1677's `pull_request: opened` event produced ZERO workflow runs (no CI, no PR Review — confirmed via actions API: 0 runs for head SHA 06601982). This addendum commit exists to re-fire `synchronize`; if workflows still do not run, escalate to the orchestrator (event delivery problem outside the workspace).

- Update: the rulings record MERGED as PR #1678 (3 review rounds — the citable-record discipline caught a never-filed follow-up, now issue #1679, plus cross-record tense fixes). Design 0065 §4 cites #1679; this PR's gap pin is referenced from it as "pin pending: PR #1677".
- The zero-workflow-runs anomaly on this PR persists (both head SHAs 06601982 and 7e22fdf8: 0 runs via actions API, while sibling PRs' events flow). CI manually dispatched on the branch per ci.yml's workflow_dispatch; this commit is a third synchronize probe — if it also produces no runs the PR needs recreating (GitHub event drop specific to this PR's opened/synchronize webhooks).

---

## Import-cycle fix (found by CI on the recreated PR; PR #1677 → #1680)

The dispatched CI + #1680's review exposed what local scoped runs missed: passkey.go importing `services/auth` (for `ErrEmailNotVerified`) is a **test-time import cycle** — services/auth's tests import handlers, so `go vet ./...` / full-suite CI fails with "import cycle not allowed in test" at services/auth [setup failed]. Fix: the message is now a local const `verifyEmailMessage` in passkey.go (documented as mirroring auth.ErrEmailNotVerified), and `TestLoginFinish_UnverifiedEmail_Rejected` pins parity against the canonical `authsvc.ErrEmailNotVerified` (test-only import — cycle-safe). Verified: `go vet ./...` clean; services/auth full package ok (8.9s); handlers full ok (74.6s).

- Event-delivery incident trail complete: repo-wide Actions silence 23:54Z–00:33Z (zero runs of any event kind, verified via API); resumed with an issue_comment run at 00:32:51Z. This PR (#1682, branch r2) was opened inside the silence window — this nudge re-fires synchronize so CI + PR Review run against b3a07f8a + this addendum.
