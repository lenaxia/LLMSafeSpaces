# Worklog: #1455 — script-node environment contract + http-node secrets coordinate

**Date:** 2026-09-19
**Session:** fix/1455-scriptwrap-interpreter-env — localize the scratch-sidecar script-node environment gap; land the loud-failure contract and the independently-implementable http-node secrets fix (investigate-first per delegation; Option-B execution redesign stays decision-gated on the issue)
**Status:** Complete

---

## Objective

Investigate #1455 (script nodes fail `create temp dir: stat /tmp` in the scratch sidecar — no /tmp, no interpreters), then land what the evidence supports WITHOUT foreclosing the owner's Option-B ruling: a loud, distinguishable failure for script nodes in an unusable environment, the adjacent http-node secrets-env defect the prior `/analyze` found, and the documented per-mode contract.

---

## Work Completed

### Investigation (all prior `/analyze` claims independently re-verified on main @ cea33f5e)
- `loadSecretsEnv` hardcoded `/sandbox-runtime/secrets-env` (workflow_execute.go:577 pre-fix); `secretsEnvPathFromEnv()` (us4b_paths.go:36) is the shared US-4b coordinate whose consumer list missed the workflow http-node reader; the read error is swallowed at the call site (`secrets, _ :=`), so `{{secrets.*}}` refs stayed literal in sidecar mode with no signal.
- scriptwrap `Execute` depends on `os.MkdirTemp("")` (TMPDIR-resolved) then `exec python3/node` against PATH (wrapper.go) — both absent in the FROM-scratch sidecar; the user mux serving `/v1/workflow/node/execute` runs in the sidecar process in sidecar mode (server.go / sidecar_mode.go wireHTTPServers), so script nodes execute there today.
- Live corroboration: THIS dev pod runs sidecar mode — `/sandbox-runtime/secrets-env` does not exist locally (the pre-fix red run of the override test read exactly that error).

### Fix 1 — http-node secrets coordinate (the independently-implementable defect)
- `loadSecretsEnv` now reads `secretsEnvPathFromEnv()`; us4b consumer comment updated. Behavior parity in single-container mode (same default path).

### Fix 2 — script_env_unavailable: loud, named, deterministic
- New `scriptwrap.EnvCheck(language)` probes Execute's own prerequisites (writable temp dir via the same `os.MkdirTemp` resolution, interpreter via `exec.LookPath`) — an environment probe, not a mode flag, so it stays correct if container layouts change.
- `execScriptNode` probes before Execute; failure surfaces HTTP-200 `errorCode: script_env_unavailable` with detail naming the missing prerequisite AND the known cause (sidecar scratch container) + the epic-64 contract. Unknown languages pass through to Execute's existing validation.
- Engine guard: `script_env_unavailable` pinned OUTSIDE the retry class (an environment does not heal in 2s) — one attempt, like other deterministic node failures.

### Docs (the per-mode contract)
- epic-64 README script section: added "Execution container is mode-dependent (#1455)" paragraph — single-container = workspace sandbox (unchanged); sidecar = fails fast with `script_env_unavailable`; Option-B execution is the open design question; http/agent/condition status stated.

### Review round 4 (1 blocking → fixed: R1a tautology)
- R1a matched the handler marker on the RAW run row — but the marker ships in every row's specSnapshot (handler source embedded), so the assertion reduced to `status == succeeded` with a dead failure branch; the succeed-without-executing drift class would false-pass. Fixed with the same `.output`-extraction pattern R2 established (`r1_output` via jq string-or-object idiom; the script node's output IS the handler's return dict, so the literal can only come from a real execution). The R1 extraction is pinned in `RowsAndAssertions` alongside R2's.

