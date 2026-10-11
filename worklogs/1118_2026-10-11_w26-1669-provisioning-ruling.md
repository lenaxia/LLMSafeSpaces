# Worklog: #1669 §9 — org provisioning-policy OWNER RULING recorded (design 0065)

**Date:** 2026-10-11
**Session:** w26 deliverable 2 of 3 — rewritten mid-flight per the orchestrator's amendment: the owner RULED design 0064 §9 before the decision document was drafted, so this records the ruling and its consequences instead of proposing options.
**Status:** Complete

---

## Objective

Record the owner's §9 ruling ("For orgs, registration should be disabled. It is provided via OIDC, so the org owner is responsible for handling sign-ups to their OIDC.") as design 0065: the ruling verbatim, its mechanical meaning, how the four deferred options resolve under it, the one remaining edge flagged as an OPEN owner QUESTION (not answered), and operational guidance for org owners on restricting signups at their IdP.

---

## Work Completed

### design/0065_2026-10-11_org-provisioning-policy-ruling.md (new)

- **§1** The question (design 0064 §9's open-door observation) and the ruling: org access is IdP-authenticated, period; the platform adds no org-local registration path; signup policy lives at the org's IdP.
- **§2** The four deferred options resolved: (i) status-quo ruled-in refined (openness is a property of the org's IdP, not the platform); (ii) invitation-required **dead** — including an explicit record of the invitation-token mechanism it would have required (signed credentialless token bound through the OIDC state cookie, consumed at callback to work around Accept's JWT gate) and a "do not build" note so it is not re-proposed; (iii) domain-gating dead as a platform gate (domains drive discovery, not admission); (iv) per-org toggle dead (duplicates the IdP's control).
- **§3** Consequences: no code change; CX (e)-invitation cell supersession recorded; docs carry the policy; abuse surface remains the org owner's with instance-level blunt instruments.
- **§4** OPEN QUESTION flagged for the owner: orgs with **no** SSO configured — invitation-only steady state, or should such orgs not exist? Deliberately unanswered.
- **§5** Per-IdP operational guidance (Google Workspace Internal-vs-Public app audience; Entra self-service signup off by default; Okta Sign-Up Policy off by default; Keycloak/Auth0 self-registration toggles) + the platform-side `auto_provision` off-switch.
- **§6** Verification trail — every code claim cited (sso.go:611/619/658-669/757, invitations.go:369-429; no org-local registration path exists; `auth.registrationEnabled` is instance-scoped).
- **§7** History incl. the note that design 0064 §8's C-vs-C-strict question is untouched.

---

## Key Decisions

- **New design file, not an edit of 0064** — design 0064 is still w24's OPEN PR #1671; editing its file in my branch would be cross-PR surgery. 0065's header carries the "Amends: 0064 §9" link with a note for 0064 to point back when it lands.
- **Ruling, not proposal** — status line says Accepted; the §4 QUESTION is the only open item and is visibly the owner's.
- **Rejection rationale recorded in detail** (option ii's token mechanics) so the dead direction is informed, not vague — the charter originally required proposing that mechanism; the amendment killed it, and the doc explains why it must not be built.

---

## Blockers

None. The §4 QUESTION awaits the owner; nothing in this lane blocks on it.

---

## Tests Run

- None — design document only (no code paths touched). Code citations verified by direct source read at 9a7900b7 (listed in §6 of the design).

---

## Next Steps

- Deliverable 3: #1652 hardening (passkey LoginFinish issuance checks; per-route rate caps on register/password-reset/verify-email; /auth/lookup D4 assessment).
- Ping the orchestrator/owner that the ruling doc is up (the amendment's "STAY ALIVE" clause: the ruling doc landing with the owner pinged is one of the exit conditions).

---

## Files Modified

- design/0065_2026-10-11_org-provisioning-policy-ruling.md (new)
- worklogs/1118_2026-10-11_w26-1669-provisioning-ruling.md (this file)
- COORDINATE.md (status flip)

---

## Post-review addendum (same day): rulings recorded — 0064 §8 = C and 0065 §4 (both records live in design 0065 §4/§4b; 0064's own status/menu lines updated to point at them)

While PR #1676 was in review, the owner resolved the two open design decisions (relayed by the orchestrator on the PR thread): **design 0064 §8 = C** (org name shown on the org-scoped login; phase-1 behavior permanent; no C-strict) and **design 0065 §4 = "All orgs require OIDC. We don't support anything else."** (no-SSO org is not a supported state). Applied: 0065 §4 rewritten from open question to ruling with the enforcement-gap status (pinned by `TestCreateOrg_NoSSORequirementGapPin` in PR #1677; enforcement mechanism — creation gate vs not-usable-until-SSO — flagged for owner decision given the bootstrap chicken-and-egg) and §4b added recording §8 = C. Status line and §7 History updated. No code change from either ruling record.

---

## Review round 1 remediation (PR #1678, CHANGES_REQUESTED → fixes)

1. "Filed as follow-up" was unvalidated prose — **issue #1679 now exists** and is cited in §4.
2. Dangling "§1/§3" cross-reference — §4 now points at §6's verification trail (+ the operator/user docs).
3. 0064 (merged while this PR was open) still advertised its owner menu as open — 0064's Status + Binding rulings lines now record §8 = C → 0065 §4b, §8's section carries the RESOLVED banner, and the header's promised §9 → 0065 pointer landed with the Binding rulings line.
4. Pin tense — §4 now says the pin "will lock … once #1677 merges (currently open)".
Worklog addendum heading corrected (both ruling records live in 0065).
