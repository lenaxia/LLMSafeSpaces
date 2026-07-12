# Worklog: Epic 58 (SSO server-KEK default) + Epic 59 (passkey-only) — design passes

**Date:** 2026-07-12
**Session:** Two related deferred items from the broader security arc — both were framed as "needs more design" by the user, neither had a written design. This worklog adds both as new-epic design documents and stops short of code pending sign-off on the threat-model tradeoffs each one asks the operator to accept.
**Status:** Design complete (code deferred pending sign-off)

---

## Objective

Two related deferrals:

1. **SSO users get server-KEK by default.** The user's framing was "drops the 'set a password first' prompt. UX win, small code change." After reading the SSO and DEK code I disagree with the size estimate — it inverts the two-tier encryption model documented in `docs/operator/security.md` and `docs/architecture/secrets.md`, and the threat-model tradeoff is non-trivial. Writing the design doc honestly is the work; the user can then decide.

2. **Passkey-only login.** The user mentioned "eventually remove password support and only support passkey/SSO." This is explicitly future-state. The design pass scopes what "remove password support" means and surfaces the load-bearing decision (DEK source for passkey users) that ties it to Epic 58.

---

## Work Completed

### Epic 58 — `design/stories/epic-58-sso-server-kek-default/README.md`

Design doc covering:

- **The threat-model tradeoff as the central decision.** Personal secrets of SSO users today are in the user-password tier (platform cannot decrypt). After this epic they would move to the server-KEK tier (platform can decrypt, bounded by KEK protection). The two-tier model documented in worklog 0616 is what would be inverted. Reasonable people can answer either way; the doc gives both answers explicitly.
- **Why the user's "small code change" framing is wrong.** The change touches the data model (new `users.dek_source` enum column), the boot path (`KeyService.GetDEK` branches on `dek_source`), the rotation CLI (`rotate-kek` walks `user_keys` with two branches now), the SSO provisioning path (`resolveUser` provisions a server-KEK-wrapped DEK), and the password-transition path (`ChangePassword` flips `dek_source` to `'password'`). ~250 LoC + 10 tests across 8 files. Not a small change.
- **Four open questions that need explicit sign-off** before pickup: accept the tradeoff, default-vs-flag, existing-SSO-user migration, audit-label distinction.
- **Pairing recommendation with KMS.** The threat-model regression is smaller under cloud KMS (the master KEK is bounded by the HSM, decrypt is independently audited) than under static/sealed. The doc recommends (does not require) that SSO-server-KEK deployments use KMS.

### Epic 59 — `design/stories/epic-59-passkey-only-login/README.md`

Design pass covering:

