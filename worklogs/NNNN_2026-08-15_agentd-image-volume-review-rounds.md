# Worklog NNNN — agentd image-volume delivery, review rounds (#863 / PR #872)

**Date:** 2026-08-15
**PR:** #872
**Issue:** #863
**Supersedes:** the test-evidence and scope claims of the initial
worklog entry for this PR (NNNN_2026-08-15_agentd-image-volume-delivery).

## What

Corrections and scope additions from PR #872's automated review rounds
(two REQUEST CHANGES verdicts), recorded per worklog discipline instead
of silently editing the original entry.

## Round-1 findings → fixes (`12882c30`)

1. **Creating/Resuming detection blind spot (blocker).** Detection lived
   only in `handleActive`. A bad pin at first boot/resume exits 81
   before the container is ever Ready, so the Running-ready branch never
   fired and `handleCreating`'s 2s fall-through requeue spun forever —
   no condition, no event, no metric, no page, in exactly the dominant
   bad-rollout scenario. All six prior detection tests pre-seeded
   Active phase, which structurally hid the gap (a fixture-choice
   lesson: seed the phase the failure actually occurs in). Fix:
   `detectAgentdVerificationFailure` also runs in `handleCreating`,
   before the gen-bump persist so one Status().Update covers both.
   Red-first regression: `TestAgentdVerify_CreatingPhase_FirstBootBadPinIsDetected`.
2. **merge-agentd pipeline could never succeed (blocker).** No
   Download-digests step (create ran against an empty /tmp/digests) and
   a tag-scheme mismatch (inspect used full-40-hex sha- tag; metadata
   emitted the 7-hex short form). Fixed by adding the download step and
   `type=sha,format=full`, mirroring the proven merge-runtime pattern.
   First green execution is necessarily post-merge (job is non-PR only).
3. **Entrypoint termination-log masking (minor).** `tee
   /dev/termination-log` under `set -euo pipefail` would exit 1 on an
   unwritable log, masking the 81/82 contract codes. Now best-effort.
4. **Reverse half-config guard (minor).** Hashes-without-image silently
   rendered no flags (operator believes overlay is on; controller runs
   legacy). Now a render-time `fail`; both directions test-locked.
5. **D5 labels/event (minor).** Counter labels `{outcome,node,agentd}`
   (agentd = first 12 hex of the pinned digest); event carries image ref.
6. **Entrypoint test suite (gap).** The security-critical sha256 gate
   had zero automated coverage — "reproduced live in /tmp" locked in
   nothing. Added `entrypoint_agentd_test.go`: 6 bash-subprocess tests
   executing the REAL script (match→exec, mismatch→81 with
   expected=/got=, missing→82, no-pin→81, legacy PATH fallback,
   termination-log best-effort).

## Round-2 findings → fixes (this commit)

1. `Closes #863` → `Refs #863`: the PR defers four live validation legs
   (kind e2e, gVisor, fallback, tamper-live) + the CI kind-pin to the
   issue; auto-closing would destroy their tracker. Issue stays open.
2. Alert annotation rendered an empty `{{ $labels.outcome }}` (bare
   `sum(...)` drops labels) → `sum by (outcome)`.
3. Entrypoint match/mismatch/missing tests set only the AMD64 pin —
   latent failure on arm64 hosts. Both pins now set (portable).
4. Dead `manifest-digest` job output removed.
5. Docs staleness from the round-1 label change: metric label list and
   "stays Active" corrected (detection now also fires from Creating).

## Assumption corrections

- The initial worklog claimed the chart tests "run in CI Lint" — they
  run in the `test`/`test-full` jobs (wherever `helm` is installed);
  corrected here.
- D4 deviation (spec said overlay-absent→baked fallback + info event;
  implementation fail-closes to exit 82) recorded on #863 with
  rationale: a missing pinned binary at boot is a rollout error, and
  silent fallback would mask it and reintroduce the stale-agentd
  problem the issue exists to fix.

## Remaining open (tracked on #863, not closed by the PR)

Live kind e2e (happy boot + tamper→alert), gVisor + resume-latency
measurement, fallback leg, tamper-live leg, CI kind node pin — all
require artifacts/pipeline that only exist post-merge.
