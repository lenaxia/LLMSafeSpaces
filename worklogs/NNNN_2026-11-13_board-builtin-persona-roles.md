# Worklog: Built-in stage-persona roles (implementer/reviewer pairs)

**Date:** 2026-11-13
**Session:** Seed six built-in agent roles for board stagePersonas — implementer+reviewer pairs for coding, data analysis, writing (tsk-primal-lavender-tasks)
**Status:** Complete

---

## Objective

Ship DEFAULT roles boards can reference from stagePersonas, so a board author doesn't need custom roles first: three implementer/reviewer domain pairs with stage-intent-clear names, seeded idempotently, selectable everywhere roles are listed, with NO stage enforcement (explicit owner non-goal).

---

## Work Completed

### The definitions — `pkg/board/builtin_roles.go` (NEW)

Six `BuiltinBoardRole` definitions beside the existing `DefaultReviewerRoleSlug`/`DefaultReviewerRoleSystemPrompt` (the role-only model's single-source pattern): slugs `implementer-coding`, `reviewer-coding`, `implementer-data-analysis`, `reviewer-data-analysis`, `implementer-writing`, `reviewer-writing`; names the owner's exact `<seat>: <domain>` convention (`implementer: coding` … `reviewer: writing`); charters composable — `builtinImplementerCore` (do the work; worktree off origin/main; conventional commits; no sleep-polling; report when blocked; move to in_review with evidence) + `builtinReviewerCore` (verify each criterion with execution evidence; verdict via board_record_verdict; findings with pointers; never merge) + one domain flavor constant each. Helpers: `BuiltinBoardRoles()` (defensive copy), `BuiltinBoardRoleBySlug`, `BuiltinBoardRoleSlugs`.

### The seed — `api/internal/app/board_role_directory.go` + `app.go`

`seedBuiltinBoardRoles`: one platform-scope `ListAgentRoles`, then a create per MISSING slug only (idempotent per role; restarts re-seed only gaps; owner-curated platform namesakes untouched; org namesakes never suppress — the reviewer seed's disciplines). Each create writes the charter as the role config's system prompt via `types.MarshalRoleConfig`. Wired in `app.go` beside `seedBuiltinReviewerRole` under the same best-effort 15s seed context (failure warns, next boot heals). The reviewer seed itself is UNTOUCHED.

### The pins

- `api/internal/app/board_builtin_roles_seed_test.go` (NEW): six exact names/slugs seed once on empty (charter parsed from the written jsonb); idempotent across a persisted re-seed AND a third pass; partial vocabulary seeds only gaps (curated platform namesake untouched, org namesake non-suppressing); `TestSeedBuiltinBoardRoles_VisibleToPersonaSelection` — the seeded vocabulary rides `AgentRolesForUser` (the exact listing stagePersonas selection validates against), every built-in platform-marked.
- `pkg/board/builtin_personas_unit_test.go` (NEW): exact names/slugs at the single source; charters carry cores + flavor; `buildBoardConfigView` renders built-in refs cleanly at both tiers (bare-slug wire form, project-override precedence, none_explicit) while the FRESH tier is unchanged (in_review still renders default_reviewer — the six are vocabulary, never new defaults); **the no-enforcement pin** — `{"in_progress":"reviewer-coding","in_review":"implementer-coding"}` decodes AND passes `validatePersonaRoleRefs` with no error.

### Docs + surfaces

`docs/api/mcp.md` Sprint Board section gains "Board configuration and stage personas" (config tiers, role-reference model, the six-role table, idempotency note, the no-enforcement ruling). MCP `board_get_config` description names the built-in pairs. CHANGELOG `[Unreleased]` entry added.

### Review round 1 (AI reviewer, CHANGES_REQUESTED → addressed)

Findings and fixes: (1) the new docs advertised `GET /api/v1/me/agent-roles` — a nonexistent route (the real registration is `meGroup.GET("/me/agent-roles")` under `/api/v1/auth` → `GET /api/v1/auth/me/agent-roles`); fixed in docs/api/mcp.md, the CHANGELOG entry, this worklog, and the PRE-EXISTING stale comment at `api/internal/handlers/agent_roles.go:385` (same mistake, one-word fix). (2) `BuiltinBoardRoleBySlug` + `BuiltinBoardRoleSlugs` shipped with zero non-test callers and a factually wrong "the seed's lookup arm" comment — removed both; the seed keys on its own present-map. (3) The reviewer's missing-test note: store-error propagation now pinned (`TestSeedBuiltinBoardRoles_StoreErrorsPropagate` — list outage and mid-vocabulary create failure both propagate, the persisted prefix healable). The Security-scan red on the first head was triaged as pre-existing stdlib noise (govulncheck GO-2026-6859, go1.26.10 toolchain vs go1.26.11 fix, failing identically on main).

---

## Key Decisions

- **Uniform `implementer:`/`reviewer:` prefixes for the writing pair** (not `writer:`/`editor:`): the owner's example convention is `"implementer: coding" / "reviewer: coding"`, and the uniform prefix keeps the stage intent self-describing across all domains (an `editor:` prefix reads as a domain word, not a stage signal). The writer/editor semantics ride the descriptions and charters.
- **Per-role idempotency via one list + missing-only creates** (not a slug-by-slug exists-check): fewer round trips, and partial vocabularies (owner curated some slugs) heal without touching curated rows.
- **No new config-view surface**: the built-ins appear wherever platform roles already list (picker via `GET /api/v1/auth/me/agent-roles`, set-path validation via the directory) — platform scope IS the existing built-in marker convention. board_get_config renders their refs cleanly when selected (pinned); a fresh tier keeps the default-reviewer posture (pinned unchanged).
- **No frontend change**: the picker enumerates `GET /api/v1/auth/me/agent-roles`; seeded platform roles appear automatically with the existing "(platform)" marker.

---

## Blockers

None.

---

## Tests Run

- `go test ./pkg/board/ -count=1` — ok
- `go test ./api/internal/app/... ./api/internal/handlers/... -count=1` — ok (37s/86s)
- `go test ./cmd/workspace-agentd/ -count=1` (description touch) — to run before push
- `make fmt-check` / vet / lint — to run before push (CI parity)

---

## Next Steps

- AI reviewer until APPROVED; orchestrator merges (owner PROCESS ruling).

---

## Files Modified

- `pkg/board/builtin_roles.go` (NEW)
- `api/internal/app/board_role_directory.go` (seedBuiltinBoardRoles)
- `api/internal/app/app.go` (startup wiring)
- `api/internal/app/board_builtin_roles_seed_test.go` (NEW)
- `pkg/board/builtin_personas_unit_test.go` (NEW)
- `cmd/workspace-agentd/mcp_server.go` (board_get_config description)
- `docs/api/mcp.md` (Board configuration and stage personas)
- `CHANGELOG.md` ([Unreleased])
- `worklogs/NNNN_2026-11-13_board-builtin-persona-roles.md` (NEW)
