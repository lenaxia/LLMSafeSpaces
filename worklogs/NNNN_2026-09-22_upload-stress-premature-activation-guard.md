# Worklog: Upload staging stress step — premature activation guard (run 35679282297)

**Date:** 2026-09-22
**Session:** The scoreboard dispatch (35679282297) died at a NEW step — "Run upload staging stress rows (design 0060 §6)", wired to the nightly by #1516 before its activation stack completed (#1518 apply leg + #1524 e2e activation both still in review). Adjudicated: guard now, evidence to the stack owner.
**Status:** Complete — PR open, iterating review

---

## Objective / Work Completed

FAST triage: SR-1–SR-5 all PASS on the half-stack (the 507 refusals ARE the designed clean-fail); SR-6's baseline-upload requires the full stack, got 507, and then **36 minutes of silence** (02:47:38 → 03:23:59 cancellation). The actual hang mechanism (r2's finding): a BARE `wait` at the SR-6/SR-6B storm joins also waits the IMMORTAL `kc port-forward` child spawned by harness_start (the script's trap replaced the lib's PF-killing trap). The hang was armed on the HAPPY path too — once #1518/#1524 land and the baseline 201s, the same bare wait fires.

Three review rounds sharpened the fix:
- r1: the in-run 507-keyed gate (the script's own sr_skip idiom), not a silent step-level `if: false` (the #1342 rule).
- r2: the gate's exit-0 now propagates prior row failures (a masked-green was introduced and fixed); both storm joins converted to per-pid waits (disarming the actual hang permanently); keyed on 507 SPECIFICALLY so non-507 failures reach the failure path.
- r3: the baseline failure assertion restored BELOW the gate (r2 deleted it — a 500/429/000 baseline with passing storms went green); the four-leg executed gate test (507-clean/507+prior-fail/500/201) mutation-verified against every blind spot.

## Key Decisions

1. The in-run gate over the step-level disable — the #1342 rule: loud gates, never silent ifs. SR-A and SR-1–SR-5 keep running (they pass on the half-stack).
2. Keyed on 507 (the DESIGNED half-stack clean-fail), not any non-201 — a genuine 500/429 baseline still fails the row.
3. The per-pid wait fix is the real hang disarm — it applies to the happy path too.

## Tests Run

- `go test -count=1 -timeout 300s ./local/` — **ok** (includes the four-leg executed gate test + structural pins).
- `bash -n` clean; YAML-validated.

## Files Modified

- `local/us-1500-upload-stress-e2e.sh` — the 507 gate, baseline assertion, per-pid waits.
- `.github/workflows/e2e-nightly.yml` — the step's comment (teaches the gate).
- `local/us_1500_upload_stress_script_test.go` — the structural pins + the executed four-leg gate test.
- `worklogs/NNNN_2026-09-22_upload-stress-premature-activation-guard.md` — this worklog.
