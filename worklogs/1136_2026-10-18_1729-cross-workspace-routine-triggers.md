# Worklog: #1729 — Automation: allow routine triggers to be created for other workspaces (owner-scoped)

**Date:** 2026-10-18
**Session:** Worker lane, issue #1729 (owner dogfood report on prod v0.38.1: the deploy-resume choreography needed triggers in three workspaces; only one could be created from a single orchestrator session).
**Status:** Complete (PR pending review)

## Objective

Implement the issue's proposed semantics verbatim:

- `CreateTriggerRequest` accepts an optional `targetWorkspaceId` for routine triggers (workflow-targeting rules unchanged);
- authorization: the authenticated trigger OWNER must have owner/admin rights on the target — 403 otherwise, 404 for non-enumerable workspaces (the existing cross-workspace disclosure policy);
- default (omitted) = current workspace — fully backward compatible;
- the created trigger runs its routine IN the target workspace, with that workspace's identity;
- audit: creation events record both creating-session workspace and target workspace;
- MCP surface (`trigger_create` in agentd) grows the optional param and documents it.

## Where the restriction lived (verified in tree)

`api/internal/handlers/pod_automation.go` — the pod-automation seam
(POST /internal/v1/automation/triggers, the surface behind agentd's
`trigger_create`). `scopeTriggerCreateBody` force-stamps `workspaceId`
to the creating pod's workspace for routine bodies (#1412) after
`normalizeTriggerTargetKeys` strips every caller-supplied workspace key
variant. The engine side was already target-driven:
`api/internal/workflows/engine.go` `executeRoutine` keys activation
(`EnsureActive`) and the agentd dispatch off `trigger.WorkspaceID`, so a
row stamped with the target fires there with the target's identity —
the work is all in the create path + authz + audit + docs.

## Design decisions (with reasons)

1. **DTO-level param, single authz gate in the delegated create.** The
   issue words the contract on `CreateTriggerRequest`, and the
   delegation ("delegation, not duplication") means the seam's create
   flows through `TriggersHandler.create` anyway. One gate there covers
   the automation surface AND the user REST API under identical
   semantics; the seam only decides stamp-vs-passthrough. Fail-closed:
   a `targetWorkspaceId` create on a handler built without the gate is
   a 500, never an existence-only create.
2. **Authz primitive: the existing `workspace.Service`
   `ResolveWorkspace` + `CheckOwnership` pair** (creator, or org admin
   on org-attributed workspaces — D5/D6). This is literally the
   platform's "owner/admin rights on a workspace" definition; no new
   authz logic was invented. Injected via deferred setter
   (`SetWorkspaceOwnershipAuthorizer`), mirroring
   `SetWorkspaceExistencer`; app wiring asserts the concrete type and
   logs LOUD on drift (the rename-wiring discipline).
3. **404/403 split per the disclosure policy.** Nonexistent target →
   404 `target workspace not found` (typed NotFound from
   `ResolveWorkspace`); exists but no rights → 403. Wrapped org-store
   faults stay 500 (infrastructure ≠ denial).
4. **Seam: fold, don't strip, the target key.** `targetWorkspaceId`
   (and its snake alias + every case variant) folds onto the canonical
   spelling — the exact bypass class the #1426 review rounds closed for
   `workflowId` (Go's decoder binds DTO fields case-insensitively; a
   `TargetWorkspaceId` variant must hit the same both-set rejection,
   not escape it). The key then SURVIVES into the delegated body (the
   DTO binds it; the gate reads it). Caller `workspaceId` keys are
   still stripped as before. When the key is present, NO stamp is
   applied; when absent, the #1412 stamp is byte-identical (pinned).
5. **`workflowId` + `targetWorkspaceId` = 400** at BOTH layers (seam
   scoping error and DTO check) — workflow-targeting rules unchanged.
6. **Update path: the key is create-only.** `scopeTriggerUpdateBody`
   rejects any `targetWorkspaceId` key in a patch (any spelling,
   including empty/null) with an explicit 400 naming the remedy (delete
   + recreate). A silent ignore would tell an agent a retarget happened
   that did not (#1426 keeps workspace retargeting forced to this
   workspace). This is a deliberate loud behavior change for a key that
   was previously dead on update.
