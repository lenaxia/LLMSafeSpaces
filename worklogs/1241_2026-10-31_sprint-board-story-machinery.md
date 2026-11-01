# Worklog NNNN — the Sprint Board's machinery says story (rename phase 2, owner ruling 2026-10-30)

Task: tsk-wayward-whistle-exhales. Branch: sprint-board-story-machinery.

## Context

Phase 1 (v0.44.11, #1997) renamed every human-facing board string to
Sprint Board / story while the wire vocabulary deliberately stayed
`task` — the docs convention shipped the note "story (the wire-term is
'task')". The owner then asked: "did we rename all the machinery too?
mcp tools should be renamed, and other code references should be
changed to story." This lane is phase 2: the machinery follows the
prose.

## What changed

1. MCP tool names (both catalogs):
   - agentd registry: board_create_story, board_update_story,
     board_delete_story, board_delete_stories, board_rearm_story,
     board_pause_story, board_resume_story (the seven task-prefixed
     tools).
   - external platform MCP (pkg/mcp): the five of the seven that live
     there plus board_start_story, board_start_stories,
     board_story_verdicts, board_story_events (every task-prefixed
     board tool, per the ruling's "mcp tools should be renamed").
2. Wire INPUT KEYS: storyId/storyIds canonical on the registry surface
   (taskId/taskIds one-release legacy), story_id/story_ids on the
   external surface (task_id/task_ids legacy). shortId unchanged;
   epicId/dependsOn/requestId unchanged; the plan payload's "tasks" key
   unchanged (platform DTO). agentd TRANSLATES storyId→taskId at the
   dispatch boundary (normalizeBoardStoryArgs) so the platform seam
   stays byte-compatible — the REST wire keeps its task term.
3. Alias window: old names + old keys resolve through v0.45.x with a
   deprecation notice (second content block, errors included); removal
   in v0.46.0. Registry aliases are call-time only (tools/list stays
   canonical — the parity matrix stays clean); external aliases are
   registered DEPRECATED-ALIAS tools (mcp-go resolves by name before
   middleware, so the alias must be a registered tool).
4. Drift pins moved in the same commit: parity matrix (agentTools +
   extTools incl. the nine external aliases), origin plugin
   INJECTED_SESSION_TOOLS, docs/api/mcp.md tool table (+ alias rows),
   ToolDescriptionGuidance (board_read's UUID-alternative pin now says
   storyId), reviewer charter (persona.go teaches
   board_update_story {storyId, ...}), canary floor net-additive.
5. Frontend: TaskCard/TaskDialog/TaskDrawer/TaskDeleteDialog →
   Story*.tsx (+ tests, + describeStoryDeleteResult); testids and API
   client method names byte-identical.
6. Storage: documented KEEP (tasks, task_dependencies, task_id, tsk-,
   board_events action vocabulary) — comment atop pkg/board/store.go +
   api/migrations/README.md note + CHANGELOG.
7. Go identifiers: pkg/mcp's integrator-facing surface renamed
   (BoardCreateStory et al., BoardStoryBody); agent-internal plumbing
   (loopback client, REST handler methods, pkg/board engine) keeps the
   wire term by judgment — the REST wire and engine are not integrator
   MCP surfaces; decision documented in the PR.

## Verification

- go build ./... ; go vet (agentd, mcp, handlers).
- go test: pkg/mcp (incl. TestBoardStoryRenameAliases_AliasPathE2E —
  old name through the real mcp-go server: story created + notice),
  cmd/workspace-agentd -short (incl. TestBoardStoryAlias_* — the
  end-to-end old-name create through mcpHandler), api/internal/handlers
  (parity suite), pkg/repolint (docs + canary parity), pkg/board.
- frontend: tsc --noEmit clean, vitest 2584/2584, eslint 0.

## Coordination

- The docs lane (tsk-rested-shanty-feels, in flight) ships examples
  under the CURRENT (pre-rename) tool names with the note "story (the
  wire-term is task)". Its drift pin firing on this rename is
  CONFIRMATION, not a bug; when this lands, the note flips and the
  examples rename on their side.
- The relay-router agedarg fixtures keep their old tool names
  deliberately: they simulate AGED model context (pre-rename traffic),
  which is exactly what still carries old names through the window.
