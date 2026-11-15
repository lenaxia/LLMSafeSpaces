# Worklog: The prelim-story pattern joins the worked-pattern catalog

**Date:** 2026-11-15
**Session:** tsk-primal-vignette-covers (epic "Epic wrap-up review (holistic, delta-scoped, role-configurable)", project "Agent DX: reference patterns")
**Status:** Complete — pushed for AI review

---

## Objective

Owner direction 2026-11-15: the Sprint Board tooling's core prompting should teach agents the process this epic itself uses —

1. PRELIM STORIES ARE A CHOICE: when an ask is complex or under-specified, the agent may create a prelim story whose deliverable is scoping + high/low-level designs, with implementation stories gated on it — and the prelim may UPDATE those implementation stories from its findings (`board_update_story` is part of the pattern, not an exception).
2. REQUIREMENTS BEFORE FILING: agents work with the user to flesh out requirements BEFORE creating stories/epics when complexity calls for it — filing is the output of design conversation, not its substitute.
3. Story updating from findings applies generally (a worker discovering charter-stale scope surfaces it; manager-side updates are normal).

Guidance only, no behavior changes. The prompt source lives IN this repo (checked before starting): the board tooling's core guidance is the worked-pattern catalog (`docs/automation/sprint-board-patterns/`, embedded byte-identical into the agentd binary, materialized to `/sandbox-runtime/docs/sprint-board-patterns/`, pointed at by the `board_create_story` / `board_import_plan` descriptions on BOTH surfaces) — so no pending_input was needed.

## Recon — where the prompting lives

- Sources of truth: `docs/automation/sprint-board-patterns/` (13 files → 14). Embedded copies: `cmd/workspace-agentd/sprint_board_patterns/` (drift-pinned byte-identical by `TestPatternDocs_DriftPins_SourceOfTruth`). Materialized by init-fs (mirror semantics).
- Canonical header (binding format ruling): `# <title>` / `<!-- pattern: slug=<stem>; doc=board; use-case=…; nodes=none; wiring=none -->` / `Description: <one sentence>`; grep is the catalog, no INDEX.
- Every fence carries `<lang> kind=… [op=…]`; board-payload fences are validated by THE EXTRACTOR against the real `board_validate_file` pipeline — so both worked payloads in the new doc had to pass the real validator (they do; the `update_task_patch` example rides the replace-semantics advisory, which warns and never blocks).
- The worker charter envelope itself is composed in behavior code (`pkg/board/rows.go`, api-side spawner) — deliberately untouched; charters reach this guidance by referencing the catalog, which is where the teaching belongs.

## Work completed

- NEW `shape-prelim-story.md` (use-case=mechanics, the shape family): "Requirements before filing" (the design conversation first; filing is its output, not its substitute — with the test for when complexity calls), "The prelim story" (the gated-design shape; implementation stories chartered with the best-known-today scope on the SAME plan — provisional is fine, empty is not), "Patch the implementation stories from findings" (`board_update_story` as part of the pattern — manager-route shape, worked `update_task_patch` payload, the REPLACE-semantics caution, edges move via add/remove dependency), "Updating from findings, generally" (workers surface charter-stale scope in the turn report; manager-side updates between claims are normal board life), and "When this shape is wrong". Worked plan: prelim + gated impl for an under-specified "notes search" ask (both fences pass the real validators).
- Cross-refs, one line each, in `shape-single-story.md` ("When this shape is wrong" gains the under-specified-ask bullet) and `shape-epic-plan.md` ("Sizing the epic" gains the prelim-opening sentence) — the catalog's cross-referencing convention.
- Embedded copies mirrored byte-identical (all three files).
- The `board_create_story` / `board_import_plan` description enumerations updated on BOTH surfaces (`cmd/workspace-agentd/mcp_server.go`, `pkg/mcp/board_tools.go`) — the only "wiring": the pointers now name "the optional prelim story that gates implementation stories and patches them from its findings". Existing drift pins (path + `grep '^Description:'`) untouched and green.
- Content pins: `TestPatternDocs_ContentPins_BoardCatalog` gains four fragments (`prelim story`, `not its substitute`, `board_update_story`, `REPLACES the full criteria set`) so a rewrite cannot silently drop the new teaching.
- CHANGELOG `[Unreleased]` entry; this worklog; COORDINATE.md row.

## Verification

- `go test ./cmd/workspace-agentd/ -run 'TestPatternDocs|TestEnsureEmbeddedPatternDir|TestInitFS|TestMCPHandler_ToolDescriptionGuidance'` — green (format pins, catalog coverage, content pins, fence extractor incl. the two new board-payload fences through the REAL validator pipeline, tool-name registry, placeholder grammar, vendor blocklist, drift pins, mirror).
- `go test ./pkg/mcp/...` — green (pointer pins on the external projection).
- `go build ./...` — green. `make fmt-check` — green.
- Full agentd suite: 2 pre-existing failures (`TestSupervisePiSubprocess_GracefulShutdown`, `TestSupervisePiSubprocess_RepeatSignalExitsAtOnce`, 60s signal-timing timeouts) — reproduced on the CLEAN shared main checkout without my changes (environmental, this sandbox; unrelated to a docs/description-string change). Everything else green.

Docs + pins + description strings only; no tool behavior changed.
