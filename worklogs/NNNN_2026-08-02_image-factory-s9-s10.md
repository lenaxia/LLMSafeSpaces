# Worklog NNNN — Image Factory S9+S10: E2E tests + frontend

**Date:** 2026-08-02
**Scope:** S9 (e2e tests + adversarial review gate) and S10 (frontend).

## Summary

Completed the image factory with comprehensive e2e tests covering the full
build lifecycle (catalog → create → callback → ready) and failure path
(create → callback failed → rejected → blocked), plus the React frontend
for the workspace images settings tab.

## S9 — E2E tests

5 handler-level e2e tests using a full in-memory store simulating real DB
behavior (atomic transitions, coalescing, known-failure blocking):

1. **FullRoundTrip**: catalog read → POST /configs (novel dispatch) →
   callback succeeded → config ready → coalesce (second POST same selection
   → no dispatch, immediately ready)
2. **FailurePath**: POST /configs → callback failed → config rejected →
   known failure recorded → second POST blocked (422)
3. **CallbackSecurity**: wrong token → 403, cross-build token isolation,
   correct token → 204
4. **IdempotentReplay**: succeeded build + replayed failed callback →
   stays succeeded
5. **AdminGuard**: non-admin → 404, admin → 200

## S10 — Frontend

- `frontend/src/api/imageFactory.ts` — typed API client (catalog, configs,
  create)
- `frontend/src/components/settings/WorkspaceImagesTab.tsx` — settings page
  showing saved configs with status pills, extension checkboxes, base
  selector, and create-and-build form
- `frontend/src/components/settings/WorkspaceImagesTab.test.tsx` — 5 tests
  (renders configs, renders extensions, button enable/disable logic)
