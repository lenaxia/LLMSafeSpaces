# Worklog: #1716 — light color scheme contrast (sentinel badge + palette AA fixes)

**Date:** 2026-10-16
**Session:** Frontend lane, issue #1716 (owner dogfood report: "message from other session" sentinel invisible in light mode). Follow-up sweep filed as #1717.
**Status:** Complete

## Objective

Two failure classes per the report: (1) light-scheme token pairs below WCAG AA, (2) elements whose foreground composes to (near-)equality with their background — confirmed case the agent-origin sentinel badge. Fix the sentinel first, red-first; audit and fix the light scheme's clear text-surface failures; leave the dark palette byte-identical.

## Root cause (verified in tree)

- **Sentinel**: `AgentOriginBadge` borrowed muted-surface tokens (`text-muted-foreground` on `bg-muted/40`) while rendering INSIDE the user bubble — an inverted surface (`bg-primary text-primary-foreground` light; `dark:bg-slate-600 dark:text-slate-100`). In light mode the badge bg composes to `#69707d` against `#64748b` text = **1.05:1** (invisible — the owner report). Dark measured **3.89:1** (visible but sub-AA; matches "dark reads fine"). Neither token is wrong on its own surface — the defect is cross-surface borrowing.
- **Palette**: light `muted-foreground` (46.9% L) measured **4.34:1** on muted/secondary (assistant-bubble meta, thinking blocks, code headers); light `destructive` (red-500 grade) measured **3.76:1** as text on white, **3.29:1** on its own `/10` banner tint (ChatPage error strips, RunDetail, destructive Badge), and its button pair (near-white on destructive) **3.60:1**. All below the 4.5:1 normal-text bar.
- **Timeline meta**: assistant-bubble timestamp `text-muted-foreground/70` on `bg-muted` = **2.58:1**.

## Audit — every failing pair found (light scheme)

| Pair (used on) | Pre-fix | Post-fix |
|---|---|---|
| sentinel badge on user bubble, LIGHT | 1.05 | ≥10 |
| sentinel badge, DARK | 3.89 | ≥10 |
| muted-foreground / muted + secondary | 4.34 | 4.88 |
| muted-foreground / background | 4.76 | 5.35 (pin) |
| destructive / background | 3.76 | 6.43 |
| destructive / own `/10` tint | 3.29 | ~5.5 |
| destructive-foreground / destructive | 3.60 | 6.15 |
| assistant timestamp / muted | 2.58 | 4.88 |
| user timestamp / primary (pin) | 8.8 | 8.8 |

