# Worklog: design 0051 — US-0 preconditions, supervisor invariant, D6.1 rollback

**Date:** 2026-08-20
**Session:** Close the four real gaps from the post-merge design assessment (self-review + owner prompt). No code.
**Status:** Complete

---

## Objective

The v6 assessment surfaced five gaps/weaknesses. Triage: four are real and doc-level (control-socket protocol unspecified; secrets-env uid-crossing undefined; no rollback story; no supervisor scope invariant); one is owned (gVisor V9 — validation item + fallback clause already exist); sequencing/shelve is the owner's call; the MCP carve-out weakness is upstream-inherent and ledgered. The upstream opencode asks are explicitly NOT being filed this session (owner instruction).

## Work Completed

- **US-0 preconditions** (new, Phasing §8): no Phase-2 implementation before (1) the control-socket protocol is specified+reviewed — message shapes, versioning, unknown-method rejection, restart idempotency, and the capability-equivalence rule stated in-spec so nobody "fixes" the unauthenticated socket into holding secrets; (2) the secrets-env crossing decision — option (a) IPC handoff at spawn preferred, option (b) uid-1000-readable copy acceptable only if ledgered as a residual; (3) D6.1 reviewed.
- **Supervisor scope invariant** (D1): supervise-* is plumbing (spawn/reap/signal/status/metrics-forward); it runs INSIDE the snoopable space, so any capability growth is wrong-sided by definition; reviewers reject growing PRs. This is the rule that prevents the PID-1 toehold from accreting features.
- **D6.1 rollback** (new): mixed-generation convergence per the #933 pattern — extra Secret keys inert to legacy readers; restartGeneration drain, not force-recreate; both mux credentials valid during the window (two-credential table is rollback-compatible by construction); one-directional file relocation re-owned by reset()/re-materialize. Rollback is EXERCISED in US-5's canary (on→validate→off→validate→on), not asserted.

## Key Decisions

1. Spec-as-precondition, not spec-now: writing the full IPC protocol before implementation pressure exists would reproduce the unreviewed-0050 failure mode (detailed spec, zero adversarial rounds). US-0 names the decisions; US-0.1 does the specifying under review.
2. secrets-env preference recorded (IPC handoff) but the copy option stays live with a ledger requirement — measurement (US-4) may legitimately flip it.
3. Rollback inherits #933's proven convergence primitives rather than inventing new ones.

## Tests Run

None (design doc). Structural checks: all three sections present, cross-references (§D1 table, D6.1, US numbering) consistent.

## Files Modified

- `design/0051_2026-08-18_agentd-uid-separation.md`
- `worklogs/NNNN_2026-08-20_design-0051-us0-preconditions.md` (this file)
