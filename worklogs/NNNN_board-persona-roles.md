# Worklog: column personas reference agent roles, not parallel persona definitions (PR #TBD)

Date: 2026-10-29 · Lane: board-persona-roles (tsk-chalky-guitar-supports) · Branch: board-persona-roles · Base: origin/main 6ce1859f6 (post-0.44.7).

## Objective — the owner ruling (verbatim)

> "The column personas are wrong. I was thinking we can use the agent roles the platform already has. Rather than redefining them that way they can be reused."

The live defect behind the ruling: `stagePersonas` mapped stage → `PersonaConfig {name, instructions}` — a PARALLEL definition surface duplicating what the platform's agent-roles system (api/internal/handlers/agent_roles.go: full CRUD, org-level + workspace-scoped, model/tools/system-prompt per role) already encodes. Hand-authored persona prose drifts from the role definitions it shadows, and a persona could not adopt a role's model or tool surface at all.

## Assumptions, stated and validated

1. **The board Service can consume the role vocabulary through a narrow seam without importing the api module.** Validated: `board.RoleDirectory` (pkg/board/config.go) is a one-method interface (`AgentRolesForUser`); the production concrete `boardRoleDirectory` (api/internal/app/board_role_directory.go) reads `ListAgentRoles(platform)` + `ListAgentRoles(org, user's org)` via `PgOrgStore` — the same composition the member-readable endpoint renders. pkg/board stays import-cycle-free.
2. **The spawn dispatch already has the agent-role seam.** Validated: the workflow engine's routine fires stamp `spec["agent"]` (the opencode agentID); the board spawner's `NodeExecRequest{NodeType: "agent"}` only needed the same key set from the persona's role reference (board_spawner.go boardSpawnRequest).
3. **Backward compat is decode-level, not write-level.** Validated: `decodePersonaMap` admits the legacy `{name, instructions}` object verbatim (stored AND wire), the new `{roleId?, name?, instructions?}` object, and the compact bare-string form (`"in_review": "reviewer"`); a persona must carry roleId or name (the empty-both shape rejects). Round-trip through storage rows preserves the reference (unit-pinned).
4. **Migration is LAZY and read-path only.** Validated: `migratePersonaRoleRefs` resolves stored name-only personas against the owner's vocabulary in the DECODED shape (name→slug then name→name match); the stored row is byte-identical until a client writes the resolved map back (integration-pinned: the raw jsonb still reads `"reviewer"`, no `roleId`).
5. **Set-path role validation must not brick role-blind deployments' legacy personas.** Validated: only personas CARRYING an explicit `roleId` validate; a name-only persona writes without any directory round-trip. A role-bearing set with no directory installed (or an unresolvable reference) refuses — never store a reference this deployment can never honor.
6. **The default reviewer keeps working with and without a reviewer role.** Validated: `applyPersonaRole` (persona.go) resolves the composition's prose persona against the vocabulary at spawn/config-read time — a reviewer role (slug or name match) gets the reference stamped with the charter prose layered on as the override; no reviewer role → the legacy shape stands byte-identically (both arms integration-pinned).
7. **The frontend needs a member-readable roles endpoint.** Validated: the admin CRUD routes are admin-gated; a selection vocabulary is a member read (the org prompt read's posture). New `GET /api/v1/me/agent-roles` (router.go meGroup, ListForMe in agent_roles.go) renders platform + acting-user org roles. openapi.yaml gains the path; router parity holds (`make -C sdks sdk-check`).

## Change

### pkg/board — the role-first vocabulary

- `config.go`: `PersonaConfig.RoleID` (json `roleId`, ≤128 chars — a UUID or slug token, never a prompt; `PersonaRoleIDMax` joins the caps family). `AgentRoleRef` (id/slug/name/scope — the wire shape the directory renders), the `RoleDirectory` seam, `ResolveAgentRoleRef` (explicit RoleID matches id or slug EXACTLY — an unresolvable explicit reference is never silently re-pointed; name-only matches slug then name), and `MigratePersonaRoles` (the lazy name→role migration helper). `decodePersonaMap` admits all three shapes with the strict-vocabulary discipline intact (DisallowUnknownFields on the object form). Service seams: `rolesForUser` (directory read, warn-and-degrade on failure), `migratePersonaRoleRefs` (both tiers' decoded overrides), `validatePersonaRoleRefs` (the SET belt: every explicit reference must resolve — id or slug — else `GuardInput` naming the reference and the available slugs), `decodeProjectOverrides` (the shared migrated project read; effectiveConfigForProject and both config reads route through it). `SetUserBoardConfig`/`SetProjectBoardConfig` validate before the REPLACE write.
- `persona.go`: `PersonaConfig.Label()` — the display/stamp identity (name when set, else the role reference) for the claim_persona provenance stamp and event payloads (a role-first persona may carry no name). `stagePersona` canonicalizes the resolved persona through `applyPersonaRole`: slug references stamp the canonical role id, and the DEFAULT REVIEWER composition points at the reviewer agent role when the vocabulary carries one (the ruling's fix — the prose survives as the override layer; the verdict-channel mechanics are exactly the edge-case layer a generic reviewer role cannot know).
- `service.go`: `WithRoleDirectory` Option + the `roles` field (nil = role-blind: legacy personas run byte-identically, role-bearing SETs refuse).
- `stage.go`/`reconciler.go`: the persona stamp sites render `Label()`.

### app — wiring + the dispatch

- `board_role_directory.go` (NEW): the production concrete over the `PgOrgStore` subset (structurally satisfied; fakes pin the seam without a DB).
- `app.go`: `board.WithRoleDirectory(newBoardRoleDirectory(pgOrgStore))` on the engine's Option list (the service AND the reconciler share spawnOpts).
- `board_spawner.go`: `boardSpawnRequest` stamps `spec["agent"] = persona.RoleID` when the persona carries a role (the opencode agentID seam — the role's model/tools/system-prompt ride the dispatch; the charter prompt stays the board's layered context and renders `Label()`). A role-less persona carries no agent key — the dispatch shape is byte-identical to pre-role.

### api — the member-readable vocabulary

- `agent_roles.go`: `ListForMe` (platform + acting user's org roles; 401 unauthenticated). `GetUserOrgID` joins the roleStore interface.
- `router.go`: `GET /me/agent-roles` on the authenticated meGroup (selection read posture; the CRUD routes stay admin-gated).

### agentd — the MCP config tools' role-first vocabulary

- `mcp_server.go`: board_get_config/board_set_config descriptions render the role-first contract (roleId with instructions as the optional override; set validates references against the available set).
- `board_payload_schema.go`: the set_config schema admits the bare-string role reference (`personaRoleRef` $def, cap-sourced from `board.PersonaRoleIDMax`) and the `{roleId?, name?, instructions?}` object (name no longer required); the semantic layer names the role-or-name rule (`persona-role-or-name`) for an object carrying neither.

### Frontend — the persona editor is a role dropdown + optional override

- `BoardConfigPanels.tsx`: per-stage `Select` of the available roles (from `agentRolesApi.listForMe`, the shared `["agent-roles","me"]` query; load failure/empty vocabulary degrades to the legacy name lane with an explanatory note); the name input renders ONLY for role-less stored personas (backward compat — no free-form composition for the base behavior); the instructions textarea relabels to the optional override; the role note renders "the role is the single source of truth"; the one-click reviewer import prefers the reviewer ROLE (reference + mirrored prose override) and falls back to the legacy prose persona when no reviewer role exists. Unresolvable stored references ride as their own dropdown option (visible, preserved).
- `api/board.ts`: `BoardPersonaConfig` gains optional `roleId` (name becomes optional).
- `api/agentRoles.ts`: `listForMe`.

### SDK/openapi

- `sdks/openapi.yaml`: `GET /me/agent-roles` (listMyAgentRoles) + `BoardPersonaConfig` role-first (roleId ≤128, name optional, `required: [name]` dropped). `make -C sdks sdk-check` green (spec valid + router parity).

## Tests (all green; the pinned seams)

- `pkg/board/persona_roles_unit_test.go` (NEW): shape admission (bare string, {roleId}, {roleId, instructions}, {roleId, name}, legacy {name, instructions}; empty-both/oversize/unknown-field rejects), decode round-trip through storage rows, `ResolveAgentRoleRef` precedence table.
- `pkg/board/persona_roles_integration_test.go` (NEW, PG): set-config validation (unknown reference rejects naming it; id and slug resolve; legacy name-only writes unvalidated; role-blind deployment refuses a role-bearing set), the lazy read migration (both config reads + the effective engine path resolve the stored name to the role reference; unresolvable names keep the legacy shape verbatim; the stored row is byte-identical), the default reviewer POINTS AT the reviewer role (slug match; prose as the override layer), and the fallback (no reviewer role → the legacy composition stands, RoleID empty).
- `api/internal/app/board_spawner_runid_test.go`: `TestBoardSpawnRequest_RoleRidesTheDispatch` — the reference rides `spec["agent"]`, a name-less role persona's charter renders the Label fallback, the legacy persona carries no agent key with instructions intact; persona-less dispatch carries no agent key.
- `api/internal/handlers/agent_roles_test.go`: `TestListForMe` (platform+org, platform-only solo, 401).
- `cmd/workspace-agentd/board_validate_file_test.go`: set_config role-first happy shapes + the violation classes (persona-role-or-name finding, empty bare reference, unknown field, ineligible stage).
- Frontend `BoardConfigPanels.test.tsx`: re-pinned to the dropdown lane (role select writes roleId, legacy lane only for role-less personas, the reviewer-role import, save bodies roleId-first).
- Suites: pkg/board (unit + FULL integration, PG), api handlers/app/server, cmd/workspace-agentd (365s), frontend typecheck + vitest (471), `make -C sdks sdk-check`, golangci-lint 0 issues.

## Environment notes

- The pod's shared postgres carries sibling-lane in-flight migrations; the integration suite ran against a THROWAWAY postgres 15 cluster on :5433 (pgdist binaries + golang-migrate over api/migrations), `TEST_DATABASE_URL=postgres://postgres:testpass@127.0.0.1:5433/llmsafespaces_test`.
- frontend/package-lock.json churn (an npm-version `dev: true` strip) was reverted — no dependency changed.

## Review rounds

### Round 1 (pr-review.yml REQUEST_CHANGES on 4723cc2cf — addressed on this head)

1. **Stale role references rode the dispatch (CONFIRMED, fixed).** `applyPersonaRole` returned an explicit unresolvable reference UNCHANGED — still carrying `RoleID` — and `boardSpawnRequest` stamped `spec["agent"]` from it: after an admin deletes a referenced role (the delete path has no stagePersonas cleanup), every persona spawn dispatched a dead agentID. Fix: the explicit-unresolvable arm now DEGRADES to the role-less shape — the reference is dropped (never dispatched), preserved as the display label (`staleRoleRefLabel`, rune-truncated to `PersonaNameMax`), instructions ride as the override; a warn line names the drift. The stored row keeps the reference (the config view renders the drift for the owner to fix); only the spawn degrades. Pinned: `TestPersonaRoles_StaleRefDegradesToRoleless` (spawn envelope carries no RoleID, label + override intact, stored row verbatim) + `TestStaleRoleRefLabel`.
2. **Directory outages were indistinguishable from bad references (CONFIRMED, fixed).** `rolesForUser` collapsed the directory read error into nil, so `validatePersonaRoleRefs` rendered a server-side outage as the same 400-class "could not be read" GuardInput as an unresolvable reference. Fix: `rolesForUserErr` (the error-carrying core; `rolesForUser` stays the warn-and-degrade read wrapper) and three DISTINCT classes: role-blind deployment → GuardInput naming the deployment remedy; directory read failure → plain error (500-class via renderBoardError's default arm) with "retry" in the message; unresolvable reference → GuardInput naming the reference and the available slugs. Pinned: `TestPersonaRoles_SetDirectoryOutageIs500Class` (outage is NOT a GuardError; a name-only set writes clean straight through the outage) + the role-blind arm re-pinned to the deployment-posture message.
3. **agentd↔platform validator divergence on degenerate names (CONFIRMED, fixed).** `{"roleId":"x","name":""}`: platform accepted, agentd schema refused (`name` had `minLength: 1`); whitespace-only names: platform accepted, agentd's semantic check refused. Fix — ONE contract, trim-aware on both surfaces: the platform decoder's role-or-name rule now `TrimSpace`s both slots (whitespace-only roleId or name rejects; empty name alongside a roleId stays legal), and the agentd persona schema drops `name`'s `minLength` (the trim-aware role-or-name rule is the semantic check's, identically). Pinned: platform unit arms (`name:"   "`, `roleId:"   "`, `roleId+name:""`) + agentd validate-file arms (`roleId + empty name` valid; whitespace name → `persona-role-or-name`).
4. **Missing test arms (added).** `ListForMe` 500 paths (platform listing, user-org resolution, org listing — never a partial vocabulary rendered as success).
5. **Documented (no code change):** `effectiveConfigForProject` now propagates user-tier DECODE errors (pre-role they were silently swallowed — a drifted user-defaults row ran code defaults silently). Fail-closed matches the storage-drift discipline everywhere else in the config surface; the behavior change is now stated here and in the PR description. The composed role-bearing chain (agentd→engine→opencode in one process) stays pinned at both ends (service-integration claim envelope; dispatch unit `spec["agent"]`) — a full-stack live spawn harness is future work outside this lane's scope.

