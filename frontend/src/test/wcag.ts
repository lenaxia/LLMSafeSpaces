import { readFileSync } from "node:fs";
import { resolve } from "node:path";

/**
 * #1716 — WCAG contrast math + theme-token resolution for tests.
 *
 * jsdom never applies the Tailwind stylesheet (vitest stubs CSS imports),
 * so computed-style reads can't see token colors. These helpers do what
 * jsdom can't: parse the token values out of src/styles/index.css, resolve
 * the Tailwind color utilities a component actually carries (class-level,
 * including `/alpha` dilution), and compute WCAG 2.1 contrast ratios for
 * a foreground/background pair — the same arithmetic a real browser's
 * cascade performs, on the same inputs.
 *
 * Precedent: styles/index.test.ts reads the stylesheet via fs for the
 * same reason (documented there).
 */

export type Rgb = readonly [number, number, number];

/** hsl(h s% l%) → sRGB 0-255 triple (integer rounding like the browser). */
export function hslToRgb(h: number, s: number, l: number): Rgb {
  const sat = s / 100;
  const light = l / 100;
  const c = (1 - Math.abs(2 * light - 1)) * sat;
  const x = c * (1 - Math.abs(((h / 60) % 2) - 1));
  const m = light - c / 2;
  let rgb: [number, number, number];
  if (h < 60) rgb = [c, x, 0];
  else if (h < 120) rgb = [x, c, 0];
  else if (h < 180) rgb = [0, c, x];
  else if (h < 240) rgb = [0, x, c];
  else if (h < 300) rgb = [x, 0, c];
  else rgb = [c, 0, x];
  return [
    Math.round((rgb[0] + m) * 255),
    Math.round((rgb[1] + m) * 255),
    Math.round((rgb[2] + m) * 255),
  ];
}

/** WCAG 2.1 relative luminance. */
export function relativeLuminance([r, g, b]: Rgb): number {
  const channel = (v: number) => {
    const s = v / 255;
    return s <= 0.04045 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4);
  };
  return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b);
}

/** WCAG 2.1 contrast ratio (1.0 = identical colors). */
export function contrastRatio(a: Rgb, b: Rgb): number {
  const la = relativeLuminance(a);
  const lb = relativeLuminance(b);
  const [hi, lo] = la >= lb ? [la, lb] : [lb, la];
  return (hi + 0.05) / (lo + 0.05);
}

/** CSS source-over alpha compositing: `fg` at `alpha` over opaque `bg`. */
export function alphaBlend(fg: Rgb, bg: Rgb, alpha: number): Rgb {
  return [
    Math.round(fg[0] * alpha + bg[0] * (1 - alpha)),
    Math.round(fg[1] * alpha + bg[1] * (1 - alpha)),
    Math.round(fg[2] * alpha + bg[2] * (1 - alpha)),
  ];
}

/** The app stylesheet (vitest runs with the frontend package as cwd). */
export function readStylesheet(): string {
  return readFileSync(resolve(process.cwd(), "src/styles/index.css"), "utf8");
}

export interface ThemeTokens {
  /** `--color-*` values from the `@theme` block — the LIGHT scheme. */
  light: Record<string, Rgb>;
  /** `--color-*` values from the `.dark` block — the DARK scheme. */
  dark: Record<string, Rgb>;
}

const HSL_RE = /--color-([a-z-]+):\s*hsl\(([\d.]+)\s+([\d.]+)%\s+([\d.]+)%\)/g;

function tokensIn(cssBlock: string): Record<string, Rgb> {
  const out: Record<string, Rgb> = {};
  for (const m of cssBlock.matchAll(HSL_RE)) {
    const [, name, h, s, l] = m;
    if (!name || !h || !s || !l) continue;
    out[name] = hslToRgb(Number(h), Number(s), Number(l));
  }
  return out;
}

/** Fetch a token or fail loudly (typo'd token names must not silently pass). */
export function token(tokens: Record<string, Rgb>, name: string): Rgb {
  const rgb = tokens[name];
  if (!rgb) throw new Error(`missing token: ${name}`);
  return rgb;
}

