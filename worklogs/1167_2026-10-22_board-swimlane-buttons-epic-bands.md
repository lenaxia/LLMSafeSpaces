# Worklog: Board swimlane creation buttons + collapsible epic bands (#1832 items 1-2, PR-C1)

**Date:** 2026-10-22
**Session:** PR-C1 of seven sliced #1832 tasks — (1) New Epic / New Task buttons in each project swimlane header (contextual creation; the greyed-until-dropdown global path stops being the only one); (2) collapsible epic bands inside the swimlane, tasks grouped beneath, band click highlights own tasks + dims the lane's others, the selected epic scopes New task.
**Status:** Complete

---

## Continuity note (reclaimed claim)

This task was reclaimed mid-flight: the predecessor's lease lapsed at 2026-01-45Z for tooling reasons (a hallucinated heartbeat timestamp — not work failure). Continuation decisions, per the orchestrator's disposition:

- Continued from the predecessor's commit dd380efd (full implementation + RED-FIRST tests) rather than clobbering; its uncommitted WIP (E2E_PORT-driven playwright config, two strict-TS `?? ""` mock guards) was evaluated and kept.
- A foreign process tree (predecessor's orphaned e2e run, vite on port 18517) was found touching the worktree post-claim, reported to the orchestrator, and killed on their explicit authorization; worktree integrity verified before and after (HEAD + porcelain hash unchanged by the foreign run).
- RED-FIRST was re-verified empirically by this session (below), not taken on the predecessor's word.

## Objective

Deliver #1832 items 1-2 per the charter: contextual swimlane-header creation buttons and collapsible epic bands with selection semantics, RED-FIRST tested at the component and e2e layers, no overlap with the C2/A2/D lanes (no kebab/delete, no config UI, no drawer changes), #1824 phrase-id join untouched.

## Work Completed

### Implementation (inherited dd380efd, verified + fixed by this session)

- **Item 1 — contextual creation**: each swimlane header carries `new-epic-<short_id>` / `new-task-<short_id>` buttons. The New-epic dialog was retargeted from a boolean to `createEpicTarget: string | null` (the swimlane's project, or the focus dropdown's for the global button) — the dialog names its target and the POST rides it; no dropdown selection is required anywhere. The global toolbar button remains (greyed at All-projects scope) but is no longer the only path.
- **Item 2 — epic bands**: `epicsByProject` derives per-lane epics from that project's own task rows (display inheritance) merged with the created/opened-epics bridge (`knownEpics`); each band renders title, short id, derived progress (done/total scoped to the lane's rows), paused badge, and a collapse toggle (default expanded — "tasks grouped beneath" is the at-rest view). Tasks render as compact rows beneath their band (click = task dialog, stopPropagation).
- **Selection semantics**: band click = selection (per-project, `selectedEpicByProject`); the swimlane's cards get `dimmed` (other epics) / `highlighted` (selected) props (data attrs + styles on TaskCard). Derived `selectedEpicOf` — a selection whose epic vanished from the read model reads as none, never a stale creation scope. The selected epic scopes the lane's New task (TaskDialog create mode); with no selection an epic-picker dialog opens (epics of that lane; empty lane → create-epic escape hatch). Display-only: selection never filters rows and never bleeds across swimlanes.

### Fixes by this session (the predecessor's 3 e2e reds + 1 latent test-util bug)

