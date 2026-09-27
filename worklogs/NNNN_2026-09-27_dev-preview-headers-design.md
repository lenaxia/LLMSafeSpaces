# Worklog: #1583 — the dev-preview header configuration design doc (0062)

**Date:** 2026-09-27
**Session:** The design-first lane for dev-preview header configuration (inject via secretKeyRef + forward via allowlist) — design/0062 authored per the 0060/0061 convention, grounded file:line in the existing proxy chain
**Status:** Complete (design stage; implementation follows owner approval)

---

## Objective

Settle the approved framing into a reviewable design: exact CRD v1 shape + migration, settings/API surface + authz, the injection component (and its interaction with the 503 gate and the origin/path topology modes), multi-service scoping (recommend), and Secret lifecycle (rotation, deletion).

## Work Completed

- **The proxy chain surveyed and grounded**: the two-hop path (API `HandleDevPreview` → agentd `devPreviewHandler` → the service); G34's fixed allowlist (`proxy_helpers.go:25`); the agentd hop's pass-through-minus-Authorization; the 503 gate stack; both topology modes funneling through the one director; the relay-handoff Secret-read precedent (API clientset, workspace namespace).
- **design/0062 authored** — the shape: headers as a SIBLING list under `spec.networkAccess` (the bool toggle untouched → zero migration, no conversion webhook); injection LAST in the director (config can't be shadowed by caller input, callers can't be confused with injection); the reserved-header denylist as the mechanism making that ordering safe; per-request Secret resolution (no cache → rotation instant, load bounded by maxConns); loud 502-naming-the-Secret on missing config (the #1580 lesson — skip-on-missing is the silent-degradation class); gate ordering pinned (disabled/kill-switch 503 BEFORE any Secret read); forward's edge-trust boundary documented with the operator caveat; multi-port DEFERRED with the additive path recorded; feature_status's new entry on the count-pair projection (the basis question worked through honestly: the in-pod tool cannot read the DTO under D3 — counts project, names/refs/values never).
- **A self-caught design error, fixed before review**: the first §7 draft proposed the count pair as pod env, then "corrected" itself mid-document to a DTO read — which D3 makes impossible for the in-pod tool. Rewritten to the honest resolution (counts project; the invariant is values-never/names-never-in-pod/counts-are-fine) rather than leaving correction theater in the record.

## Key Decisions

- Sibling-not-nested CRD shape (zero migration); injection at the API boundary only; per-request Secret reads; loud 502 failure semantics; forward-allowlist extension of G34 with the identity-header trust assumption documented; counts-only projection for the in-pod inspector.

## Blockers

None — awaiting design review.

### r1 review round (the critical security finding, closed in-design)

The review's critical finding: the r0 shape (user-supplied `secretKeyRef`) was a **one-PUT arbitrary-Secret-read primitive** — the shared namespace houses `master-secret` (the KEK root), `jwt-secret`, and every tenant's workspace password; the API already holds unscoped Secret reads; so `inject: [{secretKeyRef: {name: "jwt-secret"}}]` would deliver the platform's signing key to the caller's own preview browser. Closed by the **mint model** (§5.0): the PUT carries values write-only, the service mints labeled+owner-referenced Secrets, resolve-time mint-name + workspace-label checks backstop forged specs; user-supplied references no longer exist. Plus the inject/forward denylist split (`X-Forwarded-User` forward-only), the injection position pinned against the P0-2 WS block, and SecretKeySelector's no-namespace-field reconciliation.

### r2 review round (feasibility + identifier pins)

- **Rotation was RBAC-infeasible as written**: the API holds `update`/`patch` only on three named Secrets (the chart documents `resourceNames` cannot express per-workspace prefixes) — pinned to **delete + recreate** (both verbs held broadly), the millisecond Get-gap self-healing.
- **The minted-name identifier pinned to the CRD UUID** (`ObjectMeta.Name`) — the human display name is non-unique/mutable/unvalidated and would reopen a same-name collision channel.
- **The 502 mechanics corrected**: `Rewrite` has no error return — resolution happens at HANDLER level (after every gate), the director only applies resolved values; the ordering invariant restated at its true location.
- Carried r0 staleness fixed (§2's DTO-vs-CRD gate claim, §3's dead namespace bullet, §9's dangling row); §4's gate-order list corrected (final r4 form: empty-id 400 :112-116 → kill-switch 503 :118-121 → port 400 :131-147 → 308 :164-172 → wsGetter-nil 503 :174-177 → 404 :179-183 → phase/PodIP 503 :185-192 → flag 503 :194-197 → pwProvider 500 :199-203 → conn-cap 429 :205-212 → handler-level resolution → proxy); citation drifts (Rewrite 248-307; bootstrap 612-614).

### r3 review round

- r3 closed all five r2 findings (verified against source by the reviewer): the §3 mint/replace wording, the §8 rotation-path pin (delete+recreate asserted via UID change + the no-update-verb RBAC assertion), the identifier-disjointness negative, the sha8-collision triad (§5.0/§6/§8), and §4's gate-order list / in-handler kill-switch correction.
### r4 review round

- The two citation residuals: the "complete list" label gained the empty-workspaceID 400 (:112-116) and the port-parse span widened to :131-147 — a list labeled complete must actually be.

### r5 review round

- Two minors: the worklog's own five-count enumeration completed (the fifth was §4's gate-order/kill-switch fix — this very lane's subject); §4's second invariant clause scoped to HEADER Secrets (the password provider's cache-miss fetch legitimately precedes the 429 — it is the tunnel's credential, not header configuration).

### r6 review round

- The IDENTICAL unscoped claim r5 fixed at :155 had survived six lines above (:149) — the instance was fixed, not the class; :149 now carries the scoped form.

### r7 review round

- The header-staleness class fixed AT THE CLASS LEVEL: per-round subsections (this structure) so appending round N+1 can never stale a header again.
- §5.2's example body had conflated the forged-spec and manual-deletion failure modes; replaced with the DELIBERATELY UNIFORM body (a per-class reason would hand a forged-spec prober an existence oracle over namespace Secrets) — the uniformity now stated as intentional, the WARN log carrying the server-side class.

## Tests Run

None (design doc); §8 defines the implementation PR's test contract (validation tables, director unit tests, gate-ordering pins, the pod-boundary source-scan pin, the e2e arm with a header-demanding fixture service).

## Next Steps

Design review rounds; on approval, §10's seven-step rollout (each step green independently; the config's absence is the flag).

## Files Modified

- `design/0062_2026-09-26_dev-preview-headers.md` — NEW (the design)
- `worklogs/NNNN_2026-09-27_dev-preview-headers-design.md` — this worklog
