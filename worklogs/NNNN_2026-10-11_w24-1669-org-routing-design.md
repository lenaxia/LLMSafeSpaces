# Worklog: #1669 — multi-org arbitrary-email login routing decision document (design 0064)

Session: w24 (ses_*), branch feat/1669-org-routing from main @ b97dd185.

## Charter

Design-first: a decision document as a PR against design/, analyzing the issue's
three candidate options (A org picker / B invitation-only / C subdomain-first)
against the enumeration-safe constraints, with a recommendation, acceptance
criteria mapping, and a phased plan. Implementation of phase 1 only after an
owner selection lands in review.

## What I verified before writing (all read-only, this tree @ b97dd185)

- `login_discovery.go` — lookup is membership-only, always-200, uniform body
  shape; D4 accepted-risk body oracle (found vs not_found).
- `org_sso.go` + `pg_org_store.go:1812` — `/auth/sso/domains` lists only
  DNS-verified domains; `start` is public, org-parameterized, org-binds the
  HMAC state cookie.
- `services/sso/sso.go` — auto-provision gates on `AutoProvision` ONLY (no
  domain gate anywhere); callback refuses org-swap (org.ID != payload.OrgID →
  ErrStateInvalid); email_verified mandatory (F8). So the #1669 gap is pure
  routing/discoverability, not a provisioning hole.
- `LoginPage.tsx` — verified-domain matched button + "Continue with email"
  lookup; the discovery button doesn't even render when no org has verified
  domains (domains.length > 0 gate).
- `SSOStartPage.tsx` + router — `/sso/:orgSlug` shortcut already exists.
- `helm/templates/frontend-wildcard-ingress.yaml` — wildcard routes `/api` →
  API and `/` → frontend on `*.baseDomain`; so the SPA on an org subdomain
  reaches the API same-origin, and `/auth/config` can derive org context from
  the Host server-side. The Epic 54 subdomain surface is a redirect target
  today, never a login surface — the login page there is generic.
- `invitations.go` — Accept requires JWT + email match; InvitationPage routes
  unauthenticated users to /login or /register → a brand-new SSO-only user
  cannot accept invitations today (chicken-and-egg) — this drove B's cost
  assessment.
- audit #1649 SSO section + D4 — the enumeration baseline the design must not
  grow: `/auth/sso/:slug/start` already reveals SSO-configured org existence.

## Decision (design/0064_2026-10-11_multi-org-arbitrary-email-login-routing.md)

Recommend **C, additively scoped**: org-scoped entry points (subdomain login
page gains a first-class org SSO button via an optional `orgContext` on
`/auth/config`; `/sso/:slug` documented as the non-subdomain equivalent).
Root login unchanged (keeps membership lookup + verified-domain button) — the
issue's "root serves personal/password only" phrasing would break the
existing-users-unchanged acceptance, and the doc reconciles that explicitly.

- A-as-picker rejected: enumerates the arbitrary-email org directory on the
  public root page — unsatisfiable vs "the lookup oracle does not grow". The
  oracle-free typed variant (A′) is dominated by C (same knowledge, worse
  ergonomics) but kept as an optional phase-3 add-on.
- B rejected as vehicle: breaks direct-URL onboarding at the existing
  arbitrary-email orgs on deploy, highest implementation cost (invitation
  binding must flow through the OIDC state cookie because accept requires a
  JWT), and conflates routing with the real but separate provisioning-policy
  question (slug = open-registration door for orgs with open IdPs) — filed
  separately in the doc (§9), owner to rule.

Phase 1 (releases on owner selection): `orgContext` on `/auth/config` (Host
parse vs configured baseDomain, SSO-configured orgs only, fail-soft omit) +
LoginPage org button + red-first pins (handler truth table, router Host test,
lookup/domains contracts re-pinned unchanged, LoginPage vitest, openapi
additive). Phase 2: docs + dead-end copy. Phase 3: A′ / hardening, ruling-gated.

## Status

Design PR open; implementation HELD pending review per charter. No production
code touched in this PR.

## Review round 1 (formal bot review, CHANGES_REQUESTED) — both findings accepted

1. **Issue closure gate**: PR body said "Closes #1669" — would auto-close a
   design+implement issue on a docs-only diff. Fixed: `Refs #1669`; issue stays
   open for phases 1-2.
2. **Privacy-claim precision** (verified by reviewer against source):
   `/auth/sso/:slug/start` reveals existence-at-slug but NOT the org display
   name; `orgContext {slug, name}` additionally reveals the name. My original
   "byte-for-byte the accepted baseline" claim was payload-inaccurate — the
   correct statement is *set-equal on existence, plus a bounded name delta*.
   Fixed in §2 (baseline now splits existence-oracle vs metadata-reveal axes),
   §4C (score + justification of the delta), §6.1, §5, §10 (pins now assert
   the existence contract; the name is pinned as intended payload, not as a
   parity claim). Added a "C-strict" zero-delta variant (slug-only orgContext,
   generic label) to the §8 owner menu so the delta is an explicit ruling.

Recommendation unchanged: C (default with name, or C-strict).

## Review round 2 (CHANGES_REQUESTED) — residual accepted

r1 findings confirmed resolved (Refs; privacy claims in §2/§4C/§5/§6.1/§8/§10;
UNVERIFIED citation verified verbatim). One blocking residual: the §4C
blast-radius bullet still claimed a spoofed-Host reveal is "information equal
to probing start" — the same payload-inaccuracy as r1 finding 2 (start reveals
existence only; the name has no existing public surface for non-verified-domain
orgs), and it contradicted the corrected lines elsewhere in the doc. Fixed in
the r2 commit: the blast-radius bullet now states existence-at-slug equality
PLUS the same disclosed name delta, and notes it collapses to exact
start-parity under C-strict. My r1 reply's "both findings accepted and fixed"
was overstated — one instance survived; corrected here.

## Review-infrastructure note (2026-10-11 ~03:05-03:30Z)

The round-3 "PR Review" bot run (38107249319, for the r2-fix commit) failed
bot-side at "Verify review submitted" — the model never delivered a formal
verdict (both attempts + salvage failed). `/review` comment retrigger was
SKIPPED (commands are owner/member/collaborator-only; the PR-author identity
does not qualify). Retriggered instead with a genuine content commit: §6.2
refinement — on an org subdomain, the email-driven verified-domain button for
a DIFFERENT org is now explicitly suppressed (previously ambiguous "wins"
wording); root-page matching behavior unchanged. Orchestrator informed.

## Post-r2 substantive refinement (same commit)

§6.2: subdomain login suppresses cross-org verified-domain buttons; the
cross-org user's path remains the root page (existing behavior). No change to
recommendation or phases; phase-1 frontend tests gain the suppression case.