7. **Audit:** the seam stamps the creating pod's workspace into the gin
   context (`creatingWorkspaceCtxKey`) before delegation;
   `TriggersHandler.logCreate` records `targetWorkspaceId` (from the
   row, when set) + `creatingWorkspaceId` (when present) in the
   `trigger.create` metadata. Covers the omitted-param case too (one
   workspace recorded twice when they coincide is fine; the user REST
   path simply carries no creating-workspace key).
8. **agentd MCP docs:** the `trigger_create` description pins the
   default (forced to this workspace), the param (camelCase), the
   rights check (403/404), where the routine runs (IN the target, with
   its identity), and create-only. The old "you cannot schedule work
   into other workspaces" pin — a deliberate invariant pin from a
   prior round — was REPLACED, not loosened: the invariant this issue
   ships is the owner-scoped exception, and the pins now name it.

## Work completed (commit order)

### Commit 1 — `test(api): #1729 red-first — cross-workspace routine-trigger authz pins`

`api/internal/handlers/pod_automation_cross_workspace_1729_test.go`:
HTTP-level pins through the pod seam + REAL delegated handlers over
mock stores.

| Pin | Class |
|---|---|
| owner→own other workspace: 201, row targets ws-2 (not ws-1) | **red-first** (main: 201/ws-1) |
| owner→non-owned existing workspace: 403, nothing persisted | **red-first** (main: 201) |
| owner→nonexistent workspace: 404 | **red-first** (main: 201) |
| org-admin (non-creator) on org workspace: 201, targets it | **red-first** (main: ws-1) |
| workflowId + targetWorkspaceId: 400 | **red-first** (main: 201) |
| SA identity mismatch: 403 pre-delegation | green pin (pre-existing) |
| param omitted: 201, forced stamp ws-1, delegated body byte-identical (JSONEq pin on `scopeTriggerCreateBody` output) | green pin (backward compat) |

Verified RED on main before implementation (5 failures, each for the
named reason — not compile errors; the red commit compiles standalone).
The test file's router helper deferred the authorizer wiring line to
the impl commit (one line: `SetWorkspaceOwnershipAuthorizer`), stated
here and in the commit message.

### Commit 2 — `feat(api): #1729 — owner-scoped targetWorkspaceId for routine triggers`

- `pkg/types/workflows.go`: `CreateTriggerRequest.TargetWorkspaceID`
  (`targetWorkspaceId,omitempty`).
- `api/internal/handlers/triggers.go`: `workspaceOwnershipAuthorizer`
  interface (implemented by `*workspace.Service`), deferred setter,
  create-path gate (mutual exclusion → 404 → 403 → 500-fail-closed),
  target routing into `wsID`, `logCreate` audit enrichment +
  `creatingWorkspaceCtxKey`.
- `api/internal/handlers/pod_automation.go`: `triggerTargetKeys` struct
  (the parse outgrew tuple returns), target-key folding (alias + case
  variants + contradiction/null/non-string rejection), stamp-skip on
  target present, both-set rejection, update-path create-only
  rejection, creating-workspace context stamp in `TriggerCreate`.
- `api/internal/app/app.go`: ownership-authorizer wiring (user + org
  triggers handlers), loud on type drift.
- Tests added in the same commit: audit-both-ends pin
  (`TestPodAutomation_CrossWorkspaceRoutineCreate_AuditRecordsBothWorkspaces`),
  fail-closed pin (`..._UnwiredFailsClosed`), update create-only
  rejection pin (`..._Update_TargetKeyRejected`, all spellings);
  existing seam tests updated to the new `scopeTriggerCreateBody`
  signature (same assertions).

### Commit 3 — `feat(agentd): #1729 — trigger_create documents targetWorkspaceId`

- `cmd/workspace-agentd/mcp_server.go`: tool + `trigger` property
  descriptions.
