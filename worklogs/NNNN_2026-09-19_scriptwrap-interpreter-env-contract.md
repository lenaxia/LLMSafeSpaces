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
- Mutation evidence: (A) loadSecretsEnv reverted to hardcoded path + (B) EnvCheck probe call removed → 4 handler/unit tests FAIL; (C) EnvCheck body stubbed to return nil → 2 scriptwrap tests FAIL; restores green.

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
- `worklogs/NNNN_2026-09-19_scriptwrap-interpreter-env-contract.md` — this worklog
