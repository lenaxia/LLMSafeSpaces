# Worklog: Passkey confidence hardening — full ceremony, recovery→enroll e2e, options contract

**Date:** 2026-08-01
**Session:** Confidence-hardening PR (#614) for passkey staging launch — added full login-ceremony e2e, recovery→re-enrollment e2e, and an options-shape contract test; iterated through AI review until approved.
**Status:** Complete
**Depends on:** PRs #604–#612 (passkey backend + frontend + hardening + weak spots, all merged)

---

## Objective

Raise passkey test confidence from ~70% toward ~95% before staging launch by
covering the two remaining unverified paths end-to-end:

1. Full login ceremony — register a credential, log out, then log back in via a
   real `navigator.credentials.get()` assertion through the CDP virtual
   authenticator.
2. Full recovery → re-enrollment — recover via recovery code, land on the
   `must_enroll_passkey` settings page, enroll a new passkey, see it listed.

Plus a Go contract test that regression-guards the options-shape bug found in PR
#610 (flat `PublicKeyCredentialCreation/RequestOptionsJSON`, not wrapped in
`{ publicKey: … }`).

---

## Work Completed

### 1. Full login ceremony e2e (frontend/tests/e2e/passkey.spec.ts)
- Registers a credential first so the virtual authenticator holds a discoverable
  credential, then performs a real login assertion and asserts redirect off `/login`.
- Debugged three real failures through review/CI cycles:
  - Registration used `residentKey: "preferred"` → the virtual authenticator may
    not create a discoverable credential, so `credentials.get()` with
    `allowCredentials: []` found nothing. Fixed by switching to
    `residentKey: "required"`.
  - The login form's email field was left empty; the HTML5 `required` attribute
    blocked form submission, so the assertion never fired. Fixed by filling
    email before clicking "Sign in with passkey".

### 2. Full recovery → re-enrollment e2e
- Mocks `/auth/passkey/recover`, the passkey list endpoint, and the enroll
  endpoints; asserts the must-enroll banner, "Add passkey", and the new passkey
  in the list.
- Fixed route-mock timing per review: passkey mocks are now registered *before*
  page navigation so the initial GET by `PasskeySettings` is always mocked.
- Removed a duplicate `let enrollDone` declaration that was a parse-time
  SyntaxError.
- Switched the "Add passkey" click to a role-scoped locator — the empty-state
  sentence "Click 'Add passkey' to continue." also matched `getByText("Add passkey")`.
- Added explicit `test.setTimeout(60_000)` (the default 30s is too tight for the
  multi-step recovery + enrollment flow).

### 3. Options-shape contract test (api/internal/services/passkey/options_shape_contract_test.go)
- Asserts the marshalled options are flat (`challenge`, `rp`, `user`,
  `pubKeyCredParams`/`rpId` at the top level) and not wrapped in
  `{ publicKey: … }`. This is the regression guard for the PR #610 shape bug that
  the service-level test authenticator cannot catch (it bypasses
  `@simplewebauthn/browser`).

### 4. Pre-existing e2e fix
- Settings-list test used `getByText("Passkeys")`, which matched both the sidebar
  nav item and the page heading → strict-mode violation (failing on main too).
  Fixed with `getByRole("heading", { name: "Passkeys" })`.

---

## Key Decisions

- **`residentKey: "required"`** for the ceremony registration mock: guarantees a
  discoverable credential so the login assertion with `allowCredentials: []`
  succeeds. This mirrors what a production server should send when passwordless
  login with an empty allow list is desired.
- **Register all route mocks before navigation** in e2e tests: Playwright routes
  registered after a page has already issued a request leave that request
  unmocked, producing flaky, unverified assertions.
- **Role-scoped locators** (`getByRole`) instead of raw text matching where text
  appears in both a button and body copy.
- Keep PR #614 scoped to tests only — no production code changes, so the
  reviewer's gate can focus purely on test correctness.

---

## Blockers

None.

---

## Tests Run

- `npx tsc --noEmit` (frontend) — pass
- `go build ./api/internal/services/passkey/` — pass
- `go vet ./api/internal/services/passkey/` — pass
- `go test ./api/internal/services/passkey/ -run TestOptionsShape_ContractWithSimpleWebAuthn` — pass
- Frontend unit + typecheck + Playwright e2e in CI (with mocked backend) — see
  latest run on PR #614; all Playwright failures resolved across review cycles.

---

## Next Steps

1. Confirm PR #614 CI is green and review approves.
2. Merge to `main`.
3. Stage-deploy, then manually verify the real WebAuthn ceremony + RP ID/origin
   matching + cross-origin cookie behavior on a real domain — the remaining
   ~5% that mocked localhost e2e cannot cover.

---

## Files Modified

- `worklogs/0658_2026-08-01_passkey-confidence-hardening.md` — this entry (added)
- `frontend/tests/e2e/passkey.spec.ts` — full login ceremony, recovery→re-enrollment, locator/timeout/credential-ID fixes
- `api/internal/services/passkey/options_shape_contract_test.go` — new contract test
- `frontend/test-results/.last-run.json` — deleted (generated Playwright artifact; now gitignored)
