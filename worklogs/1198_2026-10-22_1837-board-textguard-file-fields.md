# Worklog: Board tooling hardening P1 — emission-corruption defenses + file-backed long fields (#1837)

## Scope
Issue #1837 P1a+P1b only (P3 dual-encoding and P4 append/dryRun stay
queued per the charter; P2 lands as PR-B). The #1832 dogfood class:
>2KB inline string arguments corrupt — sometimes silently (a stored
description literally containing `","title":"PLACEHOLDER"}`).

## P1b — server-side rejection heuristics (pkg/board/textguard.go)
- Enforced at the Service layer (CreateProject/UpdateProject,
  CreateEpic/UpdateEpic, CreateTask/UpdateTask, ImportPlan's
  validatePlan incl. dryRun) — REST, external MCP, and the agent seam
  share it by construction.
- Byte caps: name 256 / title 512 / description 128 KiB / outcome
  64 KiB / criterion text 8 KiB / tag 64 bytes × 16 tags (GuardInput,
  400). Empty fields pass (patch semantics).
- Degenerate repetition (GuardTextRejected, new code — 400): (a) a
  fragment exactly tiling the value ≥50 times (smallest-period trick;
  unit must carry an alnum rune so `=====` dividers pass); (b) one
  line (≥8 bytes, alnum) repeated ≥50 times. Threshold 50 = the
  acceptance bar and the observed class; 40 repeats passes.
- JSON structural splice: `","key":"` adjacency inside string values
  rejected (whitespace-tolerant; line-start and
  post-unterminated-fence artifacts included — review round 2).
  False-positive gates (all test-pinned): TERMINATED fenced code
  blocks, balanced {...}/[...] containers (stack scanner with string
  state). Legit compact JSON examples pass (as balanced containers);
  a fenced block QUOTING the artifact passes. Review round 2 added:
  junk-wrapped repetition (runRepetition, units 1..512 bytes),
  tag-channel corruption scan, no-file-tool *_file refusal.
- Echo diff-flag: create/update task+epic responses carry
  `content_echo_diff: [title|description|outcome]` only when a
  submitted field stored differently (handlers tier; REST + agent
  seam — external MCP passes REST through).
- renderBoardError maps GuardTextRejected → 400;
  boardErrorCodes.json + guardConstValues parity updated.

## P1a — file-backed long fields (agent surface only, per the review ruling)
- board_create_task/board_update_task accept description_file;
  board_create_epic/board_update_epic accept outcome_file. Resolved
  in dispatchBoardManagerTool BEFORE the wire marshal; the platform
  API has no *_file keys.
- loadImages discipline (PR #1364): O_NONBLOCK open (FIFO wedge),
  regular-file gate, stat AND read caps (TOCTOU) at 128 KiB, UTF-8
  validation (byte-identical JSON round-trip requires it). Inline +
  file → ambiguity error; a *_file key on the wrong tool → refused
  loudly (no silent wire drop).

## Tests (RED first — commit 06cd8e1b quotes the failing output)
- pkg/board/textguard_test.go: caps both sides of every boundary,
  50-fold fragment reject + 40-fold pass, junk-wrapped line loop
  reject, dividers/tables pass, splice artifact reject, four legit
  JSON/markdown fixtures pass, fenced-artifact-quote passes, >4KB
  varied prose passes.
- pkg/board/textguard_integration_test.go (PG, CI): service tier —
  repetition reject, splice reject, >4KB charter round-trips
  byte-identical.
- cmd/workspace-agentd/board_file_params_test.go: >4KB byte-identical
  resolution, wire contract (description on the wire, _file consumed),
  ambiguity/wrong-field/cap/FIFO/missing/non-UTF-8 refusals.
- api/internal/handlers/board_textguard_echo_test.go: seam renders
  text_rejected as 400+code; echo diff-flags drift; clean response
  shape unchanged (key absent).

## Gates
go build/vet clean; pkg/board, api/internal/handlers (81s incl.
parity), cmd/workspace-agentd (360s), pkg/mcp, pkg/repolint all ok;
make openapi-validate ✓; make repolint ✓.
