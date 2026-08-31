# Worklog: design 0053 completion audit (S5 dispatch + verification pass)

**Date:** 2026-08-31
**Session:** First S5 workflow dispatch + a section-by-section correctness/completeness audit of design 0053 against the merged tree.
**Status:** Complete

---

## Objective

Verify design 0053 is correct and complete as implemented; run the S5 suite; fix every audit finding.

---

## Work Completed

- **S5 dispatched**: first real run of `s5-overlay-validation.yml` on main (run 33362912631) after merging #1169.
- **Audit findings (all fixed in this PR):**
  1. **Duplicate pin coordinate (real defect)**: the `OPENCODE_VERSION` ARG + validation comment block survived in `runtimes/base/Dockerfile` after the S3 strip — two places declared the pin, and `opencode-version-bump.yml` updates only the artifact file, so a future bump would desync silently (the fixture gate reads artifact-first, base-fallback — a stale base ARG would mask nothing but mislead). Removed; base now carries a pointer comment. Also fixed two stale comments ("opencode/mise install steps", "opencode assets above").
  2. **Design doc staleness**: Status header said "Approved (no implementation yet)"; §7 had no story statuses and the US-70.1 gate line mentioned in #1156's review wasn't in main's copy. Header now records S1–S4 merged + gate satisfied (#1164); §7 carries per-story status; §8 risks carry dispositions (Renovate → bespoke bump workflow + CI-printed values, with rationale).
  3. **Runbook staleness**: `sidecar-flip.md` Step 5 described deleting the baked path "later" — #1156 already did it (and deleted `MinBaseVersion` outright, not ratcheted). Step 5 marked DONE with the actual rollback artifact (last pre-strip base tag); preconditions gained S5-green (#4) and US-70.1-deployed (#5).

## Verified correct and complete (no action)

- §4.1: redact subcommand (dispatch ×2 incl. wrapper call sites), entrypoints dir gone, supervisor wrapper wired in both modes.
- §4.2: artifact Dockerfile, Helm values + template gates (12 refs), controller pins/overlay, CI stamping (`opencode.sha256-*` annotations).
- §4.3: strip inventory exact — builders/opencode-install/ENV-blocks/ENTRYPOINT gone; debian+useradd+USER+WORKDIR/mise/gh/aws//etc/gitconfig kept; controller carries the env (mise homes, PATH, git layer).
- §4.4: factory stamps no ENTRYPOINT (comment-only reference), zero `MinBaseVersion` refs, seed row `2026.08.0`.
- §4.5: both render gates + both startup validators mandatory-fatal; `validateOverlayDeliveryPins` at buildPod.
- §8 remaining risk: registry availability doubled — accepted by design, ConfigMap cache covers annotation resolution.

## Tests Run

`bash -n` on the S5 script (earlier); this PR is Dockerfile-comment + docs only — CI validates the base build.

## Next Steps

- Triage S5 run 33362912631 results (esp. S5.5's measured resume number → README's ~22s figure, and S5.6 gVisor leg on ubuntu-latest).
- Execute the flip inventory per the runbook once green.

## Files Modified

- `runtimes/base/Dockerfile` — duplicate OPENCODE_VERSION block removed; stale comments fixed
- `design/0053_2026-08-28_platform-overlay-delivery.md` — status, §7 statuses + gate, §8 dispositions
- `docs/runbooks/sidecar-flip.md` — Step 5 marked done; S5 + US-70.1 preconditions
- `worklogs/NNNN_2026-08-31_0053-completion-audit.md` (this file)
