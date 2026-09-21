# Worklog: Trigger-create workflow contract (run 35597973572 ruling) + automation R1–R4 reshape

**Date:** 2026-09-21
**Session:** Three-part adjudicated lane from the R1–R9 arbitration's first finding: (1) product — create-with-nonexistent-workflowId answers a named 400; (2) harness — R1–R4 ride the ruled contract; (3) ledger hardening folded in. Process note recorded below: the branch was cut from a stale origin/main (pre-#1514); caught before commit, rebased, verified — fetch-before-branch is now non-negotiable.
**Status:** Complete — PR open, iterating review

---

## Objective / Work Completed

### (1) Product: named 4xx at the handler (`api/internal/handlers/triggers.go`)

Run 35597973572's finding: valid create + ghost `workflowId` → opaque 500 in 4.5ms (store-layer FK shape). Fix per the ruling: pre-flight existence check via the store's owner-scoped `GetWorkflow` — `400 {"error":"target workflow not found"}` (mirroring the update path's in-family wording, triggers.go:611's precedent), `500 "failed to fetch workflow"` for non-NotFound lookup faults, **ordered after cron validation** so invalid exprs keep answering the cron error (R1a's contract). The FK stays the integrity anchor (ON DELETE SET NULL, the #1440 loud path) — this is the contract check, not a replacement. **OWNER-OVERRIDE FLAG**: this overturns a mock-only contract — `TestTriggerInputMapping_V6`'s ghost arm pinned "un-opted ghost stays creatable", but that only ever held in the FK-less mock; the real DB answered those creates with the opaque 500, so the two contracts were already inconsistent in production. The V6 arm now pins the ruled 400 (its guard-scope intent is preserved by the neighboring schema-less-workflow arm).

Tests (TDD red→green): `TestTriggerCreate_NonexistentWorkflow_Named400` (400 + nothing stored), `TestTriggerCreate_WorkflowFetchError_500` (mock knob `getWorkflowErr`), `TestTriggerCreate_CronValidationErrorOutranksWorkflowCheck` (error ordering). Existing-test fixtures completed (the mock never modeled create-path workflow existence): seeds in Cron/Webhook/V7/TargetPresenceGuard + the pod-automation pair's trigStore (unified-store topology noted in-comment).

### (2) Harness: R1–R4 on the ruled contract (`local/issue-1410-1412-automation-e2e.sh`)

- A shared REAL workflow (`REAL_WF`) is created before R1 (R4d's specYaml shape, `R5_WS` hoisted to the var block); every create targets it (`create_trigger` + R1a's inline body).
- **R4a–c reshaped to the #1440 path**: create-valid → `DELETE /workflows/${REAL_WF_ID}` (FK SET NULL → targetless) → the imminent slot must fire LOUD: failed fire recorded (R4a), payload `trigger_has_no_target` (R4b — the ruling's "arguably better row"; a `workflow not found` fire class is unrepresentable when the FK SET-NULLs), consecutiveFailures driven (R4c). R4d unchanged (same mechanism, auto-disable face). GHOST_WF removed; header comment updated.
- Pins updated: REAL_WF wiring, R4's delete step, the targetless payload, ghost-needle pins removed.

### (3) Ledger hardening folded in

`TestIssue1410E2E_HarnessStartFirst` strengthened per the #1514 adjudication note (Index-of-first ordering pins are move-tolerant within a span): **every** authenticated `api METHOD` call must sit after `harness_start`, not just the first. #1510's kind-row execution is NOT here — it is sequenced behind that sibling PR's merge (its row isn't on main; no open-PR touching).

### Assumptions stated and validated (Rule 7)

- The FK-on-workflow_id hypothesis — from the run's 4.5ms store-layer 500; confirmed decisive via the V6-test conflict analysis above (mock-creatable vs DB-rejected).
- R1/R2/R3's monthly triggers never fire during the run, so R4's workflow delete mid-script is harmless to them (monthly slots; they are trap-cleaned).
- Stale-base catch: verified post-rebase that #1514's bootstrap and this lane's reshape coexist cleanly (structure grep + full suites).

---

## Blockers

None.

## Tests Run

- `go test -count=1 -timeout 300s ./api/internal/handlers/` — **ok** (89.4s; new pins RED pre-fix → GREEN).
- `go test -count=1 -timeout 300s ./local/` — **ok** (32.5s). `go build ./...` — ok. `bash -n` — clean.

## Next Steps

- APPROVED → merge → dispatch → the full R1–R9 arbitration (now on the ruled contract) + #1452/#1455/#1417/revisions/dev-preview.
- Owner: confirm the V6 ghost-creatability overturn (flagged in the PR body); #1510's row wiring follows its merge.

## Files Modified

- `api/internal/handlers/triggers.go` — the create-path workflow-existence check.
- `api/internal/handlers/triggers_test.go` — three new pins + fixture completions + V6 arm update + mock knob.
- `api/internal/handlers/pod_automation_test.go` — unified-store seed in two fixtures.
- `local/issue-1410-1412-automation-e2e.sh` — REAL_WF + R4 reshape + header/R5_WS hoist.
- `local/issue_1410_automation_e2e_script_test.go` — row pins updated; harness-start pin hardened.
- `worklogs/NNNN_2026-09-21_trigger-create-workflow-contract.md` — this worklog.
