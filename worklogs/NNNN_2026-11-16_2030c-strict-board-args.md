# Worklog: Strict board-tool argument checking (#2030c) — hidden identity stamps + loud extra-field rejection

**Date:** 2026-11-16
**Session:** story tsk-visioned-estate-stencils (#2030c, gated on #2030a) — issue #2030 live-instance comment, candidate M2
**Status:** Complete — pending the pending_input rollout-default decision (default OFF shipped; flip is config-only)

---

## Objective

Schema-side hardening for the #2030 injection-stamp bleed family: board-facing tool schemas that reject unknown argument fields LOUDLY (instead of `additionalProperties` accepting them silently), with the identity keys (`lsp_injected_session`, `sessionId`, `session_id`) declared-and-hidden so their rejection is a precise teaching error, not a generic unknown-field one. Rollout-guarded behind a config flag whose default is decided by the post-#2030a-hygiene residual emission rate — instrumented first, per the charter.

## Assumptions (stated, then validated)

| # | Assumption | Validation |
|---|---|---|
| A1 | The 980/17974 "extra-field" baseline in the issue counts model emissions. | **Corrected by measurement.** The opencode DB persists the ORIGIN PLUGIN's stamp: every tool in `INJECTED_SESSION_TOOLS` shows 100% carriage (current-pod window: board_update_story 49/49, board_validate_file 20/20, board_list 13/13). The plugin's `tool.execute.before` hook overwrites `output.args.lsp_injected_session = input.sessionID` unconditionally — at agentd's boundary the stamp on a stamp-set tool is never attributable to the model. True emissions are measurable only on surfaces the plugin does NOT stamp. |
| A2 | Model emissions are measurable on the legacy `*_task` alias names (the plugin set lists canonical names only). | Confirmed from `board_story_alias.go`'s own window note + the DB: **272 all-time events** of `lsp_injected_session` as an extra field on non-stamp-set tools, almost all on `board_update_task`/`board_create_task` (+ a few pause/resume_task), across ~50 sessions — the board-worker population, not just the gauntlet agent. |
| A3 | The harmful class is semantic-param value-bleed. | **13 all-time events** on platform tools: board_import_plan projectId ×4, board_read storyId ×3, send_message message ×2, board_update_task claimToken, board_validate_file file, board_create_task epicId/description — 11 of 13 in the gauntlet session `ses_e862c3aa…` (incl. the issue's verbatim example). |
| A4 | #2030a hygiene is live, so a post-hygiene rate is measurable. | **Not yet.** tsk-windy-windmill-hovers is board-done (approved, CI green) but PR #2059 is OPEN/unmerged, and this pod's binaries (agentd Nov 14, opencode Aug 7) predate the commit (684ee38a2, branch only). Everything measured is the PRE-hygiene control arm; the post-hygiene residual is unmeasurable until merge + deploy. |
| A5 | Strict mode can safely reject every arriving identity key. | **No — would reject every valid call.** The plugin stamps all 37 `INJECTED_SESSION_TOOLS` calls BEFORE agentd sees them. The split that keeps valid calls landing: stamp-set tools' `lsp_injected_session` = platform's own (admit silently); everywhere else = model emission (teach). The stamp-set check keys on the ORIGINAL wire name so the never-stamped legacy aliases land on the teaching side. |
| A6 | The catalog schemas are the admitted-vocabulary source of truth. | Adopted: properties ∪ design-0070 file-spec keys ∪ storyId↔taskId pairs (the alias shim translates BEFORE the check runs) ∪ wrapper keys ∪ requestId (non-file tools). `TestStrictBoardArgs_PluginSetParity` additionally pins the stamp-set mirror against `runtimes/opencode/plugins/llmsafespaces-origin.js` so the silent-admission set cannot drift from the plugin that writes the stamp. |
| A7 | Byte-identity under flag-off is testable. | Pinned three ways: validator no-op on junk, `strictBoardToolSchemas` = identity, and no `additionalProperties` in any served board schema (raw schemas stringified — a `%v` on `json.RawMessage` is a byte dump and would pass vacuously; caught in review of my own first draft). |

## Measurement record (the evidence-gate data, pre-hygiene arm)

Workspace `7eaa3f1b…`-class pod, `/workspace/.local/opencode/opencode.db`, read 2026-11-16 ~00:05Z. Windows: all-time (17,707 tool parts; parts with `state.input` maps), current pod (post 23:27Z restart; 3,721 parts). Note: research traffic (agents grepping the stamp string, incl. this session) inflates any raw substring count — all numbers below are key-presence / value-bleed per the stated classes, not substring hits.

- All-time extra-field `lsp_injected_session`: 1,106/17,707 (6.2%) — but decomposes into plugin stamps (A1: 834 on stamp-set tools, 100% carriage) + true model emissions (272 on non-stamp-set surfaces, A2).
- Harmful semantic value-bleed: 13 all-time (A3); rate ≈ 0.07% of tool parts, concentrated in the long-replay-history gauntlet session.
- Current-pod window: fresh board-worker sessions ~0 emissions; gauntlet session 88/400 extras (22%) — consistent with the replay-continuation mechanism AND with #2030a's thesis (strip stamps from the SERVED replay → the pattern to continue disappears).
- Legacy `sessionId`/`session_id` extras on tools without those params: 0 in the current-pod window.

**Decision filed via pending_input** (criterion 1): the charter's persist branch ("if it persists, ship as recommended-on") matches the pre-hygiene data, but the gate's stated purpose — severity decided by POST-hygiene evidence — is unanswerable until #2059 merges and deploys. Shipped default OFF (byte-identical); recommendation: enable on gauntlet-class pods now, re-measure post-deploy, flip the default on the data.

## Work completed

- `cmd/workspace-agentd/strict_board_args.go` (new): the flag (`AGENTD_STRICT_BOARD_ARGS=1`), the stamp-set mirror (37 tools, parity-pinned), `strictBoardArgsCheck` (deterministic order: identity keys first, then unknown fields, both sorted), the admitted-vocabulary derivation, and `strictBoardToolSchemas` (map schemas shallow-copied; raw-JSON schemas byte-spliced with the `additionalProperties:false` prefix — #1849 order preserved, re-validated by decode).
- `mcp_server.go`: the check hooks tools/call AFTER alias normalization / BEFORE `callMCPTool`, rejecting through the standard isError envelope (the deprecation notice still rides, pinned); `mcpToolCatalog` returns `strictBoardToolSchemas(tools)`.
- `strict_board_args_test.go` (new, 8 test funcs / 15 subtests): the pins listed in the CHANGELOG, including the no-valid-call-rejected catalog sweep and the raw-splice byte pin.
- CHANGELOG `[Unreleased]` entry.

## Verification

- `go build` / `go vet` clean; `golangci-lint run ./cmd/workspace-agentd/...` — 0 issues; `make fmt` + `make fmt-check` clean.
- Full agentd suite, default env, minus `TestSupervisePiSubprocess*`: **green**.
- Full agentd suite under `AGENTD_STRICT_BOARD_ARGS=1`, same skip: **green** (the suite exercises the real handler over tools/call for the board battery, so this is the no-valid-call-rejected pin at suite scale; `TestStrictBoardArgs_DefaultOffIsNoOp` clears its own env so both sweeps coexist).
- `TestSupervisePiSubprocess_GracefulShutdown` / `_RepeatSignalExitsAtOnce`: fail identically on pristine origin/main (verified in a detached baseline worktree) — pre-existing flakes, unrelated.
- `pkg/repolint` `TestLive_Worklogs_NoDuplicates` fails on pristine origin/main too (the in-flight #2043 lane's `NNNNN_2026-11-15_progress-renewal-split-lapse-arm.md` sentinel) — pre-existing, unrelated to this branch.

## Process

Worktree off origin/main (`fix/2030c-strict-board-args`); AI reviewer until APPROVED; orchestrator merges; no sleep-polling.
