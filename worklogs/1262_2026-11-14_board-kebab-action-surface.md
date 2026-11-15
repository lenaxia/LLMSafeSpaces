# Worklog: Board Kebab Menus — the Full Per-Entity Action Surface (story + epic cards)

**Date:** 2026-11-14
**Session:** tsk-fossil-tanker-loses (epic "Frontend reliability mopup", project "Agent DX: reference patterns")
**Status:** Complete — pushed for AI review

---

## Objective

The owner ask 2026-11-14: board kebab menus were under-populated (story cards: Delete only; epic cards: Delete only on main). The backend supports a much larger per-entity action surface — expose it in the UI, mapped 1:1 onto the EXISTING endpoint/client plumbing (nothing invented server-side; gaps noted). Frontend + minimal API-client scope, worktree off origin/main.

## Work Completed

### 1. Recon — the endpoint inventory (all pre-existing, zero backend work)

- Story: `pauseTask`/`resumeTask` (`POST /me/tasks/:id/pause|resume`), `moveStage` (`pending_input→backlog|in_progress`, `in_progress|in_review→failed` — the engine's legal map), `updateTask` with `setCriteriaMet {index, met}` (#1837 P2b TWO-WAY form), and `POST /me/tasks/:id/rearm` (#1832 WS3 — the ONLY missing client method).
- Epic: `activateEpic`/`pauseEpic`/`archiveEpic`/`unarchiveEpic` all already in the client. **Gap noted: epic "Resume" has NO endpoint** (activate un-pauses; only projects carry a resume route) — omitted, recorded in the PR description + CHANGELOG.
- Engine truth read from source, not assumptions: rearm requires `stage=failed ∧ pending_origin=escalated` (rearm.go); pause refuses only done (pause.go); the epic allowed-map is activate⟵{draft,paused,completed,archived}, pause⟵{active}, archive⟵{draft,active,paused,completed}, unarchive⟵{archived} (service.go epicLifecycle).
- **Discovered en route**: PR #2021 (epic band activate/pause) is OPEN, not on main — the story's "from #2021, keep" presumes it landed. This PR carries the equivalent work (kebab activate/pause + the activate-wrapper client fix) off origin/main; overlap noted for the orchestrator's merge sequencing.

### 2. Client (`api/board.ts`) — three minimal additions

- `rearmTask` (POST + requestId body, matching pause/resume's idempotency discipline).
- `BoardEpicActivateResponse` — activate's 200 is the `{epic, initial_ready_set}` WRAPPER (hurl-pinned; #2021's fix, ported): the client typed it bare, so a production activate fed the wrapper into the drawer/knownEpics. Both call sites unwrap `.epic`.
- `setCriteriaMet?: number | {index, met}` — the two-way wire form (the bare number stays the legacy one-way).

### 3. Story kebab (`BoardPage.buildStoryKebabItems`) — the state matrix

backlog→Pause; in_progress/in_review→Pause+Move-to-failed; pending_input→Pause+Requeue+Resume-work; paused→Resume; failed+escalated→Pause+Re-arm; done→NOTHING stage-shaped (criteria+Delete only — the terminal pin). Worker-owned transitions (→in_review, →done, the bounce) deliberately absent. Per-criterion toggles render as `✓/○ n. text…` items (truncated to the w-40 menu).

### 4. Optimism per lane convention

- Stage items reuse the DRAG machinery wholesale (`runMove`): optimistic placement, snap-back, "Move refused" notice. New `via` seam swaps the wire call — Re-arm gets the same visual (failed→backlog) over `rearmTask`.
- pause/resume + criteria toggles: a per-card override map (`cardOverrides` → `decorateTask` into StageCell) flips the badge/glyph instantly; refusal rolls back + renders the page-level `BoardErrorNotice` (the recent tool-error lane).
- Epic lifecycle: server-confirmed `rememberEpic` + invalidate (the #2021/handleProjectLifecycle pattern — nothing to roll back).

### 5. Epic kebab + legibility

Conditional items per the allowed-map (archived→Unarchive ONLY; everything else→Activate|Pause + Archive; Delete stays). `draft`/`completed`/`archived` badges join `paused` on the band. `KebabMenu`'s sectioned mode gained a bare divider before a trailing unsectioned item (the destructive tail) — component test pinned.

### 6. Pins — `BoardPage.kebab.test.tsx` (NEW, 22 tests) + client/component pins

Full matrix × exact item sets; every action's real endpoint + params (`pauseTask("tsk-bbbb0002")`, `rearmTask` NOT moveStage, `moveStage(id,"backlog",undefined)`, `updateTask(id,{setCriteriaMet:{index,met}})` both directions); the optimistic paused-badge flip held against a deferred promise; rollback + `data-board-error` codes on refusal; done's no-stage-moves; the epic matrix across all five statuses with `activateEpic("epc-…")` short-id addressing and the badge flip.

## Verification

- Full frontend suite: **2644/2644 green (230 files)**; `tsc -b` clean; `eslint` 0 errors on touched files (one PRE-EXISTING exhaustive-deps warning at `openEpicDrawer` — same one #2021's worklog recorded, not this lane's code).
- **Born-RED 21/22**: the kebab suite against the pre-change BoardPage/board.ts/KebabMenu fails every matrix/action pin (the 1 pass is "done exposes no stage moves" — the old Delete-only behavior satisfies it; an honest both-ways invariant).
- `make fmt-check` green.

## Key Decisions

- **Base off origin/main, not #2021's branch** (binding constraint) — the overlap (epic activate/pause + the wrapper fix) is re-implemented and flagged in the PR description for merge sequencing; whichever lands first, the other adapts.
- **Manager-legal only**: the kebab never offers in_progress→in_review (worker-owned). "stuck→failed" exposes the engine's self-fail arm for in_progress AND in_review non-flagged rows; a claimed in_progress row's claim-token 409 renders honestly (the engine arbitrates — same posture as the drag affordance mirror).
- **Re-arm through runMove's `via` seam** rather than a bespoke handler: identical optimistic stage placement + snap-back, one line of difference (the wire call).
- **Criteria toggles via the indexed write** (`{index, met}`), never full-array replacement — a toggle cannot clobber a concurrent edit's criteria array.
- **TASK_PAUSED fixture stays out of `defaultTasks()`** — the shared fixtures' counts are pinned by three other suites; the kebab suite seeds it explicitly.

## Blockers

- None known. AI review per PROCESS ruling (never self-merge; orchestrator merges).

## Review r2 — stale-card-override-mask (CHANGES_REQUESTED @cb4607c33)

The round-1 review (2026-11-14, verdict request-changes) validated every PR-body backend claim against source and gated on ONE correctness finding: **the override map's success-path lifecycle**.

- **The finding**: `cardOverrides` rolled back on refusal but was never cleared on SUCCESS — `applyCardOverrides` then re-applied the stale `paused`/`acceptance_criteria` on every render, permanently masking later authoritative changes to that row (a dialog's full-array criteria edit, a second tab, an SSE/board.event refetch) until page reload. The reviewer validated it empirically with a scratch repro (toggle criterion 2 → land an edited row with a third criterion → the new criterion never renders). The r1 in-diff comment claiming "the server's row supersedes it after the invalidated read lands" was false — nothing removed the entry.
- **The fix** (`BoardPage.clearCardOverride`): both success paths (pause/resume, criterion toggle) now retire their field AFTER `await invalidateQueries(["board"])` lands the server's row; the refusal rollbacks retire their field too (was: whole-entry delete — field-scoped retirement also preserves a concurrent op's optimism on the same row); the entry drops when its last field leaves, so the map never holds a row it no longer speaks for. The block comment now states the actual mechanism.
- **Pins** (born-RED at the r1 head — verified by stashing the fix, 2 failed / 2 passed with it): `BoardPage.kebab.test.tsx` gains the "optimism yields to authoritative rows" describe — (a) toggle → land a dialog-edited row (third criterion) via the cross-context `invalidateQueries` path → the third criterion AND the server's own met-state render; (b) pause → the server's paused row renders → land a cross-context resume → the badge clears and the kebab offers "Pause story", never a stale 409-bait "Resume story". Both use the stateful per-project read script (initial / the write's own refetch / the cross-context read) the #1964 pattern established.
- **Also this round**: merged origin/main (subscriptions-PVC #2028 — CHANGELOG conflict only, both entries kept); full frontend suite **2676/2676 green (231 files)** at the r2 head (2644 at r1 + main's 30 + these 2 pins); `tsc -b` clean; `eslint` 0 errors (the one PRE-EXISTING `openEpicDrawer` exhaustive-deps warning, unchanged — now line 1029); `make fmt-check` green.
