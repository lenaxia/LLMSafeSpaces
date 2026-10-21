# Worklog: Trigger inline gate script + jinja-style var substitution (#1812)

**Date:** 2026-10-21
**Session:** Implement issue #1812 — triggers gain an INLINE executable script (language + handler source, workflow-script-node parity, NOT a pointer) whose `{proceed: bool, vars: {str: str}}` return gates the fire before any LLM call; vars substitute jinja-style (`{{key}}`) and append as trailing metadata; failures fail closed. Documentation mandate across every consumable surface.
**Status:** Complete

---

## Objective

Ship the six proposed defaults from #1812 as written: inline script (not pointer), boolean gate with no-LLM-call on false, `{str: str}` vars substitution (literal token replacement, unmatched left literal), all-vars trailing metadata, fail-closed on any script failure, prompt-only backward compat + script-only triggers with no LLM leg — plus the owner's documentation mandate (MCP tool descriptions discoverable alone, openapi field-level self-teaching descriptions, frontend editor + explainer + live validation, all 4 SDKs, README-LLM.md + docs/, gate outcome + vars in the fire audit).

---

## Work Completed

### Seam map + milestone (a)

- Anchors verified by reading source (all held): the routine fire path `Scheduler.tick → fireTrigger → fireRoutineTarget → executeRoutine` (engine.go — cron AND webhook fires both funnel through executeRoutine, so ONE gate insertion covers both); the agent leg dispatch `NodeExecRequest{NodeID: "routine-agent"}`; ScriptNodeData `{language, handler}` parity (spec.go:57); the legacy ScriptPath pointer pre-script (engine.go:885 — explicitly NOT what #1812 asks for); ActionResult as the "pre-execution action payload" column (store.go:159-165), unused on routine fires → the gate audit's home.
- Milestone (a) packet sent to the orchestrator; RATIFIED with Amendment 1 (frontend arm R9 added) and Amendment 2 (java SDK mapping completed: `sdks/java/.../TriggersService.java`, untyped Map service, create() gains `Object script` param + javadoc, update() raw-Map javadoc).

### Model + validation (red-first: `api/internal/handlers/triggers_script_test.go` before handler code)

- `types.TriggerScript{Language, Handler}` (pkg/types/workflows.go) — the API shape; `wf.TriggerScript` alias for store/spec code.
- Store columns `script_language`/`script_handler` (NOT NULL DEFAULT '' — empty = no script, the legacy shape) via migration `000040_trigger_script_gate` (+ helm mirror via `make chart-sync-migrations`, repolint-enforced).
- Create validation: both fields or neither; language ∈ {python, node} (scriptwrap runtimes, DAG `validScriptLanguage` parity); handler non-empty ≤ 64 KiB; **script + workflowId → 400** (routine-only ruling, ratified): a gate scripts a PROMPT leg and needs the routine's workspace sandbox; a DAG carries its own script/condition nodes.
- Update: `script` key-presence discriminated on the DTO RawMessage (absent=keep, `null`=clear, object=replace — the 0059 `input` pattern); conflict enforced on the POST-PATCH MERGED view in both directions (script onto DAG target; workflowId onto scripted routine).

### Gate execution (red-first: `api/internal/workflows/engine_gate_test.go` — zero-spawn pins)

- `pkg/workflows/trigger_gate.go`: `ParseGateScriptOutput` (strict {proceed: bool, vars:{str:str}} schema — every violation an error naming field + shape + rule; keys must not be empty / "."-prefixed ({{.x}} platform-token namespace) / brace-carrying; serialized vars ≤ 64 KiB), `SubstitutePromptVars` (single-pass `strings.NewReplacer` — no recursive re-substitution, unmatched literal), `PromptVarsMetadata` (deterministic sorted-JSON `[trigger script vars]` block), `ValidateTriggerScript`.
- `api/internal/workflows/engine_gate.go`: `runRoutineGate` dispatches `NodeExecRequest{NodeID: "routine-gate", NodeType: "script", spec: {language, handler}, input: fire envelope, timeout 5m}` via `executeWithRetry` (the #1458 bounded-transient-retry contract). proceed=false → skipped fire + `{gate:{proceed,vars}}` in ActionResult, NO failure accounting. Any failure (transport error after retries, errorCode envelope, empty output, unparseable JSON, schema violation) → fail-closed failed fire + `{gate:{proceed:false,error}}` + `accountTriggerFailure`, zero agent legs.
- engine.go `executeRoutine`: gate inserted AFTER EnsureActive (scripts run in the workspace pod), BEFORE memory/pre-script/agent legs. proceed=true: gate payload written BEFORE the agent leg (new `SchedulerStore.UpdateTriggerFireAction`/store method); vars substitute LAST (after {{.prevResult}}/{{.scriptResult}}/{{.input}}) so values land verbatim; metadata appended. **Script-only (script + empty prompt): no agent leg even on proceed=true → delivered**; prompt-only no-script keeps firing an agent leg exactly as before.
- mockAgentd grew a `calls []string` dispatch counter — the no-LLM-call pins COUNT spawns (zero "routine-agent" dispatches), never a log line.

### Documentation mandate (each surface an acceptance checkbox)

- **agentd MCP** (`cmd/workspace-agentd/mcp_server.go`): trigger_create/trigger_update/trigger_fires descriptions teach the full contract + a worked example (nightly digest); description pins extended in mcp_server_test.go (INLINE SOURCE, return schema verbatim, NO LLM call, {{key}}, fail-closed, routine-only, worked example, skipped/gate payload shapes).
- **Platform MCP** (`pkg/mcp/workflow_tools.go` + client.go): script arg on trigger_create/update (JSON-object-string convention, same as input), descriptions with the gate contract; passthrough + garbage-JSON boundary pins in server_test.go.
- **openapi** (sdks/openapi.yaml): `TriggerScript` schema with the full contract + example; script on Trigger/TriggerCreateRequest/TriggerUpdateRequest with field-level key-presence semantics; TriggerFire.actionResult documents the gate payload. `go run ./validate` + validate tests green.
- **Frontend**: `TriggerScript` types (api/workflows.ts) + shared `TriggerScriptEditor` component (language selector, inline source editor, visible gate/substitution explainer, live partial-script validation naming the missing field, aria labels); wired into create + edit-target forms; script-only create submittable; clearing patches `script: null`; detail card shows the stored script. 24/24 TriggersPage tests (6 new).
- **SDKs**: go (Trigger/CreateTriggerReq/UpdateTriggerReq + docs), python (create `script` kwarg + full docstring; update docstring), typescript (TriggerScript interface + create/update req docs), java (TriggersService create param + class/method javadoc).
- **Docs**: docs/api/mcp.md trigger table + a full "inline gate script" section with the worked example; README-LLM.md Development Workflow gains the "Trigger script gates (#1812)" subsection with the same example + mechanics pointers.
- **Fire audit**: gate outcome + substituted vars in ActionResult for every gated fire (open, closed, failed) — fires self-explain via trigger_fires.

### e2e (nightly-registered)

- `local/issue-1812-trigger-gate-e2e.sh`: R0 self-teaching 400s; R1 false-gate (skipped + payload); R2 true-gate script-only (delivered + vars + in-workspace side-effect marker); R3 fail-closed; R4 prompt-only compat. Every execution row asserts ZERO workspace sessions (the no-LLM evidence). **Adaptive arms per #1455**: the nightly's agentd sidecar mode cannot execute script nodes (script_env_unavailable, named) — those clusters assert the fail-closed arm + zero sessions; exec-mode clusters assert the real gate outcomes. No LLM_MODEL needed (script-only triggers).
- `local/issue_1812_trigger_gate_e2e_script_test.go`: bash-syntax + row/assertion pins + nightly registration pin (the #1452 lesson).
- Registered in `.github/workflows/e2e-nightly.yml` (PORTFWD_PORT **18093**). **r1 correction:** my first port choice (18089) collided with the US-72 rogue-agent sweep's EXCLUSIVE port (the `TestUS72SweepWiring_PortIsolated` pin requires 18089 to appear exactly once — it is the sweep's own reserved port, not a free one). The first "fix" was worse than the collision: a global sed moved the sweep itself to 18092 alongside my step, breaking the pin on the pushed head (caught by the r1 review as the blocking finding). Corrected: sweep restored to 18089, the #1812 step moved to the genuinely unused 18093; `go test -run 'TestUS72|TestIssue1812' ./local/` green on the corrected head.

---

## Key Decisions

1. **Gate script executes in the target workspace pod via agentd script node** (not API-server-side) — scriptwrap's package contract mandates sandbox-only execution; parity with workflow script nodes is the issue's requirement. Consequence: EnsureActive precedes the gate (a closed gate still activates the pod — accepted; the promise is no LLM call, not no pod).
2. **Gate outcome lives in ActionResult** — the documented "pre-execution action payload" column, unused on routine fires; the Result column shape is untouched, protecting the `{{.prevResult}}` memory read path (#1453's agentd envelope contract).
3. **Closed gate = `skipped`, no failure accounting** — a gate returning false is the script working, not failing; auto-disable budget untouched. Script FAILURE is `failed` + accounting (mirrors the existing pre-script failure semantics).
4. **Script rejected on workflow-targeted triggers** (ratified ruling) — no prompt to template, no workspace column to execute in, DAGs have their own gating nodes. Enforced at create AND on the post-patch merged update view, both directions.
5. **Vars substitute LAST, single pass; ".="-prefix keys rejected** — var values land verbatim (a value containing `{{.input}}` or another token is never re-processed); the `{{.x}}` namespace stays reserved for platform tokens.
6. **Script-only = no LLM leg even when the gate opens** — the literal reading of "gate + side-effect execution with no LLM leg"; this is also what makes the e2e LLM-free.

## Assumptions (stated + validated — per Rule 7)

- A1 scriptwrap must run in the workspace sandbox → validated: package doc "Execute MUST only be called inside a workspace pod"; #1455's script_env_unavailable contract.
- A2 no-LLM-call is pinnable by counting dispatches → validated: mockAgentd `calls` counter; e2e zero-sessions assertion.
- A3 ActionResult is the audit home → validated: store.go:159-165 column semantics; TriggerFireResponse exposes it.
- A4 UpdateTriggerFireResult COALESCEs completed_at → validated: store.go:760 — skipped/failed gate fires complete correctly.
- A5 e2e needs no LLM_MODEL → validated: script-only triggers never dispatch the agent leg (engine test R4 pins it); #1455 precedent runs script nodes with no model.
- A6 SchedulerStore has exactly 2 test implementors + the real store → validated by grep; both updated.

## Blockers

None. One observation for the orchestrator: the FULL `./cmd/workspace-agentd/` suite times out (>190s) on this pod **including on the clean origin/main baseline** (hangs at TestManagedProcess_StopWithSlowChild_SIGKILLFallback under sibling-lane load) — pre-existing/environmental, not from this diff; the touched tests (mcp_server/mcp_tools) pass targeted. Full sweeps ride CI per the standing directive.

---

## Tests Run

(On the final corrected head unless noted; the intermediate gocyclo-fix head briefly carried the sweep-port break below — r1's review caught it, and the r1 push corrected it.)

- `go test ./pkg/workflows/` — ok (gate units + integration suite compile; PG rows skip without TEST_DATABASE_URL).
- `go test ./api/internal/workflows/` — ok (75s; engine gate pins false/true/error/schema/script-only/compat).
- `go test ./api/internal/handlers/` — ok (80s; script validation + lifecycle).
- `go test ./pkg/mcp/` — ok (script passthrough + description pins). `go test -run 'TestUS72|TestIssue1812' ./local/` — ok **after the r1 port correction** (the pre-correction head FAILED TestUS72SweepWiring_PortIsolated — my sed had moved the sweep off its reserved 18089; see the e2e section's correction note).
- `go test -run 'TestMCPHandler_ToolsList' ./cmd/workspace-agentd/` — ok (description pins).
- `cd sdks && go run ./validate/main.go openapi.yaml && (cd validate && go test .)` — ok.
- `cd frontend && npx vitest run src/pages/TriggersPage.test.tsx` — 24/24.
- `go test -run TestOpenAPIRouterContract ./api/internal/server/` — ok.
- Repolint + gofmt + goimports + helm-render via pre-commit — pass (after chart-sync-migrations + make fmt).

## Next Steps

- Orchestrator: mutation-verify (revert engine_gate.go's agent-leg skip / parse strictness → R1-R4 pins go red) and merge; release train/CHANGELOG yours.
- If a future lane lifts the sidecar script-node limitation (#1455), the e2e's exec arms become the nightly-expected arms — flip the adaptive-arm comments, nothing else.

## Files Modified

- api/internal/handlers/triggers.go, triggers_test.go, triggers_script_test.go (new), pod_automation_crossws_fire_1729_test.go
- api/internal/workflows/engine.go, engine_test.go, engine_gate.go (new), engine_gate_test.go (new)
- api/migrations/000040_trigger_script_gate.{up,down}.sql (new) + helm/migrations mirror
- pkg/types/workflows.go; pkg/workflows/{store.go,store_integration_test.go,trigger_gate.go (new),trigger_gate_test.go (new)}
- pkg/mcp/{client.go,workflow_tools.go,server_test.go}; cmd/workspace-agentd/{mcp_server.go,mcp_server_test.go}
- frontend/src/api/workflows.ts; frontend/src/pages/TriggersPage.tsx, TriggersPage.test.tsx
- sdks/openapi.yaml; sdks/go/workflows.go; sdks/python/llmsafespaces/client.py; sdks/typescript/src/client.ts; sdks/java/.../TriggersService.java
- docs/api/mcp.md; README-LLM.md; .github/workflows/e2e-nightly.yml; local/issue-1812-trigger-gate-e2e.sh (new); local/issue_1812_trigger_gate_e2e_script_test.go (new); worklogs/NNNN_2026-10-21_trigger-script-gate.md (this file)
