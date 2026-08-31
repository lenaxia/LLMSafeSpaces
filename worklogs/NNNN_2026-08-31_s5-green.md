# Worklog: S5 suite GREEN — design 0053 rollout validation complete

**Date:** 2026-08-31
**Session:** Drove the S5 overlay-validation suite from first dispatch to fully green across 12 runs; recorded the results.
**Status:** Complete

---

## Objective

Green S5 kind run = the validation gate for design 0053's entire rollout (§7) and the prerequisite for the defaults flip.

---

## Work Completed (run-by-run triage → fixes)

| Run | Result | Root cause → fix |
|---|---|---|
| 1 | S5.2 fail | Workspace applied un-namespaced (`default` ns → PVC RBAC); chart's seeded RTE tripped the allow-list on upgrade → #1170/#1173 (namespaced kubectl + webhook-retry apply + `runtimeEnvironments.base.image` → local registry) |
| 2–3 | same | #1173's push failed silently (merged docs-only); verified-on-remote discipline adopted |
| 4 | S5.3 upgrade hang | `helm --wait` on the tamper controller; ALSO the deeper cause below → #1174 (drop `--wait`, rollout-status gate) |
| 5–6 | S5.3/4 false-green | Tamper pins contained non-hex → startup validation crash-looped the tampered controller; the OLD correctly-pinned pod kept serving → #1176 (tamper = 64 zeros: well-formed, never-matching) |
| 7 | S5.6 fail | gVisor's apt repo is gone UPSTREAM (suite/key/pool all 404, verified independently) → direct release-binary install + published sha512 verify (#1177) |
| 8 | S5.6 fail | My checksum-format guard was a '?' glob miscounted 66≠128 — its own diagnosis output caught it → regex + regression test executing the script's real guard (#1178; reviewer-required per Rule 0) |
| 9 | S5.6 fail | `runtimeClassName` → CRD field is `runtimeClass` + webhook requires the `allow-runtime-class-override` annotation (#1179) |
| 10 | S5.6 fail | containerd needs the SHIM (`containerd-shim-runsc-v1`), not just runsc — kubelet's error was definitive (#1180). S5.6 first reached Active-with-volumes later that run |
| 10–11 | S5.5 flake ×2 | Suspend patch hit the webhook while endpoints still targeted a terminating controller pod post-restore → endpoint wait + retried suspend/activate patches + per-leg failure messages (#1181) |
| **12** | **SUCCESS** | **All checks pass** |

## Final results (run 33402474321)

- S5.1/1b missing-pin render legs ✓
- S5.2a/b/c stripped-base launch→ready: PID1 = pinned overlay agentd, opencode :4096, redact wrapper on-pod (S2's cluster-e2e debt) ✓
- S5.3/S5.4 verify-failure surfacing (exit 81/83 → conditions) ✓
- S5.5 cold-pull resume: 13–19s across runs (budget 120s; beats the pre-overlay ~22s figure) ✓
- S5.6 gVisor: pod Active with BOTH image volumes under runsc, opencode serving — **design 0051's open item answered: image volumes work under gVisor** ✓

## Follow-ups recorded

- design 0053 status → S5 GREEN (this PR)
- README resume figures → measured 13–19s (this PR)
- **Flip inventory is now executable** per `docs/runbooks/sidecar-flip.md` (sidecar default-on, `bookworm@2026.08.0` fleet promotion) — operator decision, preconditions all green.

## Files Modified

`design/0053_2026-08-28_platform-overlay-delivery.md`, `README-LLM.md`, `worklogs/NNNN_2026-08-31_s5-green.md`
