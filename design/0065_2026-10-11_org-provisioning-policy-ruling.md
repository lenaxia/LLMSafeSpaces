# 0065 — Org provisioning policy: owner ruling (registration is IdP-administered); amends design 0064 §9

**Status:** Accepted — owner ruling recorded 2026-10-11 (this document is ruling-documentation, not a proposal; the owner's comment below decides nothing further — it is already decided).
**Date:** 2026-10-11
**Issues:** #1669 (the charter this completes: design 0064 §9 deferred "who may join an arbitrary-email org" — ruled here).
**Amends:** [`design/0064_2026-10-11_multi-org-arbitrary-email-login-routing.md`](0064_2026-10-11_multi-org-arbitrary-email-login-routing.md) §9 (in review as PR #1671 at time of writing — this file is separate to avoid editing that open PR; when 0064 lands, its §9 should point here).
**Binding ruling (verbatim):**

> "For orgs, registration should be disabled. It is provided via OIDC, so the org owner is responsible for handling sign-ups to their OIDC."

---

## 1. The question and the ruling, in one page

**Question (design 0064 §9):** an org running per-org OIDC with `auto_provision=true` and no verified email domains provisions **any** identity its IdP authenticates with a verified email claim. When the org's IdP allows open signup, the org's slug is an open-registration door into that org — anyone who learns the slug can self-join through the IdP. Who may join?

**Ruling:** that door is **the org owner's own door**. Org registration is provided via OIDC; the org owner administers sign-ups **at their identity provider**. The platform's job is to authenticate whatever the IdP vouches for — not to layer a second, platform-side admission policy on top of the one the org already controls at the IdP.

Concretely, as the code stands and stays:

- `resolveUser` (`api/internal/services/sso/sso.go:658`) gates provisioning on the org's `auto_provision` toggle alone; `email_verified` is mandatory (F8); suspended users are refused. That is the complete platform-side gate set, and under this ruling it is complete by design.
- Org access is **IdP-authenticated, period**. The platform adds no org-local registration path (there is none today — `auth.registrationEnabled` is an *instance*-level setting for personal accounts, not an org join mechanism) and will not grow one.
- Signup policy — domain restrictions, approval flows, open vs closed — is administered at the org's IdP. The org owner's lever, not the platform's.

## 2. How the four deferred options resolve under the ruling

Design 0064 §9 deferred four candidate policies; the ruling resolves each:

| Deferred option | Fate under the ruling |
|---|---|
| (i) Status-quo — open by URL | **Ruled in, refined.** "Open" is a property of the org's IdP, not of the platform: the slug is a door only in the sense that it leads to the IdP, and the IdP decides who passes. An org whose IdP provisions a closed directory has a closed door today, with zero platform configuration. |
| (ii) Invitation-required for arbitrary-email orgs | **Dead.** The ruling removes the motivating premise — the platform does not second-guess the IdP's admissions. This also kills the invitation-token mechanism that option (ii) would have required (a signed, credentialless invitation token bound through the OIDC state cookie and consumed at callback, working around invitation-Accept's JWT requirement for credential-less SSO newcomers). That mechanism is **not** proposed here and should not be built: it would re-create a platform-side admission layer the owner has just located at the IdP. |
| (iii) Domain-gated auto-provision | **Dead as a platform gate.** Verified domains drive *discovery* (root-page button inference, `GET /auth/sso/domains`); they do not and will not gate provisioning. An org wanting "only our domain" enforces that at the IdP (§5). |
| (iv) Per-org policy toggle | **Dead.** A platform toggle duplicating what the IdP already controls adds a second place for the same policy to live — exactly what the ruling declines. The existing `auto_provision` toggle (can new SSO identities get accounts at all) remains the one org-side platform control. |

## 3. Consequences, stated plainly

1. **No code change follows from this ruling.** The SSO callback, auto-provision, and invitation surfaces keep their current contracts. The enumeration posture of design 0064 (lookup always-200, verified-only domain listing) is unaffected.
2. **The CX matrix (e)-invitation cell stays as-is** (design 0064 §11.3): invitation accept requires an existing account (JWT + email match); an SSO-only newcomer's blessed path is the **org sign-in link** (phase 1/2 surfaces), not an invitation. The earlier "§9 hardening would close it" note is superseded — under the ruling, that cell does not get an SSO-side fix.
3. **Operator/user documentation carries the policy.** `docs/operator/oidc-sso.md` ("Provisioning policy (owner-ruled)" and "Known gaps") and `docs/user/signing-in.md` state it; this document is the citable record.
4. **Abuse surface is unchanged and remains the org owner's to manage.** Instance operators still hold the blunt instruments (suspend org/users; the org's SSO config can be deleted or `auto_provision` turned off) if an org's IdP becomes a spray source.

