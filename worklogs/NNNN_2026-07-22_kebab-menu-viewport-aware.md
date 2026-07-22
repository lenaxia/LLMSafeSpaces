# Worklog: Kebab menu viewport-aware positioning

**Date:** 2026-07-22
**Session:** Fix the kebab menu that opened off the bottom of the screen when triggered from the last sidebar session; make all kebab menus layout-aware.
**Status:** Complete

---

## Objective

The kebab (three-dot) menu on the bottom-most workspace/session in the left nav opened directly below its trigger and overflowed past the viewport bottom — partially unreadable/unusable. Make every kebab menu viewport-aware (flip/clamp/scroll). Also confirm there are no other custom popups/modals with the same issue.

---

## Work Completed

### Root cause

`frontend/src/components/ui/KebabMenu.tsx` `updatePos` hardcoded `top = rect.bottom + 4` with no viewport awareness — no flip, no clamp, no height cap. `KebabMenu` is the shared component backing three usages (sidebar workspace, sidebar session, chat header), so the fix applies to all of them.

### Scope check

`grep createPortal frontend/src` confirmed `KebabMenu` is the **only** custom portal in the codebase. Radix tooltips/modals elsewhere already do their own collision detection, so no other popup needed changes.

### Fix

- Extracted the geometry into a pure `computeMenuPosition(btnRect, menuSize, viewport, align)` so it is unit-testable without jsdom layout (which doesn't compute geometry). Returns `top`, `left`, and an optional `maxHeight`.
- Logic: open below by default; **flip above** when there's not enough room below but enough above; when taller than both sides, pick the side with more room, clamp `top`, and cap `maxHeight`; **horizontal clamp** to the viewport edges; floor/ceiling at an 8px `VIEWPORT_PAD`.
- Component measures the rendered menu in a `useLayoutEffect` (before paint — no flash) and applies the result; repositions on scroll/resize via a capture-phase scroll listener + resize listener so it stays anchored as layout changes.
- Applied `overflow-y-auto` + conditional `maxHeight` to the menu div so tall menus scroll inside the viewport.

### Tests (TDD)

- **9 pure-function tests** for `computeMenuPosition` (written first, failed on missing export, pass after): open-below-default, flip-above, both-sides-tall clamp+maxHeight (each side), left/right horizontal clamp, negative-left clamp, top≥PAD floor, right-align anchor.
- **3 jsdom integration tests**: mock `getBoundingClientRect` on the button + `offsetHeight`/`offsetWidth` on the menu, open the menu, assert the rendered `style.top`/`style.maxHeight` reflect flip (426px near bottom, not 604px overflow), default-below (128px), and maxHeight cap. Proves the wiring from DOM measurement → `computeMenuPosition` → applied style.
- **2 Playwright e2e** (`tests/e2e/sidebar-kebab-viewport.spec.ts`): (1) short viewport, 12 sessions, click the last **sidebar** session kebab (scoped to `aside[aria-label="Navigation"]` so the ChatPage header kebab isn't grabbed), assert the menu bounding box stays within the viewport; (2) very short viewport (120px) → menu capped + `overflow-y: auto|scroll`, box within viewport.

---

## Key Decisions

1. **Pure-function extraction** over in-component math. jsdom doesn't compute geometry, so testing flip/clamp logic in-component requires fragile prototype mocking. The pure function is deterministic and exhaustive; the wiring is covered by the integration tests. Follows Rule 4 (maintainable) + Rule 0 (testable).
2. **Reposition on scroll/resize** rather than close-on-scroll. "Layout aware" was the explicit ask; repositioning keeps the menu anchored to its trigger. If the trigger scrolls fully off-screen the menu clamps to the viewport edge (acceptable; reviewer noted close-on-scroll as an alternative UX, non-blocking).
3. **8px viewport PAD** on all sides so the menu never hugs the screen edge — symmetric and predictable.

---

## Blockers

None.

---

## Tests Run

```
# Pure + integration
cd frontend && npx vitest run src/components/ui/KebabMenu.test.tsx
  Tests  24 passed (24)

# Kebab consumers (Sidebar, Sidebar.forcestop, ChatPage)
cd frontend && npx vitest run src/components/ui src/components/layout/Sidebar.test.tsx src/components/layout/Sidebar.forcestop.test.tsx src/pages/ChatPage.test.tsx
  Test Files  12 passed (12) | Tests  132 passed (132)

# Full chat tree
cd frontend && npx vitest run src/components/chat
  Test Files  18 passed (18) | Tests  306 passed (306)

# Lint + typecheck (incl. e2e)
cd frontend && npx eslint <changed files> && npx tsc --noEmit   # clean

# E2E (Playwright) — NOT run in sandbox: chromium can't launch (libglib missing).
# Spec is lint+typecheck clean; must run in CI with browser deps.
```

---

## Next Steps

1. Run the Playwright e2e suite in CI (with browser deps) to exercise `tests/e2e/sidebar-kebab-viewport.spec.ts`.
2. After merge + next release cut, bump talos-ops-prod to pick up the fix in production.

---

## Files Modified

- `frontend/src/components/ui/KebabMenu.tsx` — added pure `computeMenuPosition`; refactored positioning to `useLayoutEffect` + scroll/resize reposition; added `overflow-y-auto` + `maxHeight`.
- `frontend/src/components/ui/KebabMenu.test.tsx` — 9 pure-function tests + 3 integration tests + afterEach geometry cleanup + test rename.
- `frontend/tests/e2e/sidebar-kebab-viewport.spec.ts` — NEW. 2 Playwright e2e (viewport-overflow happy path + tall-menu-scroll unhappy path).
