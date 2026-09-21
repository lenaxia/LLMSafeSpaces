# Worklog: Parent-id contract audit — the opaque-500 class retired across create/update handlers (runs 35597973572 + 35617684178; Refs #1519)

**Date:** 2026-09-21
**Session:** Adjudicated upgrade of the workflow-create parity fix to a CLASS audit: every create/update handler accepting a parent resource id checked for the opaque-500-on-nonexistent-parent shape; buggy ones fixed to named-4xx, correct ones cited/pinned. Create/update surface only — #1440 fire-time loud semantics untouched.
**Status:** Complete — PR open, iterating review

---

## The audit table (FK columns in migrations → handler surfaces)

| Column (FK →) | Surface | Verdict |
|---|---|---|
| `workflows.target_workspace_id` → workspaces (000016, SET NULL) | workflow create AND update | **BUG → FIXED** (r1 caught the update surface — the audit's third instance): named 400 on the PATCHED value both views (run 35617684178's finding — the harness's REAL_WF create died in 9.3ms) |
| `triggers.workflow_id` → workflows (000020:21, SET NULL) | trigger create | already fixed (#1517) — cited |
| `triggers.workflow_id` | trigger update | **BUG → FIXED (#1519 BOTH halves, per r1's record correction)**: PATCHED target resolves owner-scoped → named 400 for nonexistent AND cross-owner (no existence oracle; matching create). STORED targets deliberately NOT re-validated — legacy cross-owner rows stay patchable for #1440 mitigation (enabled:false), pinned by TestTriggerUpdate_StoredTargetNotRevalidated |
| `triggers.workspace_id` → workspaces (000020:11, SET NULL) | trigger create + update | **BUG → FIXED**: existence check via the unscoped primitive; named 400, scoped to the PATCHED value |
| `workflow_runs.workspace_id` → workspaces (000016:261, plain FK) | run-create `workspaceId` OVERRIDE (REST + MCP workflow_run) | **BUG → FIXED (r3's fourth instance)**: named 400 on the user-supplied override; the workflow's STORED target stays FK-anchored and untouched |
| `user_secret_bindings.workspace_id`/`secret_id` (000001) | secrets bind | **CORRECT**: workspace-scoped route resolves the parent (404 by construction; sessions of green live bind usage) |
| `workspace_credential_bindings.*` (000001) | credential bind | **CORRECT + PINNED**: `TestUserProviderCredentials_Bind_OwnershipCheck` (404 ownership ⇒ existence) |
| `mcp_server_bindings.workspace_id`/`server_id` (000012) | MCP bind — USER arm | **CORRECT + PINNED**: `TestBind_RejectsForeignServer` + the workspace-ownership 404 (mcp_servers.go:636) |
| `mcp_server_bindings.workspace_id` → workspaces (000012:83) | MCP bind — ORG/ADMIN arms | **BUG → FIXED (r4's instance 7)**: the workspace check now covers EVERY scope (user arm keeps ownership; org/admin get existence-only 404 — `TestBind_AdminScope_GhostWorkspace_404`; the stub gained the ErrNoRows knob matching the real store's contract) |
| `mcp_server_auto_apply.server_id` → mcp_servers (000012:109) | MCP auto-apply create (admin + org routes) | **BUG → FIXED (r4's instance 5)**: serverId resolved via verifyServerOwnership → 404, matching Bind's convention (`TestAutoApplyCreate_GhostServer_404` — red at pre-fix) |
| `credential_auto_apply.credential_id` → provider_credentials (000001:1541) | admin credential auto-apply create | **BUG → FIXED (r4's instance 6; r5 made it REAL — the first attempt was a functional no-op)**: GetCredential returns (nil, nil) for not-found, so the check tests BOTH the error and the nil row, exactly as the admin CRUD arms and the org twin always did (`TestAdminProviderCredentials_AutoApply_GhostCredential_404` asserts the FK-bearing store is NEVER reached — red at the no-op head) |
| org tables' `org_id` | org routes | path-resolved org (404s; R8e's fails-closed row covers live) |

## Implementation

- `pkg/workflows/store.go`: `WorkspaceExistsByID` — the UNSCALED existence primitive (the FKs' semantics; ownership NOT judged — cross-owner targets remain the fire-time loud class). Same pool, one COUNT.
- `workflows.go` create: `targetWorkspaceId` pre-flight via `SetWorkspaceExistencer` (deferred injection, SetAudit pattern — nil skips, preserving legacy construction). Ordered after spec/schema validation.
- `triggers.go`: create gains the workspaceId existence check beside the #1517 workflowId check; update (#1519) resolves the PATCHED workflowId (owner-scoped GetWorkflow) + workspaceId (existencer) → named 400s (PATCHED-scope only — see r1's correction). **r1's scope correction: the first draft re-validated the post-patch MERGED view — that made legacy cross-owner rows unpatchable (blocking #1440's enabled:false mitigation); the checks now scope to PATCHED values only** (STORED targets stay FK-anchored and untouched), pinned by `TestTriggerUpdate_StoredTargetNotRevalidated`.
- `workflows.go` update: **the third instance (r1)** — a PATCHED targetWorkspaceId resolves via the existencer → named 400.
- Store integration: `TestWorkspaceExistsByID` (seeded row exists unscoped; random id does not) in the integration-tagged suite.
- E2E unhappy rows: R1c covers BOTH ghost-parent CREATES (targetWorkspaceId on workflows; workflowId on triggers) and **R1d covers BOTH ghost-parent PATCHES** (workflow target; trigger workflowId — #1519's live face) so the arbitration run patrols the retired class on every surface. The workflow-update check sits just before the store call (r2's ordering fix — 404 and validation errors win first, matching create). The soft-deleted-row face of the existence primitive is pinned in the integration suite (unscoped semantics = FK semantics).
- `app.go`: all four handlers wired to the wfStore existencer.
- Harness: the automation script seeds the dummy workspace ROW (`00000000-0000-4000-8000-000000000001`, psql like seed_session — no CR, no pod; runs still queue-and-no-op) before the REAL_WF create; R4d's target retargeted from a second never-existing literal to the seeded row.

### Assumptions stated and validated (Rule 7)

- The FK map is complete for user-facing parents after FOUR review passes: r1 the workflow-update surface, r3 the run-override (REQUEST DTO — found by trace, not grep), r4 the three auto-apply/bind org-arm surfaces (instances 5-7; the reviewer's independent FK sweep of 000001-000032 confirms no further instances). The completeness claim was false three times; each round's enforcement is recorded here as the audit's own history.
- Existence-not-ownership for the WORKSPACE axis (org-owned workflows target user workspaces; the FK is unscoped). For the WORKFLOW axis (trigger targets), owner-scoping matches #1517's create check and closes #1519 half (b): a PATCHED cross-owner workflowId answers the same named 400 as a nonexistent one (no oracle); #1440's loud-fire design survives for STORED targets (deletion SET NULL, legacy rows).
- The r1-recorded decision: update-path checks scope to PATCHED values — stored state is FK-anchored and deliberately unvalidated (mitigation path preserved).
- Nil-existencer skip semantics keep every legacy construction site working — verified by the untouched suites.

---

## Blockers

None.

## Tests Run

- New pins: workflow-create named-400/pass/nil-skip; trigger-create workspace named-400/pass; trigger-update workflow+workspace named-400s. All red-verified against the pre-fix shapes where applicable.
- `go test -count=1 -timeout 300s ./api/internal/handlers/` — **ok** (84.5s). `./local/` — **ok** (29.7s). `./api/internal/app/` + `./api/internal/server/` — ok. `go build` — ok. `bash -n` — clean.
- Two fixture completions (swap/repair update tests seed their workflow targets — the checks resolve them now).

## Next Steps

- APPROVED → merge → dispatch → R1–R9 arbitration with the parent-id class retired (the next run should find NO third instance).

## Files Modified

- `pkg/workflows/store.go` — WorkspaceExistsByID.
- `api/internal/handlers/workflows.go` — the target-workspace create check + existencer seam.
- `api/internal/handlers/triggers.go` — create workspaceId + update merged-view checks; hoisted merge.
- `api/internal/app/app.go` — existencer wiring ×4.
- `local/issue-1410-1412-automation-e2e.sh` — dummy workspace row seed + R4d retarget.
- `api/internal/handlers/{workflows,triggers}_test.go` — the audit pins + fixture completions.
- `api/internal/handlers/mcp_servers.go` — instances 5 + 7 (auto-apply server resolution; every-scope bind workspace check).
- `api/internal/handlers/admin_provider_credentials.go` — instance 6 (the (nil,nil)-aware credential resolution).
- `api/internal/handlers/{mcp_servers,admin_provider_credentials}_test.go` — the instance 5/6/7 pins.
- `worklogs/NNNN_2026-09-21_parent-id-contract-audit.md` — this worklog (audit table complete through instance 7).
