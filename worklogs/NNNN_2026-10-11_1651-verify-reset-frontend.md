# Worklog 1113 — #1651 email-verification + password-reset frontend (worker w23)

Branch: `feat/1651-verify-reset-frontend` (from main @ b97dd185 = v0.36.0)
Issue: #1651 (audit #1649 gap B, HIGH)

## Design note (posted before implementation)

### Backend contract (verified by reading handlers, not assumed)

| Endpoint | Request | Responses |
|---|---|---|
| `POST /auth/verify-email` | `{token}` | 200 `{verified:true}`; 400 missing token; 404 not found / wrong kind; 410 `"token expired"`; 410 `"token already used"` |
| `POST /auth/verify-email/resend` | `{email}` | **202 always** (enumeration-safe; 400 only on malformed email) |
| `POST /auth/password-reset/request` | `{email}` | **202 always** (only sends to existing + verified mailboxes) |
| `POST /auth/password-reset/confirm` | `{token, newPassword≥8}` | 200 `{reset:true}`; 400; 404; 410 expired/used |

Email links carry the token in `?token=` (`email/service.go` buildLink). The 410 body error strings distinguish expired vs already-used.

### Key design decisions

1. **Routes live OUTSIDE `GuestOnly`** (alongside `/invitations/:token`, `/sso/:orgSlug`).
   Register issues a session cookie even when `emailVerified=false`, so a user
   clicking an emailed link while nominally "logged in" must not be bounced to
   `/chat` by `GuestOnly` — that would re-create the lockout the page fixes.

2. **POST-on-mount interstitials, never consuming GETs** (US-49.9 scanner
   invariant the backend emails already encode). Both pages read `?token=`,
   POST once on mount, render the result state.

