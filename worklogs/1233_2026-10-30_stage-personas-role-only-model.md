# Worklog: stagePersonas is stage → ROLE reference, full-stack, ZERO migration (tsk-spurious-auction-triumphs)

Date: 2026-10-30 · Lane: board-persona-role-only-model (tsk-spurious-auction-triumphs) · Branch: board-persona-role-only-model · Base: origin/persona-role-only 65cc1a312 (the editor task's round-1 head, itself on origin/main 979a071d2 — this task's re-pins assume the editor task's UI, per the charter's land-order note).

## Objective — the owner rulings (verbatim)

> "Board stages should ONLY ever have roles configured, there is no custom definition in the board itself. ROLES are the way to define behavior for board stages."

> "We do not need migration, we have never created any personas ever."

Supersedes #1980's compat posture at the MODEL layer: the board stops being a persona-definition surface entirely. It REFERENCES platform agent roles; behavior (model, tools, system prompt — including the reviewer's verdict-channel charter) lives in the agent-roles system, nowhere else. NO migration machinery of any kind: no compat decode of legacy shapes, no lazy name→role resolution, no backfill. Old shapes simply stop existing.

## Change

### pkg/board — the data model collapses to role references

- `config.go`: `PersonaConfig` is `{RoleID string}` — nothing else. `MarshalJSON` emits the BARE STRING (canonical wire: `"in_review": "reviewer"`); `UnmarshalJSON` admits the bare string and the `{roleId}` object (decodable, carries nothing else) and rejects EVERYTHING else loud (`a persona is a role reference — the bare id/slug string (canonical) or the {roleId} object`). `decodePersonaMap` enforces non-empty + `PersonaRoleIDMax` (128) per reference; unknown fields inside the object form reject (DisallowUnknownFields). `PersonaNameMax`/`PersonaInstructionsMax` are DEAD (the retired 4096 cap survives only as a test-local tombstone asserting the seeded system prompt stays a prompt). #1980's decode-time `migratePersonaRoleRefs` and the `MigratePersonaRoles` helper are DELETED — no lazy resolution anywhere. `validatePersonaRoleRefs` (the SET belt: every explicit reference must resolve id-or-slug against the installed directory, else GuardInput naming the reference and available slugs) is the validation the ruling keeps. The no-directory hard guard stays: a role-bearing set on a role-blind deployment refuses.
- `persona.go`: `DefaultReviewerRoleSlug` ("reviewer") + `DefaultReviewerRoleSystemPrompt` — the default reviewer's verdict-channel prose VERBATIM (board_record_verdict, the criteria walk, the Done guard), now the SEEDED role's system prompt's single source. `DefaultReviewerPersona` is the bare slug reference. `applyPersonaRole` resolves at spawn time: slug → canonical id on the dispatch; unresolvable (deleted role, blind directory, read failure) → DEGRADE to persona-less (transition lands byte-identical to pre-persona, never a role-less spawn, never a failed spawn; the config view still renders the stored ref so the owner sees the drift; the re-arm heals).

### app — the seeded built-in reviewer role + the dispatch

- `board_role_directory.go`: `seedBuiltinReviewerRole` — idempotent platform-scope seed of the `reviewer` role carrying `DefaultReviewerRoleSystemPrompt` as its system prompt, when the vocabulary lacks the slug. ROLE SEEDING, not a persona migration: no persona data is read, transformed, or written. `app.go` calls it best-effort at startup (a transient DB outage warn-logs and retries next boot; an unseeded vocabulary leaves the default reviewer degrading to persona-less until then). The default reviewer therefore resolves to a role DETERMINISTICALLY; an org/user overriding it points `in_review` at their own role.
- `board_spawner.go`: `spec["agent"] = <role>` is the ONLY persona dispatch shape. The charter renders the role reference by name (`You are the '<role>' agent role for task…`); the verdict-channel contract rides the ROLE's system prompt, never the charter; no name/instructions reach the dispatch. Persona-less (or degraded) claims dispatch with NO agent key — byte-identical to pre-persona.

### agentd + api — the set path and its docs

- `board_payload_schema.go` (board_validate_file op set_config): the stagePersonas value's oneOf is bare-string | object-with-`required:["roleId"]`-and-unknown-fields-rejected; the semantic rule renames `persona-role-or-name` → `persona-role-ref`.
- `mcp_server.go`: `board_get_config`/`board_set_config` descriptions state ROLE REFERENCES ONLY (canonical bare string, {roleId} decodable, platform-validated, the seeded reviewer role).
- REST config PUTs ride the same `decodePersonaMap`/`validatePersonaRoleRefs` — role refs only, validated.

