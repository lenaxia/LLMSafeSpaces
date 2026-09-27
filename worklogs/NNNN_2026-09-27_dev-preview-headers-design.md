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

## Tests Run

None (design doc); §8 defines the implementation PR's test contract (validation tables, director unit tests, gate-ordering pins, the pod-boundary source-scan pin, the e2e arm with a header-demanding fixture service).

## Next Steps

Design review rounds; on approval, §10's seven-step rollout (each step green independently; the config's absence is the flag).

## Files Modified

- `design/0062_2026-09-26_dev-preview-headers.md` — NEW (the design)
- `worklogs/NNNN_2026-09-27_dev-preview-headers-design.md` — this worklog
