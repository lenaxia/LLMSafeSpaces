# tests/stress

Stress harness for the session-truthfulness incident class (design 0050, issue #907).

## Files

- `g7-stress.sh` — the G7 harness: drives CPU storm + live turn + forced
  restart against a real workspace and asserts the acceptance criteria
  against agentd metrics and kubelet state.
- `g7-self-test.sh` — self-test for the harness's own parsing logic
  (series-sum extraction, session-id field, marker detection, empty-scrape
  behavior). Run before trusting a harness run.

## Conventions

- **Always run a dedicated throwaway workspace.** The harness uses only
  the workspace named by `WORKSPACE_ID` and refuses to guess; it performs
  a CPU storm and SIGKILLs opencode inside that workspace. Never point it
  at a workspace with live user work.
- **No admin credentials.** agentd `/metrics` is unauthenticated on the
  admin port (the PodMonitor scrapes it that way); the harness reads it
  in-pod without touching `AGENTD_ADMIN_TOKEN` or any other secret.
- **Fail-open semantics.** Metric reads that return nothing are treated as
  failures, not as zero — an empty scrape must never green a run.
- **Assertion discipline.** Only the criteria actually asserted appear in
  the header. A soft `note:` means "signal not present this run, assertion
  present for when it is" — it does not green a criterion on false evidence.

## Usage

```sh
# self-test (no cluster needed)
bash tests/stress/g7-self-test.sh

# live harness (cluster admin context)
WORKSPACE_ID=<dedicated-active-workspace> bash tests/stress/g7-stress.sh
```

## Relationship to G7 / D3

The harness is the manual gate that D3's merge requires per the issue
ruling. It is not wired into CI — workspace stress runs need live compute
and a dedicated workspace, which the normal CI matrix does not provide.
Until a nightly/LLM-cred-gated runner exists, treat it as the documented
pre-D3 checklist, and re-run it against a throwaway workspace before any
D3 PR merges.