### Frontend — the dead fields drop

- `api/board.ts`: `BoardPersonaConfig = string | { roleId: string }`; name/instructions gone from the wire types. `boardRules.ts`: `DEFAULT_REVIEWER_PERSONA = "reviewer"` (the slug).
- `BoardConfigPanels.tsx`: `PersonaRef = string` end-to-end; `reviewerImport` returns the role id when the client vocabulary carries the slug, else the SEEDED slug itself (the platform validates against the real vocabulary — never a role-less fallback row). Editor round-1's F1–F3 persistable-map guards carry over unchanged (the unpicked draft row is now the only role-less form left anywhere).

### openapi.yaml

- `BoardPersonaConfig` re-spec'd: oneOf bare string (minLength 1, maxLength 128) | `{roleId}` object; description states the ruling, the seeded reviewer role, and that name/instructions do not exist in this layer.

## Tests (re-pinned, decode/migration suites deleted with their paths)

- pkg/board: `persona_roles_unit_test.go` / `persona_roles_integration_test.go` re-pinned to role-ref admission — the legacy `{name, instructions}` shape is a must-REJECT; the migration pins are gone with the code they pinned. Directory-outage and unresolvable-ref degradation pins (persona-less, warn-logged, re-arm heals) in both files. `persona_config_unit_test.go`, `persona_integration_test.go`, `persona_provenance_test.go`, `persona_frontend_mirror_test.go`, `config_{integration,view}_test.go`, `criteria_met_integration_test.go`, `bounce_1959_*`, `harness_test.go` (the `WithRoleDirectory(personaRoleDirectory())` fixtures), `persona_spawn_race_integration_test.go` follow.
- api: `board_role_seed_test.go` (NEW — seeds-when-absent with the charter prose as system prompt; idempotent; scope-respecting), `board_review_charter_test.go` (the charter renders the ROLE line, no verdict prose), `board_spawner_runid_test.go`, `handlers/board_config_test.go` (`"in_review":"reviewer"` bare-string PUT), `board_phrase_bounce_integration_test.go` (role-directory doubles), `board_manager_e2e_integration_test.go` (the e2e vocabulary mirrors the seeded role).
- agentd: `board_validate_file_test.go`, `mcp_board_config_test.go` — role-only admission (name/instructions findings), the renamed rule.
- frontend: `BoardConfigPanels.test.tsx` re-pinned for the model collapse — F1/F3 repurposed to the unpicked-draft-row premise (the ONLY role-less form left; a stored one cannot exist on the wire), the user-tier F2 pin now lands the SEEDED slug (a role-bearing ref the platform validates — never a silent suppression), the project-tier F2 pin imports an inherited UNRESOLVABLE ref verbatim (rides, never stripped), and the stale-ref/ghost-ref saves ride as bare strings. 33/33 file, 2576/2576 full suite, tsc clean, eslint 0 errors.

## Gates

`go build ./...`; `go vet ./api/... ./cmd/... ./pkg/...`; golangci-lint 0 issues; pkg/board integration `ok 39.058s`; cmd/workspace-agentd `ok 373.792s`; api/internal/app + handlers `ok`; `make -C sdks validate` (openapi) clean; frontend vitest 2576/2576 + tsc + eslint 0.

## Coordination

