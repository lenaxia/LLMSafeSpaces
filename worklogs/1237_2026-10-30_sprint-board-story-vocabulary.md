# Worklog — 2026-10-30: the Task Board becomes the Sprint Board; tasks → stories in every human-facing surface (owner ruling 2026-10-30)

Task: `tsk-orient-lantern-speeds` (epic "Sprint Board: naming, onboarding, and surface
polish", project "Agent DX: reference patterns").

## The ruling (verbatim intent)

> "Rename the task board to sprint board. And rename tasks to stories/story. Task
> conflicts too easily with other features such as subtasks for opencode. Story is
> more explicit and doesn't conflict with other wording."

## The scope line (settled before the first edit)

PRESENTATION-LAYER rename only. Every human-readable surface says "Sprint Board" and
"story/stories". IDENTIFIERS/WIRE/DB STAY `task`: the `tasks` table, `tsk-` short ids,
REST paths (`/me/tasks`), SDK methods (`CreateTask`, `boardApi.createTask`), MCP tool
names (`board_create_task`, `board_read`, …), testids (`task-card-*`, `new-task-*`) are
byte-identical — the pending_input flag-not-stage precedent: the UI vocabulary moves,
the wire contract doesn't. Where prose must reference the wire term, it reads
"story (the wire/API term is 'task')" ONCE per surface, not at every mention.

Stage names (Backlog, In Progress, In Review, Done), "Acceptance criteria", and
epic/project vocabulary are untouched — the ruling covers board/task naming only.

## 1. Frontend sweep

- `BoardPage.tsx`: the h1 and the Sidebar nav item say **Sprint Board**; the swimlane
  creation button says **New story** (title attrs "New story in <epic/project>");
  the epic picker dialog title "New story in … — pick an epic"; the picker's empty
  state "a story needs its epic"; the epic-delete notice counts "1 story / 2 stories
  cascaded"; the epic band tooltip highlights "its stories"; the search input's
  aria-label is "Search stories"; the FAILED filter tooltip says "stories"; the card
  kebab offers "Delete story…".
- `TaskDialog.tsx` (create/edit): aria-labels and titles "New story" / "Story <title>",
  placeholder "Story title", the destructive button "Delete story".
- `TaskDrawer.tsx` (details drawer): aria-label "Story <id>", "Edit story" footer
  button, and the audit-timeline ACTION_LABELS render "story created/updated/deleted/
  paused/resumed" (the wire action keys `create_task` etc. and the `task_start`
  dispatch label keep their wire spellings).
- `TaskDeleteDialog.tsx`: "Delete story <title>", dependents copy "this story blocks
  nothing else" / "lose their edge on this story" / "inherits this story's direct
  upstream edges".
- `EpicDrawer.tsx` / `EpicDeleteDialog.tsx`: "New story" button; "every story under it
  are removed"; cascade count "1 story / N stories".
- `TaskCard.tsx`: "Story details — charter, criteria, full audit timeline",
  "Story start (task_start) — manual one-off dispatch…", "Story actions for …".
