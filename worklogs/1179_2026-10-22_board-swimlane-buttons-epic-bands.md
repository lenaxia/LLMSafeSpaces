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

---

# Round 2 — rebase + review-findings rework (2026-10-22, second session)

**Context:** the orchestrator's 03:35Z adjudication approved the substance but required a rebase; this session rebased, re-gated, pushed — and the formal review automation (github-actions, 04:55:14Z on the rebased head) returned CHANGES_REQUESTED with three findings. Per the verdict-disagreement protocol the lane orchestrator (meek-moonlight-dines) was messaged and AUTHORIZED in-turn findings work (option a); the "rebase+gates only" mandate was amended accordingly.

## Rebase (before the findings work)

- `f26daea7` → `7f1565f2` onto `origin/main` tip `e6764841` (past the renumber-bot commit). CHANGELOG content conflict resolved by UNION (main's #1814 entry + our #1832 entry; diff vs main = pure +21-line addition, 0 deletions). No worklog entries deleted; the branch worklog auto-renumbered `NNNN_` → `1167_` by the post-rewrite hook.
- Gates on the rebased head: BoardPage vitest 73/73, typecheck clean, lint 0 err/6 pre-existing warnings, e2e 18/18 (E2E_PORT=18519), full vitest 2383/2383, `make repolint` all-pass.
- Platform anomaly: GitHub recorded the PushEvent for `7f1565f2` (04:40:30Z) but created ZERO workflow runs (a sibling branch's push at 04:39:17Z fired its full suite instantly). Re-triggered with an empty commit `6c284c99` (tree identical, 0-line diff vs `7f1565f2`); the suite then fired normally. Rebase evidence noted on the PR (issuecomment-6332869799).

## The three findings (verified, not taken on faith)

1. **Zero-task epics invisible after reload / unselectable** — verified: `GET /me/projects/:id/epics` exists at this merge base (router.go → `ListProjectEpics` → `pkg/board.Service.ListEpics`) and already existed at the pre-rebase base; the frontend client had no list method and `BoardPage` inferred epic existence from task rows (the stale "seam does not exist yet" comment was wrong even before the rebase).
2. **Filtered lanes showed a false "No tasks yet."** — verified: bands derived from unfiltered tasks while band rows came from the filtered lane.
3. **Happy-path-only coverage** — verified: no unhappy-path unit/e2e rows for the new creation flows; no test failed when the `fix:` commit's band-header hit-surface or `seedBoard` exact-pid changes were reverted.

## RED-FIRST evidence (this round)

- Unit: 4 rows written red first (zero-task server epic band/picker/create; epics-read-failure = load error; filtered-vs-empty band distinction ×2) — `npx vitest run src/pages/BoardPage.swimlane.test.tsx` → **4 failed | 12 passed** at the exact expected rows pre-implementation, **16/16** after.
- E2E: the reload row (`git stash` of the two impl files, old code) — `a zero-task epic created this session survives a reload` **failed** at the post-reload band assert (the finding's deterministic repro); the 3 pinning rows passed; after implementation **4/4**.
- Reversion guards now exist: band container/task-row/toggle clicks never select (a container-level onClick regression goes red); the 3-project row asserts each lane renders only its own rows (the exact-pid mock regression goes red).

## Implementation (this round)

- `board.ts`: `listProjectEpics` wired to the existing GET seam (`{ epics }` envelope, same client shape as `listProjectTasks`).
- `BoardPage.tsx`: `useQueries` per project (`["board","epics",id]`, 30s refetch, same enabled gate as tasks; invalidated with every `["board"]` invalidation). `epicsByProject` = task-derived fallback → server (authoritative) → session `knownEpics`, merged by id per lane — one derivation feeding bands, picker, AND chips (the r1 duplicate derivations collapsed; chips deduped by id with server/known precedence for real `epc-` short ids). Epics read failures extend the r2 not-emptiness posture (`board-read-error` + Retry). Bands distinguish `band-filtered-empty-*` ("No tasks match the current filter.") from `band-empty-*` ("No tasks yet.") via the lane's unfiltered per-epic row counts. Stale ListEpics comment removed.
- e2e mocks: state-driven projects list (third-project row), GET+POST `/me/projects/*/epics` (created epics become server truth), per-project server epic derivation, zero-task-epic task creation (project rides the epic list), `failEpicCreate`/`failTaskCreate` 500 levers.

## Validation (round 2 — exact commands + outcomes)

| Command | Outcome |
| --- | --- |
| `cd frontend && npx vitest run src/pages/BoardPage.swimlane.test.tsx` (pre-impl) | **4 failed \| 12 passed** (RED at the exact finding rows) |
| `cd frontend && E2E_PORT=18519 npx playwright test -g "#1832 r2"` (impl stashed) | reload row **failed**, 3 pinning rows passed (RED) |
| `cd frontend && npx vitest run src/pages/BoardPage` | **82/82 passed** |
| `cd frontend && E2E_PORT=18519 npx playwright test tests/e2e/board.spec.ts` (full) | **22/22 passed** (18 prior + 4 new; the #1702 chip row now targets the server short_id `epic-chip-epc-epic2000`) |
| `cd frontend && npm run typecheck` | clean (0 errors) |
| `cd frontend && npm run lint` | 0 errors, 6 pre-existing generated-abi warnings |
| `cd frontend && npx vitest run` (full) | 211 files, **2392/2392 passed** |
| `make repolint` | all checks passed |

## Residual flags (round 2)

- The scope-switcher epic chips dedupe by id with server precedence; a same-id epic spanning projects shows ONE chip (pre-r1 behavior restored after the per-lane list briefly duplicated it — caught by the r1 drawer rows, fixed in-turn).
- Mid-load: a zero-task epic's band appears when its list response lands (task-derived bands render immediately); no skeleton was added (out of findings scope).
- Keyboard access to band selection (header row is pointer-only, non-blocking per the review) — left to the a11y lane.

## Assumptions (round 2)

1. The server epic list is authoritative for existence + progress; task-row derivation is a fallback, not a second source of truth.
2. Creation POST failures keep dialogs open with fields preserved — pinned by test at both layers (was already the behavior; now guarded).

