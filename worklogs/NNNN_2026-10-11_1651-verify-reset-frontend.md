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

(in progress)

## Test runs

(pending)

## Incidents / disclosures

(none yet)
