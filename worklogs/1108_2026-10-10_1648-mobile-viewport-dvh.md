# Worklog: NNNN (pending) — #1648 mobile viewport regression (h-screen → dvh)

**Date:** 2026-10-10
**Session:** w21 — issue #1648 (app shell taller than visual viewport on mobile; header OR composer off-screen)
**Status:** Fix + red-first pin implemented, verified locally; PR up

---

## Recon (issue + code, all read before editing)

- `AppShell.tsx:34` `flex h-screen overflow-hidden overscroll-none` — h-screen = 100vh =
  the LARGE viewport on mobile; with `overflow-hidden` the header/content/composer column
  exceeds the visible viewport, so one end is always clipped (which end depends on
  URL-bar state → the "only one visible at a time" symptom).
- Regression composed from #1626 (v0.34.19): `viewport-fit=cover` in index.html + safe-area
  paddings INSIDE the shell (`AppShell.tsx:53` header pt-calc(0.5rem+env(safe-area-inset-top)),
  `Composer.tsx:531` pb-calc(1rem+env(safe-area-inset-bottom))). Insets add height inside a
  100vh box. The paddings themselves are CORRECT (under-cover layout requires them); the box
  height was the bug. Composer/Sidebar untouched (Sidebar is w20/#1646 lane).
- Full h-screen census (`grep -rn "h-screen|min-h-screen" src`): 2 shell roots
  (AppShell:34, PortalLayout:40 — same `overflow-hidden overscroll-none` pattern), 6
  full-viewport standalone wrappers (router.tsx:81/88/95, SSOStartPage:19,
  OrgAdminLayout:34/41, PlatformAdminLayout:23), 6 scrollable min-h pages (InvitationPage:39/246,
  NotFoundPage:6, ErrorBoundary:28, AuthCard:13). **Decision: convert ALL to dvh** —
  the audit invariant becomes "zero h-screen/min-h-screen left in src", trivially
  checkable, no half-converted pattern left to explain in a follow-up. min-h pages never
  clip (they scroll), but on mobile the centered card sits in the large viewport, not the
  visible one — same one-token fix.

## The dvh emission question (verified, not assumed)

Scratch-used `h-dvh min-h-dvh h-[100dvh]` in src, ran the real `vite build`, grepped
`dist/assets/*.css`: Tailwind v4 emits `.h-dvh{height:100dvh}` / `.min-h-dvh{min-height:100dvh}`
and **no vh fallback**. Without a fallback, a pre-dvh browser (Safari <15.4, Chrome <108)
drops the only height declaration → shell collapses to content height — WORSE than the bug.
Added to index.css:

```css
@layer utilities {
  @supports not (height: 100dvh) {
    .h-dvh { height: 100vh; }
    .min-h-dvh { min-height: 100vh; }
  }
}
```

Layer discipline: unlayered CSS outranks the whole utilities layer in the cascade → an
unlayered fallback would force 100vh on modern browsers too (re-regression). Verified in
dist: fallback block sits inside the utilities layer AFTER Tailwind's 100dvh rule; guarded
by @supports it's inert in dvh browsers, load-bearing (100vh = pre-#1648 behavior) in
old ones. Pinned in styles/index.test.ts including the layer requirement.

## Red-first pin (verified numbers)

`AppShell.viewport.test.tsx` (vitest+jsdom): viewport model innerHeight=660 (visual),
chrome band 120 → large viewport 780; mobile matchMedia; insets live via the #1623
header padding (asserted present). Resolves the shell root's height utility the way a
mobile browser does and asserts box ≤ window.innerHeight, plus the shell stays a
height-clipped context (overflow-hidden, no min-h).

- **RED at f9a74a19** (commit a448cf30 carries the pin): `expected 780 to be less than or
  equal to 660` — the diagnosis reproduced exactly. The 3 CSS-fallback pins also red
  (block absent).
- **GREEN with the fix**: 10/10 across both pin files.

## Verified vs reasoned (honesty ledger)

**Tested (unit tier):** shell box resolves to the visual viewport, not the large one;
fallback block exists, carries 100vh, and is layer-qualified; full suite 183 files /
2029 tests green; eslint 0 errors (6 pre-existing warnings in generated abi files);
tsc clean; built CSS inspected for emission + layer placement.

**Reasoned, NOT browser-tested:** on-screen keyboard interaction. dvh tracks the visual
viewport on iOS 16.4+ and Chrome 108+ (keyboard shrinks visual viewport → shell shrinks →
composer stays visible; older Android resizes the layout viewport, same net effect). I
could not honestly emulate the keyboard/vh-dvh divergence: headless Chromium (Playwright)
has no browser chrome — 100vh == 100dvh there — so a mobile-emulation e2e would be
vacuous green. Declined to add one for this bug; disclosed in the PR body.

## Files

Fix: AppShell.tsx, PortalLayout.tsx, router.tsx, SSOStartPage.tsx, InvitationPage.tsx,
NotFoundPage.tsx, OrgAdminLayout.tsx, PlatformAdminLayout.tsx, AuthCard.tsx,
ErrorBoundary.tsx, styles/index.css. Tests: AppShell.viewport.test.tsx (new),
styles/index.test.ts (+3), AppShell.test.tsx (selectors h-screen→h-dvh),
PortalLayout.test.tsx (same). Not touched: Sidebar/SidebarDrawer/useEventStream/
SessionActivityProvider (w20/#1646 lane), Composer (its inset padding is correct).

---

## Round 1 review (CHANGES_REQUESTED @18:52Z, on #1661) — findings and response

Reviewer confirmed the core fix (dvh + fallback + red pin independently reproduced) and
raised three substantive items; all accepted:

**Finding 1 — keyboard mechanism claim was FALSE.** I claimed "dvh tracks the visual
viewport → keyboard shrinks it → composer visible". Wrong: Chrome 108+ defaults to
`interactive-widget=resizes-visual` — the OSK resizes only the visual viewport, viewport
units do NOT change; iOS Safari overlays the keyboard without resizing anything. dvh
covers browser-chrome dynamics only. Corrected in code, not just prose:
- `index.html`: viewport meta += `interactive-widget=resizes-content` (Android Chrome
  resizes the layout viewport for the OSK → 100dvh shrinks natively; where honored, the
  guard below never even activates since innerHeight shrinks with it).
- `useOskViewportGuard` (new hook, AppShell + PortalLayout): when the visual viewport
  shrinks ≥24px below the layout viewport, sets `data-osk-open` + `--shell-visual-height`
  (min(vv.height, innerHeight − vv.offsetTop)) on `<html>`; CSS rule
  `[data-osk-open] .h-dvh { height: var(--shell-visual-height, 100dvh) }` (higher
  specificity, same layer) pins the shells to the live visible span. Conditional by
  design: no keyboard → attribute absent → purely declarative dvh; the guard cannot
  regress the primary fix. 6 unit tests incl. pan-offset and no-visualViewport cases.

**Finding 2 — layer rationale was FALSE.** I wrote that the @layer wrapper prevents the
fallback from "unseating 100dvh in modern browsers". Wrong: the @supports not(...) guard
alone makes the block inert there; in dvh-less browsers the 100dvh declaration is dropped
at parse, so nothing competes. The @layer wrapper is co-location, not protection.
Corrected the index.css comment and replaced the layer-pinning test with
guard-semantics pins (fallback anchored INSIDE the @supports block, adjacency for
min-h-dvh, exact guard condition).

**Minor** — "dvh tracks the visual viewport" / "iOS 16.4+" phrasing corrected everywhere
(dynamic viewport; dvh is iOS 15.4+).

**Missing tiers added:**
- Served-artifact e2e `tests/e2e/app-shell-viewport.spec.ts` (route-mocks include the
  session-origins + runs/active composition lesson): served meta carries cover +
  resizes-content; served stylesheet carries `.h-dvh{height:100dvh}`, the @supports 100vh
  fallback, and the osk override; mobile-emulated geometry smoke + LIVE override check
  (setting the guard's attribute+var switches the shell's computed height to the var —
  red pre-fix for the class/rule absence since Tailwind only emits used utilities).
- Pin arithmetic now includes the nonzero insets (interior-budget invariant) with a
  documented analysis of why the literal "box + insets ≤ innerHeight" formula is
  incoherent (insets are paid FROM the box; the literal sum would demand a shell shorter
  than the screen).

## Honest ledger (supersedes the "Verified vs reasoned" section above)

The earlier section's keyboard claim is RETIRED — see Finding 1. Current truth:
- Unit-tested: shell resolves to the visual viewport; fallback guard semantics; OSK
  guard behavior incl. thresholds, pan offset, cleanup, no-vv no-op; AppShell wiring of
  the guard. 2038/2038 vitest green, lint 0 errors, tsc clean.
- E2E-tested (headless Chromium, dev-server served artifacts): meta keys, served CSS
  rules (dvh + fallback + osk override), osk override live in cascade, geometry smoke.
- REASONED, device-QA-required: actual soft-keyboard behavior on real iOS Safari /
  Android Chrome. Android mechanism is documented platform behavior (Chrome 108+
  release notes: resizes-content resizes the layout viewport); iOS mechanism is the
  standard visualViewport pinning pattern. Neither is device-verified here — headless
  Chromium has no keyboard; state this in any release note.
- Known flake observed (pre-existing, unrelated): sidebar-gesture.spec.ts "hamburger
  both halves" fails ~1-in-8 under parallel load, passes standalone and on retry.

## Operational note (round-2 push, 2026-10-10 ~19:20Z)

Round-2 head b829b398 pushed at ~19:07Z — before the fleet's interim mode-B credential
flip, so its review run inherited the app-identity event sender and died at the
permission assert (no verdict; CI unaffected — all checks re-ran on the new head).
This append rides the post-flip user identity to fire a healthy review round on the
same tree. Known pre-existing flake watch: sidebar-gesture hamburger test (~1-in-8
under load, green standalone/retry — same as round 1).
