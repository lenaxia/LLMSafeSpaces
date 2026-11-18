# Worklog 1291 — 2026-11-17: board rail orientation re-apply (owner report #3, second occurrence)

**Session:** tsk-tangible-signal-notices (Sprint Board, "Frontend reliability mopup" epic) — the owner-reported rotation defect that survived the .16 cut: "rotating the phone landscape then back to portrait leaves the sprint board columns at the wrong sizes — the portrait 95/3+5-peek layout does not re-apply after orientation change." Worktree `/tmp/opencode/rail-orient-wt`, branch `fix/board-rail-orientation-reapply` off `origin/main` (b483a7fd0, v0.44.16).

## What .16 shipped and why it was not enough

PR #2087's orientation commit (59476e682) fixed the **event-delivered** lens survival: `mdPlus` became component state with a render guard (`effectiveFocused = mdPlus ? null : focusedIndex`). Its own record stated the residual: "a genuinely UNDELIVERED event still leaves mdPlus stale; no poll." The reviewer's r2 finding said the same ("The shipped fix is still sound for real rotations (events fire...) — the record overstates it as missed-event-proof"). The owner's second report landed in exactly that residual: the md row's correctness remained a function of ONE delivered `matchMedia` change event, and nothing else in the system owned the layout across a viewport-class change.

## The reproduction hunt (why the pins pass in Chromium)

Five probes (gap/flow/freeze, deleted after the hunt; the orchestrator's `_land-probe3` matrix predates them):

1. **setViewportSize round-trip**: portrait→landscape→portrait restores the portrait layout **byte-identically** (railCW 364, railSW 713, 95/119/98/360 — both directions, fraction-exact).
2. **CDP `Emulation.setDeviceMetricsOverride` with `screenOrientation`** (the real rotation shape — what a phone renderer actually does): identical convergence.
3. **Real CDP swipes then rotation**: scroll restoration across rotation replays the pre-rotation **content offset** (SL 230 → 0 → 230) — faithful in same-geometry round-trips.
4. **Lens + mid-flight rotation** (rotate inside the 200ms width transition + 220ms resnap window): intermediate states show the transition retargeting (done 360→224→313) but the settled state converges to the correct portrait overview.
5. **Frozen-page rotation** (`Page.setWebLifecycleState("frozen")`, rotate, unfreeze): Chromium heals — mdPlus catches up, overview restores, snapped.

Conclusion: **Chromium cannot honestly redden the pure rotation** — the .16 guard works whenever events deliver, and Chromium always delivers. The residual defects are structural and engine-timing-dependent (WebKit-class rotation timing, the frozen/hidden window, coalesced delivery). Three of them, all matching the task's diagnosis direction:

- **[CSS] No md: authority over focus widths** — `STAGE_COL_FOCUS_WIDTH` (70%/10%/full) carried no `md:` variants; the md row was reachable only through JS state being right. Any stale `mdPlus` renders portrait focus geometry at md+ AND back — the owner's literal symptom.
- **[Clock] One clock** — no `resize`/`orientationchange` listener anywhere (suspect #1); a lost `matchMedia` change never heals.
- **[Position] Unowned scroll across class changes** — rotation scroll-restoration replays stale content offsets against changed column widths; nothing re-establishes snap legality (a rail straddling columns reads as "wrong sizes" even with correct classes). The e2e orientation pins asserted widths only — never position.

## The fix (three walls, `BoardPage.tsx` only)

1. **CSS authority** — `STAGE_COL_FOCUS_MD_WIDTH`: every column in ANY focus state carries its overview `md:` width (mirroring `STAGE_COL_WIDTH`'s md: halves exactly, `md:min-w-[8rem]` for in_progress included so tailwind-merge resolves identical md geometry). At md+ the media query alone renders the 24/28/24/24 row — with the lens state stale, dead, or adversarial.
2. **The rotation clocks** — the `mdPlus` effect additionally subscribes `resize` + `orientationchange`, each an idempotent `setMdPlus(mq.matches)` re-read. Every real rotation fires resize on both engines; a lost mq event heals on the next resize frame. A same-class resize is a no-op (React bails on the unchanged value — pinned).
3. **Position ownership** — the crossing effect (now flip-gated via `prevMdPlus`) re-owns the rail's position on the md+→portrait crossing: after the width transition settles (FOCUS_RESNAP_DELAY_MS), re-derive the nearest column start from live geometry and re-snap to it — the same "target settled geometry, never a moving snap area" contract every other mode change honors. Already-legal restores are left alone (>1px guard).

## Born-red record (the exact residual, simulated honestly)

`BoardPage.orientation.test.tsx` (NEW, 7 tests) builds the undelivered-event world with a controllable `matchMedia` whose `matches` flips **without any event** — the honest shape of the lost event (no synthetic delivery, no state poking):

- On **pristine main** (BoardPage.tsx stashed, tests only): **5 failed / 2 passed** — the CSS-authority pair (stale lens renders without md: widths), the resize heal, the orientationchange heal, and the portrait re-snap are RED; the idempotence pin and the on-column no-op are green (they pin absence of misbehavior, true on both trees).
- With the fix: **7/7 green**.
- Mutation separation: dropping wall 1 reddens only the authority/drift pins; dropping wall 2 reddens only the heal pins; dropping wall 3 reddens only the re-snap pins.

## E2E (the permanent wall)

`board-rail-touch.spec.ts` 6→7 tests, both orientation tests now asserting the missing contract:

- `expectSnappedLegal` — scrollLeft inside some column's valid snap range after every rotation step (the assertion class that was absent; a straddled rail is the user-visible "wrong sizes").
- The existing round-trip test gains legality checks at every portrait checkpoint.
- NEW test: orientation round-trip via **CDP `setDeviceMetricsOverride` + `screenOrientation`** — the real-rotation emulation shape: both directions fraction-exact, rapid cycling converges, lens round-trip resets, legality at every step, override released at the end.
- Honest scope: these e2e tests are GREEN on pristine main too (Chromium delivers events and restores legally in pure flows — verified by mutation run); they are the wall against geometry regressions, while the undelivered-event residual is caught by the jsdom pins. The record states which layer catches what.

## Evidence

- vitest full: **239 files / 2762 passed** (2755 base + 7 new).
- e2e `board-rail-touch.spec.ts` **7/7** (incl. the new device-emulation rotation test); `board.spec.ts` + `sidebar-gesture.spec.ts` + `app-shell-viewport.spec.ts` **60/60**.
- `tsc --noEmit` clean; eslint 0 errors (BoardPage's pre-existing exhaustive-deps warning on base); `make fmt-check` clean (no Go changes).
- Probes deleted from the main checkout after the hunt (`_land-probe3.spec.ts`, the orchestrator's, left untouched).

## Process

AI reviewer until APPROVED; orchestrator merges; no sleep-polling. Worktree off origin/main; origin/main merged before push (branch cut from b483a7fd0 == origin/main at all times during the lane; re-checked before push).