- `boardRules.ts` BOARD_GUARD_MESSAGES (the 409 hint map — user-visible in
  BoardErrorNotice): claim_token / task_paused / graph_limit / self hints now read
  "story/stories" ("The story holds a live worker claim…", "A story cannot depend on
  itself…").
- Testids/data attributes untouched everywhere (not user-facing).

Re-pinned suites: `BoardPage.test.tsx` gains the vocabulary pin (heading "Sprint
Board", the new-task button's textContent "New story" + title, `Search stories`
aria-label, and `queryByText("New task")` never rendering); `BoardPage.deleteflows`
menuitem names "Delete story…"; swimlane/validation test titles follow; e2e
`board.spec.ts` ("New story in …" ×6, "Delete story…" ×2, describe "Sprint Board UI")
and `board-config.spec.ts`; `SettingsPage.test.tsx` asserts BOTH "Sprint Board" and
"Task Board" absent from Settings (the relocated-config pin).

## 2. MCP tool descriptions — both catalogs

- agentd `cmd/workspace-agentd/mcp_server.go` (the in-workspace manager + worker
  catalog): every board tool description is story-first. `board_create_task` leads
  "Create one story (the wire/API term is 'task') in an epic's backlog";
  `board_list` "List EVERY story (the wire/API term is 'task') of one board project";
  `board_read` (worker view) "Read ONE Sprint Board story (the wire/API term is
  'task') — your claimed story's full worker view"; `board_get_config`/`board_set_config`
  "the Sprint Board's configuration"; `board_create_project` "a Sprint Board project";
  the dependency pair "story→story within this workspace"; bulk tools count stories;
  the plan tool explains the payload key: "stories … (the plan payload key is
  'tasks' — the wire/API term for stories)". The heartbeat's recovery line now reads
  "re-read it with board_read" — and the ToolDescriptionGuidance drift pin moved in
  the same commit ("re-read it with board_read", the #1415/0059 same-commit protocol).
  Input-schema property descriptions follow ("Story short id (tsk-…)", "the story's
  UUID") — parameter NAMES (`taskId`, `taskIds`) are wire and unchanged.
- api-side `pkg/mcp/board_tools.go` (the external platform MCP projection; NOTE: the
  charter named `pkg/mcp/workflow_tools.go`, but the board catalog lives in
  board_tools.go — workflow_tools.go carries no board tools; the spirit "both
  catalogs" is honored): `board_get`'s tsk- arm "a story (with blocked-by derivation;
  the wire/API term is 'task')", `board_read` "the project's stories (the wire/API
  term is 'task')", `board_create_task` "Create one story (the wire/API term is
  'task') in an epic's backlog" + "a review story whose subject's done transition
  settles it", `board_move_stage` "Move a story (…) to another stage", pause/resume/
  rearm/delete/bulk/import/dependency tools story-first, `board_delete` "a story or
  epic … delete_epic: cascades to its stories".

## 3. OpenAPI + SDK doc-comments (description text only)

- `sdks/openapi.yaml`: the board tag description is "Sprint Board (formerly task
  board) — projects, epics, stories (the wire/API term is 'task'), structural
  dependencies…"; board-section summaries/descriptions read story-first ("List a
  project's stories (wire rows: tasks)" — inner-colon summaries quoted for YAML);
  schema prose (derived progress "done stories", pause lever "every story under the
  project", verdict/events/search/health prose). ZERO path/operationId/field/schema
  renames — `make -C sdks validate` green and the router/schema parity tests green
  prove the wire untouched.
- SDK doc-comments: `sdks/go/board.go` header + BoardTask/ListProjectTasks/
  DeleteEpic/DeleteTask/PauseTask comments; `sdks/typescript/src/{client,types}.ts`
  section headers; `sdks/python/llmsafespaces/client.py` BoardService docstring;
  `sdks/java/.../BoardService.java` class javadoc — each carries the one-time
  "prose says story; the wire/API term stays task" note.

## 4. Docs

- `docs/api/mcp.md`: the section is retitled "### Sprint Board (design 0067)" with
  the discoverability line "Formerly the Task Board (owner ruling 2026-10-30): the
  UI and this prose say **story**; the wire/API term stays `task`…" and the tool
  table's prose is story-first. README/README-LLM carry no board sections (checked).

## Sibling-lane coordination (same epic, same clone)

`wooden-engine-ambles` (tsk-glassy-shovel-feels, the help drawer) had already wired
BoardPage.tsx hunks in the shared tree when this lane started; we exchanged
coordination messages, they extracted their delta to an isolated worktree (branch
`board-help-drawer`, commit 7ab64ee3, worklog 1236) and reverted their hunks from
this tree — zero residue verified (`git status` clean of their files before this
lane's commits). The two lanes touch BoardPage.tsx at disjoint hunks; either landing
order merges clean. Their drawer copy already uses the sprint-board/story vocabulary
and pins "never says 'task board'".

## Gates

- frontend: vitest 2577/2577 (224 files, full suite), tsc clean, eslint 0 errors
  (pre-existing warnings only), e2e specs repinned (browser suites run in CI).
- go: `go test ./pkg/mcp/` green, `go test ./cmd/workspace-agentd/` green (372s,
  incl. TestMCPHandler_ToolDescriptionGuidance with the moved pin), gofmt clean.
- `make -C sdks validate` green; `TestOpenAPIRouterContract` + BoardTask schema
  parity green (paths/fields byte-identical).
- `make fmt-check` green.

## Files

Frontend: `frontend/src/pages/BoardPage.tsx`, `board/{TaskCard,TaskDialog,TaskDrawer,
TaskDeleteDialog,EpicDrawer,EpicDeleteDialog}.tsx`, `layout/Sidebar.tsx`,
`lib/boardRules.ts`, suites `BoardPage.{test,deleteflows,swimlane,validation}.test.tsx`,
`SettingsPage.test.tsx`, e2e `board.spec.ts` + `board-config.spec.ts`.
MCP: `cmd/workspace-agentd/{mcp_server.go,mcp_server_test.go}`,
`pkg/mcp/board_tools.go`. Spec/SDKs: `sdks/openapi.yaml`, `sdks/go/board.go`,
`sdks/typescript/src/{client.ts,types.ts}`, `sdks/python/llmsafespaces/client.py`,
`sdks/java/src/main/java/com/llmsafespaces/sdk/services/BoardService.java`.
Docs: `docs/api/mcp.md`. Bookkeeping: this worklog, CHANGELOG [Unreleased],
COORDINATE.md claim row.
