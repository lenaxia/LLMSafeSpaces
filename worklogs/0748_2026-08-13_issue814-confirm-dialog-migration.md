# Worklog: #814 — ConfirmDialog migration

**Date:** 2026-08-13
**Issue:** #814
**PR:** #815

---

## Context

#775's immediate fix (`safeConfirm`) closed the fail-open data-loss bug
but left all confirm dialogs as native `window.confirm`. Issue #814
tracked the full migration to the accessible `ConfirmDialog` component.

## Changes

### New: `useConfirmDialog` hook (`frontend/src/hooks/useConfirmDialog.tsx`)

Imperative API backed by Radix Dialog:
```tsx
const { confirm, dialog } = useConfirmDialog();
confirm({ title, description, confirmLabel, destructive, onConfirm });
// render {dialog} in JSX
```

### Migrated: 14 call sites across 10 files

| File | Actions |
|---|---|
| ChatPage.tsx | Session delete |
| Sidebar.tsx | Workspace delete + session delete |
| SecretsTab.tsx | Secret delete |
| McpServersTab.tsx | MCP server delete |
| WorkspaceImagesTab.tsx | Image delete |
| PlatformUsersTab.tsx | User suspend |
| OrgSettingsTab.tsx | Org suspend + delete |
| OrgSSOTab.tsx | SSO remove + token rotate |
| TriggersPage.tsx | Trigger delete + webhook rotate |
| WorkflowsPage.tsx | Workflow delete |

### Deleted: `safeConfirm.ts` + test (orphaned after migration)

### Tests: 12 new dialog integration tests across 6 files

Each site verified: dialog opens on click → confirm calls action,
cancel does not call action.