- **Three interpretations of "remove password support"** with explicit verdicts: passkey-as-additional-option rejected (doesn't satisfy user intent); phased rollout (b) is the realistic shape; hard cutover (c) rejected as destructive. The epic covers Phase 1 only; Phases 2 and 3 are documented for traceability but live in future epics.
- **The DEK-source question.** Passkey users face the same problem as SSO users — no password to derive a DEK from. Two options: server-KEK-wrapped DEK (same as Epic 58), or DEK wrapped by the passkey itself (fragile, loses everything on authenticator loss unless a recovery path exists). The doc recommends (i), same as Epic 58. This is why Epic 59 depends on Epic 58 sign-off — answering the DEK-source question twice risks getting two different answers.
- **Recovery — the hard part.** Three layers required: multiple passkeys, recovery codes, operator-assisted destructive recovery. The destructive-recovery path matches today's password-recovery story for users who lose both password and recovery key.
- **Migration story.** Phase 1 lands passkey endpoints (no behavior change for existing users); Phase 2 (future epic) makes passkey the default for new users; Phase 3 (future epic) deprecates password endpoints after a 90-day window.
- **Five open questions for sign-off** including the DEK-source question, recovery-code UX, sign-count enforcement strictness, operator-assisted recovery shape, and cross-device flow handling.

---

## Key Decisions

1. **Two new epics, not one.** Tempting to fold passkey into Epic 58 — both are "non-password auth path" work. Rejected: Epic 58 is a UX-and-encryption-tier change scoped to existing SSO users; Epic 59 is a multi-phase rollout of an entirely new authentication primitive. Conflating them obscures the per-epic sign-off needed for each threat-model decision.

2. **Epic 58 first, Epic 59 second — sequencing is load-bearing.** Both epics answer the same encryption-layer question ("what wraps the DEK for a user without a password?"). Doing Epic 59 first would force an answer that might conflict with Epic 58's resolution. Doing Epic 58 first resolves the question once; Epic 59 consumes the same `dek_source` enum value.

3. **No code in this worklog.** Both epics have explicit sign-off questions that must be answered before pickup. Writing code before those answers would either presuppose the answer (making the doc a rubber-stamp) or be thrown away when the answer differs. The user has framed both as "deferred / needs more design" — this worklog delivers the design; code is the next step after sign-off.

4. **Make the threat-model tradeoff the headline, not the implementation.** The natural-doc structure is "what changes, then the tradeoff." Inverted for both epics: the tradeoff comes first, the implementation second. This is because the tradeoff is the load-bearing decision; the implementation is mechanical once the tradeoff is accepted.

5. **Recovery-code generation is mandatory, not opt-in.** Phase 1 of Epic 59 specifies auto-generation with a "save these" warning that cannot be dismissed without acknowledging. The temptation is to make recovery codes opt-in for simplicity. Rejected: users who skip recovery codes are the ones who need them most. Forcing the flow is the lesson from every consumer passkey deployment that has gone before.

---

## Assumptions stated and validated (Rule 7)

1. *Auto-provisioned SSO users have no `user_keys` row today.* Validated by reading `sso.resolveUser` (`api/internal/services/sso/sso.go:606-638`) — it creates the user with a random unusable bcrypt hash and never calls `InitializeUserKeys`. The "set a password first" prompt is the consumer of this state.
2. *The `user_keys` schema can support server-KEK-wrapped DEKs without modification.* Validated by reading `api/migrations/000001_initial_schema.up.sql:526-535` — `wrapped_dek` is `bytea NOT NULL`, `salt` is `bytea NOT NULL`. The salt NOT NULL constraint would need to become nullable for Epic 58's server-KEK path (no Argon2id derivation = no salt). Documented in Epic 58's files list.
3. *Zero passkey primitives exist in the codebase today.* Validated by grep for `passkey`, `webauthn`, `WebAuthn` — only matches are in Epic 54's design doc and worklog 0497 (both deferral notes). The Epic 59 files list is the first time these names appear as code paths.
4. *The `users` table is the right place for `dek_source`.* Validated by reading the existing schema — `users.password_hash` is already there, and `dek_source` is its semantic companion (which encryption tier this user is in). Adding it to `user_keys` instead would couple the per-user encryption-tier decision to the key material, which is correct conceptually but produces a 1:1 join on every decrypt. `users` is the right shape.

---

## Adversarial Self-Review (Rule 11)

- **Phase 1 finding (real, addressed in the design doc):** Epic 58 as written moves SSO users into the server-KEK tier silently from their perspective. They get no notification that their personal secrets are now platform-decryptable when they weren't before. Phase 2 verdict: real concern, not a code bug. Remediation in the design doc: the epic's open-questions section asks the operator to decide whether to surface this to existing SSO users (recommend: yes, on first login post-epic, with a "your secrets are now recoverable by the platform" disclosure).
- **Phase 1 finding (real, addressed):** Epic 59's recovery story assumes users will enroll multiple passkeys. Field experience (Google, Microsoft, Apple passkey rollouts) shows users rarely do this without prompting. Phase 2 verdict: real concern. Remediation: Phase 1's UI explicitly prompts for a second passkey after the first, with a "skip for now" that surfaces again at every login until satisfied or the user explicitly opts out.
- **Phase 1 finding (real, addressed in the design doc):** Epic 58's pairing-with-KMS recommendation is non-binding. An operator on a static-KEK deployment could enable SSO-server-KEK-default and end up with SSO users' personal secrets protected only by a static key in process memory — a strictly weaker posture than they had as password users. Phase 2 verdict: real concern. Remediation: documented in the design doc as a recommendation, but the doc explicitly notes that making it a requirement would block the epic on Epic 57 adoption, which is a separate decision. The recommendation + the threat-model refresh in the docs is the right level of force.
- **Phase 2 false alarm initially considered:** "Does Epic 58 break the rotate-kek CLI's existing walk over user_keys?" Validated by reading `cmd/rotate-kek/main.go` and `pkg/secrets/rotation.go` — the walk already supports per-row purpose derivation; adding a branch on `dek_source` is additive. False alarm.

---

## Blockers

- **Sign-off on the threat-model tradeoff for Epic 58.** The user has not yet confirmed that the UX win of removing the "set a password" prompt is worth downgrading SSO users' personal secrets from user-password tier to server-KEK tier. Without that sign-off, code work cannot start.
- **Sign-off on the same tradeoff for Epic 59 (passkey users).** Same question, different user population. Blocked on the same decision.
- **Sign-off on Epic 59's recovery story.** The three-layer recovery (multiple passkeys + recovery codes + destructive operator-assisted reset) is the minimum viable story. The user may want a non-destructive operator-recovery path (which would require storing a backup of the DEK under operator-held key, defeating the threat-model benefit). That decision needs an explicit answer.

---

## Tests Run

None — design-only worklog. The tests will be written as part of the implementation epics once sign-off is in.

---

## Files Added

- `design/stories/epic-58-sso-server-kek-default/README.md` — design doc with sign-off questions.
- `design/stories/epic-59-passkey-only-login/README.md` — design doc with sign-off questions.

---

## Next Steps

1. Open this PR.
2. User reviews both design docs and answers the open-questions sections.
3. If Epic 58 is approved, it becomes pickup-able. Epic 59 remains blocked on Epic 58's resolution of the DEK-source question.
4. If Epic 58 is rejected (the threat-model tradeoff is not worth the UX win), Epic 59 is also rejected in its current shape — passkey users would need a different DEK-source answer (option ii, passkey-as-encryption-key, which the design doc notes as fragile).
