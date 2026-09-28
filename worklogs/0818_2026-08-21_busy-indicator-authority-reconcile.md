# Worklog: Busy-indicator truthfulness — authority reconcile on reconnect/resync/focus

**Date:** 2026-08-21
**Branch:** `feat/998-d6-escalation`
**Status:** Complete

---

## Objective

The frontend busy indicator lied after any missed SSE transition window. Live
incident (this session): API replica `llmsafespaces-api-6bdb8cc468-b5vp9` was
liveness-killed at 06:07:54Z mid-turn; the browser reconnected, and workspace
`946a442f` session `ses_fe23fb42affe` rendered idle while a `go test` turn ran
in the pod (verified via statusz + live `/proc` inspection — tool part
`status:"running"`, ~3 min elapsed). Workspace `42ae0489` was genuinely idle
(the indicator was right there; the earlier "12 min message vs <2 m badge"
anomaly is the documented first-seen anchor at `ChatPage.tsx:724-730`).

Root cause: busy state is transition-driven (`session.status` SSE events) with
a reseed that (a) wiped all busy state on reconnect — guessing idle — and
(b) re-seeded from the stale react-query sessions cache, because nothing
invalidated `["sessions", ws]` mid-turn.

## Design lineage (why this shape)

Three prior generations of stored-busy failed (reviewed in git/worklogs):
in-memory activeSess (0309, OOMKill orphan → eternal 409), the same state in
Redis (Epic 45 — #792 stuck-busy returned because the *writer* missed events:
#805 scanner cap), and ground-truth-on-read (#795 `GetAuthoritativeActiveSessions`
via agentd statusz — the surviving pattern; #892 D2 closed its last circular
hop by resetting busy flags at opencode generation change). Principle earned:
**the live agent process is the authority; stored state is a cache; reads
reconcile against ground truth.** This change adds the missing wire — the
frontend consults the authority at the moments it knows its event-derived
state is invalid — and explicitly does NOT add new stored busy state.

## Changes

### API (`proxy_connections.go`, `router.go`, `pkg/types`, openapi, SDKs)

1. `GetAuthoritativeActiveSessionsWithSource` — provenance alongside the set:
   `statusz` (live ground truth), `no_pod` (non-Active/no PodIP — empty is
   CERTAIN), `unverified` (read failed — best-effort set, must not seed).
   Legacy wrapper keeps the old signature for `/sessions` enrichment.
2. **Zombie-import fix**: non-Active workspaces previously returned the
   Redis fallback set verbatim — re-importing busy orphaned by the pod's
   death (turn running at suspend never emits idle) as confident "active".
   Now returns empty + `no_pod`: no pod ⇒ no turn, by construction. The old
   `FallbackOnNotReady` test asserted the buggy behavior; rewritten.
3. `/sessions/active` response gains `source` (omitempty — additive; fixtures
   regenerated, contract test extended, openapi + Go/TS SDKs updated).

### Frontend (`SessionActivityProvider.tsx`, `ChatPage.tsx`, api client)

4. `onReconnect` no longer wipes `busySessions` (the wipe was the incident's
   mechanism — every reconnect guessed idle). Instead triggers an authority
   fetch per known workspace; `resync` (broker backpressure drop) does the
   same — the drop-without-disconnect window.
5. `reconcileBusyFromAuthority`: on fresh `["sessions-active", ws]` data —
   replace that workspace's busy entries, EXCEPT sessions whose last busy SSE
   event arrived within a 2 s grace of the snapshot read (the read-in-flight
   merge race: turn starts while statusz is being read). `unverified` reads
   hold prior state (never confident-idle on a failed read). Fetches are
   throttled per workspace (5 s floor).
6. `useSessionAuthority` hook mounts the query in ChatPage; react-query's
   default `refetchOnWindowFocus` (NOT disabled by the app's
   QueryClientProvider — verified by a test driving real `visibilitychange`
   events with the production staleTime) provides the tab-sleep trigger with
   zero custom wiring.

### Outbox concurrent-completion fix (flake → real bug)

`TestStress_AmbiguityStormMultiReplica` failed under full-suite CPU load:
"OnDelivered fired 2 times", sends exactly 1. Mechanism: worker A stalls past
`LockTTL` mid-verify; B acquires the expired lock; both hold the same `vals`;
both verifiers return Delivered; A's `LRem` removes 1, B's removes 0 — but the
count was ignored and the hook fired anyway. Fix: `verifyOne`'s Delivered
branch fires `onDelivered` only when its `LRem` actually removed the entry
(the remover-of-record is the hook-firer-of-record). TDD:
`TestVerifying_ConcurrentCompletionFiresHookOnce` (barrier-interleaved dual
`verifyOne`) fails 2-fires pre-fix, passes post-fix; `-race -count=10` clean.

### Test-fixture repairs on this branch's HEAD

- `Sidebar.test.tsx` (29 failures): branch commit 96df62d3 added
  `useWorkspaceHung` to `Sidebar.tsx:370` without updating the provider mock.
- 15 ChatPage test files: added `useSessionAuthority: () => {}` to provider
  mocks.

## Validation

- Validated blocking assumptions: focus-refetch fires (test #9 in
  SessionAuthority.test.tsx); non-Active zombie re-import (Go, rewritten);
  replace/merge/hold semantics (8 provider tests incl. the exact 946a
  scenario: reconnect mid-turn converges to busy; failed read holds).
- Full suites: `go test ./... -count=1` clean (incl. outbox under load);
  frontend 154 files / 1703 tests green; `tsc --noEmit` clean; eslint at
  baseline parity (35 preexisting errors, 0 new — the purity-rule hit on
  `Date.now()` in the SSE callback was fixed structurally via a module-scope
  `recordSessionEvent` helper); `go vet`/`gofmt` clean.

## Not done (deliberate)

- 60 s idle-tab poll — YAGNI; focus/resync/reconnect cover observed windows.
- Server-side statusz read coalescing — current fan-out equals today's.
- The API livez root cause (why the replica was killed) — separate workstream.
- G7-style live-cluster drill (kill replica mid-turn, watch convergence) —
  required before calling the incident class closed; not executable here.
