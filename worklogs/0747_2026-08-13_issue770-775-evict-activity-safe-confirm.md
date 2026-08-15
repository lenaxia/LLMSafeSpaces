# Worklog: #770/#775 — eviction by real activity + safeConfirm fail-closed

**Date:** 2026-08-13
**Issues fixed:** #770, #775
**PR:** #813
**Follow-up:** #814 (ConfirmDialog migration)

---

## Context

Two bugs causing silent data loss for users, identified during the
triage gap analysis while waiting for PR #810.

## #770 — wrong workspace evicted on max-active limit

**Root cause:** `enforceMaxActiveWorkspaces` sorted evictable workspaces
by DB `UpdatedAt` (bumped by ANY row mutation — background ops, phase
changes, credential refreshes) instead of `LastActivityAt` (the CRD
annotation written by `ActivityTracker` only on real user interaction,
every 60s).

**Impact:** An actively-used workspace with an old `UpdatedAt` could be
auto-suspended while a stale workspace with a recently-bumped `UpdatedAt`
stayed running.

**Fix:** `fetchUserWorkspaceStates` extracts both `Phase` and
`LastActivityAt` from the same CRD list that was already being fetched
— zero additional API calls. The sort now uses `LastActivityAt` with
`UpdatedAt` fallback for pre-US-23.3 workspaces.

**Data flow verified:**
- `ActivityTracker.Record()` → in-memory map → `flushOne()` patches CRD
  annotation every 60s
- `workspace_service.go:1680` writes annotation on Resume
- `GetLastActivityAt()` reads annotation with `Status.LastActivityAt`
  fallback

## #775 — window.confirm fail-open causes data loss

**Root cause:** Two call sites (`ChatPage.tsx`, `Sidebar.tsx`) wrapped
`window.confirm()` in try/catch where the catch block **proceeded with
the destructive action**. In sandboxed iframes or permissions-restricted
contexts, `window.confirm` throws → catch fires → session/workspace
deleted without confirmation.

**Fix:** `safeConfirm` utility that fails **closed** (returns `false` on
exception). Replaced all 14 `confirm()` call sites across 10 files for
consistency. `ConfirmDialog` migration deferred to #814.

## CI fix

While iterating, discovered the review bot was failing with "Author
identity unknown" because `persist-credentials: false` leaves no git
identity but the opencode action runs git commands internally. Added
`git config` step to all 4 workflows that use the opencode action
(pr-review, ai-comment, issue-opened, renovate-analysis).

## Tests

- `TestEnforceMaxActive_EvictsByLastActivityAt_NotUpdatedAt` (Go)
- `TestEnforceMaxActive_MixedFleet_AnnotatedFallback` (Go)
- `safeConfirm` unit tests (4 cases)
- Updated ChatPage + Sidebar tests to assert fail-closed behavior
- All 1600 frontend tests pass, all 13 Go max_active tests pass
