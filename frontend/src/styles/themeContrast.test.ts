import { describe, expect, it } from "vitest";
import {
  alphaBlend,
  contrastRatio,
  parseThemeTokens,
  readStylesheet,
  token,
  type Rgb,
} from "../test/wcag";

/**
 * #1716 — light-scheme WCAG AA audit, at the token level.
 *
 * Every pair asserted here is a pair the app actually renders text on
 * (chat timeline surfaces, banners, buttons, forms — see the audit list
 * in the #1716 worklog). Thresholds: 4.5:1 normal text (WCAG 2.1 SC 1.4.3);
 * all the asserted-on sizes (text-xs/sm/body) are normal text.
 *
 * jsdom cannot compute Tailwind cascades, so the math runs on the parsed
 * token values — identical inputs, identical arithmetic to the browser.
 */

const { light, dark } = parseThemeTokens();

const NORMAL_TEXT = 4.5;

function pairRatio(fgToken: string, bgToken: string, tokens = light): number {
  const fg = tokens[fgToken];
  const bg = tokens[bgToken];
  if (!fg || !bg) throw new Error(`missing token: ${!fg ? fgToken : bgToken}`);
  return contrastRatio(fg, bg);
}

describe("light scheme WCAG AA token pairs (#1716)", () => {
  // ── Reading-surface pairs that PASS today — pinned so the fix cannot
  //    regress them.
  it("foreground on background ≥ 4.5 (body text everywhere)", () => {
    expect(pairRatio("foreground", "background")).toBeGreaterThanOrEqual(NORMAL_TEXT);
  });

  it("primary-foreground on primary ≥ 4.5 (user bubble, primary buttons)", () => {
    expect(pairRatio("primary-foreground", "primary")).toBeGreaterThanOrEqual(NORMAL_TEXT);
  });

  it("secondary-foreground on secondary ≥ 4.5 (secondary buttons/hover surfaces)", () => {
    expect(pairRatio("secondary-foreground", "secondary")).toBeGreaterThanOrEqual(NORMAL_TEXT);
  });

  it("card-foreground on card ≥ 4.5 (cards/panels)", () => {
    expect(pairRatio("card-foreground", "card")).toBeGreaterThanOrEqual(NORMAL_TEXT);
  });

  it("popover-foreground on popover ≥ 4.5 (menus/tooltips)", () => {
    expect(pairRatio("popover-foreground", "popover")).toBeGreaterThanOrEqual(NORMAL_TEXT);
  });

  // ── The failures #1716 was filed for — RED on the pre-fix palette.
  it("muted-foreground on background ≥ 4.5 (secondary text on page)", () => {
    expect(pairRatio("muted-foreground", "background")).toBeGreaterThanOrEqual(NORMAL_TEXT);
  });

  it("muted-foreground on muted ≥ 4.5 (assistant bubble text-meta, thinking blocks, code headers)", () => {
    // Pre-fix: hsl(215.4 16.3% 46.9%) on hsl(210 40% 96.1%) = 4.34 — RED.
    expect(pairRatio("muted-foreground", "muted")).toBeGreaterThanOrEqual(NORMAL_TEXT);
  });

  it("muted-foreground on secondary ≥ 4.5 (secondary/hover surface carries the same value as muted)", () => {
    expect(pairRatio("muted-foreground", "secondary")).toBeGreaterThanOrEqual(NORMAL_TEXT);
  });

  it("destructive on background ≥ 4.5 (inline error text on cards/forms)", () => {
    // Pre-fix: hsl(0 84.2% 60.2%) on white = 3.76 — RED.
    expect(pairRatio("destructive", "background")).toBeGreaterThanOrEqual(NORMAL_TEXT);
  });

  it("destructive on its own /10 banner tint over background ≥ 4.5 (error banners, destructive Badge)", () => {
    // The banner pattern `bg-destructive/10 text-destructive` (ChatPage
    // error strip, RunDetail error card, Badge destructive variant): the
    // text sits on destructive blended at 10% over the page background.
    const tint = alphaBlend(token(light, "destructive"), token(light, "background"), 0.1);
    expect(contrastRatio(token(light, "destructive"), tint)).toBeGreaterThanOrEqual(NORMAL_TEXT);
  });

  it("destructive-foreground on destructive ≥ 4.5 (destructive buttons, error toasts)", () => {
    // Pre-fix: 3.60 — RED.
    expect(pairRatio("destructive-foreground", "destructive")).toBeGreaterThanOrEqual(NORMAL_TEXT);
  });
});

describe("dark scheme freeze (#1716: dark palette must not change)", () => {
  // The issue is light-scheme only — dark reads fine. The fix adjusts
  // LIGHT tokens and component base utilities, and must leave the dark
  // palette byte-identical. This pins every .dark token's raw value; ANY
  // edit to the dark block fails here and must be a conscious decision
  // with its own justification.
  const DARK_TOKENS_FROZEN: Record<string, string> = {
    background: "hsl(222.2 84% 4.9%)",
    foreground: "hsl(210 40% 98%)",
    muted: "hsl(217.2 32.6% 17.5%)",
    "muted-foreground": "hsl(215 20.2% 65.1%)",
    popover: "hsl(222.2 84% 4.9%)",
    "popover-foreground": "hsl(210 40% 98%)",
    card: "hsl(222.2 84% 4.9%)",
    "card-foreground": "hsl(210 40% 98%)",
    border: "hsl(217.2 32.6% 17.5%)",
    input: "hsl(217.2 32.6% 17.5%)",
    primary: "hsl(210 40% 98%)",
    "primary-foreground": "hsl(222.2 47.4% 11.2%)",
    secondary: "hsl(217.2 32.6% 17.5%)",
    "secondary-foreground": "hsl(210 40% 98%)",
    accent: "hsl(217.2 32.6% 17.5%)",
    "accent-foreground": "hsl(210 40% 98%)",
    destructive: "hsl(0 62.8% 30.6%)",
    "destructive-foreground": "hsl(210 40% 98%)",
    ring: "hsl(212.7 26.8% 83.9%)",
  };

  it("every .dark token value is unchanged", () => {
    const darkBlock = /\.dark\s*\{([^}]*)\}/.exec(readStylesheet())?.[1] ?? "";
    const declared = [...darkBlock.matchAll(/--color-([a-z-]+):\s*(hsl\([^)]*\))/g)];
    expect(declared.length).toBe(Object.keys(DARK_TOKENS_FROZEN).length);
    for (const m of declared) {
      const name = m[1];
      const value = m[2];
      if (!name || !value) continue;
      expect(value).toBe(DARK_TOKENS_FROZEN[name] ?? `(unexpected dark token --color-${name})`);
    }
  });

  it("dark scheme keeps its reading-surface pairs at AA (regression tripwire for future palette edits)", () => {
    const pairs: Array<[string, string]> = [
      ["foreground", "background"],
      ["muted-foreground", "background"],
      ["muted-foreground", "muted"],
      ["primary-foreground", "primary"],
    ];
    for (const [fg, bg] of pairs) {
      expect(pairRatio(fg, bg, dark)).toBeGreaterThanOrEqual(NORMAL_TEXT);
    }
  });

  it("helper sanity: identical colors must measure 1.0", () => {
    const white: Rgb = [255, 255, 255];
    expect(contrastRatio(white, white)).toBeCloseTo(1.0);
  });
});