## 4. OPEN QUESTION for the owner (flagged, deliberately unanswered)

The ruling covers orgs **with** SSO configured. It does not address orgs with **no** SSO configuration at all:

- Today such an org's only member-acquisition path is **invitations**, and invitations work only for users who hold (or create) a password/passkey account — the instance-level `auth.registrationEnabled` gate therefore indirectly shapes who can join a no-SSO org.
- Is "invitation-only, riding instance registration policy" the intended steady state for no-SSO orgs — or should orgs without SSO not exist (i.e., SSO configuration become a prerequisite for org creation)?

This document does not answer it. **Owner: one comment here picks a branch.** Until then, no change: invitations remain the no-SSO org path, and nothing in this ruling is read to require SSO.

## 5. Operational guidance for org owners (where signup policy now lives)

The org owner configures admission at their IdP. Common shapes, per IdP:

- **Google Workspace:** the directory is admin-provisioned — accounts exist only when an admin creates them (or a delegation rule does), and the OAuth app's **audience setting is the gate**: *Internal* (the default for apps registered inside a Workspace tenant) admits only tenant accounts; *Public/External* would let any Google account consent. Keep the app Internal and the org door is closed to non-tenant identities regardless of platform configuration.
- **Azure AD / Microsoft Entra ID:** external identities follow the cross-tenant access and collaboration settings; **self-service signup** is a per-app policy an Entra admin can leave disabled (the default for enterprise tenants). Keep it disabled and the directory is admin-controlled; email one-time-passcode and guest invites are the admin-gated openings to review.
- **Okta:** the **Sign-Up Policy** (and self-registration) is off by default — users are admin- or HR-provisioned. The org door tracks that policy: no sign-up rule → no self-join.
- **Generic OIDC (Keycloak, Auth0, Zitadel, …):** look for the realm/application-level **self-registration** toggle (Keycloak: realm "User Registration" on the login tab; Auth0: database connection "Disable Sign Ups"), plus domain allowlists / email verification requirements where offered. If self-registration is on and the realm accepts arbitrary emails, the org is an open door **by the org's own configuration** — which this ruling makes legitimate and the org owner's to change.

In every case the platform-side `auto_provision` toggle remains available as the org's off-switch for *account creation* wholesale (existing members keep logging in; new identities get `provisioning_disabled`).

## 6. Verification trail (this tree, @ 9a7900b7)

- Provisioning gate set: `resolveUser` `sso.go:658-669` (`AutoProvision` only), F8 `email_verified` mandatory `sso.go:611`, suspended refusal `sso.go:619`.
- No org-local registration path exists: org member acquisition = SSO auto-provision ∪ invitation accept (`invitations.go:369-429`, JWT + email-match); `auth.registrationEnabled` is instance-scoped (settings key, `/auth/register`).
- Invitation-token direction (§2, option ii) mechanics confirmed as described: state cookie payload `{s, v, o, e}` HMAC-signed (`sso.go:757`), accept requires JWT — recorded so the rejection is informed, not vague.

## 7. History

Deferred by design 0064 §9 (2026-10-11, PR #1671); w26 chartered to prepare a decision document (options i–iv + recommendation); **the owner ruled before the decision document was drafted** (orchestrator amendment, 2026-10-11) — this file records the ruling and its consequences instead. Design 0064 §8's C-vs-C-strict org-name display question remains the owner's one open decision in that design; it is untouched and unrelated to this ruling.
