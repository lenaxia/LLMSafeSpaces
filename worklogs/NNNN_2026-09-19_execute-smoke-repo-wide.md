# Worklog: Repo-wide ExecuteSmoke closure — 7 nightly harness scripts under shared shims — Refs #1474/#1480

**Date:** 2026-09-19
**Session:** Orchestrator-assigned residual lane on branch `fix/execute-smoke-repo-wide` (worktree wt-1453): extend #1480's shared execution-smoke machinery to the remaining nightly-registered harness scripts (1342 sequenced after #1478 — excluded here).
**Status:** Complete

---

## Objective

Every nightly-registered harness script must demonstrably EXECUTE under shims to a row verdict — closing the latent-death class (two corpses found in two days prior). Per the orchestrator: watch for more never-executable corpses; behavioral fixes beyond smoke coverage get STOPPED-and-REPORTED, not fixed in-lane.

---

## Work Completed

### Corpse #3 of the class: 1455 carried the identical never-executable api()

`issue-1455-scriptenv-e2e.sh` had the pre-#1474-r4 `api()` verbatim (status side-channel + 5 capture callers) — `api_status: unbound variable` at its first status read. **Judgment call, documented for reviewer veto**: this is NOT a new behavioral decision — it is mechanical application of the no-subshell contract already ruled through review in #1474 r4 and applied to 1410/1417/1452 (helper rewrite + 5 call-site conversions via the same depth-counting transformer; no other line touched). Flagged prominently in the PR body per the STOP-and-report spirit; if the reviewer rules it out of scope, it reverts cleanly (its own commit).

### Shim extensions (driven empirically, one real traversal failure each)

- `{.status.secretsDelivery.spawnedRev}` → a rev string (us-70 revisions' `secrets_converged`; empty = never healthy).
- `{.spec.containers[0].name}` → `workspace` (dev-preview's `runtime_container`).
- Existing surfaces (auth, postgres pod, secret data, podName, phase, registry exec, /runs 202, -o/-w) sufficed for the rest.

### Smoke suite

`local/e2e_smoke_repo_wide_test.go` — `TestHarnessExecuteSmoke_RepoWide`, table-driven over the 7 scripts (test.sh, us-68-attachments, us-70-secret-delivery, 1455-scriptenv, us-70-revisions, dev-preview-tunnel, us-63-v2-behavior). Each asserts: row-verdict death (the shared `✗` die marker + non-zero exit — these scripts use die-on-row conventions, and the generic shim deterministically trips a semantic assertion) or its script-specific green gate; no abort signatures; no `whsec_` leakage. us-70-secret-delivery runs with its scale/suspend knobs dialed down via env (SUSPEND_SECONDS=1, RESUME_SCALE=1, RESUME_SCALE_TIMEOUT_S=5, RECONCILE_INTERVAL_S=1 — all pre-existing env overrides).

Empirical traversal outcomes (deterministic under shims): 1455 dies at R2 setup (R1 completes); dev-preview at #1333-A; revisions at REV-0; us-68 at E2; test.sh at the pvcName assertion; us-70-secret-delivery at AC-1; us-63 at US-63.3 — each after executing real harness logic (workspace seeding, API flows, polls), zero secret leakage anywhere.

---

## Key Decisions

1. **1455 api() fix in-lane** — ruled-pattern application, isolated commit, flagged for veto (see above).
2. **`✗` as the universal fail marker** for die-on-row scripts — traversal evidence without over-pinning WHICH row fires; abort signatures banned separately so unbound/command deaths cannot pass.
3. **us-70 knobs via env only** — no script edits for speed.

---

## Blockers

None. Not reported as blockers: us-70-secret-delivery's deep chaos legs (suspend/resume scale) truncate at AC-1 under shims — full coverage remains the nightly's job; the smoke proves executability.

---

## Tests Run

- `go test -run ExecuteSmoke -v ./local/` — 4 tests PASS (repo-wide suite 2.9s + the three originals).
- `go test -timeout 600s -count=1 ./local/` — ok (21.5s full package: pins incl. 1455's, jq-compile, registration).
- `go vet`, gofmt clean. Memory directive: local package only.

---

## Next Steps

- Review loop to APPROVED; orchestrator merges.
- 1342's smoke remains sequenced after #1478 lands (orchestrator holds the ordering).

---

## Files Modified

- `local/issue-1455-scriptenv-e2e.sh` — the ruled no-subshell api() contract (corpse #3; own commit, veto-able)
- `local/e2e_smoke_helpers_test.go` — two new kubectl shim rules
- `local/e2e_smoke_repo_wide_test.go` — new: the 7-script table smoke
- `worklogs/NNNN_2026-09-19_execute-smoke-repo-wide.md` — this worklog

## Review Round 1 (depth not pinned — three mutations proved it; fixed)

The reviewer mutation-tested my suite: (1) breaking wait_phase's comparison in the SHARED lib (5 of 7 rows depend on it) left the suite green; (2) deleting either new kubectl shim rule left it green; (3) test.sh's empty green marker accepted any exit-0. Root cause: any-depth ✗ deaths. Fix: a depthMarker table column pinning each script's deterministic pre-death boundary (for revisions, the death line itself — an earlier "REV-1 setup" marker sat BEFORE the spawnedRev shim-rule dependency and still passed mutation 2). Mutation-verified locally after the fix: wait_phase break → 1455 row FAIL; spawnedRev-rule deletion → revisions row FAIL; containers-rule deletion → dev-preview row FAIL. test.sh's empty green marker replaced by the "Workspace reached phase=Active" depth pin (its pvcName death stays covered by ✗+exit). Non-blocking noted: banned-signature phrases could false-positive on future legitimate log lines — left as-is per the reviewer's "if it ever bites". Worklog title typo fixed (shokes→shims).