/** Parse both scheme palettes out of index.css. */
export function parseThemeTokens(css = readStylesheet()): ThemeTokens {
  const theme = /@theme\s*\{([^}]*)\}/.exec(css);
  const dark = /\.dark\s*\{([^}]*)\}/.exec(css);
  if (!theme?.[1]) throw new Error("no @theme block in index.css");
  if (!dark?.[1]) throw new Error("no .dark block in index.css");
  return { light: tokensIn(theme[1]), dark: tokensIn(dark[1]) };
}

/**
 * Tailwind default-palette literals used by chat surfaces (hex values are
 * identical in Tailwind v3 and v4 for these steps). Only the steps the
 * components under test actually reference are listed — extend on demand.
 */
export const TAILWIND_SLATE: Record<string, Rgb> = {
  "slate-100": [241, 245, 249],
  "slate-600": [71, 85, 105],
  "slate-900": [15, 23, 42],
};

const TEXT_TOKENS = new Set([
  "foreground",
  "muted-foreground",
  "primary-foreground",
  "secondary-foreground",
  "accent-foreground",
  "popover-foreground",
  "card-foreground",
  "destructive",
  "primary",
]);

const BG_TOKENS = new Set([
  "background",
  "muted",
  "primary",
  "secondary",
  "accent",
  "popover",
  "card",
  "destructive",
]);

export interface ColorRef {
  rgb: Rgb;
  alpha: number;
}

export interface ResolvedColors {
  fg?: ColorRef;
  bg?: ColorRef;
}

/**
 * Resolve the text/background color utilities carried by a class list
 * against one scheme's tokens. Non-color utilities (text-xs, font-mono,
 * arbitrary values like text-[11px], borders, …) are ignored.
 *
 * Scheme semantics mirror the real cascade: base utilities apply in BOTH
 * schemes (the `--color-*` custom properties themselves flip values under
 * `.dark`), and a `dark:`-prefixed utility overrides its base twin when
 * the scheme is dark. For "light", `dark:`-prefixed utilities are inert.
 */
export function resolveColorClasses(
  classAttr: string,
  scheme: "light" | "dark",
  tokens: Record<string, Rgb>,
): ResolvedColors {
  const out: ResolvedColors = {};
  const consider = (cls: string) => {
    const m = /^(text|bg)-([a-z0-9-]+)(?:\/(\d+))?$/.exec(cls);
    if (!m) return;
    const kind = m[1];
    const name = m[2];
    const alpha = m[3];
    if (!kind || !name) return;
    const rgb = tokens[name] ?? TAILWIND_SLATE[name];
    if (!rgb) return;
    const ref = { rgb, alpha: alpha ? Number(alpha) / 100 : 1 };
    if (kind === "text" && (TEXT_TOKENS.has(name) || name in TAILWIND_SLATE)) out.fg = ref;
    if (kind === "bg" && (BG_TOKENS.has(name) || name in TAILWIND_SLATE)) out.bg = ref;
  };
  for (const raw of classAttr.split(/\s+/)) {
    if (!raw || raw.startsWith("dark:")) continue;
    consider(raw);
  }
  if (scheme === "dark") {
    for (const raw of classAttr.split(/\s+/)) {
      if (raw.startsWith("dark:")) consider(raw.slice("dark:".length));
    }
  }
  return out;
}

/**
 * Compose an element's resolved fg/bg over the (opaque) ancestor surface
 * it renders on, applying source-over alpha compositing in DOM order
 * (surface → element bg → element fg), and return the WCAG ratio of the
 * resulting fg against the resulting bg.
 */
export function textOnSurfaceRatio(
  colors: ResolvedColors,
  surface: Rgb,
): { fg: Rgb; bg: Rgb; ratio: number } {
  const bg = colors.bg ? alphaBlend(colors.bg.rgb, surface, colors.bg.alpha) : surface;
  if (!colors.fg) throw new Error("no text color utility resolved on element");
  const fg = alphaBlend(colors.fg.rgb, bg, colors.fg.alpha);
  return { fg, bg, ratio: contrastRatio(fg, bg) };
}

/** Opaque background an element sits on, from the ancestor's own classes. */
export function surfaceFromClasses(
  classAttr: string,
  scheme: "light" | "dark",
  tokens: Record<string, Rgb>,
  fallback: Rgb,
): Rgb {
  const { bg } = resolveColorClasses(classAttr, scheme, tokens);
  return bg ? alphaBlend(bg.rgb, fallback, bg.alpha) : fallback;
}
