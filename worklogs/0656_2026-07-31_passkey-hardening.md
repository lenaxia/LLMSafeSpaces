# Worklog: Passkey full hardening — cookie auth, settings, email verification

**Date:** 2026-07-31
**Status:** Implementation
**Depends on:** PRs #604–#610 (passkey backend + frontend, all merged)

## What shipped

1. **Cookie-based auth** — passkey handlers set HttpOnly+Secure cookie.
   Eliminates localStorage XSS surface. Frontend removed localStorage
   token + Bearer header.

2. **Settings endpoints** — GET/DELETE/POST for passkey management
   (list, delete with last-credential guard, regenerate recovery codes).

3. **Settings frontend page** — PasskeySettings component with passkey
   list, delete, regenerate, enrollment banner.

4. **Email verification** — RegisterFinish calls SendVerification when
   verifier is wired (production). Dev mode auto-verifies.

5. **Sign-count logging** — passkey.Service gains Logger interface.
   FinishLogin logs sign-count update failures.

6. **mustEnrollPasskey redirect** — recovery-code login redirects to
   /settings/passkeys with enrollment banner.

7. **Playwright stability** — Set-Cookie headers in ceremony mocks,
   60s timeout.

## Tests
- Backend: handler tests (settings endpoints, error paths), service tests.
- Frontend: PasskeySettings component tests, AuthProvider cookie tests.
- Playwright: ceremony mocks updated for cookie-based auth.
