# Worklog: Epic 70 close-out — flake hardening, operator runbook, threat-model note

**Date:** 2026-09-01
**Session:** Post-US-70.5 verification pass answering "is Epic 70 done?" — surfaced and closed the remaining DoD items: main-CI flake (Epic 68 attachments spec), the missing secret-delivery operator runbook, the missing 0027-lineage threat-model note, and the stale epic story row.
**Status:** Complete

---

## Objective

Verify Epic 70 against its own DoD (issue #1158) instead of declaring done off merged PRs; fix everything in reach.

---

## Work Completed

### Verification findings (evidence-based)

- #1209 closed by #1224 (merged `e6799d3a`); all stories + R2b landed.
- **main CI red at merge**: `attachments.spec.ts` (Epic 68, mocked-backend Playwright) — but it also failed on main at 17:42Z (docs-only commit) and 22:30Z (#1222), i.e. **pre-existing flake, 3 failures today, predating the demolition**. Same spec family failed the 08-31 nightly (kind cluster, Epic 68 rows). Local repro impossible (sandbox lacks libglib for Chromium; no root). Failure signature: 5s `toBeVisible` chip-budgets expiring, different subtest per run — timing variance (Vite cold-compile stall pattern), not a deterministic bug.
- **Nightly broken**: 09-01 failed at `Build and load images` (infra step, pre-test); 08-31 failed at the Epic 68 attachment rows. No green nightly exists post-demolition.
- **DoD gaps**: operator runbook missing; 0027-lineage threat-model note missing; epic story row stale ("in review").
- README-LLM relay-config rows were already rewritten inside #1224 (verified via `git show`).

### Fixes

- `frontend/tests/e2e/attachments.spec.ts`: the four 5s chip-visibility/count budgets → 15s (matches the file's own `gotoChat` 10s+ precedent; asserts-late not asserts-never — a functional failure still fails, just after the cold-compile window).
- `docs/runbooks/secret-delivery.md` (new): signal inventory (CRD `secretsDelivery`, healthz `delivery:"v2"`, the four metric families), divergence-reason → action table (codes verified against source: `legacy_format`/`missing_rev`/`stale_seq`/`notify_failed`, `spawn_env_*`/`spawn_files_*`, `pull_failed`/`pull_unauthorized`, `dek_unwrap_failed`), force-reconcile paths (API manual route, agent-side `secrets_resync` MCP tool, the loop), alert triage table, keyrewrap specifics, rollback (code-only demolition; helm rollback + CRD note).
- `design/0027_2026-05-24_security-policy-v21.md` Appendix E: threat-model deltas (push-body elimination, terminal verification, durable-unwrap surface shrunk, replay artifact removed, non-sensitive fleet marker).
- Epic README: US-70.5 row → landed (#1224) + close-out pointer.

---

## Key Decisions

1. **Timeout hardening over refactoring** the attachments spec — failure distribution (different subtest per run, same tree pass/fail variance, retry passes) indicates budget marginality; 15s aligns with the file's existing budgets.
2. **Runback drill documented, not executed here** — no kind cluster in this environment; the runbook references the pool's chaos/partition legs as the machine-verified equivalent and states the manual drill (helm rollback + canary force-reconcile). Flagged as remaining operator action.

---

## Blockers

None in-repo. Evidence still accruing at session end: main-CI rerun (in_progress), US-70 pool run 22:30Z (in_progress — the "pool re-dispatch pending" note on US-70.0), nightly needs a green post-demolition run (the 09-01 failure was a build-infra step).

---

## Tests Run

`attachments.spec.ts` cannot run in this sandbox (missing libglib — environmental, documented above). Docs/markdown only otherwise; CI validates the spec change.

---

## Next Steps

1. Land this PR; confirm main CI green (rerun was already dispatched).
2. Watch the pool run; update the US-70.0 row ("pool re-dispatch pending" → outcome).
3. Re-dispatch the nightly; triage the `Build and load images` infra failure if it repeats.
4. When a green nightly exists post-demolition + pool evidence is in: close #1158 with a DoD→evidence map comment.
5. Fleet-evidence-gated retires (not now): W15 legacy bare-array response, W1 multi-version window.

---

## Files Modified

`frontend/tests/e2e/attachments.spec.ts`, `docs/runbooks/secret-delivery.md` (new), `design/0027_2026-05-24_security-policy-v21.md`, `design/stories/epic-70-secret-delivery-v2/README.md`, this worklog.
