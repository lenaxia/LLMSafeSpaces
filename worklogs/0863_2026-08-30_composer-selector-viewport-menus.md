# Worklog: Viewport-aware composer selector menus + frontend lint debt cleanup

**Date:** 2026-08-30
**Session:** Make the model/persona selector dropdowns (moved to the composer drawer at the bottom of the screen in US-67.5) and all similar floating elements viewport-aware so they never render off-screen; then clear all pre-existing frontend eslint errors (35 errors + 1 warning from the v0.25.4 merge).
**Status:** Complete

---

## Objective

The composer drawer (e90cfef5, US-67.5) relocated `ModelSelector` and `RoleSelector` to the bottom of the chat screen, but their dropdowns still used `absolute right-0 top-full` — opening downward off-screen. A work-in-progress uncommitted attempt to fix this was found broken: the portaled menu divs were missing `fixed`/z-index classes (so inline `top/left` were ignored and the menu rendered statically at the end of `<body>`), the toast portal had no positioning at all, and `NewWorkspaceSplitButton`'s error notice reused the popup menu's ref and its unmeasured `pos` state, rendering at the viewport top-left. Goal: finish the fix properly, make every similar floating element edge-aware, and leave `eslint .` at zero errors.

---

## Work Completed

### Viewport-aware floating elements (TDD)

