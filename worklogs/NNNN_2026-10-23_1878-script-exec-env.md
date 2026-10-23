# Worklog: #1878 — trigger inline scripts + workflow script nodes must execute in the workspace container environment

**Date:** 2026-10-23
**Branch:** `fix/1878-script-exec-env`
**Status:** red-first pinned, implementation in progress

---

## Objective

Owner ruling (2026-10-23): "the script should be executed in the same
environment that the sessions run" / "if the inline script cannot reach
github then thats a bug". Today both trigger inline scripts (gate +
pre-script) and workflow script nodes execute inside agentd's own
process: in sidecar mode (`agentdSidecar.enabled`) that is the FROM-
scratch uid-2000 container — no `/tmp`, no interpreters, no `gh`, and a
READ-ONLY `/workspace` mount. Workflow script nodes fail loudly
(`script_env_unavailable`, #1455); trigger scripts failed SILENTLY on
the deployed engine (probe `sidecar-verify-probe`: fire `delivered` in
0.77s, sentinel never created — issue #1878 verification comment).
Sibling #1882 traced the routine dispatch path
(engine.go:953 → workflow_execute.go:456) and measured the carrier-
session churn (~75 ghosts/hour/workspace) of the remaining ScriptPath-
only + empty-prompt fall-through.

## Design (posted to #1878 before implementation — issuecomment-6357735742)

**Route script dispatch to the workspace container's existing execution
channel: the design-0051 Appendix-A control socket (127.0.0.1:4099).**
New `script_exec` method: the sidecar's `execScriptNode` forwards script
nodes to the workspace-container supervisor (`agentd
supervise-opencode`, uid 1000), which runs `scriptwrap.Execute` with the
SAME env construction the opencode child spawns with
(`opencodeBaseEnv()` = secrets-env sourced + redact PATH prepend +
opencode exports, plus the live spawn delta) — extracted to one helper
so script children and session children cannot drift.

Rejected: equipping the sidecar (toolchain-free by design-0051 posture;
RO `/workspace`; secrets must not cross into uid-2000 space) and routing
via the opencode session API (mints the carrier session per script —
the owner explicitly ruled "we shouldn't need to create a session to run
a script"; #1882's cleanup lane).

Loudness: the closed error enum (`bad_request`, `script_env_unavailable`,
`script_failed`, `script_timeout`) maps 1:1 onto the workflow error
envelope; forward-transport failure (supervisor restarting) is a loud
terminal `script_env_unavailable` naming the control socket — no silent
success. The #1812 fail-closed gate already carries script errors into
the fire audit's `actionResult.gate.error`; the remaining engine gap
(ScriptPath-only + empty prompt → agent leg with empty prompt) is closed
here: script-only fires never reach `execAgentNode`.

## Red-first pins (both verified failing on main @ 43222359)

1. `api/internal/workflows/engine_test.go`
   `TestExecuteRoutine_ScriptPathOnlyEmptyPrompt_NeverDispatchesAgentLeg`:
   ```
   Error: []string{"routine-script", "routine-agent"} should not contain "routine-agent"
   Messages: a script-only fire must NEVER reach the agent leg — that mints
   the ephemeral carrier session (#1878/#1882)
   ```
2. `cmd/workspace-agentd/workflow_execute_test.go`
   `TestWorkflowExecute_ScriptNodeChildSeesSessionEnv`:
   ```
   script child must see the secrets-env-sourced session env (the #1878
   ruling); output: {"probe":null}
   ```

## Implementation ledger

- [x] Design note posted (#1878 comment)
- [x] Red pins committed
- [ ] `scriptwrap.ExecuteWithEnv` (env-explicit exec; Execute unchanged)
- [ ] `opencodeBaseEnv()` extraction (managed_process.go)
- [ ] supervisor-side `script_exec` engine + control-socket method
- [ ] sidecar-side forwarder seam (serverDeps + execScriptNode)
- [ ] engine.go sessionless early-return covers ScriptPath-only
- [ ] e2e contract flip (#1455/#1812 adaptive rows) + docs contract
- [ ] born-green tests for the new seams

## Assumptions

- Same digest-pinned image serves sidecar + supervisor per pod (#863):
  no mixed-generation `method_unknown` window within a pod.
- `upload_apply`/`disk_recover` precedent licenses a long-held,
  ctx-bounded control-socket method with a closed error enum.
- #1882's delete-propagation items are OUT of this lane.
