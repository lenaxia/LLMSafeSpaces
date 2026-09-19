# Worklog: Nightly 35437562027 follow-up — harness workspace cleanup on all exit paths (#1456 adjudication)

**Date:** 2026-09-19
**Session:** Fix the two leaked-workspace sources the nightly census exposed (attachment step's WS_A/WS_B standing after exit; test.sh's API-created disposable sandbox never deleted), per the orchestrator's adjudication of the run-35437562027 triage. Hygiene-before-capacity: frees ≈2 pods' CPU requests on the 1-node kind runner — the exact margin us-70 AC-1c was missing. Capacity levers intentionally HELD (only if tomorrow's run still fails AC-1c with the leaks fixed).
**Status:** Complete — PR open, iterating review

---

## Objective

Every workspace pod standing when a nightly step ends is CPU margin stolen from every later step. Run 35437562027's failure census (5 Running workspace pods + system pods on one kind node; AC-1c's pod `FailedScheduling: Insufficient cpu` for 4m8s) showed two harness-owned leaks:

1. `local/us-68-attachments-e2e.sh` exits 0 (sidecar skip AND green completion) without deleting WS_A/WS_B — pre-existing, but only load-bearing since #1463 let the job proceed past this step.
2. `local/test.sh` Test 8a's disposable sandbox (census pod `77cf2232-…`, Running 6m28s at failure): the create-response extraction read the DISPLAY name (`d.get('name')` → "disposable-e2e") instead of the CR id (uuid.New() per the API create contract, `pkg/types.Workspace` has both `id` and `name`), so `DELETE /api/v1/workspaces/disposable-e2e` targeted nothing; the non-204 path only warns.

---

## Work Completed

- `local/us-68-attachments-e2e.sh`: `cleanup()` (the existing EXIT trap) now deletes WS_A/WS_B `--ignore-not-found --wait=false`, guarded `|| true` — `--wait=false` is the fire-and-forget part: kubectl's DEFAULT `--wait=true` blocks on finalizers, and a wedged finalizer must never stall the EXIT trap (review r1's blocking catch — my first draft omitted the flag while claiming fire-and-forget). The seed-time pre-clean got `--wait=false` too (same hang class, pre-existing). `${WS_A:-}` guards keep the die-before-seed path `set -u`-safe. Port-forward teardown unchanged.
- `local/test.sh`: extraction yields `d.get('id')` only (an id-less response yields empty and dies loudly at the existing guard — the display-name and dead `metadata.name` fallbacks dropped, review r1's non-blocking note adopted); a kubectl backstop delete (`--wait=false`) of `${DISPOSABLE_SB}` follows the API DELETE attempt (same hygiene pattern as Test 13's WORKSPACE_NAME cleanup).
- ExecuteSmoke: the shared kubectl shim (`e2e_smoke_helpers_test.go`) now (a) appends every invocation to `$SMOKE_KC_TRACE` when set (inert otherwise), (b) answers the us-68 sidecar gate's combined both-lists jsonpath with `workspace agentd` — the nightly's actual mode — so the attachments smoke now traverses the SIDECAR path (dies at the D1 clean-fail assertion under the shims, a semantic row death per the smoke philosophy) instead of falling through to E11.
- Pins (TDD red→green): structural (cleanup trap contents; extraction/backstop presence + ordering), executable (real `cleanup()` against a tracing fake kc, incl. the unset-vars early-death path; the real extraction against a `types.Workspace`-shaped fixture, incl. the id-less loud-die case), and `TestUS68CleanupExecuteSmoke` — the REAL script under the shared shims with the trace armed: the EXIT trap must fire the deletes after the shim-driven death (≥2 deletes per workspace in the trace: seed pre-clean + trap).

## Key Decisions

1. **Fire-and-forget deletion in the trap via `--wait=false`**: kubectl delete's DEFAULT is `--wait=true`, which blocks on the Workspace finalizer — a wedged finalizer must never stall the EXIT trap (r1's blocking correction of this very decision's first draft). Teardown starts immediately; the next step's first seed is ~30s later.
2. **`id`-only extraction in test.sh** rather than "fixing" the DELETE route: the API contract is id-addressed; the display name was simply the wrong field to extract. No fallbacks — an id-less response dies loudly at the existing guard. Backstop covers any residual async/warn path.
3. **Shim tracing is opt-in via env** (`SMOKE_KC_TRACE` unset → `/dev/null`): zero behavior change for the seven existing repo-wide smoke rows; verified by the full `./local/` package passing.
4. **Sidecar-shaped shim answer** makes the smoke representative of the nightly's actual mode (agentdSidecar.enabled=true) — the gate's detection path is now smoke-executed, not just unit-pinned.

### Assumptions stated and validated (Rule 7)

- Workspace CR deletion promptly starts pod teardown (frees node CPU requests) — controller finalizer behavior, consistent with the census (e2e00000-0001 terminating within seconds of Test-13's delete, gone by census time).
- `types.Workspace` carries `id` (CR name) distinct from `name` (display) — verified at `pkg/types/workspace.go:12-14` (returned by `CreateWorkspace`, `api/internal/services/workspace/workspace_service.go`); the 77cf2232 census pod matches the uuid.New() create contract (test.sh:39 comment).
- No other script consumes the shim's previously-empty answer for the combined jsonpath — grep: only `us-68-attachments-e2e.sh` uses that jsonpath (introduced by #1463).

---

## Blockers

None. Runtime arbitration: tomorrow's nightly (~10:30–12:10Z window per the 11-run createdAt pattern). Expectation: us-70 AC-1c's pod now schedulable with ~2 fewer standing workspace pods; if AC-1c still fails on capacity WITH these leaks fixed, the HELD capacity levers (250m nightly --set / pre-wave shrink / 2-node kind) become the next adjudication.

---

## Tests Run

- `go test -timeout 300s ./local/ -run 'TestUS68Cleanup|TestTestSh'` — RED pre-fix (5 pins), GREEN post-fix.
- `go test -timeout 300s ./local/` (full package, incl. the repo-wide ExecuteSmoke suite under the updated shim) — **ok** (21.7s).
- `bash -n local/us-68-attachments-e2e.sh` / `bash -n local/test.sh` — clean (pinned: `TestUS68AttachmentsScript_BashSyntax`, `TestTestSh_BashSyntax`).

---

## Next Steps

- Iterate review to APPROVED; orchestrator merges (target: before tomorrow's ~10:30–12:10Z nightly window).
- Watch tomorrow's run: AC-1c verdict decides whether the HELD capacity levers get pulled; automation R1–R9 (incl. first-run R8/R9) + #1342/#1452/#1455 suites still need their first post-#1463 execution.

---

## Files Modified

- `local/us-68-attachments-e2e.sh` — cleanup() deletes WS_A/WS_B on every exit path.
- `local/test.sh` — disposable-workspace extraction prefers the CR id; kubectl backstop delete after the API DELETE.
- `local/e2e_smoke_helpers_test.go` — kubectl shim: opt-in invocation tracing; sidecar-shaped answer for the combined container-list jsonpath.
- `local/us68_attachments_script_test.go` — cleanup pins (structural + executable + ExecuteSmoke row).
- `local/test_sh_hygiene_test.go` — NEW: test.sh hygiene pins (syntax, extraction-executes-against-fixture, backstop presence + ordering).
- `worklogs/NNNN_2026-09-19_nightly-workspace-cleanup-hygiene.md` — this worklog.
