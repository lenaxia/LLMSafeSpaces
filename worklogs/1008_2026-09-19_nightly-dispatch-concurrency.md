# Worklog: nightly e2e punctuality — platform trigger dispatch + workflow concurrency

**Date:** 2026-09-19
**Session:** fix/e2e-nightly-concurrency-dispatch — on-time nightly dispatch via a platform cron routine (dogfooding), with a concurrency group so the lagged GitHub backup never double-runs
**Status:** Complete

---

## Objective

GitHub's 06:00Z cron for e2e-nightly.yml fires 4-6h late (11-run sample). Land the on-time path: a platform cron trigger in this workspace dispatches the workflow at 06:00Z via `gh workflow run` (the scheduler's own punctuality is proven — every routine fire today claimed within ~1s of the minute boundary), and the workflow gains a concurrency group so the two entry paths never double-run.

---

## Work Completed

### Investigation (all claims empirically validated before relying on them — Rule 7)
- **Dispatch permission**: `gh workflow run` works with the workspace's GH_TOKEN (fine-grained PAT). Verified by dispatching `security-scan.yml` (scan-only: gitleaks/govulncheck/trivy, no deploy, no cluster — chosen as the harmless target per delegation): exit 0, a `workflow_dispatch` run created 15:44:30Z and observed in_progress.
- **Corroborated the triage**: today's scheduled security-scan run was created 10:26Z vs its 06:00Z cron — 4.4h late, same lag class.
- **End-to-end routine path PROVEN with a temporary probe trigger** (`*/2` cron, deleted after): fire claimed 15:46:01.3Z → delivered 15:46:24.1Z (23s); the spawned routine agent ran gh itself and reported `DISPATCH-RESULT: {"createdAt":"2026-09-19T15:46:13Z","event":"workflow_dispatch","status":"in_progress"}` — the dispatched run was created 12s after the fire and confirmed by the routine's own check. This proves the spawned-session env carries the gh credential, the dispatch works from the routine context, and the confirm loop works.

### Live trigger (orchestrator authority delegated)
- **`nightly-e2e-dispatcher`** (id 34cc2670-be45-4915-bca0-82f79621ea69): cron `0 6 * * *` UTC, first fire 2026-09-20T06:00:00Z. Prompt: dispatch e2e-nightly.yml → sleep 15 → confirm the newest run is `workflow_dispatch` and created within 5 minutes → reply `DISPATCHED:`/`DISPATCH-UNCERTAIN:`/`DISPATCH-FAILED:` with the JSON verbatim. captureMode full (the fire row carries the verdict — watchable via trigger_fires). autoDisableAfter 3 (a persistently failing dispatcher self-disables loudly; the GitHub backup keeps running). Deliberately minimal: no run-cancelling, no completion-watch (the orchestrator's "completion-check arm" grows later).

### Repo side (this PR)
- `e2e-nightly.yml`: `concurrency: {group: e2e-nightly, cancel-in-progress: true}` — one shared group for BOTH entry paths; whichever run starts latest wins, the other is cancelled. The GitHub schedule STAYS (reliability backup), `workflow_dispatch` stays (the dispatcher's entry).
- `local/nightly_dispatch_test.go`: structural pins — the concurrency group + cancel-in-progress present; the 06:00Z schedule backup retained; workflow_dispatch retained (three legs of the contract, none can rot silently).

---

## Key Decisions

1. **Probe before production**: the `*/2` temporary trigger validated the exact production path (spawned-session gh + dispatch + confirm) end-to-end in 23s without touching the nightly; deleted immediately after one delivered fire.
2. **security-scan.yml as the harmless dispatch target** — scan-only and idempotent; the nightly was never dispatched during verification.
3. **cancel-in-progress semantics accepted**: overlap is impossible (latest-start wins); a lagged backup arriving AFTER the dispatch completed still runs a second nightly — known residual, bounded by the routine's future completion-check arm (cancel/dedupe), kept out of this lane's scope per delegation.
4. **Newest-run confirmation, not blind success**: the routine checks the newest run's `event` is `workflow_dispatch` and `createdAt` within 5 minutes — a GitHub-cron race producing `schedule` as the newest run yields `DISPATCH-UNCERTAIN`, not a false `DISPATCHED`.

### Assumptions stated and validated (Rule 7)

- GH_TOKEN can dispatch workflows — validated empirically (security-scan dispatch).
- The spawned routine session carries the gh credential — validated by the probe's delivered result (the routine ran gh successfully).
- The platform scheduler's cron punctuality — validated today: every routine fire observed (probe 15:46:01.3Z vs 15:46:00 boundary; the #1452 experiment's 21:37:07/21:38:07 fires) claimed within ~1.3s of the minute.

---

## Blockers

None.

---

## Tests Run

- `gh workflow run security-scan.yml` + run-list confirm — exit 0, run in_progress (harmless target).
- Probe trigger: one fire claimed → delivered, result verified (see above); trigger deleted.
- `go test -run 'TestE2ENightly' -v ./local/` — 2/2 pins PASS.
- `python3 -c yaml.safe_load(e2e-nightly.yml)` — parses; concurrency block present.

---

## Next Steps

- Watch tomorrow 06:00Z: the dispatcher's first production fire (trigger_fires on 34cc2670…; expect `DISPATCHED:` + a workflow_dispatch run created ~06:00:0xZ). If `DISPATCH-UNCERTAIN/FAILED` appears, triage the fire result.
- The watch problem's structural fix: grow the routine a completion-check arm (poll run status, surface failures) once the dispatch leg has burned in.

---

## Files Modified

- `.github/workflows/e2e-nightly.yml` — concurrency group (cancel-in-progress); schedule + dispatch retained
- `local/nightly_dispatch_test.go` — structural pins (new)
- `worklogs/1008_2026-09-19_nightly-dispatch-concurrency.md` — this worklog

---

## Review round 1 (3 findings → fixed)

- **F1 (misspell, the staging trap)**: the first commit attempt failed pre-commit; the sed fix landed in the working tree but the index still held the pre-sed blob, and the retry committed the STAGED copy — CI lint caught 'cancelled' in the pushed content while the worklog claimed the fix had passed. Follow-up commit 2150cf47 carried the fix (verified in the pushed blob), and this entry corrects the record: the r0 "pre-commit gates pass" claim was false for the pushed head. Second occurrence of the verify-edits-landed class this session — the check is now against the PUSHED blob, not the working tree.
- **F2 (false sibling comment)**: e2e-attachments-single-container.yml's "the nightly declares no concurrency group" became false — rewritten to state the actual mechanism (group-name separation: workflow-name group vs the shared e2e-nightly group; neither can cancel the other).
- **F3 (parsed-YAML pins)**: substring pins passed on commented-out blocks and never proved parseability. Pins now assert on the PARSED document (yaml.v3, already a dependency): concurrency.group/cancel-in-progress typed-decoded; the on: legs by KEY PRESENCE on the raw map (workflow_dispatch: carries no value — value-decoding reads nil even when present); parse failure itself fails the pin. Mutation-verified: commenting out the block fails the pin; restored, green.

## Tests Run (r1)

- `go test -run 'TestE2ENightly' -v ./local/` — 2/2 PASS (parsed-YAML layer).
- Mutation: concurrency block commented out → 1 pin FAIL; restored → green.
