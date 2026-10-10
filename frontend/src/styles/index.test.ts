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
// The shell roots use h-dvh / min-h-dvh (100dvh tracks the VISUAL
// viewport; 100vh is the LARGE viewport and made the shell taller than
// the visible screen — header OR composer permanently clipped, the
// #1626/#1648 regression). Tailwind v4 emits `.h-dvh{height:100dvh}`
// with NO fallback (verified against the built CSS): in a browser
// without dvh support the only declaration is dropped and the shell
// collapses to content height. This @supports block restores the legacy
// large-viewport height there — same behavior as before #1648, never
// worse. It must live in @layer utilities so it joins Tailwind's
// utilities layer (unlayered CSS would outrank the layer wholesale).
function supportsBlock(): string {
  const start = stylesheet.indexOf("@supports not (height: 100dvh)");
  if (start === -1) throw new Error("no @supports not (height: 100dvh) fallback block in index.css");
  return stylesheet.slice(start);
}

describe("dynamic viewport fallback (#1648)", () => {
  it("h-dvh falls back to 100vh in browsers without dvh support", () => {
    expect(supportsBlock()).toMatch(/\.h-dvh\s*\{\s*height:\s*100vh/);
  });

  it("min-h-dvh falls back to 100vh in browsers without dvh support", () => {
    expect(supportsBlock()).toMatch(/\.min-h-dvh\s*\{\s*min-height:\s*100vh/);
  });

  it("fallback sits in the utilities cascade layer (cannot unseat 100dvh in modern browsers)", () => {
    // If this rule were unlayered, unlayered CSS beats layered CSS in the
    // cascade and 100vh would override Tailwind's layered 100dvh in
    // modern browsers too — reintroducing the regression. Pin the layer.
    const layered = stylesheet.match(/@layer\s+utilities\s*\{[^@]*@supports not \(height: 100dvh\)/s);
    expect(layered).not.toBeNull();
  });
});