1. **Band selection surface moved to the header row** (`band-header-<id>`). The container-level onClick sat atop the task rows: in a real pointer click, Playwright aims at the element's geometric center — epic-2's band has exactly one task row, so the center landed on the task row (task dialog opened; selection never fired; the subsequent header-button click timed out behind the dialog overlay). Component tests never caught it because `fireEvent.click` dispatches on the node without hit-testing. The header row is now the click/double-click surface; toggle and task rows stopPropagation. Component + e2e tests retargeted to `band-header-*` (the `data-selected` assertion stays on the band container).
2. **Scope-switcher chips disambiguation completed**: the #1702 e2e suite's `getByText("E2", { exact: true })` went strict-mode-violating (epic chip + band title both say "E2") → now clicks `epic-chip-epic-epic-2` by testid (finishing the disambiguation the predecessor started for other suites).
3. **Side-columns precondition in the dim test**: Borealis's only task lives in `pending_input` (side column, collapsed by default) — `not.toHaveAttribute` on an unrendered card fails on element-not-found. The test now opens the lane's side columns and waits for the card before asserting it carries no dim.
4. **`seedBoard` mock fix (React duplicate-key warnings)**: the util's `listProjectTasks` mock returned Apollo's tasks for ANY pid ≠ proj-uuid-b, so the 3-project picker test seeded each Apollo task twice into the merged `tasks` state (duplicate React keys in the lane's lists; 20 warnings per run). Now filters by exact `t.project_id === pid`. Zero warnings, all suites green.

### RED-FIRST evidence (re-verified by this session)

- `git checkout dd380efd~1 -- <impl files>` → `npx vitest run src/pages/BoardPage.swimlane.test.tsx`: **7/7 fail** (the RED state — buttons absent, bands absent, creation unscoped).
- Restore dd380efd → 7/7 pass. Same method verified the band-header fix flips the two pointer-semantics e2e rows from red to green (18/18 before vs after at each step of the fix chain).

### Ports (us72 registry discipline)

- Nightly `PORTFWD_PORT` registry (.github/workflows/e2e-nightly.yml): 18083–18093 in use; nothing claims the Vite range.
- This lane's e2e: **E2E_PORT=18519** (distinct from the predecessor's 18517 and the 5173 default siblings may occupy). The committed playwright config makes the port env-driven (`E2E_PORT`, default 5173, `--strictPort`, `reuseExistingServer: true` only meaningful per-port) so concurrent sibling worktrees cannot latch a foreign server — CI leaves it unset.

## Validation (exact commands + outcomes)

| Command | Outcome |
| --- | --- |
| `cd frontend && npx vitest run src/pages/BoardPage` | 7 files, **73/73 passed** |
| `cd frontend && npx vitest run` (full) | 211 files, **2383/2383 passed** |
| `cd frontend && npm run typecheck` | clean (tsc --noEmit, 0 errors) |
| `cd frontend && npm run lint` | 0 errors, 6 pre-existing warnings (generated `src/abi/**_pb.ts` unused eslint-disable directives — untouched by this PR) |
| `cd frontend && E2E_PORT=18519 npx playwright test tests/e2e/board.spec.ts` | **18/18 passed** (~40s) |
| RED-state check (impl reverted to dd380efd~1) | swimlane suite **7/7 failed** (RED-FIRST proven) |

## Assumptions

1. **Band selection is per-project and display-only** (never filters, never crosses lanes) — the charter's "clicking an epic highlights its tasks and dims others" scoped to the swimlane; other lanes' cards untouched (pinned by both layers).
2. **Derived epics carry display-grade metadata** (`epic-<id8>` short id, empty outcome, active status) — the board's task rows already surface `epic_name`; only real epics from the list endpoint / created this session carry authoritative metadata. Progress is per-lane (a same-id epic spanning projects counts each lane's rows).
3. **The epic picker is the no-selection New-task path** (not a dead/greyed button) — a task requires its epic (board API), so the picker (with empty-lane create-epic escape hatch) is the honest contextual path.
4. **Default expanded bands** — "tasks grouped beneath" is the at-rest view; collapse is opt-in and un-persisted (view state only).

## Residual flags

- The e2e `vite` proxy logs ECONNREFUSED noise for unmocked endpoints (orgs, image-factory, timezone, sessions) — pre-existing board-spec behavior, not this PR.
- The 6 eslint warnings in generated abi files pre-date this branch.
- Band double-click opens the epic drawer (pre-existing drawer, unchanged — D-lane owns drawer work).
- `reuseExistingServer: true` remains in the playwright webServer for CI parity; concurrent local lanes must still pin distinct E2E_PORTs (strictPort makes a collision loud, not silent).

## Mutation-verify suggestions

- Flip `dimmed`/`highlighted` prop wiring in StageCell → the dim/highlight e2e + component rows go red.
- Drop `stopPropagation` on the band toggle → collapse clicks also select (the header-row rows catch it).
- Revert the `seedBoard` exact-pid filter → the picker test re-emits 20 duplicate-key warnings (a canary, not a failure — worth pinning if warnings are ever gated).
