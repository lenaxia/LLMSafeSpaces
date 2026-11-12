# NNNN — workers-tree header: aggregate activity spinner when ANY worker is busy (tsk-upland-marigold-masks)

Board story: tsk-upland-marigold-masks (epic "Frontend reliability mopup", project "Agent DX: reference patterns")

## The gap

The workspace view's tree carries the #1966/ruling-6 "Workers" group —
collapsed by default with a count badge. The count says how many board
workers exist, nothing about what they are doing: with the group
collapsed (its normal state), a busy worker is invisible at the exact
moment activity is the interesting fact. The per-row BusyIndicator
exists but only renders once the group is expanded.

## Changes

1. **Provider** (`frontend/src/providers/SessionActivityProvider.tsx`):
   new `useAnySessionBusy(sessionIds: string[]): boolean` — true iff
   any listed id is busy. An AGGREGATION over the same busy map
   `useIsSessionBusy` reads (SSE latches, REST seeds, the #1821
   busy-snapshot flights and leases — unchanged), never a second
   source of truth. Empty set → false.

2. **Sidebar** (`frontend/src/components/layout/Sidebar.tsx`):
   `WorkersGroup` renders the shared `BusyIndicator` (size="sm", the
   same glyph every session row uses, `aria-label="Agent working"`)
   next to the count, iff `useAnySessionBusy(worker ids)` — the ids of
   the group's own rows, so the signal mirrors the count badge's
   denominator. Deliberately NOT `useWorkspaceBusyCount`: that counts
   the whole workspace; a busy USER session must not light the
   workers spinner. Busy nested subagents do not light it either (the
   header mirrors its rows, like the row-level indicators). Test id
   `workers-group-spinner` on the wrapper.

3. **Reduced motion** (`frontend/src/styles/index.css`): the existing
   `prefers-reduced-motion: reduce` convention block (which already
   disables `.animate-unread-pulse`) gains
   `.workers-activity .animate-spin { animation: none; }` — scoped to
   the header wrapper so other spinners are untouched;
   reduced-motion users see the static glyph as an activity marker.

## Tests

- `SessionActivityProvider.anyBusy.test.tsx` (NEW, real provider, the
  house harness — mocked `useUserEventStream`, SSE-shaped
  `session.status` events): empty-set floor; any-vs-all OR semantics;
  out-of-set busy sessions never leak in; both-direction flips
  (idle→busy→idle→busy); lit while any member remains busy (OR, not
  last-write).
- `Sidebar.workers.test.tsx`: mixed busy+idle workers → spinner
  present inside the group header with the honest count 2; all idle →
  spinner absent (with a busy NON-worker session pinning the
  worker-scoping); live transitions flip the header via rerender
  without a refetch.
- `src/tests/integration/workers-group-activity.test.tsx` (NEW, review
  r1): the REAL provider + REAL Sidebar on one tree (no provider-module
  mock) — busy user stays dark, busy worker lights, idle extinguishes,
  OR while any remains. Closes the mocked-factory OR gap.
- `frontend/tests/e2e/sidebar-workers-activity.spec.ts` (NEW, review
  r1): the real shell in Chromium — sessions seeded on the listing,
  busy/idle delivered over the user SSE stream (deferred-fulfill
  route, fired at ALL parked connections — StrictMode parks two and
  the older may be aborted), asserting the collapsed header glyph:
  busy user does NOT light it (row badge = landed-proof), busy worker
  DOES (count stays 2), idle extinguishes. Mutation-verified born-RED
  (feature-flagged out → the spec fails; restored → passes).
- The four suites that `vi.mock` the provider module AND render
  Sidebar/AppShell teach their factories the new export (23 suites
  mock the module; the other 19 are ChatPage-only and unaffected);
  the rest of the suite runs against the real provider untouched.

## Test commands

- frontend: `npx vitest run` (229 files / 2608 tests, green)
- e2e: `npx playwright test tests/e2e/sidebar-workers-activity.spec.ts`
- `npm run typecheck`, `npm run lint` (0 errors; 7 pre-existing
  warnings in generated protobuf files + BoardPage, none in touched
  files)
- `make fmt-check` (clean)

## Design choice

Aggregate hook vs. reading `busySessions` directly in the Sidebar:
the context map is provider-internal; the hook keeps the Sidebar on
the public provider surface (all existing Sidebar activity reads go
through hooks) and gives the provider-level transition pin a natural
home — the task's "activity transitions flip the spinner (provider
test)".

## Residuals

- Busy WORKER-SUBTREE subagents (a worker's Task-tool children) do
  not light the header spinner — the header mirrors its own rows, the
  same denominator as the count. If field use wants subtree activity
  surfaced, the aggregation can widen to descendant ids without a
  plumbing change.
- CI note (review r1 classified it diff-unrelated, tracked
  independently): the Security-scan workflow's govulncheck job is RED
  on EVERY head incl. origin/main (GO-2026-6859, Go stdlib net/http,
  fixed in go1.26.11; the repo pins 1.26.10 in go.mod + workflows).
  Needs an owner-run toolchain bump on main — a ~15-file chore, out of
  this frontend-scoped story.
