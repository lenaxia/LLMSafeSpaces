# Worklog 1107 — w19 auth-flow completeness audit (registration / auth / OAuth)

Branch: `audit/auth-flows` @ f9a74a19 (v0.35.0) + per-fix branches. Charter: audit-first map
of every user-facing registration/auth/OAuth flow; small fixes as PRs, big gaps as issues.

## Deliverables

- **#1649** — tracking issue with the full completeness report (every flow EXISTS/MISSING/PARTIAL + file:line + tests + docs + UNVERIFIED list).
- **#1650** — gap A (HIGH): `auth.registrationEnabled` (static config + runtime setting) advertised via `/auth/config` but never enforced on `/auth/register` or passkey signup.
- **#1651** — gap B (HIGH): no frontend for email verification / password reset; emailed links target `/verify-email` + `/reset-password` SPA routes that don't exist → email-wired instances lock out new password registrants (login gated on email_verified) and nobody can self-reset.
- **#1652** — LOW follow-ups: passkey LoginFinish issuance-side checks, per-route rate caps on register/reset/verify, `/auth/lookup` body oracle, OIDC nonce note.
- **PR #1653** (`w19/fix-register-enumeration`) — gap C: duplicate-register 409 leaked "user email already exists" on the wire; fixed with generic-message APIError, red-first service pin + new wire-level pin + OpenAPI 409 entry.
- **PR #1654** (`w19/remove-dead-changepassword`) — gap D1: dead `secretsApi.changePassword`/`rotateKey` client methods (routes removed in d11b98ef) deleted.

## Audit method (what was actually verified)

Static map of the full auth surface first (router.go authGroup + registerAuthRoutes, services/auth,
services/sso, handlers password_reset/email_verify/passkey/org_sso/login_discovery/auth_unlock/
invitations, frontend router.tsx + LoginPage/RegisterPage/SSOStartPage + api clients, sdks/openapi.yaml,
docs/getting-started + docs/operator/oidc-sso.md), then targeted reads of each flow's security-critical
paths (state/PKCE/email_verified, token hashing, lockout keying, enumeration pads, single-use tokens,
TOCTOU consume guards, last-admin and last-credential guards).

Test runs (counts verified, not assumed):
- `services/auth` — 168 PASS / 9.1s (real bcrypt).
- `services/sso`, `services/passkey` — ok.
- `handlers` full package — ok / 81s.
- `server` full package — ok (auth-scope `-run` filter: 56 RUN / 0 FAIL; router tests mock the
  auth service — this is exactly how gap C hid: the generic-error pin mocks the service return).
- Frontend auth pages (LoginPage ×2, RegisterPage ×2, SSOStartPage) — 36/36 vitest.
- Red captured for the fix: `TestRegister_DuplicateEmail` NotContains "already" →
  `"conflict: user email already exists (registration failed)"` pre-fix.

## Security invariants — explicit results

- OAuth email-matching takeover: BLOCKED (`sso.go:586-588` requires `email_verified`); org-swap
  replay blocked (orgID bound in signed state cookie); F11 redirect-base hardening present.
- Tokens stored raw: NO — hashes only (RevokeToken dual hash+jti keys; trackUserSession hash entries).
- Auth endpoints rate-limited: login/passkey-finish/passkey-recover per-route + global IP limiter;
  register/reset/verify global-only (filed LOW #1652).
- User enumeration: login/reset/resend uniform + bcrypt timing pads; register duplicate LEAKED (fixed
  PR #1653); lookup body oracle noted (#1652 D4).

## Incident log (in-flight)

1. **/tmp disk exhaustion (100%) during full `server` package compile** — k8s fake-clientset parallel
   compile blew /tmp; `go test` failed with "no space left on device". Remedied under the standing
   permission: removed the two `/tmp/go-build*` dirs (333M + 767M) + `go clean -cache`; /workspace
   94% → 77%. Re-ran the full server package: green. No sibling worktrees touched.
2. **Suspicious-fast test timings investigated** — server auth tests at 0.08s looked vacuous; verified
   56 RUN entries with -v and confirmed router tests intentionally mock the auth service (real logic
   coverage in services/auth, 9-26s real-bcrypt runs). Not a false green — but this mock seam is how
   gap C escaped detection; the new wire-level test closes it.

## UNVERIFIED (disclosed in #1649; needs live instance)

Live SES delivery + link click-through; live IdP authorize→callback; wildcard-subdomain cookie
behavior; Turnstile vs Cloudflare live API.

## Status

- Issues #1649/#1650/#1651/#1652 filed. PRs #1653/#1654 pushed, remote-tree verified (origin greps;
  main still shows the leak, branches show the fixes), AI review in flight — polling to verdict.
