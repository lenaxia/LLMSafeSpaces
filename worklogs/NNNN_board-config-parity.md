# Board config parity — UX relocation + persona editor + MCP config tools (PR #TBD)

Date: 2026-10-28 · Lane: board-config-parity (tsk-peaceful-bunker-speeds) · Branch: board-config-parity · Base: origin/main 326151fe2.

## Objective — the owner rulings (verbatim)

> "api/sdk/mcp/ux are all first class citizens."

> "The taskboard settings should not be in 'Settings' — that doesn't make sense for something as dynamic as the board. Configuration should be alongside the projects and on the board itself."

> "There are no configuration options for the board."

The live finding behind the third line: the ONLY board config UI hid under Settings → 'Task Board' (a tab labeled as one), unreachable from the board view itself — and it carried NO persona editor at all (BoardConfigTab rendered the three toggle keys + per-project override selects; stagePersonas rode only the round-trip wipe-protection comment). The board's stage-persona machinery (pkg/board/persona.go) had no first-class editing surface anywhere.

## Assumptions, stated and validated

1. **The verbatim round-trip wipe-protection semantics live in BoardConfigTab.tsx:194-201** (the project-PUT REPLACE rule: absent keys are DELETED, so a save of the toggles must carry a stored stagePersonas override back verbatim). Validated: read the deleted source at HEAD (`git show HEAD:frontend/src/components/settings/BoardConfigTab.tsx`); both wipe-protection rules (the user-tier stagePersonasSet rule at :84-95 and the project-tier verbatim carry at :194-201) are preserved verbatim-as-semantics in BoardConfigPanels.tsx's two save bodies and pinned by the round-1/round-2 tests.
2. **The persona-eligible stage vocabulary is `in_progress, in_review`** (a live claim can only exist there). Validated: `pkg/board/config.go personaEligibleStages` + the server rejection prose; mirrored as PERSONA_ELIGIBLE_STAGES in boardRules.ts.
3. **The frontend's one-click default-reviewer import must be byte-identical to the engine's persona.** Validated by construction: `TestDefaultReviewerPersona_FrontendMirror` (pkg/board) parses DEFAULT_REVIEWER_PERSONA out of boardRules.ts and pins it byte-identical to `board.DefaultReviewerPersona` — neither side can drift alone.
4. **`board_set_config` should follow design 0070's file-payload discipline** (configFile; tier/projectId are routing keys, inline-only). Validated: board_file_params.go's placement rule ("routing and control keys are read from the inline arguments — in the file they would be silently unread") + the DTO-parity convention (the set_config schema mirrors `board.BoardConfigOverride`, pinned by the new parity arm).
5. **The pod SA seam can render the source annotations only if the platform exposes the RAW user map** (the merged row cannot disclose inherited-vs-override: a user value equal to the code default is indistinguishable). Validated: the REST /me surface pins effective-only (openapi); the seam gets `GetUserBoardConfigSurfaces`/`GetBoardConfigView` service-side, never a new /me wire shape → SDK/openapi UNAFFECTED (criterion 5's first arm).
6. **The automation seam's project reads are workspace-bound (#1747)**. Validated: `requireManagerAction` reused inside `GetBoardConfigView`'s project arm; pinned by the 403 refusal-shape test.

## Change

### UX — config ON the board (the relocation)

- `frontend/src/components/board/BoardConfigPanels.tsx` (NEW): `BoardDefaultsDialog` (user tier, opened from the board header's "Board defaults" button) + `ProjectConfigTrigger`/`ProjectConfigDialog` (per-project gear on EVERY swimlane header — active lanes and the archived drawer, which reuses `renderSwimlane`): worker slots (the project PATCH — slots are fixed on the project row, not a config key), review column + auto-assign reviewer (override PUT with Inherit + provenance badge), and the shared `PersonaEditor` (below). Every config control reachable from the board in ≤1 interaction.
- `PersonaEditor` (both tiers): per persona-eligible stage name + multiline instructions; add/remove; UNLISTED stages render explicitly "no persona (the explicit map owns every stage it does not list)" while a map is explicit; read-only status rows (inherited persona / default-reviewer-composition / no persona) while inheriting; the `{}` opt-out affordance at the project tier ("Suppress inherited personas"); the legacy-reviewer-suppression warning (composition- and inherited-kind, with the one-click import — the inherited persona when one exists, else DefaultReviewerPersona); the effective merged view (GetProjectBoardConfig) with override/inherited provenance badges.
- Settings → 'Task Board' tab REMOVED (BoardConfigTab.tsx + test deleted; SettingsPage tab list + router entry gone); `/settings/board-config` → `/board?config=defaults` redirect (router.tsx), and BoardPage auto-opens the defaults dialog off `?config=defaults` then strips the param — muscle memory lands on the config, never a dead end.
- `frontend/src/api/board.ts`: the config PUTs no longer stamp `requestId` — the config body is a strict key-gated map and the server's decoder rejects unknown keys (the bug was live: a 400 on every save).

### pkg/board — the annotated view

- `config.go`: `UserBoardConfig` (overrides + effective; the raw-map surface the seam's annotations need) + `GetUserBoardConfigSurfaces`; the source vocabulary (`code_default | user_default | project_override`), `BoardPersonaView`, `BoardConfigView`, the pure `buildBoardConfigView` (per-key sources by merge precedence; per-stage persona provenance via `PersonaProvenanceForStage` with the persona rendered — including `DefaultReviewerPersona` under the legacy composition), and `GetBoardConfigView` (either tier; the project arm workspace-bound via `requireManagerAction`).
- `persona.go`: `PersonaProvenanceForStage` + the provenance vocabulary — the display twin of `personaForStage`'s spawn-time precedence (project map owns → user map owns → legacy composition → none; explicit-map-omits = `none_explicit`, visible never silent).

### Platform — the automation seam's config routes

- `api/internal/handlers/board_automation_config.go` (NEW): `GET /internal/v1/automation/board/config?projectId=` (board_get_config — read-open, the source-annotated view) and `PUT /internal/v1/automation/board/config` (board_set_config — body `{tier, projectId?, <config keys verbatim>}`; the REAL key-gate decodes the config remainder: unknown keys / non-eligible stages / malformed values refuse 400 before any write; tier refusal shapes: missing/bogus tier, projectId on tier=user, missing projectId on tier=project; response = the annotated view). Router registration + the `boardManager` interface widened with `GetBoardConfigView`.
- The parity matrix (board_parity_test.go): the four #1832 item-6 config rows' agent cells go LIVE (`board_get_config` ×2 reads, `board_set_config` ×2 writes — the "MCP tools for it are future work" note resolves here); the route table gains GET/PUT config; external MCP column stays absent BY DESIGN.

### agentd — the MCP config tools

- `mcp_server.go`: `board_get_config` (readOnly-annotated, optional projectId) + `board_set_config` (`{tier, configFile}`, projectId for the project tier) in the catalog with the #1747-repeal voice; `board_validate_file`'s op enum + description gain `set_config`; both route through the mcpAutomation dispatch.
- `board_manager_tools.go`: `board_set_config`'s file spec (`configFile` payload-only; `tier`/`projectId` inline-only — a file spelling is refused by the placement pre-check) + the wire cases (`BoardGetConfig` body-less GET with query stamps; `BoardSetConfig` PUT with the stamped verbatim body).
- `pkg/agent/opencode/loopback.go`: `BoardGetConfig`/`BoardSetConfig` (the automation seam's config routes).
- `board_payload_schema.go`: the `set_config` op — schema = `board.BoardConfigOverride`'s exact key set (every key optional; `{}` = the legal full-inheritance reset), persona `$defs` with the platform caps (`board.PersonaNameMax/InstructionsMax` — single-sourced), semantic findings: non-eligible stage keys (named, not "propertyNames"), the `defaultWorkerSlots` user-level-key warning, and the reviewer-suppression advisory (warning, never error — the opt-out is legitimate when deliberate). Advisory prose cross-references board_get_config.
- `runtimes/opencode/plugins/llmsafespaces-origin.js`: both tools in INJECTED_SESSION_TOOLS (the identity supply the audit attribution rides; pinned by board_injection_supply_test).

### Tests

- Frontend: BoardConfigPanels.test.tsx (25 cases: both tiers' persona round-trips, the {} opt-out, both suppression-warning kinds + imports, provenance/effective view, the slots PATCH seam, the untouched open→save never writes), BoardPage.test.tsx (defaults affordance, ?config=defaults auto-open, per-project Configure), SettingsPage.test.tsx (no Task Board tab), router.integration.test.tsx (the redirect lands on the open defaults dialog).
- pkg/board: config_view_test.go (per-key precedence, persona provenance arms, the {} project opt-out, composition-dead-under-user-map), persona_provenance_test.go (the precedence table + the display-twin/spawn-resolution agreement), persona_frontend_mirror_test.go (the byte-identical DEFAULT_REVIEWER_PERSONA pin).
- api: board_automation_config_test.go (read-open both tiers, the workspace-binding refusal shape, the REAL key-gate arms, the tier refusal shapes, the empty-{} reset, routing keys never riding the decoded config).
- agentd: mcp_board_config_test.go over the real mcpHandler surface (catalog shape; GET wire both tiers + attribution; SET wire verbatim + file lifecycle — deleted on success, KEPT on failure AND on local refusal; the inline/stray-file/placement refusals; validate_file's set_config findings happy/advisory/error); registry + injection pins updated.

## Environment notes (pre-existing, not regressed — verified by stash on pristine origin/main)

- The SHARED postgres (TEST_DATABASE_URL default) carries untracked in-flight migrations 000052/000053 from a sibling lane; every testharness consumer fails "no migration found for version 52" on PRISTINE origin/main too. Verification ran against a clean throwaway postgres (`initdb` + `TEST_DATABASE_URL=postgres://postgres@/postgres?host=/tmp/opencode/pg-parity&port=55432`): `api/internal/services/metering` green (9.9s).
- `TestRearmLoop_TerminalFailure_BoundedBackoff` flakes under full-suite load on this pod (passes `-count=3` in isolation, both on this branch and pristine main) — a load/timing artifact of the 398s single-package run, not a regression.

## SDK/openapi

UNAFFECTED — no /me wire shape changed (the annotated view exists only on the internal automation seam, which openapi does not describe). The codegen freshness gate has nothing to regenerate (criterion 5, first arm).

## Review round 1 (pr-review.yml REQUEST_CHANGES on b29190fc → addressed on this head)

Findings and their resolutions:

1. **"e2e never updated — still drives the removed settings URL" (the CI Frontend FAILURE)** — fixed. `board-config.spec.ts` now walks the relocated two-dialog flow end to end: `/settings/board-config` → the redirect auto-opens the defaults dialog over the board (the old `getByText("Board defaults")` assertion was a strict-mode collision between the trigger button and the dialog heading) → save-global (full-config PUT, no `requestId` — the stale expectation would have failed row 3 next) → close → the project's Configure gear on the swimlane header → provenance + effective view → the override save carrying only the set keys. Added the UNHAPPY tier: a refused save surfaces inline (`cfg-error-global` / `cfg-error-<project>`) in whichever surface sent it, dialog stays open.
2. **"Service.GetBoardConfigView has zero real-wiring coverage"** — added `TestBoardConfig_ViewRealWiring` + `TestBoardConfig_ViewGuards` (BoardSuite, real PG): user tier (raw stored map, per-key sources, the flipped composition rendering `default_reviewer` with the built-in persona payload), project tier (its own raw map, the overlay order per key, persona provenance `project_override`/`none_explicit` with payload), empty-actor authz both tiers, the #1747 workspace binding on the agent surface's project arm, the scoped 404, short-id/UUID resolution parity.
3. **"GetUserBoardConfigSurfaces + UserBoardConfig are dead code"** — removed both (the annotated `BoardConfigView` — which `GetBoardConfigView` builds internally — carries strictly more information; no caller anywhere, no test referenced them).
4. **"the seam config PUT lacks the 64 KiB cap its REST twin enforces"** — added `boardConfigMaxBodyBytes` (64 KiB): an early ContentLength refusal plus a cap+1 `MaxBytesReader` under the resolve's capture (a legitimate exactly-cap body parses; an over-cap body deterministically surfaces as cap+1 captured bytes → the same 400 refusal class), pinned for sized AND chunked (no ContentLength) bodies with the "no write fired" assertion.
5. **go-jose CVE-2026-116587 (the Security-scan red)** — bumped v4.1.4 → v4.1.6 (the prior claimant's commit, pushed in this round). The arm64 build red was a transient `go mod download` network error (retried clean on this head).

Local verification on this head: `go build ./...`; `go test ./cmd/workspace-agentd/... ./pkg/board/...` (372s, all ok); `go test -tags=integration -run TestBoardSuite ./pkg/board/` (race, real PG); handlers config suites; vitest 2535/2535; tsc clean; playwright board-config spec 2/2 (happy + unhappy); `golangci-lint run ./api/... ./pkg/board/...` → 0 issues.
