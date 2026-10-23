# Worklog: #1905 — Longhorn RecurringJob snapshot cron invalid day-of-week (cross-repo fix + deploy-tree cron guard)

## Charter

Issue #1905: the Longhorn RecurringJob `default-snapshot` cron reads
`0 1 * * 8` — day-of-week 8 is outside the valid 0-7 range, so the job
(as declared) never schedules. Deliver: fix the cron in the deploy
manifests; red-first pin FIRST (a test parsing every cron expression in
the deploy tree against a 5-field validator, failing on the current
invalid value); green with the fix. Orchestrator: leisure-wasabi-covers
(packets at ack / PR open / review rounds / verdict).

## Location finding (the repo split)

The buggy manifest does NOT live in this repo. Exhaustive sweep of the
LLMSafeSpaces tree (`RecurringJob`, `0 1 * * 8`, cron-shaped yaml keys)
is clean; docs/operator/storage.md line 271 already states snapshots
are configured at the Longhorn level, not here. The manifest lives in
**lenaxia/talos-ops-prod** at
`kubernetes/apps/storage/longhorn/app/recurring-jobs.yaml`
(introduced whole in one 2024-12-28 "add storage" commit `3a541a0e`,
never edited since). Execution per the worklog-0601 dual-repo precedent,
orchestrator-approved: fix + guard test ride a talos-ops-prod branch;
this repo's branch carries the bookkeeping + evidence.

## Intent verification (posted to the issue: #1905 comment 6375576449)

Corrected cron: **`0 1 * * *` (daily 01:00)** — NOT the issue's guessed
`0 1 * * 0` (weekly Sunday). Evidence, in priority order:

1. **The manifest's own label.** `default-snapshot` carries
   `label/1: dailySnapshot`; sibling `default-backup` in the same file
   demonstrates the label↔cadence convention (`0 0 * * 1` ↔
   `weeklyBackup`). A weekly-Sunday snapshot would contradict the
   file's own cadence declaration for this job.
2. **Posture coherence.** Weekly-Sunday would duplicate the
   weekly-Monday backup cadence (two weekly rollback points 24 h
   apart, retain 2 each). Daily snapshot + weekly backup is the
   standard tiered pair the file models.
3. **Typo mechanism.** `8` is the un-shifted form of `*` (Shift+8 on
   US layout): intended `0 1 * * *` with a missed shift on the final
   field yields exactly `0 1 * * 8`. No mechanical path yields 8 from
   an intended Sunday (0 or 7).

Orchestrator accepted the daily correction; issue record amended.

## Owner ruling (folded into the issue comment)

Prod backups run on **Kopia** (kyverno `snapshot-cronjob-controller`
path), NOT Longhorn. The dormant recurring-jobs/backup posture
(`recurring-jobs.yaml` AND `backup-secret.sops.yaml` are commented out
of the app kustomization) is **by design**; re-enabling is permanently
off the table. The cron fix is latent correctness of the declared
manifest. Forensic note for #1808/#1892: Longhorn-level scheduled
snapshots were never in effect on these volumes — snapshot history,
where it exists, comes from the Kopia path.

## Red-first pin (talos-ops-prod)

`tests/test_cron_validity.py`: walks every YAML under `kubernetes/`,
validates each literal cron in `cron:`/`schedule:` string fields
against a 5-field standard-range validator — minute 0-59, hour 0-23,
dom 1-31, month 1-12, **dow 0-7** — the same class as this repo's
platform trigger validator (`pkg/workflows/schedule.go`, robfig
5-field parser). Honest-scope mechanics:

- Six-field CloudNative-PG backup schedules (`0 0 12 * * *` — 5
  clusters) validate seconds 0-59 + the five standard fields (not
  skipped, not false-positived).
- Known `@`-shorthands (`@hourly`, `@midnight`, `@every <dur>`) pass a
  form check; unknown `@`-tokens fail.
- sops ciphertext (by name or top-level `sops:` block) and `${...}`
  templates skipped as non-literal.
- Lenient loader: unknown tags stringified (ESPHome `!secret` shares
  the tree), duplicate anchors last-wins (pyyaml is stricter than the
  Flux/kustomize tooling that consumes these files — gamevault's
  helm-release would otherwise fail).
- Unparseable files frozen behind an explicit `KNOWN_UNPARSEABLE`
  allowlist (one legacy mis-indented NFD rule); NEW unparseable files
  fail the guard — the audit surface cannot silently shrink.
- Walker pins: harvest floor (≥20) + longhorn-app coverage, so
  skip-regressions cannot pass vacuously.
- 26 validator unit pins (dow 8 rejection, ranges, steps, descending
  ranges, 6-field, @-forms) independent of tree content.

**RED:** `733bce3d` (guard alone) → `1 failed, 55 passed`, failing
exactly on `recurring-jobs.yaml [spec.cron]: invalid cron '0 1 * * 8':
value 8 out of range [0, 7]`.
**GREEN:** `6d9a5aa3` (the fix) → `56 passed`.

## The fix

`recurring-jobs.yaml`: `cron: "0 1 * * 8"` → `cron: "0 1 * * *"`, with
a manifest comment recording the intent (dailySnapshot label match,
missed-Shift origin) and the owner ruling (file intentionally dormant —
Kopia owns prod backups; do not re-enable). No drive-by re-enable of
the kustomization; no other files touched.

## PRs

- talos-ops-prod#2795 (fix + guard, red hash cited in the PR body):
  https://github.com/lenaxia/talos-ops-prod/pull/2795
- This repo: bookkeeping PR (this worklog, CHANGELOG, COORDINATE row).

## Mutation recipes (for the mutation lane)

- **M1** — revert `recurring-jobs.yaml` cron to `0 1 * * 8`:
  `test_cron_expression_is_valid[recurring-jobs.yaml:0 1 * * 8]` must
  fail.
- **M2** — loosen the validator's dow upper bound (FIELD_RANGES dow
  `(0, 7)` → `(0, 8)`): `test_validator[0_1_*_*_8_out_of_range]` must
  fail (validator unit pin, no manifest change needed).
- **M3** — walker skip (e.g. skip `recurring-jobs.yaml` by name, or
  break rglob): `test_walker_covers_longhorn_storage_app` and/or
  `test_walker_harvest_is_nontrivial` must fail.
- **M4** — drop the seconds-field validation for 6-field dialects
  (treat as 5-field): `test_validator` 6-field cases must fail.

## Tests run

- `python3 -m pytest tests/test_cron_validity.py -q` (talos-ops-prod,
  at both red and green commits): 55+1 → 56 passed.

This repo: no code changes (bookkeeping only); repolint gate run.