- `cmd/workspace-agentd/mcp_server_test.go`: description pins updated
  (old "cannot" pin replaced by the five #1729 pins).
- `pkg/agent/opencode/loopback.go`: `TriggerCreate` comment.
- `pkg/agent/opencode/loopback_automation_test.go`: wire pin — the
  param rides verbatim beside the resolver stamp.

### Commit 4 — `test(api): #1729 — cross-workspace routine fires in the TARGET workspace`

`api/internal/handlers/pod_automation_crossws_fire_1729_test.go` — the
end-to-end acceptance: create through the automation surface
(`targetWorkspaceId: ws-2`, owner user-7 owns both ws-1 and ws-2), then
the REAL `api/internal/workflows.Scheduler` ticks the created row
(mock store hands the API-created row back at its due slot; activator +
agentd are recorders). Pins: row targets ws-2; exactly one routine
fire, delivered; activation keyed ws-2, never ws-1; the agent turn
dispatches with workspaceID ws-2.

### Commit 5 — `docs(worklog,changelog): #1729`

This worklog + the CHANGELOG `[Unreleased]` entry.

## What I did NOT do (scope discipline)

- No schema/migration: `triggers.workspace_id` already carries the
  target (the FK's existencer contract is unchanged; the target path's
  existence+ownership gate is stricter than the workspaceId path's
  existence-only stance, which stays as documented in #1440).
- No `pkg/board` edits (explicitly out of bounds).
- The platform-facing MCP server (`pkg/mcp/workflow_tools.go`, the
  user-token MCP surface) was NOT extended — the issue names the AGENTD
  surface; the REST DTO change is the shared contract.
- No update-path retargeting capability (issue is create-only).

## Verification (what I actually ran)

- Red run on main (before impl): the 5 new-behavior pins FAIL for the
  named reasons; 2 green pins pass.
- Post-impl: `go test ./api/internal/handlers/... -count=1` → ok
  (full package, ~81s); `go test ./api/internal/app/... ./pkg/types/...`
  → ok; `go test ./cmd/workspace-agentd/ -run 'TestToolsList|TestMCP|trigger'`
  → ok; `go test ./pkg/agent/opencode/ -run 'TestSeam'` and full package
  → ok; `go build ./api/... ./pkg/...` clean.
- Pre-commit gates pass on every commit (gofmt/goimports/lint-new-from-
  merge-base/worklog-prefix; gitleaks not installed locally — CI owns
  it).
- Environment note: the shared 15G PVC hit ENOSPC mid-red-run (91%);
  cleared the idle 3.6G default go-build cache (no active builds
  verified first) and moved to `GOCACHE=/tmp/opencode/gocache`,
  `GOTMPDIR=/tmp/opencode/gotmp`.

## Honest caveats

- The integration test's scheduler harness fakes the transports
  (activator/agentd as recorders) and the due-claim (the store hands
  back the created row with its slot stamped due — production does this
  in SQL `next_fire_at <= now`). The engine code between create and
  dispatch is real and synchronous (`Start`'s first tick under a
  100ms-bounded context).
- `gitleaks` is not installed in this workspace; the hook skips it and
  CI's run is the gate of record.
- The red commit's helper carried `_ = authorizer // wiring lands with
  the implementation commit` — flagged in both commit messages; the
  impl commit replaced it with the real setter call (tests otherwise
  untouched between the two commits except the 4-value signature fix).

---

## Review round 1 (verdict on b8a9e643: CHANGES_REQUESTED) — remediation

The reviewer's findings (all from the full verdict body; remedies
pre-approved scope):

1. **BLOCKER — org-scope gate dead (empirically proven by the
   reviewer).** `create()` passed `ownerID` to `CheckOwnership`; on the
   org REST route that is the ORG id, which can never match as a user →
   every org-scope `targetWorkspaceId` create was an unexplained 403,
   while app.go wired it and the PR/CHANGELOG claimed REST parity.
   **Fix (org-aware arm):** the gate authorizes the ACTING user
   (`c.GetString("userID")`) — the router has authenticated them and, on
   the org route, verified their admin rights on the OWNING org; the
   target question is THEIR creator/org-admin rights on the target.
   User route and pod seam have actor == ownerID (unchanged answers).
   **Red-proof run locally:** with the pre-r1 subject restored
   (TEMP-RED), `TestTriggerOrg_CrossWorkspaceCreate_AuthorizesActingUser`
   fails exactly as the reviewer proved (403 on the legitimate
   creator-branch create); with the fix it passes (creator branch, admin
   branch) and the no-rights case still 403s.
2. **REST update silent-ignore.** The seam 400'd the create-only key but
   the REST PATCH silently ignored it (200, nothing applied). **Fix:**
   `update()` peeks the raw body pre-bind (capture-replay, the seam's
   own pattern) and rejects any spelling of the key with the SAME
   message — extracted to one shared constant (`errTriggerTargetCreateOnly`)
   so the surfaces cannot drift.
3. **Nit — dead third return** of `scopeTriggerCreateBody`: dropped
   (signature back to 3 values); tests updated.
4. **Nit — gate-vs-cron-validation ordering:** made a conscious,
   documented choice — authz BEFORE payload validation (403/404 must not
   depend on payload validity; validation detail must not reach callers
   without target rights). Comment on the gate.
5. **Test depth (the reviewer's missing list, all added):**
   - org-scope cross-ws create (would have caught #1):
     `TestTriggerOrg_CrossWorkspaceCreate_AuthorizesActingUser`
     (creator branch, admin branch, no-rights 403; owner stays the org).
   - gate fault branches:
     `TestTriggerREST_CrossWorkspaceCreate_GateFaultBranches` —
     ResolveWorkspace internal error → 500 "failed to check target
     workspace"; CheckOwnership wrapped (non-APIError) fault → 500
     "failed to authorize target workspace"; nothing persisted.
   - DIRECT REST user route:
     `TestTriggerREST_CrossWorkspaceCreate_GateArms` (201/404/403) —
     the gate is no longer only exercised through the delegation.
   - D5 fidelity: the fake now models current-org-membership on the
     creator branch; `TestTriggerREST_CrossWorkspaceCreate_LapsedOrgMember`
     pins the lapsed-creator 403.
   - REST update rejection:
     `TestTriggerREST_UpdateRejectsCreateOnlyTargetKey` (all spellings
     400 + ordinary patches still apply).
   - **e2e rows** in the #1410 style:
     `local/issue-1729-cross-workspace-automation-e2e.sh` (API-only, no
     pods, no LLM) — R1 happy create targets the NAMED workspace (+list
     check), R2 disclosure 404, R3 authorization 403 + nothing-persisted,
     R4 omitted-param compatibility, R5 workflowId+target 400 (real
     workflow per the create contract), R6 the create-only 400 on the
     REST update + ordinary-patch regression, R7 the org-route rows
     (acting-user admin branch on an org-attributed workspace owned by
     another user + fail-closed 403; SKIPs loudly where org creation is
     platform-admin gated, mirroring the #1410 R8 skip). Registered in
     e2e-nightly.yml (own port 18088); structural source pins in
     `local/issue_1729_cross_workspace_e2e_script_test.go`
     (bash -n, rows-and-assertions, workflow registration,
     harness_start-first).

### r1 verification (what I ran)

- Red-proof of the org fix (TEMP-RED subject swap): org test FAILS with
  the pre-r1 gate (403 on the legitimate create), passes with the fix.
- `go test ./api/internal/handlers/ -count=1` → ok (~90s; an earlier
  FAIL batch was my own error — two full-suite runs racing on shared
  ports, not a code failure; single runs green before and after).
- `go test ./local/ -run TestIssue1729 -count=1` → ok (4 structural
  pins). `bash -n` clean.
- `golangci-lint run ./api/... ` → 0 issues (full sweep before push).
- The nightly e2e rows have NOT executed yet (they run on the nightly
  kind cluster); the script is syntax-checked and structurally pinned
  only. Not claimed as run.

### r1 commits

- `fix(api): #1729 r1 — org-scope target gate authorizes the acting user`
  (also the ordering ruling comment)
- `fix(api): #1729 r1 — REST update rejects the create-only targetWorkspaceId`
  (shared const + pre-bind peek; includes the dead-return removal from
  the same review)
- `test(api): #1729 r1 — org-scope, REST-route, gate-fault, D5 pins`
- `test(e2e): #1729 r1 — cross-workspace automation e2e rows`
- `docs(worklog,changelog): #1729 r1 — verdict record`
