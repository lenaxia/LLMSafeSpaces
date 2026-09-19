# Worklog: nightly e2e schedule moved off-peak — dispatcher decommissioned per owner reversal

**Date:** 2026-09-19
**Session:** fix/e2e-nightly-offpeak-schedule — the owner's reversal on the dispatcher experiment: fix the schedule slot itself ("move it to 2am… make us more robust, don't build an entirely new system")
**Status:** Complete

---

## Objective

Replace the platform-dispatch architecture (PR #1485's trigger half) with the standard mitigation its own lag data pointed at: move the nightly cron off the high-contention top-of-hour 06:00 UTC slot to the owner's 2am Pacific (09:17 UTC) — odd-minute, off-peak-ish. Keep the concurrency group (source-agnostic double-run protection). Decommission the live dispatcher trigger.

---

## Work Completed

- `e2e-nightly.yml`: schedule `0 6 * * *` → `17 9 * * *` (2:17am Pacific / 09:17 UTC), comment in house style with the local anchor and the 06:00 lag evidence. Concurrency group UNCHANGED (comment reworded to source-agnostic — no dispatcher reference).
- `local/nightly_dispatch_test.go`: `TestE2ENightlyScheduleBackupRetained` → `TestE2ENightlyScheduleSlotRetained`, pinning `17 9 * * *`; header comments rewritten to the schedule-fix rationale (dispatcher experiment mentioned as decommissioned).
- **Live trigger DELETED**: `nightly-e2e-dispatcher` (34cc2670-be45-4915-bca0-82f79621ea69) — created under delegated orchestrator authority, removed the same way, BEFORE the PR (sequencing: tonight's 06:00Z dispatch cannot fire while the slot fix is in review). The Phase 2/3/4 design notes die with it; the run-results-reporting idea is shelved as maybe-later, not filed.

---

## Key Decisions

1. **Delete-before-PR sequencing** — a live 06:00Z dispatcher racing a merged 09:17Z schedule would recreate the double-dispatch class the concurrency group only partially contains (sequential doubles).
2. **Pin renamed, not just re-pointed** — "BackupRetained" named the dead architecture; "SlotRetained" names the live contract (the slot IS the fix now).
3. **Odd-minute + local anchor in the comment** — the owner's "2am" was Pacific; the cron is written in UTC with the Pacific anchor stated so the next reader doesn't repeat the timezone ambiguity this lane itself hit mid-flight (02:17 UTC drafted first, corrected to 09:17 UTC).

### Assumptions stated and validated (Rule 7)

- The concurrency group is retention-safe: #1485's reviewer verified group-name uniqueness across workflows; nothing else references `e2e-nightly`.
- The pinned cron is the owner's stated intent — corrected once via the orchestrator (Pacific anchor); final value `17 9 * * *` validated by parsed-YAML pin + `yaml.safe_load`.

---

## Blockers

None.

---

## Tests Run

- `go test -run 'TestE2ENightly' -v ./local/` — 2/2 PASS at the final slot value.
- Mutation (at the interim 02:17 value): cron reverted to `0 6 * * *` → pin FAILS; restored → green. The same pin class bit on the corrected value transition (any drift off `17 9 * * *` fails).
- `yaml.safe_load` — parses; schedule reads `[{'cron': '17 9 * * *'}]`.

---

## Next Steps

- Watch: with the slot at 2:17am Pacific (09:17 UTC), the expected results window is the owner's Sunday morning (~4am-9am Pacific depending on queue lag). The watch pattern is now RESULTS-DRIVEN — triage when a run exists — not schedule-driven (no 06:05Z dispatch check anymore; that trigger is gone).

---

## Files Modified

- `.github/workflows/e2e-nightly.yml` — schedule slot + comments (concurrency group retained)
- `local/nightly_dispatch_test.go` — pin renamed/re-pointed; comments to the schedule-fix rationale
- `worklogs/NNNN_2026-09-19_nightly-offpeak-schedule.md` — this worklog