3. **URL hygiene**: after a *terminal* token result (success / used / expired /
   not-found) the page strips `?token=` via `history.replaceState` (same pattern
   as LoginPage's param cleanup) so a refresh cannot re-POST a single-use token
   and land on a confusing 410. `?email=` is kept for resend prefill.

4. **Enumeration-safety UX contract**:
   - request-reset and resend-verification NEVER differ in copy between
     "exists" and "doesn't exist". Both show a single neutral confirmation
     ("If an account exists for that email, a reset link has been sent." /
     "If that email needs verification, a new link has been sent.").
   - Errors surfaced are only transport-level (429 rate-limited, network) —
     never account-existence.
   - The login 403 `emailVerified:false` branch is NOT an enumeration vector
     (backend comment: reaching it proves possession of the password), so there
     we may name the reason and prefill the email.

5. **Redirect chains**:
   - `verify-email` success → "Continue to sign in" → `/login` (+`return_to`
     when threaded through from register).
   - register with `user.emailVerified === false` → check-your-email card
     (replaces the blind redirect), carrying "resend / verify" →
     `/verify-email?email=<registered>&return_to=<preserved>`, and a secondary
     "Continue" honoring return_to (the register session is valid — only the
     NEXT login is gated).
   - reset-confirm success → `/login?reset=success`; LoginPage renders a
     "Password updated — sign in with your new password." notice.
   - expired/used reset → offer "request a new link" (request mode) and login.

6. **Login page**: "Forgot your password?" entry (password mode only, next to
   the sign-in button); 403 `email_not_verified` mapped to an actionable
   message + resend link (plain `<a href="/verify-email?email=…">` hard-nav,
   consistent with existing auth-page navigation and router-context-free).

7. **Style**: reuse `AuthCard` shell (`min-h-dvh`, token-based dark mode),
   existing `Button`/`Input`/`Spinner`. No new `h-screen` (#1648 lesson).

### State machines

`/verify-email`: `missing-token → verifying → verified | expired | already-used | not-found | error`
(+ resend sub-state with neutral confirmation). `?email=` prefills resend.

`/reset-password`: **request mode** (no token): `form → sent(neutral) | rate-limited | error`.
**confirm mode** (`?token=`): `form → submitting → done → redirect /login?reset=success | expired | already-used | not-found | error`.

### Red-first pins (vitest)

- router.integration: `/verify-email` + `/reset-password` render the pages (not NotFoundPage).
- VerifyEmailPage: no-token; happy 200; 410-expired (resend offered, email prefilled); 410-used; 404; resend 202 neutral copy (no existence leak); token stripped from URL after terminal result.
- ResetPasswordPage: request-mode neutral confirmation identical copy for existing/non-existing mock outcomes; 429 + network generic; confirm-mode client validation (min-8, match); 200 → `/login?reset=success`; 410-expired → request-a-new-link path; 410-used; 404.
- LoginPage: forgot-password link; 403 email_not_verified message + resend link carrying email.
- RegisterPage: `emailVerified:false` → check-email card, no redirect; absent/true → existing redirect preserved.

### OpenAPI sync check (to complete with implementation)

Client methods consume exactly the four spec'd endpoints. Spec gaps found
(response documentation only — routes/methods are contract-tested and match):
verify-email lacks 404/410; resend documents 200 but handler returns 202;
confirm lacks 404/410; request lacks 400. Will fix doc-only gaps in this PR if
the route-level Go contract test stays green; flagged regardless.

## Implementation log

Verified (commands run, counts checked) unless labeled reasoned.

1. **Read-before-write**: router.tsx, all auth pages/forms, AuthCard, api
   client + types, AuthProvider, email/service.go (link builder — `?token=`
   query param), email_verify.go + password_reset.go handlers (exact status
   codes + body error strings), openapi.yaml auth-lifecycle section,
   LoginPage/RegisterPage/SSOStartPage/InvitationPage tests, the Go
   OpenAPI↔router contract test (route-level only — response docs not
   enforced there, which is why the doc gaps survived).
2. **Red-first**: wrote VerifyEmailPage.test.tsx (10), ResetPasswordPage.test.tsx
   (13), router.integration additions (4), LoginPage additions (3+1 later),
   RegisterPage additions (2) BEFORE any implementation. Confirmed RED:
   new-page suites fail on missing module; router tests render NotFoundPage;
   LoginPage/RegisterPage new tests fail on missing UI. One RegisterPage
   guard test ("still redirects when emailVerified absent") passes on
   current code by design — it is the regression pin for auto-verify
   instances. Fixed cross-test pollution in my own new tests during the red
   phase (window.location stub not restored on failure → try/finally).
3. **Implementation**:
   - `api/auth.ts`: verifyEmail, resendVerification, requestPasswordReset,
     confirmPasswordReset (exactly the four spec'd endpoints).
   - `lib/tokenResult.ts`: classifyTokenError (410 body-string → expired |
     used | gone; 404 → not-found) + stripQueryParam (terminal-state URL
     hygiene via history.replaceState).
   - `pages/VerifyEmailPage.tsx`: states missing/verifying/verified/expired/
     used/not-found/gone/error; POST-on-mount with a StrictMode double-invoke
     ref guard (dev double-POST would misreport single-use tokens as
     already-used); resend sub-form (email prefilled from ?email=); neutral
     always-202 resend copy; return_to threaded to the sign-in link.
   - `pages/ResetPasswordPage.tsx`: request mode (neutral fixed confirmation,
     429/network transport errors only) + confirm mode (client-side min-8 +
     match validation; 200 → done + /login?reset=success CTA; 410-expired →
     request-a-new-link returns to request mode; used/not-found/gone states).
   - `router.tsx`: both routes added OUTSIDE GuestOnly next to /invitations
     and /sso (register sets a session cookie even unverified — GuestOnly
     placement would re-create the lockout).
   - `LoginForm.tsx`: 403 + body.emailVerified===false → actionable message
     + prefilled `<a href="/verify-email?email=…">` resend link (hard-nav,
     router-context-free).
   - `LoginPage.tsx`: "Forgot your password?" link (password mode);
     `?reset=success` → notice + param stripped.
   - `RegisterPage.tsx`: password signup now inspects the register response;
     strict `user.emailVerified === false` → check-your-email card (no
     redirect; user state deliberately not populated so GuestOnly doesn't
     bounce; verify link carries email + return_to; secondary
     "Continue without verifying" honors return_to since the register
     session is valid). Absent/true → loginWithToken best-effort +
     existing redirect (passkey-flow pattern).
   - `types.ts`: User.emailVerified?, ApiError.emailVerified?.
4. **OpenAPI sync**: doc-only fixes — verify-email + confirm now document
   404/410; resend 200→202 (matches handler); request documents 400.
   Route/method contract test re-run green; handler suites re-run (32 pass)
   to confirm documented statuses match behavior.

## Review rounds (AI reviewer, per-push)

- **Round 1** (vs design-note commit a03f1d0f): CHANGES_REQUESTED — "no
  code yet" (expected; draft).
- **Round 2** (vs a3309619): CHANGES_REQUESTED. Findings + dispositions:
  - e2e layer missing → already landed in 963ba09b (9 tests — commit
    message's "(8)" was a miscount, see Errata); extended to
    12 with browser-level 410-used (verify + reset) and 404 unhappy paths.
  - issue piece 4 (user-facing docs) → ADDED `docs/user/account-recovery.md`
    (both flows, single-use/TTL semantics, session revocation, secrets-erasure
    consequence, takeover-notification guidance) + registered a User Guide
    nav section (also surfaces the previously orphaned dev-preview.md).
  - ResetPasswordPage error card description "Password reset" read as
    success → now "Password reset failed".
  - LoginForm 403 resend link dropped return_to → new `verifyHrefFor` prop;
    LoginPage threads sanitised return_to (unit-pinned).
  - router.integration stale comment + dead `me:` stub → corrected/removed
    (enforcement comes from the AuthProvider mock, now stated accurately).
  - VerifyEmailPage "used"-state over-promise + hard-coded TTLs → caveat
    comment on the consume-before-UpdateUser ordering; TTL comments tying
    copy to the backend constants at all three surfaces.
- **Round 3** (vs 963ba09b): CHANGES_REQUESTED. Docs piece 4, error-card
  copy, return_to threading, dead stub — all already addressed in 59990e34
  (the review raced the push; its findings were re-checked against 59990e34
  and are fixed there). NEW findings acted on:
  - VerifyEmailPage missing-token state said "link is invalid or
    incomplete" — false for the register check-email path (no link was ever
    clicked). Copy split: missing → resend-oriented ("we'll send a
    verification link"), not-found keeps "invalid or incomplete";
    unit-pinned both ways.
  - Worklog count erratum (Rule 7): earlier revisions claimed
    VerifyEmailPage "(11)" tests (actual 10) and the e2e suite "(8 tests)"
    in the 963ba09b commit message (actual 9 at that commit; 12 after the
    round-2 additions). Corrected here; commit messages are immutable.
  - Stale "DRAFT" preamble in the PR body — PR body rewritten to final
    state.

## Errata / count corrections

- VerifyEmailPage.test.tsx: 10 tests (not 11 as first logged).
- verify-reset.spec.ts: 9 tests at commit 963ba09b (commit message said
  8); 12 tests after round-2 additions (current).
- New-test totals at head: 33 vitest (10+13+4+2+4) + 12 e2e.

## Test runs (verified counts)

- Frontend targeted: 5 files, **56/56 PASS** (vitest); after the provider
  fix: 15 auth-related files, **115/115 PASS**; after round-2 fixes:
  5 files, **57/57** (new return_to-threading pin).
- Frontend FULL suite: **188 files, 2088/2088 PASS** (re-run after every
  remediation).
- Playwright e2e FULL suite: **187 passed, 13 skipped, 0 failed** (~5m);
  verify-reset.spec.ts now **12/12** (register→check-email, verify
  consume/expired/used/404/return_to + URL-strip, reset request
  neutral-202, confirm success chain, expired→request-new, used→request-new,
  login-403 resend link), 108 consecutive green across repeat-each batches
  after the flake fix.
- `tsc --noEmit`: clean. `eslint .`: 0 errors (abi_pb warnings pre-existing
  on main).
- Go: `TestOpenAPIRouterContract` PASS; handlers `-run "TestPasswordReset|
  TestEmailVerify"` **32 PASS**.

## Incidents / disclosures

- During the red phase my first RegisterPage test run showed 2 failures
  where 1 was expected: my new test threw before restoring a stubbed
  `window.location`, polluting the next test (pre-existing file pattern had
  the same latent hazard but never tripped). Fixed with try/finally in the
  new tests. No production code was affected; disclosed for the record.
- jsdom note (reasoned, not a defect): `<a>` clicks don't navigate in jsdom,
  so redirect assertions use link-href attributes (reset-success) and
  href-setter spies (existing patterns) rather than click-through.
- **CI round 1: "Frontend (unit + typecheck + e2e)" FAILED** — the existing
  `register-turnstile.spec.ts` happy path broke. Verified root cause: the
  old register flow navigated via `setUser` → GuestOnly bounce to /chat;
  my refactor replaced it with `loginWithToken` (a `/auth/me` round-trip),
  which 401s in the route-mocked e2e (no cookie propagation) so the URL
  stayed /register. Fix: `AuthProvider.register` now returns the
  `AuthResponse` and skips `setUser` ONLY when `user.emailVerified ===
  false` — verified registrations get the exact old navigation semantics
  back (and one less request in prod); unverified ones stay on /register
  for the guidance card. This is a behavior-affecting change to a provider
  method (return type widened, setUser made conditional) — flagged for the
  reviewer; RegisterPage is its only caller (grep-verified).
- New e2e suite `verify-reset.spec.ts` (8 tests): one test flaked ~2/90
  runs (click-after-SPA-navigation race: click landed, no submit state
  change). Could not reproduce with tracing; restructured the test to pin
  the forgot-password link by href and drive the request flow via direct
  navigation (deterministic). 108 consecutive green runs after.
- Playwright `getByPlaceholder` substring-matching pitfall: "New password"
  also matched "Confirm new password" → strict-mode violation; fixed with
  `{ exact: true }`.

## Follow-ups (not in scope)

- Per-route rate caps for register/reset/verify (audit D3).
- Passkey signup on verifier-wired instances: passkey issuance is not
  email-gated (audit D2) — no lockout, but no verification nudge either.
- A verified-email banner inside the app for logged-in unverified users.
