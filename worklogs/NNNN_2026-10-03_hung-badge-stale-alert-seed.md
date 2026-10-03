# Worklog: Hung badge latched forever by stale persisted alerts (#998 follow-up)

**Date:** 2026-10-03
**Session:** User reported a phantom "active worker session" indicator on chat/dda717cb…/ses_f10ab3208ffe… despite no visible activity. Diagnosed live, fixed frontend-side.
**Status:** Complete

---

## Objective

Explain and fix the stuck busy/hung indicator on the orchestrator session page after the busy-truth cluster work.

---

## Work Completed

### Live diagnosis (workspace dda717cb, worker ses_f000ed167)

- agentd statusz (authoritative): ALL sessions idle, including the worker `ses_f000ed167` ("goKore Worker: album injection seam"). opencode 5.4% CPU — not wedged. API sessions list: idle.
- Persisted `session_hung` alerts for the worker existed from 03:45–04:59 (worker genuinely hung 04:06→~05:15; pod recreated 05:15 → everything idle server-side with no browser attached).
- Frontend `SessionActivityProvider.seedBusy` (D6 #998 history surface) seeded `hungWorkspaces` from the mere EXISTENCE of persisted alerts. The alerts feed is append-only 24h history (no resolution state; `sessionalerts` service has no clear path). The only badge clear path is a live `session.status idle` SSE event — which never fires for a session already idle at page load. Result: every page load after recovery latches the badge permanently. The shipped test literally pinned this ("idle session + alert → hung").

### Fix

- `seedBusy` now builds the workspace's currently-busy session-id set (already iterating that list) and seeds hung ONLY for alerts whose `sessionId` is currently busy. Stale history (recovered/idle/deleted session) no longer badges; genuine recovery-on-reconnect still works (a really-hung session is still busy in REST, and the live SSE path re-alerts each cooldown cycle anyway).
- Tests: flipped the buggy pin into the stale-history regression (two stale alerts + idle session → healthy), added true-recovery (busy session + alert → hung) and deleted-session cases; kept empty/fetch-failure cases.

---

## Key Decisions

1. Frontend gate, not backend resolution state. The persisted feed is deliberately history (24h retention, append-only). Teaching it resolution (clear-on-idle writes or a resolvedAt column) is a bigger surface; the client already holds the truth it needs (current busy set) in the same function. Aligns with the busy-truth cluster's direction: SSE is the authority, REST seeds must not contradict live state.
2. No loss of real signal: the live SSE `workspace.alert` path still sets the badge immediately while the stream is attached, and re-fires per cooldown for still-hung sessions; a reconnect mid-hung recovers via the gated seed (session still busy).
3. Left alone: `SessionAuthority.test.tsx` (untracked, someone's in-flight WIP, 7 failing tests in the working tree, absent on main) and the pre-existing `fold.ts` typecheck errors + 19 broken test files — all reproduced on clean main, out of scope.

### Review round 1 (probe-confirmed latch paths — commit 3)

- Late-resolving alerts fetch re-latched after an idle SSE clear → the gate now reads the LIVE busy set (`busySessionsRef`, SSE-truthed) instead of the seed-time snapshot.
- Reconnect kept a recovered session's badge → `onReconnect` clears hung state; the gated re-seed re-adds only genuinely-busy sessions.
- `workspace.phase` non-active now clears the workspace's hung entry.
- Gate asserts `alert === "session_hung"`.
- `WorkflowsPage` amber dot: same still-busy gate (was mere-existence).
- New e2e (`hung-badge.spec.ts`): stale-history absent / true-recovery present (REST-only; the seed path needs no SSE transport).

### Review round 3 (sibling terminal paths — commit 4)

- `session.status` aborted/deleted, `agent_died`, and `resync` now clear the badge (a stopped/deleted/dead session can never emit the idle clear; resync may have dropped it) — via a shared `dropHungWorkspace` helper also used by the idle and phase clears.
- The vacuous reconnect re-seed test became genuine (delayed resolution + post-reconnect cache update); the WorkflowsPage stale test settles its queries before the negative.
- New clear-path tests: aborted/deleted (`it.each`), agent_died, phase, resync.

### Review round 4 (polish — commit 5)

- Fixed the twice-flagged dangling `%s` in the belt assertion (pass `scanTime`).
- `busySessionsRef` bound comment corrected: the race's victim just went idle, so no further idle clears it — the actual bounds are reconnect/resync/phase-change.
- Worklog/PR body brought up to date with the full series (this entry).
- Robustness notes accepted as follow-ups: workspace-granularity idle clear can drop a badge while a sibling session is still hung (self-heals ≤ cooldown); resync/reconnect clears can unbadge a still-hung workspace ≤ cooldown. Both are missing-badge-bounded, never latches. The stacked #1618 (server-side `resolved_at`) addresses the class.

**Tests at this head:** alerts file 13/13, provider 94, WorkflowsPage 10, e2e both directions; full suite 178 files green; `go vet`, lint 0 issues.

---

## Blockers

None.

---

## Tests Run

- `npx vitest run src/providers/SessionActivityProvider.alerts.test.tsx src/providers/SessionActivityProvider.test.tsx` — 99/99 pass (incl. 5 alerts cases: true-recovery, stale-history, deleted-session, empty, fetch-failure).
- Full `npx vitest run`: my delta green; the 7 remaining failures are the untracked WIP `SessionAuthority.test.tsx`; 19 other broken files + `fold.ts` typecheck errors reproduce identically on clean main (verified in a worktree).
- Live-cluster verification of the diagnosis: statusz all-idle + stale alerts in the persisted feed at 06:03.

---

## Next Steps

- PR → review → merge → release train.
- Consider (follow-up, backend): `sessionalerts` could expose `resolvedAt`/active filtering if the history surface grows other consumers; not needed for this fix.

---

## Files Modified

- frontend/src/providers/SessionActivityProvider.tsx — gated hung-history seed, live-ref gate, terminal-event clears, alert-type assert
- frontend/src/providers/SessionActivityProvider.alerts.test.tsx — 13 cases: resolved/stale/deleted/empty/failure, late-fetch race, reconnect drop + re-seed, aborted/deleted, agent_died, phase, resync
- frontend/src/pages/WorkflowsPage.tsx + WorkflowsPage.test.tsx — still-busy gate + genuine stale test
- frontend/tests/e2e/hung-badge.spec.ts — both directions (REST-only)
- pkg/utilities/json_duplicate_keys_test.go — belt 1.5s→4s (repeat CI/release flake) + %s fix
