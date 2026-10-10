import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

// #1629 — the CSS half of the full-edge gesture fix. The browser's
// horizontal swipe-back navigation is a root-scroller overscroll action:
// `overscroll-behavior-x: none` on html/body (longhands, not the shorthand)
// is the load-bearing declaration that removes the OS/browser race at the
// platform level, which is what makes the restored EDGE_ZONE touchstart
// claim (#590 shape, see useSwipeableSidebar) sufficient.
//
// jsdom cannot run Tailwind/the full cascade for this stylesheet (and
// vitest stubs CSS imports), so the pin reads the source directly via
// fs; the e2e (sidebar-gesture.spec.ts) asserts the RUNTIME computed
// style on documentElement/body in real Chromium.

// vitest runs with the frontend package as cwd (npm test / npx vitest);
// import.meta.url is virtual under the jsdom environment, so resolve
// from cwd instead.
const stylesheet = readFileSync(resolve(process.cwd(), "src/styles/index.css"), "utf8");

function ruleFor(selector: string): string {
  const m = stylesheet.match(new RegExp(`${selector}\\s*\\{[^}]*\\}`));
  if (!m) throw new Error(`no ${selector} rule in index.css`);
  return m[0];
}

describe("root overscroll policy (#1629)", () => {
  it("html declares overscroll-behavior-x: none (kills horizontal swipe-back navigation)", () => {
    expect(ruleFor("html")).toMatch(/overscroll-behavior-x:\s*none/);
  });

  it("body declares overscroll-behavior-x: none", () => {
    expect(ruleFor("body")).toMatch(/overscroll-behavior-x:\s*none/);
  });

  it("keeps the y-axis containment on html and body (no pull-to-refresh / bounce chaining)", () => {
    expect(ruleFor("html")).toMatch(/overscroll-behavior-y:\s*none/);
    expect(ruleFor("body")).toMatch(/overscroll-behavior-y:\s*none/);
  });

  it("does not regress to the ambiguous shorthand alone", () => {
    // The shorthand is behaviorally identical, but the x longhand is the
    // documented #1629 mechanism — keep it explicit and greppable.
    expect(ruleFor("html")).not.toMatch(/overscroll-behavior:\s*[^;-]/);
    expect(ruleFor("body")).not.toMatch(/overscroll-behavior:\s*[^;-]/);
  });
});

// ── #1648 — dynamic-viewport fallback for the app shells ───────────────
//
// The shell roots use h-dvh / min-h-dvh (100dvh is the DYNAMIC viewport —
// resizes with browser chrome, not the on-screen keyboard; 100vh is the
// LARGE viewport and made the shell taller than the visible screen — the
// #1626/#1648 regression). Tailwind v4 emits `.h-dvh{height:100dvh}`
// with NO fallback (verified against the built CSS): in a browser
// without dvh support the only declaration is dropped at parse time and
// the shell collapses to content height. The @supports not(...) guard is
// the load-bearing mechanism: inert in every dvh-capable browser, active
// exactly where the dvh declaration was dropped. The @layer utilities
// wrapper is co-location, not protection — nothing here ever competes
// with a declaration that applies (pinned: the fallback declarations
// live INSIDE the guard).
function supportsBlock(): string {
  const start = stylesheet.indexOf("@supports not (height: 100dvh)");
  if (start === -1) throw new Error("no @supports not (height: 100dvh) fallback block in index.css");
  return stylesheet.slice(start);
}

describe("dynamic viewport fallback (#1648)", () => {
  it("h-dvh falls back to 100vh in browsers without dvh support, inside the guard", () => {
    // Anchored to the guard's opening: inert on modern browsers (the
    // load-bearing property is being INSIDE @supports not (100dvh)).
    expect(supportsBlock()).toMatch(
      /@supports not \(height: 100dvh\)\s*\{\s*\.h-dvh\s*\{\s*height:\s*100vh/,
    );
  });

  it("min-h-dvh falls back to 100vh immediately inside the same guard", () => {
    // Adjacent to the h-dvh fallback rule within the guard block.
    expect(supportsBlock()).toMatch(
      /\.h-dvh\s*\{[^}]*\}\s*\.min-h-dvh\s*\{\s*min-height:\s*100vh/,
    );
  });

  it("guard condition is exactly the absence of dvh support (not a broader query)", () => {
    expect(supportsBlock().startsWith("@supports not (height: 100dvh)")).toBe(true);
  });
});

describe("on-screen keyboard override (#1648 criterion 4)", () => {
  it("shells adopt the live visual-viewport height while the keyboard is open", () => {
    // dvh does not shrink for the OSK (Chrome default is resizes-visual;
    // iOS overlays). The [data-osk-open] rule — set by useOskViewportGuard —
    // overrides the shell height with the tracked visual viewport span.
    // Higher specificity than .h-dvh, so it wins only while it applies.
    expect(stylesheet).toMatch(
      /\[data-osk-open\]\s+\.h-dvh\s*\{\s*height:\s*var\(--shell-visual-height,\s*100dvh\)/,
    );
  });
});
