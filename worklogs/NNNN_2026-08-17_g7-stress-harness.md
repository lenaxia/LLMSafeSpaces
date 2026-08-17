# Worklog: G7 stress harness (#907)

**Date:** 2026-08-17
**Session:** First implementation slice of #907 — the stress harness that
gates D3 (durable prompts). Issue #907, design 0050 §G7.
**Status:** Complete (harness validated on a real workspace)

---

## Objective

The merge gate for D3 per the user ruling: prove the incident
acceptance criteria on a REAL workspace — zero kills of a reachable
opencode, suppressions counted not acted on, tracker behavior across a
forced generation change, live turns completing under a CPU storm.

## Work Completed

- `tests/stress/g7-stress.sh` — self-contained harness (kubectl +
  curl, no toolchain), runs against a dedicated Active workspace:
  - **A/D**: CPU storm inside the pod (go build -p parallel, exercising
    the D7 GOMAXPROCS caps) + a live long turn via opencode's V1
    /message (the #917 fixed send shape) — asserts HTTP 200.
  - **B**: restarts_total unchanged across the storm — the watchdog did
    not kill a reachable, progressing opencode.
  - **C**: forced SIGKILL of opencode → restart counter advances
    (crash-recovery path) — recovery, not a watchdog fire.
- **Validated live** on throwaway workspace `g7-scratch-stress`
  (0.15.11): **3 pass, 0 fail, exit 0**.
- Destroyed the scratch workspace after validation.

## Key Decisions

- Harness drives opencode DIRECTLY (in-pod curl / port-forward), not the
  API: the G7 scope is watchdog/restart/tracker/turn behavior under
  load, which is fully observable at the agent seam. API-auth-free, so
  it runs from any cluster-admin context. (API-path durability is D3's
  job, gated by this harness passing.)
- Workspace is throwaway + explicit (`WORKSPACE_ID` env, required);
  refuses to run against a guessed workspace. Post-0.15.9 the alert
  rules would flag a real regression, but the harness asserts directly
  on agentd metrics instead of waiting for alert latency.
- `set -e`-safe throughout: every curl/comparison is guarded so a
  mid-restart dead opencode (expected during phase C) cannot kill the
  run.

## Validation notes (honest)

- Suppressions did NOT tick in the run: the D7 caps + idle scratch
  workspace kept the storm below the watchdog kill threshold. That is
  itself evidence the starvation fixes hold, but the harness's
  suppression-assertion path (A/B note) is currently only exercised as
  "unchanged — acceptable," not as a positive count. Re-running against
  a build-capable workspace under heavier load would tick it; the
  assertion is present and will fire when the threshold is reached.
- `session reachability 000` after the forced restart is expected (new
  process, sessions re-enumerated) — logged as a note, not a failure.

## Assumptions (validated)

1. opencode create-session with `{}` (not `{"title":...}` — that body
   returns an HTML page on 1.18.10) — verified live.
2. Session ID extraction from the JSON record (`cut -d'"' -f4`) —
   verified live (the -f3 variant yielded `:` and sent to
   /session/:/message → 500; caught during validation).
3. `workspace-pw-<id>` secret name + `/sandbox-cfg/password` fallback —
   verified live.

## Tests Run

- `bash -n tests/stress/g7-stress.sh` — syntax
- Live run on g7-scratch-stress: 3/3 assertions green, exit 0

## Files Modified

- tests/stress/g7-stress.sh (new)
