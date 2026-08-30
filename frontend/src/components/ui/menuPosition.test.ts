import { describe, expect, it } from "vitest";
import { computeMenuPosition } from "./menuPosition";

// ── Viewport-aware positioning ──────────────────────────────────────────
//
// Regression coverage for the kebab menu that opened off the bottom of the
// screen when triggered from the last sidebar item. The positioning logic
// is a pure function so it can be tested deterministically without jsdom
// layout (which doesn't compute geometry).
// ── Viewport-aware positioning ──────────────────────────────────────────
//
// Regression coverage for the kebab menu that opened off the bottom of the
// screen when triggered from the last sidebar item. The positioning logic
// is extracted into a pure function so it can be tested deterministically
// without jsdom layout (which doesn't compute geometry).

describe("computeMenuPosition (viewport-aware)", () => {
  const VW = 1280;
  const VH = 800;
  const MENU_W = 160;
  const MENU_H = 150;

  it("opens below the button when there is room (default)", () => {
    const btn = { top: 100, bottom: 124, left: 50, right: 90 };
    const r = computeMenuPosition(btn, { width: MENU_W, height: MENU_H }, { width: VW, height: VH }, "left");
    expect(r.top).toBe(128); // btn.bottom + 4
    expect(r.left).toBe(50);
    expect(r.maxHeight).toBeUndefined();
  });

  it("flips above when not enough room below but enough above", () => {
    // Button near the bottom: only 20px below, plenty above.
    const btn = { top: 700, bottom: 724, left: 0, right: 40 };
    const r = computeMenuPosition(btn, { width: MENU_W, height: MENU_H }, { width: VW, height: VH }, "left");
    // spaceBelow = 800-724-4 = 72 (< 150); spaceAbove = 700-4 = 696 (>= 150) → flip above.
    expect(r.top).toBe(700 - MENU_H - 4); // 546
    expect(r.maxHeight).toBeUndefined();
  });

  it("opens above and clamps to PAD when above has more room and the menu is taller than both sides", () => {
    // Both sides smaller than menu; above is larger → pick above, clamp top to PAD.
    const btn = { top: 420, bottom: 440, left: 50, right: 90 };
    // spaceBelow = 800-440-4 = 356; spaceAbove = 420-4 = 416. Above is larger → clamp.
    const r = computeMenuPosition(btn, { width: MENU_W, height: 700 }, { width: VW, height: VH }, "left");
    expect(r.maxHeight).toBeDefined();
    expect(r.top).toBe(8); // clamped to PAD
  });

  it("caps height when the menu is taller than the viewport on the chosen side (open below)", () => {
    // Tiny space below, huge menu, below is the larger side.
    const btn = { top: 100, bottom: 120, left: 0, right: 40 };
    const r = computeMenuPosition(btn, { width: MENU_W, height: 900 }, { width: VW, height: VH }, "left");
    // spaceBelow = 800-120-4 = 676 (larger than spaceAbove=96) → below, capped.
    expect(r.top).toBe(124);
    expect(r.maxHeight).toBe(800 - 8 - 124); // vh - PAD - top
  });

  it("clamps left so a left-aligned menu near the right edge stays on screen", () => {
    const btn = { top: 100, bottom: 124, left: 1200, right: 1240 };
    const r = computeMenuPosition(btn, { width: MENU_W, height: MENU_H }, { width: VW, height: VH }, "left");
    // left would be 1200, but 1200+160 = 1360 > 1280-8 → clamp to 1280-8-160 = 1112.
    expect(r.left).toBe(VW - 8 - MENU_W);
  });

  it("clamps left for a right-aligned menu that would overflow", () => {
    const btn = { top: 100, bottom: 124, left: 1260, right: 1280 };
    const r = computeMenuPosition(btn, { width: MENU_W, height: MENU_H }, { width: VW, height: VH }, "right");
    // right-aligned: left = 1280-160 = 1120; 1120+160 = 1280 <= 1272? no, > 1272 → clamp.
    expect(r.left).toBe(VW - 8 - MENU_W);
  });

  it("never returns a negative left", () => {
    const btn = { top: 100, bottom: 124, left: -50, right: -10 };
    const r = computeMenuPosition(btn, { width: MENU_W, height: MENU_H }, { width: VW, height: VH }, "left");
    expect(r.left).toBe(8); // PAD
  });

  it("never returns a top above the PAD margin", () => {
    // Button near the bottom; huge menu. Above is the larger side, so the
    // menu opens upward and its top clamps to PAD.
    const btn = { top: 780, bottom: 790, left: 0, right: 40 };
    const r = computeMenuPosition(btn, { width: MENU_W, height: 900 }, { width: VW, height: VH }, "left");
    expect(r.top).toBe(8); // clamped to PAD
    expect(r.maxHeight).toBe(780 - 4 - 8);
  });

  it("respects the right align anchor when it fits", () => {
    const btn = { top: 100, bottom: 124, left: 100, right: 260 };
    const r = computeMenuPosition(btn, { width: MENU_W, height: MENU_H }, { width: VW, height: VH }, "right");
    expect(r.left).toBe(260 - MENU_W); // right edges align
  });
});
