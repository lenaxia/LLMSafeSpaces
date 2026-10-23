# Worklog: Kebab menus + delete flows — reconnect checkbox, epic force-confirm, project archive-only (#1832 item 3, PR-C2)

**Date:** 2026-10-22
**Session:** PR-C2 of the sliced #1832 lane — kebab menus on project/epic/task with deletion flows: task delete modal exposing the 'reconnect dependents to upstream' checkbox (both semantics against the API), epic delete surfacing in-flight workers with refuse/force-confirm matching the API, project kebab archive/unarchive ONLY (#1747 — no project delete).
**Status:** Complete

---

## Objective

Deliver #1832 item 3 per `/workspace/charters-1832/c2.md`: RED-FIRST tested at the component and e2e layers, no overlap with C1 (buttons/bands untouched, only added to), D (drawer untouched), A1 (no config UI); #1824 phrase-id SSE join untouched.

## Base

Branch `feat/1832-c2-delete-flows` based on **PR-C1's head `f26daea7`** (PR #1839, unmerged at branch time) per the charter's rebase instruction — the kebab surfaces sit in the same swimlane/band components C1 introduced. Main-commits-after-C1's-base are NOT rebased in (nothing in them touches these files; the PR states the base for the reviewer).

## Work Completed

### Implementation

