# Worklog: design 0060 PR 4 — the sidecar upload e2e un-skip

**Date:** 2026-09-21
**Session:** feat/upload-e2e-unskip — §9 PR 4: the nightly's Epic 68 rows flip from assert-clean-fail to assert-delivery (the Rule-0 gate #1518's review holds on)
**Status:** Complete (this PR; stacked on #1518/#1523)

---

## Objective

The nightly installs sidecar mode; pre-0060 the attachments script asserted uploads clean-fail there and skipped E2/E10/E11. Design 0060 ships delivery — un-skip the rows: they run UNMODIFIED in both modes.

---

## Work Completed

- `local/us-68-attachments-e2e.sh`: the sidecar gate now LOGS the mode ("design 0060 stage-and-signal delivery (rows run unmodified)") and falls through — the clean-fail assertion, the exit-0, and the skip messages are deleted. E2/E10/E11 (persistence/isolation/chaos) run against the delivery path identically in both modes.
- `local/us68_attachments_script_test.go`: the gate's behavioral pins updated to the new semantics (sidecar detected → logs + FALLS THROUGH, not exit-0; the "upload accepted must die" sub-test retired WITH the assertion — a 201 in sidecar mode is the designed outcome); the new `TestUS68SidecarRows_Unskipped` pins the un-skip against rot (no exit-0 fall-through killer, no resurrected clean-fail).
- `e2e-nightly.yml`: the step comment rewritten (delivery in both modes; the skip is retired).

---

## Key Decisions

1. The rows themselves are untouched — mode-uniformity is the point: the same 201+path+persistence contract in single-container and sidecar modes. A mode-specific row set would test my expectations instead of the contract.
2. The gate KEEPS the mode detection (both container lists — the #1456 lesson) and still LOGS it: the nightly output stays mode-legible; only the branch action changed.

---

## Tests Run

- `go test -run 'TestUS68' ./local/` — green (bash-syntax, the rewritten gate behavior pins incl. the executed-gate harness, the new un-skip pin, the F8 workflow pins, cleanup pins).
- `bash -n` + `yaml.safe_load` — clean.

---

## Next Steps

- The three-review stack (#1518, #1523, this) iterates; rebase successors as predecessors merge; the stack rides the later train per the orchestrator's scheduling note.

---

## Files Modified

- `local/us-68-attachments-e2e.sh` — the gate falls through; clean-fail retired
- `local/us68_attachments_script_test.go` — semantics updated + the un-skip pin
- `.github/workflows/e2e-nightly.yml` — the step comment
- `worklogs/NNNN_2026-09-21_upload-e2e-unskip.md` — this worklog


## Review round 1 (2 findings: the script header + the §9 item-4 docs retirement)

- The script HEADER still described the retired clean-fail/skip — rewritten to the delivery semantics (mode detection stays; rows run unmodified in both modes).
- The design §9 item-4 docs retirement landed: uploads.go's sidecar caveat replaced (delivery + the mechanism pointer); README-LLM's D1 as-built caveat replaced (the design-0060 delivery paragraph).
- Process note: the first attempt landed these edits on the WRONG branch (the supervisor branch — reset cleanly, no push); this commit is the same delta on the right branch.


## Review round 2 (Finding 2 half-fixed + Finding 3 untouched, both closed)

- The gate block's first comment (:203) still asserted the retired premise directly above the rewritten block — corrected to the detection framing.
- The adjacent stale docs, now affirmatively false: e2e-attachments-single-container.yml's header rewritten (the single-container complement to the nightly, not the only-functional-mode runner; 404→403); the epic-68 README's deviation 1 + line 103 + the rows-execute-fully-only-in-single-container claim all replaced with the both-modes truth.
- The 404-vs-403 drift fixed in the script header (the assertion was right; the header was wrong — the reviewer code-traced NewForbiddenError → 403).
- Round-1 record correction: the r1 addendum said '2 findings' — the review listed 3 (Finding 3, the adjacent docs, was deferred and is closed HERE).


## Review round 3 (1 stale run-step comment + the version-history convention)

- The workflow's run-step comment (:258-264) still asserted the retired gate-passes-through premise — rewritten to the both-modes complement framing.
- README-LLM: the retirement gained its version-history row (1.29) + the Last Updated bump, per the doc's own convention.


## Review round 4 (the defective version-history fix corrected)

- The r3 version-history fix was defective: a duplicate 1.29 number, mis-placed (after the 1.28 row, not newest-first), and the header was never bumped (it still read 1.30 through r3 — my earlier record's "said 1.29" was itself a misstatement) — and the r3 worklog claimed a 'Last Updated bump' that did not exist in the commit. Corrected: the row is 1.31 (unique, newest-first above 1.30), the header is Version 1.31 / Last Updated 2026-09-21 (the r3 record's claim is now true of THIS commit).


## Review round 5+6 (the guard commit untested; the two stale enumerations)

- The tmpfs-guard fix shipped with no regression test (the reviewer's r6: reverting the guard would pass the suite) — now pinned: TestBuildSidecarDeps_NoErrorLogsWithoutTmpfs (the renamed r10 family — the r5-era NoErrorLogsOrGaugesWithoutTmpfs name was replaced at a4a6ea07) drives the real wiring with a captured zap logger; TestStagingBootGuard_BothArms covers the predicate arms. Mutation-verified (the guard reverted → the wiring pin red).
- The two stale 'sidecar skip' exit-path enumerations (script comment + test comment) fixed.


## Review round 7 (all findings addressed by the r4-sync + this commit)

- The through-wiring pin: landed in the a4a6ea07 sync (TestBuildSidecarDeps_NoErrorLogsWithoutTmpfs drives the REAL buildSidecarDeps with a captured zap logger; the r7 review ran against the pre-sync head). The old predicate-only test with the disprovable name/'by construction' claims is gone — replaced, not supplemented.
- docs/api/rest.md:171 — the last live pre-0060 claim, fixed (both modes + the mechanism).
- The sweeper-outside-guard revert (mutation B): [CORRECTED r8 — this r7 claim was experimentally false (reverting 55af8d5c passed the full suite, twice-verified). The placement was UNPINNED until r9: TestBuildSidecarDeps_SweeperPlacementGuarded — a started-signal observable; the no-tmpfs case never starts it, the tmpfs case does; mutation B (the exact revert) goes red.]


## Review round 9 (the sweeper-placement gap closed with a real differentiator)

- The r8 correction's replacement clause ("the no-gauges pin covers the tick-time writes") was experimentally false — the gap was open and the record claimed closure. Closed honestly now: the stager carries a sweepStarted signal (closed synchronously BEFORE the goroutine launches — the deterministic fact); TestBuildSidecarDeps_SweeperPlacementGuarded observes it through the real wiring — no-tmpfs → never started; tmpfs → started. Mutation B (the exact 55af8d5c revert, the sweeper back outside) fails the pin. The honest record: the r8 clause was wrong; this round landed the pin it should have described as missing.


## Review round 10 (the dead seam deleted; the gauge pin order-proof; the happy-arm comment honest)

- The dead onSweepTick seam (zero assignors, comments describing a nonexistent test) — deleted; sweepStarted is the placement pin's only seam.
- The gauge pin: suite-order-proof via a test-private workspace label and a fresh PedanticRegistry inspection (the shared singleton's per-workspace series make the private label immune to pollution).
- The happy-arm comment enumerates only the pinned observable (the boot scrub); the dir-existence vacuity and the 0750/gauge claims removed.
- [RETRACTED r11 — this r10 bullet was doubly false: the name NoErrorLogsAgainstBlockedParent appeared in NO commit message (I fabricated it; the real dangling reference was the :79 NoErrorLogsOrGaugesWithoutTmpfs, corrected above), and nothing was corrected in the env-branch record (that worklog contains no Go test names; no correction was needed there).]


## Review round 11 (three record corrections; the remaining two name-hits are the corrections' own historical references)

- :79 — the phantom name (NoErrorLogsOrGaugesWithoutTmpfs, replaced at a4a6ea07) now cites the REAL current pins (NoErrorLogsWithoutTmpfs + BootGuard_BothArms) with the rename history inline.
- :100 — the fabricated r10 bullet RETRACTED in place (both halves false: the name existed in no commit message; nothing was corrected in the env-branch record). The remaining NoErrorLogsAgainstBlockedParent/OrGauges hits are within the correction texts themselves — the historical referents the corrections describe.
- :92 + the field comment — the launch phrasing corrected everywhere (the close PRECEDES the launch — the deterministic fact, not the race-shaped "when launched").
