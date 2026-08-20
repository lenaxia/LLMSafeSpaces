# Worklog: native first-page history pagination (#971)

**Date:** 2026-08-20
**Session:** Issue #971 — session load ~15s on large transcripts; GetHistory fetched and parsed the FULL history for every page. Branch `fix/971-history-pagination`.
**Status:** Complete

---

## Objective

Cut session-load latency from ~15s to ~1–2s on large sessions. Measured
on the live pod (session ses_fe5364736ffe..., 452 messages, 2.4MB):
opencode serves the full transcript in 0.096s but the API's
fetch+stream-decode+translate+slice cost ~1.8s per page; the native
`?limit=N` fetch serves a page in 0.026s. ~95% of the hot-path latency
was decoding messages that were about to be discarded.

## Live wire-shape verification (opencode 1.18.10, on the real pod)

- `?limit=N` → returns the NEWEST N messages, ascending order. ✓ usable.
- `?before=` → 400 BadRequest for every shape probed (int, epoch-ms,
  ISO8601, msg-id). ✗ unusable.
- `?cursor=` → silently ignored (returns newest N regardless). ✗
  unusable.
- Conclusion: first page can go native; back-pagination cannot.

## Work Completed

- `pkg/agent/adapter.go`: `GetHistoryPage(ctx, uid, ws, ses, limit)`
  added to the Adapter interface (implementations without native
  pagination may return the full transcript; callers slice).
- `pkg/agent/opencode/adapter.go`: `GetHistory` refactored into
  `getHistory(..., limit)`; page path appends `?limit=N`. Verified wire
  shape live (above).
- `api/internal/handlers/proxy_handlers.go` GetHistory adapter path:
  first page (no `before` cursor) → `GetHistoryPage(limit)`; older
  pages keep the full-fetch + slice (the only correct back-pagination
  on 1.18.10). Optimistic cursor: a full native page (len == limit)
  emits the page's oldest id as X-Next-Cursor — the native fetch hides
  whether older messages exist, and a spurious cursor costs one
  back-page fetch that returns empty + no further cursor.
- fakes updated (pkg/agent fakeAdapter, handlers mockAdapter with a
  pagedCalls counter).

## Key Decisions

- **First-page-only native pagination**: the session-load hot path is
  the first page; back-pages (user scroll) are rare and keep the
  existing correct behavior. Avoids guessing at unusable cursor params.
- **Optimistic cursor on full pages** rather than a second count fetch:
  one wasted request in the exactly-limit case vs an extra round-trip
  on every page.
- Interface extension (not a separate method set): callers already hold
  `agent.Adapter`; one seam, one fake to update.

## Assumptions (validated)

1. `?limit=N` returns the newest N ascending — verified live on the
   452-message session (ids/timestamps checked end-to-end).
2. The `X-Next-Cursor`/`before` contract is preserved — the existing
   pagination tests (proxy_history_pagination_test.go, adapter path
   tests) pass unchanged; the back-page e2e pins the fallback.
3. opencode's limit does not count non-displayable messages the way the
   API's displayable-only count does — page sizes may vary by a few
   messages vs the old slicing; the frontend renders pages of variable
   size already (short-session case pinned).

## Tests

- TestE2E_GetHistory_FirstPage_UsesNativeLimit: 120-msg backend; first
  page hits `?limit=50` exactly ONCE, never the unbounded URL; newest
  50 ascending; cursor = oldest of the page.
- TestE2E_GetHistory_BackPage_FallsBackToFull: `before=` keeps the
  full-fetch path; page strictly older than the cursor; next cursor.
- TestE2E_GetHistory_ShortSession_NoCursorHeader: page < limit → no
  cursor.
- All pre-existing GetHistory tests unchanged and green; full handlers
  suite green (99s); pkg/agent suites green.

## Files Modified

- pkg/agent/adapter.go (+adapter_test fake)
- pkg/agent/opencode/adapter.go
- api/internal/handlers/proxy_handlers.go
- api/internal/handlers/adapter_path_test.go (3 e2e tests)
- api/internal/handlers/mock_adapter_test.go