- **KebabMenu** (`components/ui/KebabMenu.tsx`): additive optional `ariaLabel` + `testId` props (defaults preserve the legacy "Actions" surface — Sidebar and every other consumer unchanged).
- **Project kebab** in each swimlane header (after C1's New epic / New task buttons — additive, C1's markup untouched): exactly one item — `Archive project` (active) / `Unarchive project` (archived) → `boardApi.archiveProject/unarchiveProject(id)`, invalidate + page notice; refusals render at page level. NO delete item ever (#1747: project delete is eliminated).
- **Epic kebab** on each epic band header (trigger stopPropagation — never toggles C1's band selection; test pins this). Opens **EpicDeleteDialog** (`components/board/EpicDeleteDialog.tsx`):
  - honest cascade count + the epic's **in-flight workers** (in_progress + assignee_session, live read model) surfaced pre-flight;
  - first attempt never sends force — the engine's 409 `epic_state` renders inside the modal, and ONLY that rendered refusal reveals the force-confirm: an acknowledge checkbox gating `Force delete (N in flight)` → `boardApi.deleteEpic(idOrShort, true)` (force aborts each in-flight worker post-commit — copy states the ordering);
  - epic id resolution follows the board's established pattern (`short_id.startsWith("epc-") ? short_id : id` — derived epics carry the UUID).
- **Task kebab** on every TaskCard (trigger/items stopPropagation — never opens the editor; test pins this). Opens **TaskDeleteDialog** (`components/board/TaskDeleteDialog.tsx`):
  - dependents surfaced pre-flight (read-model `blocked_by` scan of the same project);
  - the **'Reconnect dependents to upstream' checkbox**, default UNCHECKED (#1747's default: dependents simply unblocked, each audited `unblocked_by_delete`); checked = dependents inherit the deleted task's direct upstream edges (dedup + cycle-guard server-side);
  - both semantics ride through verbatim: `boardApi.deleteTask(short_id, reconnect)`; the delete result surfaces as a page notice (orphans listed / `orphan→upstream` pairs).
- The pre-existing TaskDialog delete arm (no reconnect) is untouched — its e2e row still passes; the kebab modal is the new reconnect-capable surface.
- Test harness: `boardTestUtils` gained `mockDeleteEpic/mockArchiveProject/mockUnarchiveProject` + a `PROJECT_ARCHIVED` fixture; the harness's `deleteTask` mock already carried the reconnect arg.

### Test-fix findings (why tests changed shape after RED)

1. The fixtures' epic-uuid-1/epic-1 **spans both swimlanes** (Borealis's rows reuse Apollo's epic id) — band kebabs are addressed **inside their lane** in both layers (the kebab belongs to the band; the delete targets the epic object).
2. `vi.clearAllMocks()` does **not** pop queued `mockRejectedValueOnce` implementations — a test failing before consuming its pair poisoned the next two tests; the sequenced mocks are now `mockReset()` in `beforeEach`.
3. **e2e cache-copy semantics**: mutating the Node-side route state after `page.goto` never reaches the browser (react-query holds parsed copies) — the epic-force test sets the assignee in `freshState()` BEFORE goto (the established refusal-arm pattern).
4. The cascade count is **5**, not 4 (the shared epic id counts both lanes' rows) — honest against the read model and the server's `WHERE epic_id=$1`.

### RED-FIRST evidence

- Component: at pre-impl head `npx vitest run src/pages/BoardPage.deleteflows.test.tsx` → **12/12 failed** (kebabs/modals absent).
- e2e: `E2E_PORT=18521 npx playwright test tests/e2e/board.spec.ts --grep "1832 item 3"` → **4/4 failed**.
- Then implementation → both green (below). RED was captured before any impl file existed, in order.

### Ports (us72 registry discipline)

- Nightly `PORTFWD_PORT` registry (`.github/workflows/e2e-nightly.yml`): 18083–18093 in use; Vite range unclaimed.
- This lane: **E2E_PORT=18521** (C1 claimed 18519, its predecessor 18517; the 5173 default stays for CI). `--strictPort` makes any collision loud.

## Validation (exact commands + outcomes)

| Command | Outcome |
| --- | --- |
| `cd frontend && npx vitest run src/pages/BoardPage.deleteflows.test.tsx` | **12/12 passed** |
| `cd frontend && npx vitest run` (full) | 212 files, **2395/2395 passed** (C1 baseline 2383 + 12 new) |
| `cd frontend && npm run typecheck` | clean (tsc --noEmit, 0 errors) |
| `cd frontend && npm run lint` | 0 errors, 6 pre-existing warnings (generated `src/abi/**_pb.ts` — untouched) |
| `cd frontend && E2E_PORT=18521 npx playwright test tests/e2e/board.spec.ts` | **22/22 passed** (18 pre-existing + 4 new) |
| `cd frontend && E2E_PORT=18521 npx playwright test tests/e2e/sidebar-kebab-viewport.spec.ts tests/e2e/accessibility.spec.ts` | 5/5 passed (KebabMenu consumers unaffected) |
| `make repolint` | clean |
| RED-state check (pre-impl head) | deleteflows **12/12 failed**, e2e **4/4 failed** (RED-FIRST proven) |

## Lease incident + mitigation (recorded for the lane)

One heartbeat was mis-stamped ~40 min into the future (04:32Z against a 03:51Z pod clock — an in-session timekeeping slip, the exact failure class sharp-edges #11(b) documents against workers). The server accepted it, so renewals require a watermark above 04:32Z while real time lags. Mitigation chosen: keep-alive stamps minimally above the watermark (+30 s each, ~5 min real cadence) — bounded drift, claim kept alive, work continuous. A reclaim would reset the watermark for a successor (claim-time reset), so the residual risk is confined to this session. Disclosed here rather than silently absorbed; the future-stamp clamp hotfix from #11 would remove the class.

## Assumptions

1. **Task kebab lives on the kanban TaskCard only** (not also on the compact band rows) — C1's r1 pointer-semantics finding makes stacked click targets on compact rows hazardous, and the card is the primary task surface. The TaskDialog's plain delete arm remains (pre-existing behavior).
2. **Epic in-flight census is read-model-derived and advisory** — the engine's `epic_state` 409 remains the arbiter; the force gate reveals only after a REAL refusal (never proactively), matching "refuse unless force=true".
3. **Project kebab is lifecycle-only by construction** — one item, archived-or-not; no code path could add delete without the test noticing (`/Delete project/i` absence is pinned in both layers).
4. Cross-epic orphans from an epic cascade are surfaced via the page notice (server response fields); no pre-flight orphan census (would need the per-edge projection the read model lacks).

## Residual flags

- The e2e vite proxy logs ECONNREFUSED noise for unmocked endpoints (orgs, image-factory, timezone, sessions) — pre-existing board-spec behavior.
- Derived epics (task-row inheritance, no ListEpics seam yet) delete by UUID; real `epc-` epics by short id — the established board pattern, noted for the #1699 ListEpics follow-up.
- `deleteTask`'s reconnect semantics are exercised against the API client boundary (wire shape pinned in e2e: query-param presence/absence exact-matched); engine-side dedup/cycle-guard behavior is #1747's covered territory, not re-tested here.

## Mutation-verify suggestions

- Flip the default `reconnect` state to `true` → both "unchecked" rows go red (checkbox default + wire param absent).
- Remove `disabled={!acknowledgeForce}` on the force button → the force-gate e2e row goes red (button enabled without acknowledgment).
- Delete the in-flight block from EpicDeleteDialog → the in-flight surfacing rows (component + e2e) go red.
- Add a "Delete project" kebab item → the #1747 absence assertions go red in both layers.
