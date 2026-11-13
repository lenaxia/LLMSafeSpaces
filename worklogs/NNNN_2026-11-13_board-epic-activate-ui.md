# Worklog: Per-Epic Activate/Pause on the Sprint Board Band

**Date:** 2026-11-13
**Session:** tsk-secluded-sitar-squeals (epic "Frontend reliability mopup", project "Agent DX: reference patterns")
**Status:** Complete — pushed for AI review

---

## Objective

The owner ask 2026-11-13: the frontend Sprint Board had no way to manually activate an epic — activation required API/MCP calls (or the undiscoverable double-click → EpicDrawer path). Add an activate/deactivate affordance on each epic so the owner can drive epic state directly from the board UI. Frontend scope plus the minimal API-client fix; no controller/handler changes.

## Work Completed

### 1. Recon — the surface was already half-there

- `boardApi` already exposes `activateEpic` / `pauseEpic` (POST `/me/epics/:id/activate|pause`) — no client addition needed for the routes themselves.
- The EpicDrawer already had Activate/Pause buttons (`lifecycleMutation`) — but only reachable via a double-click on the band, invisible to the owner. The gap was the ROW-level affordance.
- Backend allowed-map (`pkg/board/service.go` epicLifecycle): active ⟸ {draft, paused, completed, archived}; paused ⟸ {active}. The drawer's rule (`status !== "active"` → Activate, else Pause) is exactly the engine's map — reused verbatim for the kebab item.

### 2. The latent wire-shape bug found during recon (fixed)

- The handler's activate 200 is the `{epic, initial_ready_set}` WRAPPER (hurl `board.hurl` pins it; the TS SDK models `BoardEpicActivateResponse`), while pause/archive/unarchive return the bare epic. The frontend client declared `activateEpic(): Promise<BoardEpic>` — so a production activate fed the wrapper object into `rememberEpic`/`setEpicDrawer`, blanking the drawer's title/status after every real activate. The test mocks mocked the bare epic (the wrong shape), so the suite was green over the bug.
- Fix: `BoardEpicActivateResponse` type in `api/board.ts`; both call sites unwrap `.epic`. The drawer test's activate mock tightened to the wrapper — born-RED on the pre-fix BoardPage (`epic-status` unreachable: the drawer rendered the wrapper), green post-fix. New client pin: the wrapper passes through verbatim.

### 3. The band affordance (`BoardPage.tsx`)

- `handleEpicLifecycle(epic, action)` mirrors `handleProjectLifecycle` (the project kebab's established shape): server-confirmed write → `rememberEpic(updated)` flips the band immediately (knownEpics supersedes until the invalidated `["board"]` read lands — the create-epic flow's seam), page notice on success (names the epic, states the effect), `pageError` on failure (page-level `BoardErrorNotice` — the drawer is not open, so `drawerError` would be invisible). No optimistic flip → nothing to roll back on error; the failure path renders and state stays as-was (pinned).
- Epic kebab gains the lifecycle item ABOVE Delete: "Activate epic" (non-active) / "Pause epic" (active). No confirmation — activation/pause is not destructive and the codebase convention (the drawer buttons) never confirms.
- A `draft` badge joins `paused` on the band (same Badge pattern) so the inactive state is legible without opening anything.

### 4. Pins (TDD — born-RED verified, 3/3 + the drawer-unwrap pin)

`BoardPage.actions.test.tsx`, server-truth epics via the `#1832` r2 read (`mockListProjectEpics`, not task-derived bands):

- **Activate on a draft epic**: band shows `draft`, kebab offers "Activate epic", click fires `activateEpic("epc-1c1c1111")` (short id preferred — the `epc-` convention), badge disappears, notice renders "LC: activated", re-opened kebab offers "Pause epic" (state truly flipped).
- **Pause on an active epic**: fires `pauseEpic`, `paused` badge lands, notice renders "LC: paused".
- **Failure rollback**: `activateEpic` rejects 409 `epic_state` (the cross-epic-deps-into-inactive-epics guard) → page alert renders with `data-board-error="epic_state"`, the draft badge survives, the kebab still offers "Activate epic".
- **Drawer unwrap** (the latent-bug pin): the existing drawer activate test now mocks the wrapper — RED pre-fix (drawer blanked), GREEN post-fix.
- `board.test.ts`: activate's 200 wrapper returned verbatim (`epic` + `initial_ready_set`).

## Verification

- Full frontend suite: **2609/2609 green (228 files)** on the worktree; `tsc --noEmit` clean; `eslint` 0 errors on touched files (one pre-existing exhaustive-deps warning in `BoardPage.tsx:779` — `openEpicDrawer`, not this lane's code; my hook carries the dep).
- Born-RED: 3/3 band-kebab pins + the drawer-unwrap pin fail on the pre-fix `BoardPage.tsx` (`git checkout` swap, re-run, restore).
- `make fmt-check` green.

## Key Decisions

- **Kebab item, not an inline button**: the epic kebab already exists on every band (Delete); the project kebab's lifecycle items are the established pattern for exactly this. An inline button would compete with the row's click-to-select.
- **Separate handler over reusing `lifecycleMutation`**: the mutation's `onSuccess`/`onError` target the DRAWER (setEpicDrawer/drawerError — wrong surfaces when the drawer is closed). `handleEpicLifecycle` follows `handleProjectLifecycle`'s page-level notice/error shape instead; the mutation stays for the drawer path (its tests untouched in behavior).
- **Server-confirmed update, not optimistic**: the story allows "optimistic with rollback OR refetch per existing conventions". The existing conventions (create-epic, project lifecycle) are server-confirmed `rememberEpic` + invalidate — the flip is immediate (response row) and the refetch is the follow-up truth. On failure there is no local flip to roll back — the error renders, state stays.
- **Client returns the wrapper (typed), call sites unwrap**: matches the wire, the hurl contract, and the SDK's `BoardEpicActivateResponse` — and makes the unwrap an auditable line at each call site.

## Blockers

- None. AI review per PROCESS ruling (never self-merge; orchestrator merges).