Stacks on the editor task's branch (PR #1988, tsk-pallid-pivot-wiggles) — this task's re-pins assume its UI; land order either way (this PR retargets to main when #1988 merges). The editor's round-1 F1–F3 guards are preserved verbatim in the merged shape.

## Operational notes / hazards

- Attempt archaeology (this is attempt 3): attempt 1's worktree was lost to a reset while golangci-lint ran; attempt 2 died mid-diagnosis of a then-failing agentd suite — its cause was the e2e fixture's missing role directory (fixed here by `e2eRoleDirectory`), and its session had gone zombie post-restart and was aborting- mid-worktree-writes before this attempt fenced it (aborted; its last edits — the openapi.yaml re-spec and the e2e fixture — were verified and harvested). Anyone resuming: the worktree at /workspace/persona-role-only is the only live lane; /tmp/opencode/* worktrees are other tasks'.

## Review round 2 (PR #1990, CHANGES_REQUESTED 2026-10-30T03:15Z — resumed after the pre-0.44.9 anonymous-lease lapse)

Findings and the fix. F1 (HIGH, blocking) — the role's system prompt never reached the spawned worker: opencode NEVER errors on an unknown `agentID` (NODE-EXECUTE-CONTRACT §A8 probe: silent `build` fallback), `spec["agent"] = <roleUUID>` named an agent nothing ever materialized into the pod's opencode registry, and the charter prose deletion removed the one channel that HAD delivered the reviewer's verdict contract. Fixed with BOTH halves of the seam:

- **Materialization** (the role becomes a real named agent): `PodBootstrapHandler.SetAgentRoleReader` (wired in app.go, `pgOrgStore`) serves the workspace's addressable vocabulary in the bootstrap response (platform scope + the workspace's org — the board role directory's composition; org-list failure is non-fatal, warn-logged); the agentd bootstrap subcommand stages it verbatim to `/sandbox-runtime/agent-roles.json` (`--agent-roles-out`, `LLMSAFESPACES_AGENT_ROLES_PATH` for the US-4b sidecar relocations — the allowed-dirs discipline); the opencode ConfigWriter (`WithAgentRolesPath`, read once at init like the admin prompt) materializes each entry as `agent.<roleID>` — prompt/model/mode/tools from the role, role fields override, sibling fields of a pre-existing entry preserved, blank ids skipped, dedup by id (platform first). Freshness = next pod boot (the admin-prompt contract).
- **A8 pre-validation + spawner degrade** (an unmaterialized role can never silently run `build`): agentd's agent-node executor resolves opencode's live registry (`GET /agent`, cached per password — the registry is static within a pod's life) BEFORE the session create; an unknown name rejects with `agent_not_found` in the 200 envelope (a failed/empty registry probe fails loud as `agent_list_failed`); the board spawner reads the code and degrades ONCE to the persona-less dispatch — `boardSpawnRequestPersonalessRetry` (Persona=nil, fresh `RunID+"-np"`: #1957's mux drops re-used charter keys), one-shot by construction (no agent key left to reject), warn-logged with the stored ref visible until the next bootstrap materializes the role and the re-arm heals. Other error codes keep the fail-by-lease contract (never silently downgrade on transient outages). Persona-less claims never degrade (nothing to degrade).

F2 — the seed comment claimed "any scope" idempotence while the code (correctly) lists platform scope only and the test pins the org-namesake non-suppression: comment corrected to state the rule and the reason (org roles are invisible to boards outside that org). F3 — stale comments (`board_spawner.go`'s DefaultReviewerPersona instructions pointer, `config.go`'s "legacy prose personas decode" line) rewritten for the role-only world. Missing tests: (1) the spawn-level A8 pins — `board_spawner_a8_degrade_test.go` (degrade-once, persona-less spec + `-np` RunID + no agent key, other codes fail, persona-less never degrades), `workflow_agent_a8_test.go` (unknown rejects / known dispatches on it / empty skips / list-failure loud / cache-per-password), `board_role_materialization_test.go` (the full chain: bootstrap JSON → staged file → named agents in the rendered config; ids only — NO slug aliases), `configwriter_agent_roles_test.go`, `pod_bootstrap_agent_roles_test.go` (composition + non-fatal org failure + unwired omission), `secrets_wiring_test.go`'s reader-wired guard; (2) the user-tier twin of the stored-legacy-shape drift pin (`board_config_defaults`); (3) `board_validate_file` trim-rejects a whitespace-only BARE reference (semantic check — schema minLength cannot see whitespace; admission identical to the platform decoder on both shapes).

Also this round: the branch REBASED onto current main (the base moved under #1986/#1987/#1988; the PR retargets to main — see Coordination); a CHANGELOG duplicate heading from the merge-of-main removed; PR retargeted main ← persona-role-only (the stacked base is merged; CI now triggers on the PR).

## Gates (round 2)

`go build ./...`; `make fmt-check` clean; `go vet ./...` 0; golangci-lint 0 (after dropping the a8 stub's unused field); unit: pkg/board, pkg/agent/opencode, pkg/types, pkg/agentd, api handlers+app all `ok`; integration: pkg/board `-tags integration` `ok 37.951s`; api handlers+app `-tags integration` `ok 83.039s`/`ok 1.615s`; cmd/workspace-agentd `-tags integration` `ok 394.793s` (+faultmatrix `ok 39.715s`, +sessionstate `ok 21.305s`). Frontend untouched this round (the reviewer verified 2576/2576 + tsc on the same tree state).
