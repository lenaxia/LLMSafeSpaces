# Worklog NNNN — #1650 registration-toggle enforcement (audit follow-through)

Session: w19 (auth audit #1649), PR #1657, branch `w19/enforce-registration-toggle`
(stacked on the gap-C commit 89308223 shared with PR #1659).

## Problem (from audit #1649 gap A / issue #1650)

`auth.registrationEnabled` instance setting was advertised by `GET /auth/config`
(frontend hides the signup link) but never enforced: `POST /auth/register` and
passkey signup stayed open on closed instances. A static config twin
(`auth.registrationEnabled`) was read nowhere. Verified by grep (zero reads of
either) + red test (disabled instance → register **201'd the account**).

## Design rulings (orchestrator asked for decisions + justification)

- **403 + constant message** `registration is disabled`: instance-level state — not an
  account-existence oracle (unlike the per-account 409 duplicate case, fixed in #1659).
  403 over 404 to keep the OpenAPI route contract honest.
- **Fail-open on settings read errors** (nil service, DB error, wrong-typed value):
  matches `/auth/config`'s existing fallback so advertisement and enforcement can never
  disagree; settings outage cannot lock out the fresh-install bootstrap (registry default
  true). Documented in `docs/operator/security.md` incl. the Turnstile-as-second-layer note.
- **Scope = local account creation only** (password register + passkey signup begin/finish).
  Ungated by design: passkey login/recover, authenticated enrollment, SSO auto-provision
  (per-org flag), org invitations (JWT-gated accept) — pinned by
  `TestPasskeyLoginRecover_NotGatedByRegistrationToggle`.
- **Gate before Turnstile**: closed instances burn zero Cloudflare verifications.
- **Static config field deleted**: single-switch semantics; field was dead (verified
  read-nowhere; viper has no ErrorUnused so stale YAML keys are ignored).

## Red-first + mutation evidence (run-measured)

- `TestRegister_RegistrationDisabledBySetting_Returns403` — RED at parent 89308223
  (201, account created; captured in-session before implementing). Independent
  reproduction by the PR review bot at the parent commit confirmed the red.
- `TestPasskeySignup_RegistrationDisabledBySetting_Returns403` — RED pre-fix (reached handler).
- Review r1 asked for the missing wiring branch: `turnstile.Enabled=true` × disabled →
  added `TestRegister_TurnstileEnabled_RegistrationDisabled_403BeforeTurnstile` asserting
  403 AND **zero siteverify calls**. Mutation-checked: locally moving the gate AFTER the
  Turnstile middleware makes the test FAIL (`verifyCalls: 1`) — the pin bites; restored, green.
- Suites at HEAD: `api/internal/server` ok, `api/internal/config` ok,
  `api/internal/handlers` ok (105s), `go build ./api/...` clean, gofmt clean.

## Review r1 remediations (this session)

1. Turnstile×gate wiring test (above, mutation-verified).
2. This worklog.
3. Commit-message accuracy on the stacked gap-C commit: the WIRE-level test
   (`..._ConflictError_GenericOnWire`) passes pre-fix by construction (it pins
   respondWithError's rendering); the genuine red pin is the service-level
   `TestRegister_DuplicateEmail` NotContains assertion (verified red at f9a74a19).
   Commit reworded accordingly (same tree; patch-id identical for clean rebase
   vs #1659).
4. PR body stack reference corrected (#1653 closed → #1659).
5. docs/operator/security.md: registration-gating section + fail-open note (review's
   robustness suggestion).

## In-flight incidents

- Two pre-commit rejections this session: (a) worklog NNNN-sentinel naming (first audit
  worklog), (b) gofmt on config.go — both fixed per hook guidance, no --no-verify used.
- Review-workflow outage (fleet-wide, bot write-permission failures on PRs #1653/#1654
  attempt-2) — disclosed to orchestrator; PRs re-created by fleet as #1659/#1660.
