# Worklog 0684 — NewWorkspaceSplitButton: viewport-aware dropdown positioning

**Date:** 2026-08-05
**Scope:** The image-list dropdown in `NewWorkspaceSplitButton` overflowed the
viewport edge — it was hardcoded to `absolute right-0 top-full` with no
position awareness.

## Summary

The split button's popup menu used `absolute right-0 top-full`, anchored to
the button's container. When the button sat near the right or bottom edge of
the viewport (e.g. the chat sidebar), the popup rendered partly off-screen
and was clipped or unreachable. The fix reuses the existing
`computeMenuPosition` helper from `KebabMenu` (which already handles
flip-above-when-no-room-below, horizontal clamp, and height cap) and ports
the popup to `document.body` via `createPortal` so it is never clipped by an
ancestor's overflow/stacking context.

## Changes

- `NewWorkspaceSplitButton.tsx`:
  - Replaced `absolute right-0 top-full` with `createPortal(..., document.body)`
    + `fixed` positioning driven by `computeMenuPosition(btnRect, menuSize,
    viewport, "right")`.
  - Added `triggerRef` (on the ▼ button) and `menuRef` (on the popup) for
    measurement and outside-click detection.
  - Added `useLayoutEffect` (pre-paint) + `useEffect` (post-paint + scroll/
    resize) repositioning, mirroring the proven `KebabMenu` pattern.
  - Outside-click handler now checks both the trigger and the portal'd menu.

## Why reuse `computeMenuPosition`

The helper is already exported, unit-tested (`KebabMenu.test.tsx`), and
handles every case: flip above, horizontal clamp to viewport pad, height cap
with scroll. Duplicating that logic would drift; reusing it keeps one source
of truth for viewport-aware menu geometry.

## Validation

- `npx tsc --noEmit` — clean.
- `npx vitest run` — 1520 tests pass (142 files), including the 5 existing
  `NewWorkspaceSplitButton` tests (portal renders fine under jsdom).
