# Worklog — #1747 §5 repeal: the manager-grant RBAC concept removed from board tools

Lane: `tsk-copper-refectory-generates` (epic "De-RBAC the board tools"). Owner
ruling 2026-10-27, verbatim: "The concept of a manager grant needs to go away.
This is a sharp edge that will cause pain. Why is there even a concept of RBAC
here. We have access to the whole system and if a task is going to impersonate
another session that's the user's prompting issue not mine." Documented, not
re-litigated: a workspace pod is single-tenant (every session shares one
credential — the platform's own docs: "the origin is metadata, NOT
authentication"), so an identity-grant layer over a shared credential is
theater; the live failure (the CREATING session refused
`board_rearm_task`/`board_list`/`board_health` while create/update accepted it,
stranding rows at `pending_input`) was the incoherence that proved it.

## 1. What was removed (identity gates)

- **agentd tier** (`cmd/workspace-agentd/board_manager_tools.go`):
  `boardManagerSession` reduced from gate to identity RESOLUTION — the
  injected identity wins (send_message pattern), the declared fallback
  (3-word phrase or alias, #1816) resolves through the §4.4 precedence, and
  an absent/mistyped/unresolvable identity degrades to EMPTY AUDIT
  ATTRIBUTION with a loud stderr note (`logBoardAttributionLoss`) — never a
  refusal. The spawned-worker registry refusal is gone from the dispatch;
  manager-grade dispatch (token-less move_stage, rearm, list, health, bulk,
  CRUD) serves any in-workspace session (`dispatchBoardManagerTool`).
- **Platform belt** (`api/internal/handlers/board_automation_manager.go`):
  `resolveManager`'s required-session 403 and belt-error 503 are gone —
  `boardSessionID` (query first, then exact-key body probe) is attribution
  only; `ok=false` remains only for the pod-identity resolve failure.
- **Engine belt** (`pkg/board/pause.go`): `IsWorkerSession` (the
  live-assignee SQL the seam consulted) deleted, with its kill-test
  (`TestKill_1747_IsWorkerSession`). `managerActor` still stamps the actor
  (owner, `SurfaceAgentManager`, workspace, calling session) — the audit
  vocabulary is unchanged.

## 2. What was KEPT (correctness machinery, not RBAC)

- Claim-token arbitration: 409 `claim_token` everywhere (worker compat 1746,
  worker/manager semantics integration, parity suites re-run green).
- Stage machine + Done guards (pending_input exits stay worker-denied on the
  plain agent surface — `GuardManagerSurface` on `SurfaceAgent` only; the
  manager ROUTE is now simply open to every session).
- The #1946 reviewer-persona `setCriteriaMet` scoping, BOTH tiers: the
  engine's persona-tier grant (criteria_met.go, refusal text re-anchored to
  the new ruling) and agentd's `boardWorkerSelfCertificationPin` — the ONE
  surviving registry check (a spawned TASK worker's token-less
  self-certification through the manager shape of `board_update_task` still
  refuses; the registry's `isWorker` survives for exactly this pin).
- External REST/MCP user-token authn (real tenancy) untouched.

## 3. Tests re-pinned (born-RED refusal pins → open-behavior pins)

- agentd: `TestMCPBoard_ManagerOpenToAnySession` (no-identity proceeds with
  empty attribution; a RECORDED spawned worker reaches
  `board_epic_activate`/`board_pause_task`; injection beats a declared worker
  id; worker tools unchanged), `TestMCPBoard_ManagerGrantGate` (the old
  refusal pin) deleted, `TestMCPBoard_ManagerInjectionSupply` /
  `TestMCPBoard_ManagerDeclaredPhraseResolves` /
  `board_manager_e2e_integration_test.go` re-pinned to attribution-only
  (unresolvable declared ref → proceeds, loudly).
- API: `TestBoardAutomation_ManagerGrantRepealed` (live-worker session and
  identity-less caller both CREATED; audit actor carries/strips the session
  id; DELETE-class query-carried omission proceeds) and
  `TestBoardAutomation_ManagerBeltRemoved` (the r2 Security-1 fail-closed
  403/503 posture repealed with the grant).
- Engine/integration: `board_manager_semantics_integration_test.go` §3 (a
  REAL live assignee — `ClaimNext` then a manager create AS `claim.Assignee`
  — succeeds and attributes) and §4 **the regression for tonight's live
  failure**: the creating session re-arms its OWN attempts-exhausted
  `pending_input` row through the manager seam (backlog, attempts reset to
  0, `rearm` audit event attributed to the creating session).
- `pkg/board` criteria-met/persona suites re-anchored (the #1946 refusal
  keeps its narrow shape with the new ruling text).

## 4. Prose

Design 0067 §10.10 amendment (additive; §10.2 carries a supersession
pointer), CHANGELOG `[Unreleased]` entry, tool descriptions in
`mcp_server.go`/`mcp_tools.go`, `llmsafespaces-origin.js` header,
`board_workers.go`/`board_manager_tools.go`/`stage.go`/`board.go`/`loopback.go`
comments, `board_automation_manager.go` headers — every live citation of
"#1747 §5"/"manager grant"/"orchestrator-class" either removed or converted
to a repeal annotation citing this ruling. Historical records (past worklogs,
released CHANGELOG sections, immutable migration comments) intentionally
keep their era-accurate text.

## 5. Verification

`go build ./...` green; `go test ./cmd/workspace-agentd/... ./api/...
./pkg/board/...` green; `golangci-lint run` → 0 issues. Evidence:
`/workspace/reports/evidence/tsk-copper-refectory-generates/README.md`.
