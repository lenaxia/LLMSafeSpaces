# Worklog: #1878 — trigger inline scripts + workflow script nodes must execute in the workspace container environment

**Date:** 2026-10-23
**Branch:** `fix/1878-script-exec-env`
**Status:** implementation complete; PR opening

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

- [x] Design note posted (#1878 comment issuecomment-6357735742)
- [x] Red pins committed (b9845b59, both failing on main @ 43222359)
- [x] `scriptwrap.ExecuteWithEnv` (env-explicit exec; Execute delegates)
- [x] `opencodeBaseEnv()` extraction (managed_process.go; opencodeServeCmd
      + script children share one construction; secrets coordinate via the
      #1455 override seam — production-identical, env is sidecar-only)
- [x] supervisor-side `script_exec` engine (script_exec.go) + control-socket
      method (closed enum, deadline re-arm) + `newSupervisorControlServer`
      wiring (opencodeBaseEnv + live spawn delta)
- [x] sidecar forwarder seam: serverDeps.scriptExecForwarder, wired in
      buildSidecarDeps (controlClient.ScriptExec); execScriptNode
      forward/local split; EnvCheck text names the single-container cause
- [x] engine.go sessionless early-return covers ScriptPath-only
- [x] e2e contract flip (#1455/#1812: executes in BOTH modes) + docs
      execution-environment contract (docs/api/mcp.md)
- [x] born-green tests: engine (sentinel+belt, error mapping, bad params,
      env-check loud), control-socket round trip, mux forwarding (sentinel
      through the full mux→socket→engine path, loud failure with stderr,
      transport-loud naming control socket, timeout→504)

- [x] review r1 (PR #1885): F1 fixed — the pre-script leg was
      ErrorCode-blind (HTTP-200 error envelopes read as success), so a
      FAILED script_path-only trigger hit the new early-return as
      delivered + failure-counter reset. Now: ErrorCode != "" fails the
      fire, accounts it, and records actionResult.script.error; red
      pin TestExecuteRoutine_ScriptPathOnlyFailingScript_FailsLoud
      (failed pre-fix: triggerFail 0, status delivered). CHANGELOG
      tightened per the r1 nit. CI's -short failure
      (TestRequestSessionStatusSnapshot_Coalesces, api/internal/
      handlers) not-attributable: 0 diff files touch handlers/proxy,
      local -count=3 green, the same test PASSED in this head's
      full-suite race run (reviewer-corroborated), and main's own -short
      job failed 10 min prior (run 39909164886, scriptwrap node
      interpreter timeout) — the shared-runner contention window.

- [x] review r2 (head df85d2e8, verdict on PR #1885): F-A fixed —
      the pre-script failure arm logs the failure (errorOnlyLogger pin
      TestExecuteRoutine_PreScriptFailure_LoudAndGatePreserved) and
      logs (not swallows) the audit-write error; F-B fixed —
      composition-root pins TestSidecarDepsWireScriptExecForwarder +
      TestSupervisorControlServerWiresScriptExecEngine (the latter
      asserts delta parity through the engine's childEnv); F-C fixed —
      the failure arm carries BOTH keys ({gate:{proceed,vars}} from the
      open gate + {script:{error}}; UpdateTriggerFireAction REPLACES);
      F-D fixed — explicit bad_request→script_failed case (local-path
      behavior parity, comment + CHANGELOG claim amended). r2's own
      verdict: r1 F1 "properly fixed" + mutation-verified by the
      reviewer; issue #1878 "FULLY ADDRESSED" (skeptical pass AGREES).
      Note: r2's first delivery attempt failed mechanically (no
      verdict delivered — quota window); rerun delivered at 08:13Z.
      Secrets Integration board-subtest failures: reviewer-classified
      CI noise (pkg/board untouched; the reviewer re-ran every lane
      locally green at this head; same job failed on main + two
      sibling branches in the window).

## Live-probe instructions (post-deploy verification — the orchestrator
re-runs these against the DEPLOYED platform)

1. **Sentinel trigger (the #1878 probe verbatim).** Create a cron trigger
   (`* * * * *`) with `script` = `{"language":"python","handler":"def
   handler(input):\n    import subprocess\n    r =
   subprocess.run(['sh','-c','exec >>/tmp/opencode/sentinel-1878.log 2>&1;
   { date -u; id; which gh; gh auth status 2>&1 | head -2; echo DONE; }'],
   capture_output=True, text=True)\n    return {\"proceed\": True,
   \"vars\": {\"ok\": str(r.returncode)}}\n"}` and NO prompt. Wait one
   cycle. PASS: fire `delivered` with `{gate:{proceed:true}}` in the audit
   AND `/tmp/opencode/sentinel-1878.log` exists in the workspace showing
   `which gh` resolving + `gh auth status` output. FAIL shapes: fire
   `delivered` with no sentinel (the silent bug), or any fire `failed`
   with `script_env_unavailable`.
2. **Workflow node (the #1878 evidence shape).** `workflow_run` a
   one-node python workflow writing a marker under `/tmp/opencode/`.
   PASS: run `succeeded`, marker present in the workspace.
3. **Loud failure.** Trigger whose handler raises. PASS: fire `failed`
   with the stderr text in `actionResult.gate.error` (errors_only
   capture notwithstanding).
4. **No carrier churn.** While 1-3 run, `session_list` in the target
   workspace shows ZERO transient sessions (script-only fires are
   sessionless).

## Assumptions

- Same digest-pinned image serves sidecar + supervisor per pod (#863):
  no mixed-generation `method_unknown` window within a pod.
- `upload_apply`/`disk_recover` precedent licenses a long-held,
  ctx-bounded control-socket method with a closed error enum.
- #1882's delete-propagation items are OUT of this lane.

## Main-sync r3 (post-#1887/#1896, 2026-10-23) — union resolution

Main moved under the PR: #1887 (#1882 delete-propagation: engine
executeWithRetry gained the intermediateDeleted hook + NodeExecResponse.
DeletedSessionID; agentd envelope carries deletedSessionId) and #1896
(ai-workflows re-apply), plus #1808's fs_audit (#1891) landing on the
SAME control-socket seams. Four conflicts, all both-added-shape unions:

- engine.go: executeWithRetry call adopts main's new (…, nil,
  intermediateDeleted) signature; the #1878 F1 ErrorCode check and the
  loud/merged failure arm stay verbatim.
- control_socket.go: BOTH engine fields (scriptExec #1878 + fsAudit
  #1808) and BOTH dispatch cases (script_exec + fs_audit).
- sidecar_mode.go: BOTH buildSidecarDeps wirings (scriptExecForwarder +
  socketFsAudit) in the cc != nil block.
- supervise_opencode.go: BOTH newSupervisorControlServer wirings
  (scriptExec engine with session-env composition + fsAudit engine).

A bot-side main-sync (275ee728, pre-#1887 main) sits under this merge —
preserved, not dropped. No CHANGELOG union needed (auto-merge clean, one
[Unreleased], sections deduped by content). Post-merge: go build ./...,
api/internal/workflows (78s) + cmd/workspace-agentd (362s) full suites
GREEN with #1887's delete-tail live.