Not fixed here (deliberate, enumerated in #1717): 22× opacity-diluted `muted-foreground/50–80` (2.3–3.4:1) outside the timeline; CodeBlock header on user bubbles (1.94:1 — same cross-surface class, needs `isUser` plumbing); `border`/`input` 1.23:1 (WCAG 1.4.11, needs decorative-vs-functional ruling); idle-state copy buttons (~1.4:1 hover-reveal pattern); transient `text-green-500` (2.28:1).

## Work Completed

### (a) RED-first pins — commit `ac004ed8`

- `frontend/src/test/wcag.ts` — WCAG 2.1 math (relative luminance, contrast ratio, source-over alpha compositing) + index.css token parser (`@theme` = light, `.dark` = dark) + a Tailwind class→color resolver (base utilities apply in both schemes — the CSS vars themselves flip under `.dark` — `dark:` variants override; slate literals mapped; non-color utilities ignored). jsdom never applies the Tailwind cascade (vitest stubs CSS), so the pins compute what the browser would, on identical inputs (fs-read precedent: `styles/index.test.ts`).
- `frontend/src/styles/themeContrast.test.ts` — the token-pair audit above (5 RED pre-fix) + passing-surface pins + a **dark freeze**: every `.dark` token pinned to its exact current HSL literal, plus an AA tripwire for future dark edits.
- `frontend/src/components/chat/AgentOriginBadge.contrast.test.tsx` — renders the sentinel through the REAL `MessageBubble` user-part path (not the badge in isolation, so the pin sees the true surface), resolves the rendered classes over the bubble, asserts ≥4.5:1 in both schemes (RED: 1.05 / 3.89); structural no-muted-tokens pin; timeline meta pins (assistant RED 2.58, user pin 8.8).

### (b) The fix — commit `0c6ccf0b`

- `AgentOriginBadge.tsx`: badge ink now borrows the bubble's OWN foreground — `text-primary-foreground bg-primary-foreground/10 border-primary-foreground/25`, dark overrides `dark:text-slate-100 dark:bg-slate-900/40 dark:border-slate-100/25` mirroring `MessageBubble`'s bubble overrides. Contrast is the bubble's contrast by construction (≥10:1 both schemes).
- `index.css` (light `@theme` only): `muted-foreground` 46.9%→43.5% L; `destructive` red-500→#b91c1c grade. Dark block untouched.
- `MessageBubble.tsx`: assistant-bubble timestamp drops `/70`; user side keeps it (8.8:1).

## Verification

- New pins: 19/19 green post-fix (RED pre-fix captured in the commit-1 diff).
- Full frontend suite: **2159 passed / 194 files**; `tsc --noEmit` clean; eslint 0 errors (6 pre-existing warnings in generated `abi/*_pb.ts`).
- Dark no-regression: freeze pin green + dark values byte-identical in the diff.

## Key Decisions

- Badge keys to the bubble's ink rather than a new "inverted-muted" token set — one surface pair to maintain, and the pin makes cross-surface borrowing fail CI.
- Single-token `destructive` darkening over per-banner component edits — fixes text-on-white, banner-tint, and button-bg consumers at once.
- Out-of-scope items filed as #1717 instead of widening the PR into a sweep-wide visual change.

## Files Modified

- `frontend/src/test/wcag.ts` (new), `frontend/src/styles/themeContrast.test.ts` (new), `frontend/src/components/chat/AgentOriginBadge.contrast.test.tsx` (new)
- `frontend/src/styles/index.css`, `frontend/src/components/chat/AgentOriginBadge.tsx`, `frontend/src/components/chat/MessageBubble.tsx`
- `CHANGELOG.md`, this worklog

## Rework r1 (PR #1718 review, CHANGES_REQUESTED @ ab177f01 — verdict honored in full)

**Finding 1 — queue error pill (GO, fixed in-PR):** `QueueSection.tsx` renders the failed-send pill as `bg-destructive/20 text-destructive dark:bg-destructive/30` with `opacity-70`/`opacity-80` on its text children. Recomputed with this PR's own harness (now modeling `opacity-*` element dilution): struck text **3.06:1** light / **1.46:1** dark, error line **3.58:1** light / **1.57:1** dark, retry ink **1.80:1** dark — worse than the sentinel that opened the issue, and pre-flagged on the #1716 thread with the fix precomputed. Fix (commit `7f0e5c0e`): `dark:text-red-400` on the pill (5.8:1 on the same /30 tint) and the opacity dilutions dropped (light full-strength ink = 4.59:1; `line-through` alone carries the not-sent semantics; light headroom over AA is 0.09:1, so ANY re-added dilution fails the pin). Contained in QueueSection.tsx — zero ripple. Pins: `QueueSection.contrast.test.tsx`, 6 rows (5 RED on ab177f01, light retry-ink row passed pre-fix and is pinned).

**Finding 2 — e2e computed-style gate (GO, added):** `agent-origin.spec.ts` now carries a browser-level contrast row per theme: reads the badge's computed colors in real Chromium, composites translucent layers (oklab/oklch/rgba + positional-alpha parsing, CSS Color 4 → sRGB), asserts ≥4.5:1 + that the dark run actually applied `html.dark`. Fails loud when a layer can't be parsed (no silent white fallback). Mutation-validated against the pre-fix badge: **1.07:1 light / 3.88:1 dark** (harness 1.05/3.89; delta = Tailwind v4 oklab color-mix interpolation — precisely the resolver-vs-cascade drift the gate exists to catch; the first draft of the parser had two such drifts of its own: rgba positional alpha misread as opaque-black truncating the layer walk, and rgba-only matching skipping oklch layers).

**Housekeeping:** #1716 thread audit table trued-up to shipped values (43.5% L / 4.88:1 — not 42% / 5.17:1; badge dark ≥10 via explicit slate-100 ink — not 4.79 via inherited currentColor); #1717 annotated that the pill is fixed here.

**Verification (r1 tree):** units 2165 passed / 195 files; agent-origin e2e 6/6 (incl. 2 new contrast rows); `tsc` 0 errors; eslint 0 errors (6 pre-existing generated-abi warnings). Commits: red pill pins `f8c68fbf` → pill fix `7f0e5c0e` → e2e gate `bd434572`.
