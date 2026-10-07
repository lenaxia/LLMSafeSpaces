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
