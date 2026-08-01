# Worklog: Passkey weak spots — orphan cleanup, race, enroll, returnTo, timeout UX

**Date:** 2026-08-01
**Status:** Implementation
**Depends on:** PRs #604–#611 (passkey backend + frontend + hardening, all merged)

## What shipped

1. Orphaned user cleanup: RegisterFinish deletes the user row if
   CreateCredentialAndRecoveryCodes fails. DeleteUser error logged.
2. Double-submit race: CreateUser catches PG 23505 → 409.
3. Email verification for passkey users: verified NOT A BUG (email_verify
   handler doesn't call VerifyPassword).
4. Recovery-code re-enrollment: POST /account/passkeys/enroll/{begin,finish}
   + Add Passkey button on settings page.
5. returnTo preservation on recovery-code login.
6. Add passkey for existing password users (same enroll endpoints).
7. Ceremony timeout UX: "session expired" message in forms.

## Tests
- Handler: 12 new tests (enroll, regression, error paths).
- Service: 3 new tests (AddCredential, GetUserName).
- Frontend: 10 PasskeySettings tests + 2 expired UX tests.
- Router: enroll routes registered.
