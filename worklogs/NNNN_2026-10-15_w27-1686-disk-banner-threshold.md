# Worklog: #1686 — disk-space banner: threshold-gate the strip, compact placement, mobile-hide fix

**Date:** 2026-10-15
**Session:** w27 charter deliverable 2 of 3 (#1679 → #1686 → #1685). Owner dogfooding feedback (2026-10-15): the disk-space banner shows unconditionally, owns a dedicated line, and breaks the mobile "hide" affordance.
**Status:** Complete

---

## Objective

Make the disk-recovery strip render ONLY at the critical tier (≥95%, the v0.35.0 spec threshold mirrored from systemnotices' 0.95), give the critical banner a compact inline placement instead of a tall dedicated strip, restore coherent mobile Hide semantics, and audit the SDK/MCP/API surfaces for consistency (issue item 4).

---

## Root cause (verified in tree)

`DiskRecoveryStrip` rendered an on-demand "Free up disk space" line whenever disk metrics were merely KNOWN — at 5% as at 97% (DiskRecoveryStrip.tsx:84-96 pre-change), pinned by the test "below 95% renders only the subtle on-demand action". That line was a full-width right-aligned row directly under `DiskUsageBar`'s metrics row (the "dedicated line"), and on mobile it sat immediately below the compact row's Hide/Metrics toggle — so "hide" never cleared the disk affordance (the reported mobile breakage). The v0.35.0 worklog (1106) had spec'd "subtle on-demand action IN THE SAME METRICS AREA"; the implementation betrayed that line, and at `py-0.5` became a permanent strip row — the unconditional display the owner called the worst offender (it trains users to ignore a safety banner).

## Work Completed

### (a) RED-first pin flip — `DiskRecoveryStrip.test.tsx` (commit e4800d58)

"below 95% renders only the subtle on-demand action" → **"below 95% renders nothing at all (#1686 — no on-demand line)"** (empty container; both testids asserted absent), plus a **94/95 boundary pin**. Verified RED against main @ 9b1f317c: exactly these two pins fail (run measured: 10 pass / 2 fail); all other strip behavior pins stay green. Mutation surface: restoring the on-demand arm (or dropping the `!critical` early return) re-trips both.

### (b) Implementation — `DiskRecoveryStrip.tsx`

- **Below 95% → render NOTHING**: the on-demand arm is deleted; the component returns null unless `critical` (≥95%) or an interaction is active (`phase !== idle || error` — a report/result never vanishes mid-interaction if usage dips). The recovery API stays available at any usage; this is presentation-only.
- **Compact placement**: the critical banner collapsed from a `py-2 text-xs` multi-row strip (message row + ml-auto action block) to ONE tight line — `py-1 text-[11px]`, headline + inline muted context + inline "Review what can be freed" + inline "Dismiss", all in the same `flex-wrap` row, `border-b` continuous with the metrics block. The expanded report/table renders inside the same bordered block. Judgment call (stated in the PR for owner pushback): a critical safety alert keeps a one-line presence at ≥95% — full-bleed prominence is deliberately weakened to compact-inline, per the issue's item (2).
- **Dismiss + episode semantics (#1686 item 3)**: the banner is dismissible; a dismissal lasts for the current critical EPISODE (the continuous stretch ≥95%), implemented with the React-endorsed adjust-state-during-render pattern (leaving the tier resets `dismissedEpisode`) — so mobile Hide/Dismiss are coherent, the alert can be quieted per-spike, but never permanently silenced while usage keeps crossing the line.

### (c) Companion pins (green)

- **Dismiss/re-arm**: dismiss at 97% → empty; still critical at 98% → stays dismissed (no nagging within the episode); drop to 70% → still empty (episode ends, state resets); re-cross to 96% → banner re-armed with the fresh percentage.
- **Compact shape**: collapsed banner carries `py-1` + `text-[11px]`; headline, Review button and Dismiss all live in the same first-row flex container (`toContainElement`).
- All 12 pre-existing strip pins (dry-run first, free-now honesty, refused classes, error path, classes:null wire shapes, partial-sweep honesty) remain green — only the surfacing rule changed.

### (d) E2E — `frontend/tests/e2e/disk-recovery-strip.spec.ts` (new, 3 tests)

Real ChatPage composition, archive.spec.ts harness shape, status mock carrying `diskUsedBytes/diskTotalBytes`:
1. Below 95% → no strip element anywhere (both testids absent).
2. At 97% → compact banner visible with inline Review + Dismiss.
3. **Mobile viewport (390×844)**: with the critical banner mounted, the metrics Hide/Metrics toggle still toggles (the reported breakage), Dismiss removes the banner, and the toggle survives the dismissal — nothing disk-related lingers under it. (`test.use` viewport belongs at describe level — the in-test form throws; fixed on first run.)

### (e) Audit (issue item 4) — conclusion: API/SDK/MCP surfaces are presentation-agnostic, no changes needed

Grep-verified: `sdks/openapi.yaml` `POST /workspaces/{id}/disk-recover` documents authz + sweep semantics only — no frontend-surfacing or threshold language anywhere in the contract; no hand-written SDK (typescript/python/go/java) models the endpoint; no MCP tool description mentions disk recovery (the lever is not an MCP surface); no user docs describe the frontend threshold. The surfacing rule lives in exactly one place after this change (the frontend `SURFACED_AUTOMATIC_PCT` constant + the systemnotices 0.95 source it mirrors). CHANGELOG [Unreleased] records the UX fix (the 0.35.0 entry's "surfaced automatically at ≥95%" remains true — it is now ALSO the only place it surfaces).

---

## Key Decisions

- **Nothing below 95%, not a relocated affordance** — the owner's item (1) overrides the v0.35.0 "on-demand below threshold" idea; moving the line into the metrics row would still be an always-on element training banner-blindness.
- **Episode-based dismissal** rather than permanent dismiss or no dismiss: quietable per-spike, re-armed on re-cross — matches the safety intent without the nag.
- **Active-phase retention**: an in-flight report/error keeps the strip mounted even if usage dips below 95 mid-interaction — results must not vanish; the next non-critical idle render returns null.
- **Mobile e2e assertion is behavioral, not visual**: the Hide toggle's functionality (click → still present/functional) is the pinnable contract; "overlap" aesthetics are covered by the compact single-line placement.

---

## Blockers

None.

---

## Tests Run (measured on this branch)

- RED phase: `npx vitest run src/components/workspace/DiskRecoveryStrip.test.tsx` → 10 pass / **2 fail** (exactly the flipped pins) at e4800d58.
- GREEN: same file → **14/14** (12 prior + 2 new: dismiss/re-arm, compact shape).
- `npx vitest run` (full) → **2104 passed / 0 failed**, 188 files.
- `npx playwright test tests/e2e/disk-recovery-strip.spec.ts` → **3/3** (incl. mobile viewport arm).
- `npx tsc -b` + eslint on touched files → clean. No other references to `disk-recover-on-demand` remain (grep-verified).

---

## Next Steps

- PR for #1686 → review loop → merge-readiness report (no self-merge).
- Then #1685 (busy-15-min nudge) — final charter deliverable.
