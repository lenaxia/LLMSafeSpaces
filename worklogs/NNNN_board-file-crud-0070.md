# Worklog — design 0070: board file-backed CRUD (bodyFile/patchFile/planFile, board_validate_file, auto-cleanup)

Lane: board-file-crud (worker session suave-auction-rushes, orchestrator
merry-finch-loses). Design: the approved charter document at
`/workspace/reports/board-file-crud-design.md` (three in-flight
amendments: straight cutover; strongly typed validator + CRUD-time gate;
unified plan document + the epicId platform arm), rendered into repo
conventions as `design/0070_2026-10-26_board-file-crud.md`. PR #1969.

## 1. What was built (and why it is a cutover, not an option)

The live failure class (three occurrences in the orchestrating session):
board CRUD calls carry multi-KB *structured* JSON arguments, and model
emission corrupts exactly that shape — duplicated argument fragments,
stray file keys pasted from prior calls, parameter streams that never
parse. #1837 had already moved single long strings to files; the
remaining exposure was the structured payload itself.

- **The five payload-bearing manager tools** (`board_create_task`,
  `board_create_epic`, `board_update_task`, `board_update_epic`,
  `board_import_plan`) take their payload ONLY from a JSON file
  (`bodyFile`/`patchFile`/`planFile`) — inline payload keys are refused
  with the workflow hint (write → `board_validate_file` → submit), the
  file is REQUIRED, and the tools' schemas drop the inline payload
  properties entirely (the unsafe shape is unexpressible, not merely
  discouraged). The #1837 `descriptionFile`/`outcomeFile` params are
  deleted; the read discipline survives in the shared reader.
- **Resolver discipline**: the #1837 reader (O_NONBLOCK, regular-file
  gate, stat+read caps, UTF-8) at a 256 KiB payload cap; strict JSON
  decode (single document, duplicate keys refused at EVERY object
  depth, object top level); relative paths anchor to the workspace
  root (the live cwd bug — `open reports/charters/…: no such file`).
- **Auto-cleanup**: success deletes the consumed file; import_plan's
  dryRun keeps it (a dry run mutates nothing); delete failure folds a
  `cleanup_warning` into the succeeded result; the missing-file error
  teaches the retry semantics (check `board_list` first).
- **The validator**: santhosh-tekuri/jsonschema/v6 (the
  pkg/workflows/input_schema.go precedent incl. the `kind` package) —
  one compiled schema per op, `additionalProperties:false`, property
  sets pinned to the platform request DTOs by a REFLECTION parity test
  (DTO drift fails CI; the importable typed mirrors are pkg/mcp's
  Board*Body structs + pkg/board's PlanInput, with DECLARED
  exclusions for inline-only control-plane keys and the legacy REST
  arms). Semantic layer above the schema: #1849 atomic-create, the
  placeholder scan (ONE vocabulary via isPlaceholderClassPayload),
  plan-key uniqueness, the exactly-one-of epic arms. Findings carry
  instance paths (`tasks[2].acceptanceCriteria[0].text`), rule,
  severity, problem/fix, expected/actual.
- **Two entry points, one implementation**: the `board_validate_file`
  tool (local, advisory, zero wire calls, never deletes) AND the CRUD
  dispatch gate — error findings refuse the wire call with
  byte-identical feedback (dual-entry equivalence pinned by test);
  warnings never block (the acceptanceCriteria replace-semantics
  advisory).
- **The unified plan document + the one platform change**:
  `planFile` carries `{epic:{title,outcome,tags} | epicId, tasks:[1..N]}`
  — exactly one epic arm; `ImportPlan` gained the epicId attach arm
  (exactly-one-of validation, belongs-to-project guard, the #1948
  terminal-epic guard mirrored from create-task incl. the in-tx
  row-lock re-check, epic-create skipped in the one transaction);
  seam + REST handlers bind the typed `board.PlanInput`; openapi
  documents both arms; the legacy epicTitle arm stays wire-accepted
  for REST/external MCP.

## 2. Coordination (the shared clone)

Work landed uncommitted in the shared `/workspace/LLMSafeSpaces` clone
alongside two other live lanes (session subscriptions, authority replay
ring). Board sections only in shared files; the plugin's
INJECTED_SESSION_TOOLS and the api parity matrix gained exactly the new
tool's entries. The PR was isolated via `git worktree` with per-hunk
separation of the shared files (mcp_server.go, mcp_tools_test.go,
CHANGELOG, the plugin). Design doc numbered 0070 (the replay-ring lane
concurrently claimed 0069 in design/ and the CHANGELOG).

## 3. Verification

`go build ./...`; `go test ./cmd/workspace-agentd/... ./pkg/board/...
./api/...` (44 packages incl. the full board + MCP parity suites);
`golangci-lint run` 0 issues; integration-tagged code vetted. Coverage:
resolver discipline classes, inline-payload refusal on all five tools,
file-required refusals, path-anchoring regression, CRUD-time gate +
dual-entry byte-equivalence, validator finding per violation class,
schema-DTO parity per op, cleanup matrix (success/wire-failure/
validation-failure/dryRun/delete-failure warning), unified-doc wire
contracts, platform arm units + PG integration pins.

## 4. Review round 1 (the repo's own AI reviewer, PR #1969)

CHANGES_REQUESTED at 3a25fa89f — two blocking findings, both fixed:

- **F1 — replay-first violation in the attach arm**: the terminal-epic
  guard ran BEFORE `replayOf`, so an idempotent retry (same requestId)
  after the target terminalized returned GuardEpicState instead of the
  original outcome — contradicting both the #1948 precedent
  (service.go's CreateTask gates the identical guard on replay == nil)
  and my own comment. Fix: resolution + belongs-to-project stay
  pre-replay (static identity facts), the terminal refusal moved
  inside `if replay == nil`; the replay now also RENDERS from the
  recorded outcome (the CreateTask discipline — original epic id,
  short id, and task keys; previously the discarded `dup` left a
  phantom fresh-uuid epic + empty task map on replay). Pinned by
  `TestPlan_0070_AttachReplayAfterTerminalization`.
- **F2 — no worklog** (this file).

Non-blocking notes also addressed: the cross-project attach refusal no
longer embeds the resolved epic's short id (the unscoped `resolveEpic`
reader made it a cross-tenant UUID→phrase existence oracle — store.go's
r1-S1 leak class); the REST handler's epic/epicId binding is now pinned
(`TestBoardREST_ImportPlanBindsUnifiedPlanDoc`); the mixed
patchFile+setCriteriaMet manager call is pinned at the agentd layer.