- Extracted the proven KebabMenu positioning geometry into `frontend/src/components/ui/menuPosition.ts`: `computeMenuPosition` (pure flip-above / horizontal-clamp / maxHeight-cap function, moved verbatim from KebabMenu.tsx) plus a new `useMenuPosition(active, align, fallbackWidth, anchorRef?)` hook that owns the trigger/menu refs, measures pre-paint via `useLayoutEffect`, re-measures post-paint, and re-anchors on scroll (capture) + resize.
- `ModelSelector.tsx`: dropdown portaled to `document.body` with `fixed z-50` + `role="menu"`; menu items got `role="menuitem"`; the transient toast ("takes effect on your next message" / failure) is now a positioned portal sharing the dropdown's anchor via the hook's `anchorRef` parameter (a button accepts only one `ref`, so the second hook instance reuses the first's `triggerRef`).
- `RoleSelector.tsx`: same treatment for its dropdown.
- `NewWorkspaceSplitButton.tsx`: popup refactored onto the hook; the creation-error notice got its own hook instance anchored to the control container (`containerRef`), its own `errorRef` (no longer shared with the popup), portal to body, `w-56` restored, `role="alert"`.
- `KebabMenu.tsx`: refactored onto `useMenuPosition` (−113 lines), behavior unchanged (its 15 existing tests pass unmodified).
- Tests (red → green, 11 failing first): portal/fixed-class assertions, flip-above near bottom (exact px), left-clamp near right edge, maxHeight cap, edge-aware toast, error notice anchored + flipped. New `RoleSelector.test.tsx` (behavior + positioning). New `src/test/menuGeometry.ts` shared jsdom geometry-mock helper, adopted by KebabMenu.test.tsx and NewWorkspaceSplitButton.test.tsx (removes 3× duplicated mock code). Pure-function tests moved to `menuPosition.test.ts`.

### Frontend lint debt cleanup (35 errors + 1 warning → 0)

- `api/workflows.ts`: wire fields backed by Go `json.RawMessage` (`error`, `input`, `output`, `sourceConfig`, `scriptEnv`, `inputEnvelope`, `actionResult`) typed `unknown` — matching the generated TS SDK (`sdks/typescript/src/client.ts:714`) and the file's own `inputSchema?: unknown` convention; extracted exported `TriggerCreateInput` / `TriggerUpdateInput` from the inline create/update signatures.
- New `toCronConfig(value: unknown): CronConfig` narrowing helper in `cronUtils.ts` (TDD, 7 tests) — the OpenAPI spec documents `sourceConfig` as `cron: {expr, tz?} | webhook: {}`.
- `TriggersPage.tsx`: typed create/update payloads built via object spreads (no `any` mutation), `parseScriptEnvText`/`scriptEnvToText` helpers, `catch (e)` narrowed with `instanceof Error`, `unknown && JSX` → `!!x && JSX`.
- `RunDetailPage.tsx`: removed `run as any` (`run` is already `WorkflowRun`), typed refetchInterval callbacks, `!!x && JSX`.
- `WorkflowsPage.tsx`: dropped no-op `as any` casts and stale `as Workflow`; `BadgeVariant`.
- `NodeEditPanel.tsx`: `strField`/`numField` unknown-narrowing helpers; `DAGCanvas.tsx`: `EdgeChange[]`; `WorkflowEditor.tsx`: `instanceof Error` catch; `Badge.tsx`: exported `BadgeVariant`.
- `useConfirmDialog.tsx`: removed empty `interface ConfirmState extends ConfirmOptions {}`.
- `AuthProvider.passkey.test.tsx`: renamed the fake custom hook `useAuthHook` → `renderAuthHook` (the `use*` name made eslint-plugin-react-hooks flag `useAuth()` inside the `renderHook` callback; inlined shape matches the repo's `useComposerAttachments.test.tsx` convention).
- `tests/e2e/passkey.spec.ts`: removed unused `ORIGIN` const and a stale `eslint-disable` directive.

---

## Key Decisions

1. **Shared hook rather than per-component copies.** Four consumers (KebabMenu, ModelSelector, RoleSelector, NewWorkspaceSplitButton) had identical measure/position/effect logic — demonstrated recurrence per Rule 4/12, not speculative abstraction.
2. **`anchorRef` parameter instead of merged callback refs.** The ModelSelector toast needs the same anchor as the dropdown; a `ref` prop accepts one ref, so the hook accepts an external ref to share. Avoided a ref-merging utility.
3. **`unknown` (not invented interfaces) for RawMessage-backed wire fields.** The backend intentionally carries arbitrary JSON; the generated SDK already types these `unknown`. Consumers narrow at the boundary (`toCronConfig`, `strField`, `instanceof Error`) instead of lying about shapes.
4. **Toast/error notices treated as first-class floating elements.** Both previously used `absolute top-full` below the trigger — also off-screen near the bottom edge.
5. **Two commits, one PR.** The UI fix and the lint cleanup are independent logical units.

Assumptions validated: KebabMenu positioning tests exist and pin `computeMenuPosition` behavior (moved verbatim); Radix `Select`/`Tooltip` already flip via Floating UI (no work needed); the lint errors exist at clean HEAD 61593ee8 (verified via a throwaway `git worktree` + eslint before touching anything).

---

## Blockers

- Pushing the branch / opening the PR requires GitHub credentials (`gh` not authenticated, `.git-credentials` empty). Commits are staged locally on `fix/composer-selector-viewport-menus`, ready to push.

---

## Tests Run

- `npx vitest run` (targeted files during TDD): red first (11 positioning failures), then green.
- `npm test` — full suite: 163 → 164 files, 1825 → 1832 tests, all passing (includes 7 new `toCronConfig` tests).
- `npm run typecheck` — clean.
- `npm run lint` — 35 errors + 1 warning → **0 problems**.
- Playwright e2e not run (requires browsers + backend); component tests with mocked jsdom geometry follow the established KebabMenu #652 precedent for this class of fix.

---

## Next Steps

- Push `fix/composer-selector-viewport-menus`, open the PR (frontend scope), and iterate on the automated reviewer until APPROVE, then squash-merge.
- If the reviewer asks for e2e coverage of the dropdown flip, add a Playwright spec in `frontend/tests/e2e/` mocking a short viewport and asserting the menu's `boundingbox` stays in-viewport.

---

## Files Modified

- frontend/src/components/ui/menuPosition.ts (new), menuPosition.test.ts (new)
- frontend/src/components/ui/KebabMenu.tsx, KebabMenu.test.tsx, Badge.tsx
- frontend/src/components/chat/ModelSelector.tsx, ModelSelector.test.tsx
- frontend/src/components/chat/RoleSelector.tsx, RoleSelector.test.tsx (new)
- frontend/src/components/workspace/NewWorkspaceSplitButton.tsx, NewWorkspaceSplitButton.test.tsx
- frontend/src/test/menuGeometry.ts (new)
- frontend/src/api/workflows.ts
- frontend/src/components/workflows/cronUtils.ts, cronUtils.test.ts (new), DAGCanvas.tsx, NodeEditPanel.tsx, WorkflowEditor.tsx
- frontend/src/pages/TriggersPage.tsx, RunDetailPage.tsx, WorkflowsPage.tsx
- frontend/src/hooks/useConfirmDialog.tsx
- frontend/src/providers/AuthProvider.passkey.test.tsx
- frontend/tests/e2e/passkey.spec.ts