### Review round 3 (2 blocking → fixed; the "unvalidated script" class mechanically closed)
- **R0-fatal WS_BASE**: the 9-hex first group (`e2e145500…`) made `ws_id` construct a non-canonical UUID that PostgreSQL rejects at the seed insert (reviewer verified against PG 16's `string_to_uuid`). Fixed to 8-hex (`e2e14550-…`); added `TestIssue1455E2EScript_WorkspaceIDCanonical` (simulates `ws_id` for multiple suffixes, asserts canonical 8-4-4-4-12) so the class cannot recur silently. Cross-lane note posted on PR #1464 — its `issue1452` script (and 1342) carry the same latent defect and will die at R0 the night the #1456 gate opens.
- **R2b tautology**: asserting the literal `{{secrets.WT1455_ABSENT_VAR}}` on the raw run row could never fail — the row's specSnapshot embeds the spec. Both R2 arms now assert on the ECHOED BODY (`.output | if type == "string" then . else (.body // tostring) end`); R2a stays discriminating (the plaintext exists nowhere in the spec), R2b now genuinely pins pass-through.
- **Cleanup gaps**: the EXIT trap now deletes the workspace (1417 pattern — no pod/PVC lingers through the nightly) and reaps `PF_PID` (our trap+function shadow us70-common's own EXIT reap — one trap per signal, later definition wins).
- **jq-compile pin added** (`TestIssue1455E2EScript_JqFiltersCompile`, the 1452-r2 class: an unbalanced filter aborts under `set -e` exactly when the fix works).
- **Rule-7 residual, stated honestly**: still no live-cluster execution of the script (a local kind run is the concurrent-build OOM class under the memory directive; the nightly is the execution lane post-merge). The mechanical layer now covers syntax, jq compilation, UUID canonicity, harness sequencing, and row assertions — the two r3 findings were exactly the classes this layer now catches.

### Review round 2 (4 blocking e2e findings → resolved; R0 + tautology superseded by r3)
- **R0 abort**: the script never called `harness_start` — `seed_workspace` hard-requires its OWNER_ID; fixed, and `harness_start` added to the structural pins so the regression cannot recur silently.
- **Unwired**: the script is now a step in `e2e-nightly.yml` (after the 1452 rows; 1452's same-commit wiring precedent).
- **False premise corrected**: the nightly installs `controller.agentdSidecar.enabled=true` (e2e-nightly.yml:181) — the header's "pre-flip clusters lack sidecar mode" was wrong. Reframed: R1b (the loud-failure contract) is the EXPECTED nightly arm; R1a covers single-container pool/dev clusters. Rule-7 lesson recorded: the premise shipped unvalidated.
- **http-secrets live row written** (omission justification was contradicted by the harness — `bind_env` exists in us70-common, the 1417 script self-provisions an in-cluster HTTP upstream): R2 binds `WT1455_PROBE_TOKEN` via the convenience endpoint, waits for materialization (`secrets_converged` + `wait_env_present`, the faults-e2e helper shape inlined with attribution), then runs an http node against an in-cluster header-echo Deployment/Service (1417 pattern): the echoed Authorization header must carry the resolved value (R2a — exercises the fixed `loadSecretsEnv`→`secretsEnvPathFromEnv` join against the relocated sidecar coordinate), and an unbound ref must stay literal (R2b — the documented pass-through semantics). Echo upstream cleaned up in the EXIT trap.
- **Non-blocking reviewer notes accepted as-is** (recorded): R1's `"sidecar"` needle matches the detail's conditional-attribution sentence (static text — a single-container TMPDIR break would also match; the conditional sentence hedges and full mode-assertion needs pod-spec access the API-only harness avoids); failed runs surface `errorCode: node_failed` at the run row with `script_env_unavailable` in the detail text — the script substring-matches accordingly.

### Review round 1 (blocking → resolved; the e2e half superseded by r2)
- **Premature closure keyword**: `Fixes #1455` would auto-close the issue with its central Option-B decision open → PR body changed to "Partially addresses #1455 — Refs #1455".
- **E2E gap (r1 attempt)**: added the adaptive mode-contract row + companion structural pins. The http-node live row was omitted with a justification r2 disproved (harness HAS `bind_env` + the 1417 in-cluster upstream pattern) — the r2 section above records the corrected live row.
- **Minor findings fixed**: interpreter binaries now shared constants (`pythonBin`/`nodeBin`) between `Execute` and `EnvCheck`; the failure detail attributes the sidecar cause CONDITIONALLY ("In sidecar mode … On a single-container pod this instead indicates a broken TMPDIR/PATH") — no unconditional misattribution.
- **Flip-gate runbook**: `docs/runbooks/sidecar-flip.md` prerequisite 6 added — http secrets fixed (with pin references), script nodes loud-fail, Option-B open and a flip blocker for script-bearing workflows.

---

## Key Decisions

1. **Probe the environment, not the mode** — `EnvCheck` tests prerequisites directly; mode detection would couple the failure to a flag that correlates with the cause today but isn't the cause.
2. **errorCode over error-string matching** — `script_env_unavailable` is a first-class wire code (HTTP-200 `{errorCode, detail}` convention), deterministically non-retryable by construction (unknown codes default out of `retryableAgentdFailure`; pinned by an engine guard test).
3. **No Option-B implementation** — the prior `/analyze` explicitly reserves B1-vs-B2 for an owner ruling with a design doc; this PR lands the contract + adjacent fix only. The loud failure is the honest interim behavior the issue title asks for.
4. **Swallow-at-call-site left as-is** — missing secrets file ⇒ empty map ⇒ literal refs is existing single-container semantics shared by both modes post-fix; redesigning that signal is not this issue.

### Assumptions stated and validated (Rule 7)

- The handler-level secrets test's probe key (`WT1455_PROBE_TOKEN_ZQX`) is absent from any real pod's secrets-env — validated for this pod (no `/sandbox-runtime/secrets-env` at all); unit test carries the deterministic half.
- `os.MkdirTemp("")` honors TMPDIR — validated by the red run's error text (`stat /nonexistent-scriptwrap-sidecar-tmp`), the same resolution shape the live issue reports for `/tmp`.
- Unknown-language pass-through preserves pre-fix behavior — validated by the existing `TestWorkflowExecute_ScriptUnsupportedLanguageKeepsDetail` (green post-fix).

---

## Blockers

None. (Note: verification was interrupted by a pod OOM ~03:15Z and resumed post-cleanup; the memory directive — gate heavy commands on cgroup memory.current < 7.2e9, targeted runs only, one heavy step per turn — applied from resume onward.)

---

## Tests Run

- RED witnessed on main: `TestLoadSecretsEnv_HonorsOverridePath`, `TestWorkflowExecute_HTTPNodeSecretsResolveFromOverridePath` (literal ref sent), `TestWorkflowExecute_ScriptEnvUnavailableLoudFailure` (got `script_failed` "create temp dir: stat …"), `TestWorkflowExecute_ScriptInterpreterMissingLoudFailure` (got `script_failed` "executable file not found"); scriptwrap EnvCheck tests compile-red.
- `go test ./pkg/workflows/scriptwrap/ ./api/internal/workflows/` — ok (post-module-cache-wipe, GOPROXY=direct)
- `go test -race ./cmd/workspace-agentd/` — ok (345s)
- r1: targeted reruns — scriptwrap EnvCheck suite, agentd script/http handler suites, `./local/` companion pins — all ok
- r2: `bash -n` + `TestIssue1455` companion pins (updated for harness_start + R2 needles) — ok; nightly wiring greps verified (`issue-1455-scriptenv-e2e.sh` present in e2e-nightly.yml)
- r3: all four companion pins green (BashSyntax, RowsAndAssertions, WorkspaceIDCanonical, JqFiltersCompile); `bash -n` ok
- Mutation evidence: (A) loadSecretsEnv reverted to hardcoded path + (B) EnvCheck probe call removed → 4 handler/unit tests FAIL; (C) EnvCheck body stubbed to return nil → 2 scriptwrap tests FAIL; restores green.
- `bash -n local/issue-1455-scriptenv-e2e.sh` — ok (also pinned by TestIssue1455E2EScript_BashSyntax)

---

## Next Steps

- Reviewer rounds on the PR; Option-B (B1 vs B2) owner ruling tracked on the issue — on a ruling, a design doc + implementation PR follows (execute script nodes in the workspace container; the flip-gate checklist should keep both #1455 defects as `agentdSidecar.enabled` flip blockers, this PR clears the loud-failure + http-secrets halves).

---

## Files Modified

- `pkg/workflows/scriptwrap/wrapper.go` — EnvCheck
- `pkg/workflows/scriptwrap/wrapper_test.go` — 3 EnvCheck tests
- `cmd/workspace-agentd/workflow_execute.go` — EnvCheck probe → script_env_unavailable; loadSecretsEnv coordinate fix
- `cmd/workspace-agentd/workflow_execute_test.go` — 4 new tests (override path, http handler pin, 2 loud-failure pins)
- `cmd/workspace-agentd/us4b_paths.go` — consumer-list comment
- `api/internal/workflows/engine_test.go` — script_env_unavailable non-retry guard
- `design/stories/epic-64-triggers-workflows/README.md` — per-mode execution-container paragraph
- `docs/runbooks/sidecar-flip.md` — flip-gate prerequisite 6 (#1455 workflow-node gate)
- `local/issue-1455-scriptenv-e2e.sh` — adaptive mode-contract e2e row (r2: harness_start, corrected nightly-sidecar premise, R2 http-secrets live row)
- `local/issue_1455_e2e_script_test.go` — structural pins for the e2e script (r2: harness_start + R2 needles)
- `.github/workflows/e2e-nightly.yml` — r2: scriptenv rows wired into the nightly (after the 1452 rows)
- `worklogs/1001_2026-09-19_scriptwrap-interpreter-env-contract.md` — this worklog
