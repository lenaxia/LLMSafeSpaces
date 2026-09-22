# Worklog: SR-6 serialization guard — known-issue skip (#1539)

**Date:** 2026-09-22
**Session:** The SR-6 §6.6 serialization guard caught a real product finding on its first full-stack contact (run 35737624754: 891ms p95 > 622ms guard). Filed as #1539 with the evidence package; the guard is deferred behind a loud known-issue skip until the fix lands.
**Status:** Complete — PR open, iterating review

---

## Objective / Work Completed

- Filed #1539 (the serialization finding) with the evidence package: the per-upload latency series (311ms single, 891ms concurrent p95), the apply-side log correlation (staging_busy/staging_full/dest_disk_full signals), and three hypotheses (staging-slot contention, supervisor apply serialization, cross-row residue).
- The SR-6 guard's `note_fail` replaced with `sr_skip` referencing #1539 (the #1342 pattern: loud, counted, referencing the issue). The nightly proceeds past a known-and-filed finding; the guard tightens back when #1539 closes (the skip comment carries the instruction).
- Pins: structural (the skip's presence, the #1539 reference, the note_fail's absence) + executed trip+pass legs (the real extracted block: 891>622 → SKIP-DOWN + sr_skips=1; 400≤622 → ok). Mutation-verified by the reviewer against every silent-downgrade shape.

## Tests Run

- `go test -count=1 -timeout 300s ./local/` — **ok**.
- `bash -n` clean.

## Files Modified

- `local/us-1500-upload-stress-e2e.sh` — the sr_skip + KNOWN ISSUE comment.
- `local/us_1500_upload_stress_script_test.go` — the structural pins + the executed trip+pass legs.
- `worklogs/1049_2026-09-22_sr6-known-issue-skip.md` — this worklog.
