# Worklog: Trigger Editor Routine Parity

**Date:** 2026-08-09
**Session:** Fix trigger editor so routine triggers have full editing controls.
**Status:** Complete

---

## Objective

The trigger editor (`TriggerEditor`) only exposed a workflow dropdown for the Target section. Routine triggers — which carry `workspace_id`, `prompt`, `agent`, `script_*`, `memory_mode`, `capture_mode`, `preserve_session` — had no editing surface at all. Users could not change the target workspace, edit the prompt, or adjust any routine settings from the UI.

---

## Root Cause

The routines redesign (worklog 0699) rebuilt `TriggerCreateForm` for the new routine model but left `TriggerEditor` on the pre-redesign "Target Workflow" shape. The separate "Input Template" section was also broken: it initialized its textarea to `""` instead of `trigger.prompt`, and its save handler (`handleSavePrompt`) referenced an undefined `prompt` variable.

---

## Work Completed

### Editor (`TriggersPage.tsx`)

- **Target section is now mode-aware:**
  - **Routine triggers** (`!workflowId && workspaceId`): shows workspace dropdown + prompt textarea + agent profile input + script path/args/env + memory/capture/preserve dropdowns when editing. Read mode shows workspace name/phase, prompt preview, agent/memory/capture/session summary, and script info.
  - **Workflow triggers** (`workflowId` set): keeps the existing workflow dropdown + "Run now" button unchanged.
- **Workspaces fetched via `useQuery`** inside the editor (same pattern as `TriggerCreateForm`).
- **`handleSaveTarget`** sends the full routine field set for routine triggers, or `{ workflowId }` for workflow triggers.
- **Removed** broken `editingTemplate`, `templateStr`, `handleSavePrompt` state/handlers.

### Tests (`TriggersPage.test.tsx`)

- Added `WORKFLOW_TRIGGER`, `ROUTINE_WITH_SCRIPT` fixtures + `WORKSPACES` fixture + `workspacesApi` mock.
- 9 tests total (6 new + 3 review-requested):
  - Routine vs workflow mode switching (Target Workspace vs Target Workflow header)
  - Prompt pre-filled from trigger data
  - Workspace name displayed in target section
  - Routine field save payload (basic)
  - Memory/Capture/Session settings visible
  - Agent + script fields including parsed env in save payload
  - Cancel discards edits without calling onUpdate

---

## Key Decisions

1. **Conditional field omission matches create form.** `agent` and `scriptPath` are only included in the update payload when truthy — same as `TriggerCreateForm.handleCreate`. This means users cannot clear these fields via the editor (the API's `Partial<...>` update omits unset fields). This is intentional parity, not a regression. The core routine fields (`workspaceId`, `prompt`, `memoryMode`, `captureMode`, `preserveSession`) are always sent and freely editable.
2. **Workspaces query inside editor.** Rather than threading the query through props from the page component, the editor fetches workspaces directly via `useQuery` — matching the pattern already used by `TriggerCreateForm`.

---

## Tests Run

- `npx tsc --noEmit` — clean (0 errors in TriggersPage files)
- Note: vitest + eslint require Node 22+ (local env has Node 18); CI runs full suite
