# 0064 — Multi-org arbitrary-email login routing: org-scoped entry points (option C), not a root-surface picker

**Status:** Ruled (2026-10-11) — **owner selected C** (relay recorded in design 0065 §4b): the org-scoped login page shows the org name; the phase-1 implementation is the permanent shape, no C-strict rework. Phases 1–2 shipped (PRs #1674, #1676); §9 filed onward as design 0065.
**Date:** 2026-10-11
**Issues:** #1669 (this design's charter) · audit #1649 gap D4 (the lookup body-oracle accepted risk this design must not grow) · Epic 54 (US-54.1/54.3 — the subdomain routing this design completes).
**Binding rulings:** §8 = **C** (org name shown; recorded in [design 0065 §4b](0065_2026-10-11_org-provisioning-policy-ruling.md), relayed by the orchestrator on PR #1676) · §9 (provisioning policy) → **ruled in [design 0065](0065_2026-10-11_org-provisioning-policy-ruling.md)**: org registration is IdP-administered, and all orgs require OIDC (enforcement gap pinned; follow-up #1679).

---

## 1. The problem, in half a page

Each org can run its own OIDC IdP with auto-provision enabled and **no** claimed/verified email domains ("arbitrary-email orgs"). A **new** user of such an org has no routing mechanism at the root login page:

- `POST /auth/lookup` (`api/internal/handlers/login_discovery.go:90`) answers **membership only**: an existing user resolves to their one org; an unknown email returns the uniform `/?lookup=not_found` dead end.
- Email-domain inference on the login page (`frontend/src/pages/LoginPage.tsx:82`) works solely off `GET /auth/sso/domains`, which lists **DNS-verified** domains only (`pg_org_store.go:1812`, `WHERE array_length(verified_domains,1) IS NOT NULL`). Arbitrary-email orgs are invisible to it by construction.
- Per-org OIDC start (`GET /auth/sso/:orgSlug/start`, `org_sso.go:235`) is public and org-scoped, but the root surface never routes a **new** user to it: with N arbitrary-email orgs coexisting, nothing on the platform says "which org did you mean?" — today such users arrive only via a direct org URL or an invitation, and the org subdomain's own login page (Epic 54's wildcard ingress routes `*.<base>` → frontend) is **generic**: it does not know it is org-scoped and offers no org SSO button.

One pin before the options: auto-provision is **not** domain-gated anywhere today. `resolveUser` (`services/sso/sso.go:633`) gates on `cfg.AutoProvision` alone; domain verification only drives login-page button *inference*. An arbitrary-email org with `autoProvision=true` provisions any identity its IdP authenticates with a verified email claim (F8 holds; the IdP is the gatekeeper). The gap is therefore purely a **routing/discoverability** gap — and any fix must respect the enumeration posture: the always-200 lookup contract (`login_discovery.go:44-56`), and the acceptance criterion that the lookup oracle does not grow (#1669).

## 2. Flow map (verified this tree, @ b97dd185)

| Entry | What happens today | Files |
|---|---|---|
| Root login, email matches a **verified** domain | First-class "Sign in with {orgName}" button → `/auth/sso/:slug/start` | `LoginPage.tsx:82,222-235` |
| Root login, email valid, no domain match (and ≥1 verified domain instance-wide) | "Continue with email" → `POST /auth/lookup` → existing user: org subdomain/`start` redirect; **new user: `/?lookup=not_found` dead end** | `LoginPage.tsx:84,86-105`; `login_discovery.go:90-147` |
| Direct `GET /auth/sso/:orgSlug/start` | Works for any SSO-configured org; sets org-bound HMAC state cookie (`issueStateCookie`, payload `o`=orgID, `sso.go:732`); callback refuses org-swap (`sso.go:527-533`) | `org_sso.go:235-255` |
| Frontend shortcut `/sso/:orgSlug` | Spinner page → immediate redirect to `start` | `SSOStartPage.tsx` |
| Org subdomain `https://<slug>.<base>` | Wildcard ingress (`helm/templates/frontend-wildcard-ingress.yaml`) serves the **same generic SPA** + API under `/api`; login page has **zero org awareness** | `helm/…/frontend-wildcard-ingress.yaml:32-46` |
| Invitation link | Public detail view; **accept requires a JWT + email match** — effectively password/passkey users only; a brand-new SSO user cannot accept (chicken-and-egg) | `invitations.go:332,369,416-429` |
| Callback | `email_verified` mandatory (F8); existing-by-email or auto-provision (`AutoProvision` gate only); membership ensured; org-bound state cookie verified | `sso.go:513-614` |

Enumeration baseline (what is *already* publicly observable, all accepted):
- `/auth/sso/:slug/start` reveals "an SSO-configured org exists at this slug" (IdP redirect vs `sso=not_configured`) — existing accepted oracle. It does **not** reveal the org display name.
- `/auth/sso/domains` reveals verified domains + org slug/name — opt-in via DNS verification.
- `/auth/lookup` reveals existing-user membership by body (audit #1649 **D4**, accepted risk) — this design must not widen it.

Any new surface is scored against this baseline on two axes: the **existence oracle** (does an SSO org exist at slug X) and the **metadata reveal** (what extra — e.g. the display name — comes along). These are not interchangeable; §4C's score and §6.1's pins are stated in those terms.

## 3. Evaluation constraints

From #1669 and the audit: (i) the lookup's enumeration-safe always-200 contract is inviolable; (ii) the verified-domain listing stays public only for verified domains; (iii) state cookies are org-bound — any picker must flow the chosen org into the existing start flow (trivially satisfied: the start URL is org-parameterized); (iv) invitation UX exists; (v) existing users' routing unchanged; (vi) the lookup oracle does not grow. Scoring axes: privacy/enumeration, UX friction (esp. for the new user of an arbitrary-email org), implementation cost, blast radius.

## 4. Option analysis

### A — Org picker at discovery (root surface shows org choice)

As specified in the issue: when an email is unknown and ≥2 arbitrary-email orgs exist, the login page offers the org choice. The issue itself names the defect: showing "orgs the user could complete login for" *is* the leak — it enumerates the instance's arbitrary-email org directory on the public root page, unauthenticated, per-email. The suggested mitigation (reveal nothing until the user types the org name/slug with autocomplete) does not close it: autocomplete requires a prefix-search endpoint, which is a *finer-grained* org-directory oracle than anything the platform exposes today. Either form **grows the public oracle** and fails acceptance criterion (vi). The degenerate oracle-free variant — free-text slug entry, no listing, no autocomplete, link straight to the existing `start` URL — adds nothing the start URL doesn't already reveal, but it is no longer a picker; it demands the user already know their org slug, i.e. exactly option C's knowledge requirement expressed as a worse interface (typed exact-match vs a shareable URL), while keeping the root page the place org users thrash.

- Privacy: ✗ as a picker (new directory oracle); ✓ only in degenerate typed form.
- UX: best for genuinely-public directories — but arbitrary-email orgs are private organizations; being listed on a public login page is a feature *regression* for them. Verified-domain orgs already opted into listing via DNS; arbitrary-email orgs opted into nothing.
- Cost: moderate (new endpoint or lookup extension + picker UI + flow into start).
- Blast radius: public-surface growth (the one thing the constraints forbid).

**Verdict: reject.** The picker form violates the oracle constraint; the typed form is dominated by C.

### B — Invitation-only for arbitrary-email orgs

Auto-provision without a verified domain requires an invitation carrying the org binding; domain-verified orgs keep seamless routing. Disambiguation moves out-of-band (the invitation email *is* the org binding).

- Privacy: strongest — no new public surface, no listing, nothing revealed.
- UX: highest friction, and it is a **breaking change for the premise's own population**: orgs running arbitrary-email auto-provision today (several exist — that is why #1669 was filed) onboard new users via the direct org URL; B severs that path for every user without an invitation, effective on deploy. New-user onboarding at every such org stops until invitations are sent.
- Cost: **highest of the three.** The current invitation accept flow is password/passkey-shaped end-to-end (`Accept` requires a JWT; `InvitationPage.tsx` routes unauthenticated users to `/login` or `/register`). A new SSO-only user has no credentials and no account — carrying an invitation binding through the OIDC redirect means extending the state cookie payload or the start URL with the invitation token, teaching `HandleCallback` to consume it, and marking the invitation accepted at provision time. That is a new flow, not a wiring change.
- Blast radius: behavioral break for existing arbitrary-email orgs' onboarding; touches the SSO callback hot path; changes invitation semantics.
- Also: B conflates two questions. "#1669 asks *how does a new user reach the right org*" — B answers "*should arbitrary-email auto-provision be allowed at all*". That second question is real (anyone who learns a slug can today self-join such an org through its IdP — the org-admin's open-registration door), but it is a **provisioning-policy ruling**, per-org, with a migration story; bundling it into a routing fix makes both harder to rule on.

**Verdict: reject as the vehicle for #1669.** File the provisioning-policy question (invite-gated auto-provision as opt-in per-org hardening) as its own issue; owner rules on it independently (§9).

### C — Subdomain-first login (org-scoped entry points), additively scoped

De-emphasize the root page for new-user SSO disambiguation; each org's own entry point — its subdomain when routing is enabled, the existing `/sso/:orgSlug` shortcut otherwise — hosts a first-class org login. Disambiguation *is* the URL: `acme.app.example.com` vs `beta.app.example.com`. The user selects their org by going to it.

One reconciliation with the issue's phrasing: "root login serves personal/password accounts **only**" would remove the root SSO affordances and **violate acceptance criterion (v)** (existing users' routing unchanged). The additive scoping: the root page keeps everything it does today — membership lookup, verified-domain button — and merely stops *pretending* it can disambiguate new users among arbitrary-email orgs (it never could; it dead-ends). No existing behavior changes; the org surface is added.

- Privacy: **no new existence oracle; one bounded metadata delta, disclosed.** Org identity is revealed only on the org's own subdomain, which the user must already know (it arrived out-of-band — the same channel an invitation would use, but permanent and org-wide, not per-user). Precisely scored against the §2 baseline: subdomain→org resolution for the login button reveals **existence-at-slug exactly equal** to what `GET /auth/sso/<same-slug>/start` already reveals — no finer existence signal, no listing, no prefix search. The `orgContext` payload additionally carries the org **display name**, which `start` does not reveal (r1 review finding 2): a metadata delta, bounded by slug knowledge — enumerating names requires the same slug-guessing budget as probing `start`, and the name is low-entropy relative to the slug (an org at `acme` named something unrelated is rare, and verified-domain orgs already publish name+slug publicly). §8 offers the strict-parity variant (slug-only `orgContext`, generic button label) for the owner who wants zero delta; the default proposal carries the name because the button "Sign in with Acme" is the UX point of the surface.
- UX: the one-time knowledge requirement ("know your org's sign-in URL") is the minimum any privacy-preserving design can demand — B's invitations carry the same information per-user with ceremony on top. On the subdomain the experience is *better* than the root page has ever been for these users: "Sign in with Acme" first-class, above the password form.
- Cost: **lowest.** Epic 54 already built the substrate — wildcard ingress, `cookieDomain` session survival, `subdomainFor` — but stopped one step short: the subdomain's login page is generic. The completion is (a) `GET /auth/config` returns an optional `orgContext {slug, name}` when the request's Host is `<slug>.<baseDomain>` for an SSO-configured org (server-side Host parse against the *configured* baseDomain; fail-soft omit on unknown slug / no SSO / DB error — the endpoint's existing posture), and (b) `LoginPage` renders the first-class org SSO button from it, linking the *existing* start URL. No new endpoints, no schema, no cookie changes, no lookup changes, no SSO-flow changes.
- Blast radius: purely additive. `/auth/lookup` byte-identical; `/auth/sso/domains` untouched; state cookie and callback untouched (the button just links the existing org-parameterized start). The `orgContext` Host derivation is display-only — never an authz input; the flow's security remains path-resolved org + org-bound state cookie + callback org-swap refusal (`sso.go:527-533`). A spoofed Host yields at worst a wrong org *name* on a card — existence-at-slug equal to probing `start` for that slug, **plus the same disclosed name delta as §4C-privacy** (the name is the delta; no existing public surface reveals it for a non-verified-domain org). Consistent with the §8 ruling: under "C-strict" the spoofed-Host reveal collapses to exactly the `start` oracle.
- Architecture fit: this is the completion of Epic 54's own design intent (`values.yaml:1603`: "org-scoped login via wildcard subdomain routing" — currently realized only as a redirect target, never as a login surface).

**Verdict: recommend.**

| | A (picker) | A′ (typed entry) | B (invitation-only) | **C (subdomain-first)** |
|---|---|---|---|---|
| Existence-oracle growth | **yes** (directory) | no | no | **no** (set-equal to `start`) |
| Metadata reveal | org names (listed) | none | none | **org name, slug-bounded** (§8 strict variant: none) |
| New-user friction | none (listed) | know slug + type it | invitation per user | **know org URL (once, shared)** |
| Breaks existing orgs | no | no | **yes** (direct-URL onboarding) | **no** |
| Implementation cost | moderate | small | **highest** (invitation↔SSO flow) | **lowest-moderate** |
| Blast radius | public surface | small | SSO hot path + invitations | **additive** |

## 5. Recommendation

**Option C, additively scoped** (§4): org-scoped entry points become the blessed path for arbitrary-email orgs. Rationale, compressed:

1. It is the only option that satisfies all three #1669 acceptance criteria simultaneously — the new-user flow is completable and documentable, existing routing is untouched, and no new existence oracle is introduced (acceptance three is *unsatisfiable* for A-as-picker; B satisfies it but breaks criterion two's spirit by breaking new-user onboarding at existing orgs). C's one metadata delta — the org display name on `orgContext` — is disclosed and bounded (§4C), with a zero-delta variant on the §8 menu.
2. It has the smallest blast radius and the lowest cost because the substrate exists — Epic 54 built the routing and then stopped before building the surface.
3. It matches the codebase's enumeration posture: the user asserts org identity (by URL), the platform never asserts it for them (no listing).
4. A and B each answer an adjacent question better asked separately: A′ (typed org entry at the root dead end) is a compatible *optional* add-on (§7 phase 3) — oracle-free by the same argument; B's provisioning-policy core is a per-org hardening setting to file as its own issue (§9).

## 6. The selected design (detail)

**6.1 — `orgContext` on `GET /auth/config`.** When `auth.orgSubdomainRouting.baseDomain` is configured and the request Host equals `<label>.<baseDomain>` with exactly one label (reject deeper hosts and exact-base hosts), the handler resolves the org by slug and, **iff the org exists and has an SSO config**, includes `orgContext: { "slug": …, "name": … }` in the response (new optional field, `omitempty`). Every miss — routing disabled, non-matching host, unknown slug, org without SSO, soft-deleted org, DB error — omits the field and still returns 200 (fail-soft, matching the endpoint's existing settings-read posture). The SSO-configured check keeps the **existence oracle** exactly equal to the `start`-URL oracle: an org *without* SSO is not revealed, and the reveal set (SSO-configured orgs at known slugs) is identical. The display **name** in the payload is the one metadata delta over `start` (r1 finding 2) — accepted by this design as bounded (§4C) with the slug-only strict variant available in §8. The field is display-only; no authz anywhere consumes it.

**6.2 — `LoginPage` org awareness.** When `orgContext` is present: render a first-class "Sign in with {orgName}" button **above** the password/passkey forms, linking `ssoRedirectURL(orgContext.slug)` (the existing public start URL); keep all existing forms and affordances below it (org members may also be password users). When absent: byte-identical behavior to today. The email-driven verified-domain matched button keeps its root-page behavior unchanged; on an org subdomain it is governed by the org-slug comparison: **suppressed when the matched org differs from `orgContext.slug`** (the user is standing on one org's login surface; offering a *different* org's SSO button there is confusing and surfaces cross-org inference the root page already handles better — the cross-org user's path remains the root page), and **suppressed when it matches** (same org, one button — the `orgContext` button renders; no duplicate). Net rule: on an org subdomain exactly one org SSO button renders — the host org's.

**6.3 — Non-subdomain deployments.** `/sso/:orgSlug` (SSOStartPage → `start`) already exists as the org-scoped entry; §7 phase 2 documents it as the canonical org link for deploys without wildcard routing, alongside the subdomain form.

**6.4 — What deliberately does not change.** `POST /auth/lookup` (byte-identical contract and body), `GET /auth/sso/domains` (verified-only listing), the state cookie, `StartLogin`/`HandleCallback`, invitation flows, `subdomainFor`, the wildcard ingress.

## 7. Phased implementation plan

- **Phase 1 (this charter, upon ruling):** §6.1 + §6.2 — `orgContext` field + Host-derivation with fail-soft misses; LoginPage org button incl. the §6.2 subdomain suppression rule (matched-org ≠ host org → suppressed; matched-org = host org → single `orgContext` button); red-first pins: handler unit tests (subdomain host + SSO org → context; each miss branch → omitted + 200), router-level test with Host header, `LoginPage.test.tsx` (renders/links; absent → unchanged; both suppression arms), `AuthConfig` contract/openapi additive update. No docs beyond code comments in this phase.
- **Phase 2 (same charter, second PR):** documentation — `docs/operator/oidc-sso.md` gains an "Org-scoped login for arbitrary-email orgs" section (onboard by sharing `https://<slug>.<base>` or `/sso/<slug>`; known-gaps entry for #1669 closed), LoginPage `not_found` dead-end copy updated to point new users at their organisation's sign-in link ("contact your administrator"), values.yaml comment refresh.
- **Phase 3 (owner-ruling-gated, likely separate issues):** (a) A′ typed org entry at the root dead end — oracle-free free-text slug → `start` link, only if the owner wants a root-surface affordance; (b) §9's provisioning hardening. Not implemented without an explicit ruling.

## 8. Owner selection menu

> **RESOLVED 2026-10-11: the owner selected "C"** — `orgContext` carries `{slug, name}` as proposed; the named button is permanent (recorded in [design 0065 §4b](0065_2026-10-11_org-provisioning-policy-ruling.md)). The menu below is retained for the record.

A single comment selects; phases release as scoped above.

- **"C"** (or unchallenged survival of this recommendation through review) → phase 1 releases immediately, `orgContext` carries `{slug, name}` as proposed (the name delta of §4C is accepted).
- **"C-strict"** → phase 1 releases with the zero-delta variant: `orgContext` carries `{slug}` only and the button label is generic ("Sign in with your organisation") — forgoes the named button, keeps strict payload parity with the `start` oracle.
- **"A"** → I will treat that as A′ (the typed form) unless the owner explicitly accepts the directory oracle growth over the acceptance criterion — which then needs re-wording, since criterion three becomes unsatisfiable.
- **"B"** → breaks direct-URL onboarding at existing arbitrary-email orgs on deploy; I will ask for the migration ruling (flag? per-org setting? migration window?) and the invitation-through-SSO design will be written before any code.
- **"C + A′"** → phase 1–2 as above, A′ joins phase 3.

## 9. Filed separately (recommended, not this charter)

**Invite-gated auto-provision (per-org hardening):** today any org with `autoProvision=true` provisions whatever its IdP authenticates — for arbitrary-email orgs whose IdP allows open signup, the org's slug is an open-registration door into that org (and into instance user count). This is org-admin prerogative the same way `auth.registrationEnabled` is instance prerogative, but it deserves an explicit per-org setting ("require invitation when the org has no verified domain matching the user's email") and its own privacy/cost pass (invitation binding through the OIDC state cookie; accept-without-account). Filing as a new issue on ruling.

## 10. Test strategy (phase 1)

- **Handler/unit (red-first):** Host-derivation truth table — every §6.1 branch; `orgContext` never present when baseDomain unset; deep/exact hosts rejected.
- **Router (real wiring):** httptest against `/auth/config` with `Host: acme.app.example.com` and configured baseDomain, fake store with/without SSO config — response shape pinned including the fail-soft 200.
- **Enumeration pins:** `/auth/lookup` and `/auth/sso/domains` response contracts re-pinned unchanged in the same PR (red-first against accidental coupling). For `orgContext` itself, the pins assert the *existence* contract against the correct spec (r1 finding 2): field present iff host is `<slug>.<base>` ∧ org exists ∧ SSO configured — absent for no-SSO and unknown slugs alike (indistinguishable), absent when routing is disabled or the host doesn't match, and 200 in every miss branch. The name-delta is pinned as *intended* payload under the "C" selection (present: `{slug, name}`), or absent under "C-strict" — not as an oracle-parity claim.
- **Frontend (vitest):** orgContext → button present, href = `ssoRedirectURL(slug)`, positioned first-class; absent → DOM identical to pre-change snapshot cases with the verified-domain matched button keeping its root-page behavior; on an org subdomain, both §6.2 suppression arms pinned (matched org ≠ host org → no second button; matched org = host org → exactly one button, no duplicate).
- **Contract:** `pkg/types` contract test + `sdks/openapi.yaml` auth-config schema additive-only.
- **E2e-tier (honest scope):** true wildcard-host serving is ingress-level; pinned at router tier with Host headers, and noted as such — no live-ingress claim made from this workspace. In particular, **session-cookie behavior under real wildcard-subdomain routing is UNVERIFIED** (audit #1649's explicit UNVERIFIED list: "Cookie domain behavior under real wildcard-subdomain routing" — unit/integration/JWT-e2e coverage only). Phase 1 does not depend on it (`orgContext` is display-only; the SSO flow is untouched), but the phase-2 docs must not describe the cross-subdomain session experience as verified until a live-ingress pass says so.

## 11. Customer experience: goals, personas, journeys, coverage

(Added by owner directive on #1671: anchor the design in the workflows, not just the routing mechanics. Phase-1 surfaces referenced here are the implemented shapes in PR #1674.)

### 11.1 Platform goals, stated plainly

LLMSafeSpaces runs AI agents for people in isolated, persistent workspaces; organizations are the multi-tenant unit, each optionally with its own OIDC identity. The login experience must let five things happen without confusion:

1. **Self-serve signup** where the instance allows it (password/passkey at the root login).
2. **Org onboarding with an IdP**: an org admin wires OIDC once, then never hand-creates accounts.
3. **A member logging in**: type your email anywhere on the root surface, land in your org's flow.
4. **A new user joining a specific org**: arrive by that org's link, sign in, be *in* the org.
5. **An admin gating who gets in**: invitations (and, per §9's future hardening, invite-gated provisioning).

### 11.2 Personas × journeys (walked through the actual UI)

**(a) Personal-instance owner.** Root login, no orgs configured. Sees exactly today's page: password form (passkey if enabled), no SSO button, no org context. Phase 1 changes nothing — `orgContext` is absent on the root host (`OrgSlugFromHost` returns `""`), and the button block does not render. **Sanity: one obvious path, unchanged.**

**(b) Org admin, first-time OIDC + domains + invitations setup.** Org settings → SSO tab: discovery URL, client ID/secret, claimed domains, auto-provision toggle (existing UI, untouched). If they want seamless root-page discovery they verify a domain via DNS TXT — that is the deliberate opt-in to public listing. If they run arbitrary-email (no domains), phase 2's doc tells them the onboarding artifact to share: `https://<slug>.<base>` (or `/sso/<slug>` without wildcard routing). Invitations ride the same screen's neighborhood — org → members → invite by email (existing UX): for password/passkey newcomers the emailed link is the full journey (accept → member), and under phase 1 the admin's simplest arbitrary-email onboarding remains "send them the org link"; the invitation-vs-link choice and its §9 hardening future are the coverage matrix's (e) column. **Sanity: the admin's mental model — "my org's link is how my people arrive; invitations are for named individuals" — matches the mechanism.**

**(c) Existing org member at the root login.** Types email → "Continue with email" → `POST /auth/lookup` → found → browser redirected to their org's subdomain. **Before phase 1 that landing was a generic login page with no org awareness — the Epic 54 half-realization. After phase 1** (this is a quiet completion the CX walk surfaces): the subdomain login shows "Sign in with {org name}" first-class. One click → IdP → callback → session cookie (`Domain=.base` when configured) → `/chat`. **Sanity: the dead-feeling redirect target became the journey's front door.**

**(d) New user, email at a DNS-verified domain.** Root login, types email: the verified-domain match shows "Sign in with {org name}" right there (existing behavior, untouched). Click → `start` → IdP → callback → auto-provisioned, member, `/chat`. **Sanity: seamless case stays seamless; phase 1 adds nothing and removes nothing here.**

**(e) THE GAP PERSONA — new user, arbitrary email, wants to join a SPECIFIC org.** The journey #1669 is about, walked end-to-end through phase 1's surface:

1. *Arrival:* a colleague or the org admin shared `https://acme.safespaces.dev` (or, on a deploy without wildcard routing, `https://app.safespaces.dev/sso/acme`, which hops straight to step 3). Alternatively an invitation link — but for a brand-new SSO user the invitation path is awkward today (accept requires an account first; §4B), so the org link is the blessed artifact.
2. *What `acme.safespaces.dev` shows them:* the login page fetches `/auth/config` from its own host; the API sees `Host: acme.safespaces.dev`, baseDomain `safespaces.dev`, resolves acme — exists, has SSO — and returns `orgContext: {slug: "acme", name: "Acme Corp"}`. The page renders **"Sign in with Acme Corp"** as the first button, above the password/passkey forms, with the caption *"or sign in with a personal account"* beneath it (the password form stays: org members can also be password users, and a lost visitor can still use the root page). If the org's name were withheld (C-strict), the button reads *"Sign in with your organisation"*.
3. *Suppression from this journey:* if the visitor types an email whose domain matches some org's *verified* domain, the email-matched button does **not** appear — even if it names Acme itself. On Acme's own doorstep there is exactly one org door (§6.2). A visitor whose email matches a *different* org's verified domain and who intends that other org is one root-page visit away, where that matching already works (persona d) — the subdomain does not try to be a directory.
4. *Click:* → `GET /api/v1/auth/sso/acme/start` → the org's IdP. The user authenticates with whatever email their IdP knows (arbitrary — that is the point); `email_verified` must be true (F8) or the flow stops with a clean error.
5. *After the IdP:* callback → auto-provision (org's `autoProvision` on) → membership ensured → **lands on the app (`/chat`) at the configured frontend URL** — the session cookie carries `Domain=.safespaces.dev` (US-54.3) so it is valid on the subdomain too. Honest note: the post-callback landing host is the *root* frontend URL by existing design (`frontendRedirectWithSuccess`), not `acme.…`; org membership is carried server-side (their workspaces are org-scoped), not by the login host. Phase 1 does not change this, and it matches persona (c)'s landing. (Live-ingress cookie behavior remains audit-#1649-UNVERIFIED — §10.)
6. *Second visit:* back to `acme.safespaces.dev` — same button, one click, done. The org link is permanent, shareable, and org-wide: the one-time knowledge requirement (§4C) is paid once by the org, not per user.

**Sanity: one obvious path (the shared link → one button → IdP → in), no dead ends on the org surface, no guessing.** The dead end that remains is at the *root* page for this persona (lookup → `not_found`) — phase 2's copy improvement points them at "contact your administrator for your organisation's sign-in link"; the link itself is the answer.

**(f) New user who does NOT know which org they want.** Explicitly out of scope, by the enumeration stance: the platform will not enumerate its orgs to an unauthenticated visitor, so "show me the orgs" cannot exist on the public surface (§4A). Such a user must obtain the org's identity out-of-band (ask a colleague/admin) — the same channel that would deliver an invitation under option B, minus the per-user ceremony. This is a deliberate non-goal, not an oversight.

### 11.3 Coverage matrix

| Persona | Entry: root login | Entry: org subdomain | Entry: `/sso/:slug` link | Entry: invitation link |
|---|---|---|---|---|
| (a) personal owner | existing (password/passkey) | n/a (no org) | n/a | n/a |
| (b) org admin setup | existing (SSO tab; verified-domain DNS opt-in) | existing (admin UI org-scoped) | n/a | existing (create invitations) |
| (c) existing member | existing (lookup → subdomain) **+ phase 1 completes the landing** (org button on arrival) | phase 1 (org button first-class) | existing (auto-start) | n/a (already a member) |
| (d) verified-domain newcomer | existing (domain-matched button) | phase 1 (host-org button; email-matched suppressed §6.2) | existing (auto-start) | existing (password users) |
| (e) arbitrary-email newcomer | **not covered — by design** (lookup dead-ends; phase 2 copy points to org link) | **phase 1 (the blessed path)** | existing (auto-start; the non-wildcard equivalent) | existing-but-awkward (accept needs an account first; §9 hardening would fix) |
| (f) knows-no-org visitor | **explicitly not covered** (enumeration stance, §4A/§11.2-f) | — | — | — |

Gaps made visible rather than implicit: the (e)-root cell (phase-2 copy only), the (e)-invitation cell (the JWT chicken-and-egg — §9's invitation-through-SSO hardening would close it), and the entire (f) row (permanent, by policy).

### 11.4 Checkpoint against phase-1 surfaces (PR #1674, unmerged at time of writing)

Persona (e)'s walk above is written against the phase-1 branch's implemented code (PR #1674, not yet merged), not the design text: button label `Sign in with {name}` with the `your organisation` fallback, first-class placement above the forms, the personal-account caption, and the §6.2 suppression (both arms) — each matches PR #1674's `LoginPage.tsx` and is pinned by its vitest cases (22/22, including the two suppression arms and the generic-label fallback). The walk surfaces **no contradiction**: the implemented surfaces render exactly the journey described, and the one journey characteristic that could surprise (post-callback landing on the root frontend URL) is pre-existing behavior shared with persona (c), not a phase-1 regression. No amendment to #1674 required.
