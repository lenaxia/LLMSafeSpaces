# Worklog: design 0061 — relay-only delivery hardening (four mechanisms)

**Date:** 2026-09-23
**Session:** The post-incident architecture lane. #1548 (split-brain: API emission live while controller staging never ran — provenance drift, silent zero-credential delivery) + #1546 (the namespace-posture RBAC defects + the inferenceRelay coexistence gap) read in full; the owner/orchestrator convergence (two trimming passes) binds the design to exactly four mechanisms.
**Status:** Complete (design stage, HOLD — owner reads before implementation)

---

## Objective

Write design/0061 at the 0058 level: the four binding mechanisms (crash-loud arming; fail-open fallback + counter; the posture gate; the CRD condition), the rejected alternatives with their reasons, and the two dispositions (#1546's coexistence gap; the Epic-42 values-drift ops note).

## Work Completed

- `design/0061_2026-09-23_relay-delivery-hardening.md`: problem anatomy from both evidence chains (the common shape: every failure quiet at its moment); the four mechanisms each specified to implementation depth (M1's armed-state definition reusing SetupRelayStaging's existing conjunction + exit 85 in the 81–84 doctrine ladder; M2's precise not-ready semantics — handoff-Secret presence at the builder's decision point, per-workspace/per-provider, counter labels, audit action, the migration→strict flip criteria; M3's four assertions + the #1546 fixes carried in order with the gate-or-grant DECISION recorded; M4 as the honestly-marked preference item); the binding rejected-alternatives table; the dispositions; acceptance criteria; three open questions for review.
- README-LLM.md: the design registered in the relay section with the one-paragraph summary.

## Key Decisions

1. M1 defines NO new armed-state — the existing startup-guard conjunction, made un-skippable; the enable line is the contract M3 asserts cluster-side.
2. M2 leans direct handoff-Secret reads at batch time (open question 1) — one source of truth, no push path.
3. M3's Defect-3 resolution is GRANT not gate (the refresher's RBAC becomes unconditional; the flag governs cadence) — recorded as a decision for review, with the smaller-diff alternative named.
4. M4 reuses the existing CredentialsStaged condition fed by the batch outcome — no new type, no new surface.

## Assumptions → validation record

- The parity PR exists on its branch (not yet merged) — M1's relationship to it is written against its described behavior; the story reconciles on merge.
- Exit-code ladder 81–84 verified in constants.go/overlay comments — 85 is free.
- SetupRelayStaging's 30s guard + enable line verified at controller.go:120-167.

## Blockers

None — the two trimming rulings and the four-mechanism boundary are recorded as binding; the design does not re-litigate.

## Next Steps

1. Reviewer iteration to APPROVED; owner final read; then the implementation stories (M3's #1546 fixes first — the gate's first catch).
2. Reconcile with the provenance-parity PR when it merges (one code, 85, both classes).

## Files Modified

- `design/0061_2026-09-23_relay-delivery-hardening.md` (new)
- `README-LLM.md` (the design registered)
