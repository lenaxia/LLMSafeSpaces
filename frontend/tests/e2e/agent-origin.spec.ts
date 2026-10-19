/**
 * E2E for agent-originated message rendering (#1465).
 *
 * Proves in a real browser: a user-role message delivered by the agentd
 * send_message tool (carrying the agent-message-v1 provenance sentinel)
 * renders USER-SIDE with the provenance badge — the sentinel line itself
 * never visible, the payload intact, both attestation modes labeled. The
 * mocked history mirrors the platform-session contract shape the adapter
 * emits (pkg/session.Message), exactly what a delivered send_message looks
 * like after agentd composes the sentinel.
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

const WORKSPACE_ID = "ws-agent-origin-e2e";
const SESSION_ID = "ses-agent-origin-e2e";
const API = "**/api/v1";
const FROM_SESSION = "ses_f499ee9e6ffe52BJ8jxc2TEQQJ";

function sentineledUserText(mode: string): string {
  return `<!-- lsp:agent-message-v1 {"fromSession":"${FROM_SESSION}","mode":"${mode}"} -->\nStatus update: the vitest run is green.`;
}

async function setupAPIMocks(page: Page, messages: unknown[]) {
  await page.route(`${API}/auth/login`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ token: "e2e-token", user: { id: "u1", username: "tester", role: "user" } }) });
  });
  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true }) }));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false, instanceName: "test" }) }));

  await page.route(`${API}/workspaces`, async (r: Route) => {
    if (r.request().method() !== "GET") { await r.continue(); return; }
    await r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WORKSPACE_ID, name: "Agent Origin E2E", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
  });

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/status`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" } }) }));

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/activate`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ resumed: WORKSPACE_ID }) }));

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions?*`, (r: Route) => {
    if (r.request().method() === "POST") {
      r.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, sessionId: SESSION_ID }) });
      return;
    }
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([{ id: SESSION_ID, title: "Agent Origin Session", messageCount: messages.length, status: "idle" }]) });
  });

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/ensure`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/seen`, (r: Route) => r.fulfill({ status: 204 }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, title: "Agent Origin Session" }) }));

  // REST history — platform-session contract shape (flat array). The
  // trailing * catches the ?limit=50 query getHistoryPage appends
  // (playwright patterns match the full URL including query).
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/message*`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(messages) }));

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, (r: Route) =>
    r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: `data: ${JSON.stringify({ type: "workspace.phase", phase: "Active" })}\n\n` }));

  await mockIdleContractStream(page, `${API}/workspaces/${WORKSPACE_ID}/contract-events`, SESSION_ID);
}

async function openChat(page: Page) {
  await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
}

test.describe("agent-originated message rendering (#1465)", () => {
  test("sentineled user message renders the provenance badge, sentinel hidden", async ({ page }) => {
    await setupAPIMocks(page, [
      { id: "m1", type: "user", parts: [{ type: "text", text: sentineledUserText("injected") }] },
      { id: "m2", type: "assistant", parts: [{ type: "text", text: "Acknowledged — continuing." }] },
    ]);
    await openChat(page);

    const badge = page.getByTestId("agent-origin-badge");
    await expect(badge).toBeVisible({ timeout: 10_000 });
    await expect(badge).toContainText(`message from session ${FROM_SESSION}`);
    // Injected origin carries no self-declared marker.
    await expect(badge).not.toContainText("self-declared");
    // The payload renders; the machine sentinel never does.
    await expect(page.getByText("Status update: the vitest run is green.")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText(/lsp:agent-message-v1/)).toHaveCount(0);
  });

  test("self-declared origin is visibly labeled (degraded-pod fallback)", async ({ page }) => {
    await setupAPIMocks(page, [
      { id: "m1", type: "user", parts: [{ type: "text", text: sentineledUserText("self-declared") }] },
    ]);
    await openChat(page);

    const badge = page.getByTestId("agent-origin-badge");
    await expect(badge).toBeVisible({ timeout: 10_000 });
    await expect(badge).toContainText(`message from session ${FROM_SESSION}`);
    await expect(badge).toContainText("self-declared origin");
  });

  test("plain human message renders no badge", async ({ page }) => {
    await setupAPIMocks(page, [
      { id: "m1", type: "user", parts: [{ type: "text", text: "Just a human message." }] },
    ]);
    await openChat(page);

    await expect(page.getByText("Just a human message.")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId("agent-origin-badge")).toHaveCount(0);
  });

  // #1465 owner report: the badge TRUNCATED the ID behind an ellipsis
  // (nowrap on the label span) — a bug class text-content assertions
  // cannot see in any harness. This row observes LAYOUT in a real
  // browser at a narrow viewport: the label + 30-char ID (~357px at
  // 11px monospace) cannot fit the ~300px message column, so a healthy
  // badge WRAPS to two+ lines (single line ≈ 20px incl. padding and
  // border; wrapped ≥ ~33px — the 26px threshold splits both with
  // margin), and nothing overflows horizontally. A reintroduced
  // truncate/nowrap anywhere in the badge's constraint chain keeps the
  // box one line tall and this row fails.
  test("the origin session ID wraps at a narrow viewport instead of truncating", async ({ page }) => {
    await page.setViewportSize({ width: 375, height: 812 });
    await setupAPIMocks(page, [
      { id: "m1", type: "user", parts: [{ type: "text", text: sentineledUserText("injected") }] },
    ]);
    await openChat(page);

    const badge = page.getByTestId("agent-origin-badge");
    await expect(badge).toBeVisible({ timeout: 10_000 });
    const box = await badge.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.height).toBeGreaterThan(26);
    // No horizontal overflow in the LABEL SPAN — the flex item that
    // carries the text (blockified: real scroll metrics). The badge
    // DIV is the WRONG element for this check: an `overflow:hidden`
    // descendant (the historical truncate) clips its own overflow and
    // per css-overflow-3 it never reaches the flex container, so the
    // badge-level check passes under exactly the canonical mutation
    // (r3 finding, validated: badge 289 == 289 while the label span
    // reads 337 > 255). The height assertion above carries the
    // mutation coverage; this is the live secondary signal.
    const label = page.getByTestId("agent-origin-label");
    const noOverflow = await label.evaluate((el) => el.scrollWidth <= el.clientWidth);
    expect(noOverflow).toBe(true);
  });

  // #1716 r1: browser-level contrast gate. The unit pins resolve Tailwind
  // classes against parsed tokens — a resolver-vs-cascade drift (a utility
  // the harness mis-models, a token emitted differently than authored)
  // would pass units and still ship invisible text. This row reads the
  // badge's REAL computed colors in Chromium, composites the translucent
  // badge bg over its ancestor surfaces exactly as the renderer does, and
  // asserts WCAG ≥ 4.5:1 in both schemes. The theme is forced via the
  // same localStorage key ThemeProvider reads at mount.
  for (const theme of ["light", "dark"] as const) {
    test(`provenance badge meets WCAG AA contrast in ${theme} theme (computed styles)`, async ({ page }) => {
      await page.addInitScript((t) => localStorage.setItem("lsp-theme", t), theme);
      await setupAPIMocks(page, [
        { id: "m1", type: "user", parts: [{ type: "text", text: sentineledUserText("injected") }] },
      ]);
      await openChat(page);

      const badge = page.getByTestId("agent-origin-badge");
      await expect(badge).toBeVisible({ timeout: 10_000 });

      const measured = await badge.evaluate((el) => {
        type Rgba = { r: number; g: number; b: number; a: number };
        // Tailwind v4 serializes computed colors beyond rgb(): palette
        // literals resolve to oklch(...), alpha-diluted utilities to
        // oklab(... / 0.4). Parse every form the engine can emit and
        // convert to sRGB (oklab→linear sRGB per the CSS Color 4
        // reference), else layers silently drop and the gate measures
        // garbage.
        const parse = (s: string): Rgba | null => {
          const split = (body: string) => {
            const [mainRaw, alpha] = body.split("/");
            const main = mainRaw ?? body;
            const n = main.trim().split(/[\s,]+/).map(Number);
            return { n, alphaGiven: alpha !== undefined, a: alpha !== undefined ? Number(alpha.trim()) : 1 };
          };
          const alphaOf = (s: ReturnType<typeof split>) =>
            s.alphaGiven ? s.a : s.n[3] ?? 1;
          const gamma = (v: number) =>
            v <= 0.0031308 ? 12.92 * v : 1.055 * Math.pow(v, 1 / 2.4) - 0.055;
          const oklabToRgb = (L: number, a: number, b: number): [number, number, number] => {
            const l_ = L + 0.3963377774 * a + 0.2158037573 * b;
            const m_ = L - 0.1055613458 * a - 0.0638541728 * b;
            const s_ = L - 0.0894841775 * a - 1.291485548 * b;
            const l = l_ * l_ * l_, m = m_ * m_ * m_, sc = s_ * s_ * s_;
            const lin = [
              4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * sc,
              -1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * sc,
              -0.0041960863 * l - 0.7034186147 * m + 1.707614701 * sc,
            ];
            return lin.map((v) => Math.round(Math.min(255, Math.max(0, gamma(v) * 255)))) as [number, number, number];
          };
          let m = /^rgba?\(([^)]+)\)$/.exec(s);
          if (m?.[1]) {
            const parts = split(m[1]);
            const [r, g, b] = parts.n;
            // rgba() may carry POSITIONAL alpha (rgba(r,g,b,a)) or the
            // modern slash form (rgb(r g b / a)) — read whichever is
            // present. Missing this misreads transparent backgrounds
            // (rgba(0,0,0,0)) as OPAQUE black and truncates the layer
            // walk (validated: the dark run composited over black).
            if (r !== undefined && g !== undefined && b !== undefined) {
              return { r, g, b, a: alphaOf(parts) };
            }
          }
          m = /^oklab\(([^)]+)\)$/.exec(s);
          if (m?.[1]) {
            const { n, a } = split(m[1]);
            const [L, A, B] = n;
            if (L !== undefined && A !== undefined && B !== undefined) {
              const [r, g, b] = oklabToRgb(L, A, B);
              return { r, g, b, a };
            }
          }
          m = /^oklch\(([^)]+)\)$/.exec(s);
          if (m?.[1]) {
            const { n, a } = split(m[1]);
            const [L, C, H] = n;
            if (L !== undefined && C !== undefined && H !== undefined) {
              const rad = (H * Math.PI) / 180;
              const [r, g, b] = oklabToRgb(L, C * Math.cos(rad), C * Math.sin(rad));
              return { r, g, b, a };
            }
          }
          m = /^color\(srgb ([^)]+)\)$/.exec(s);
          if (m?.[1]) {
            const { n, a } = split(m[1]);
            const [r, g, b] = n;
            if (r !== undefined && g !== undefined && b !== undefined) {
              return { r: Math.round(r * 255), g: Math.round(g * 255), b: Math.round(b * 255), a };
            }
          }
          m = /^#([0-9a-f]{3,8})$/i.exec(s.trim());
          if (m?.[1]) {
            const h = m[1];
            const full = h.length === 3 ? h.split("").map((c) => c + c).join("") : h;
            return {
              r: parseInt(full.slice(0, 2), 16),
              g: parseInt(full.slice(2, 4), 16),
              b: parseInt(full.slice(4, 6), 16),
              a: full.length === 8 ? parseInt(full.slice(6, 8), 16) / 255 : 1,
            };
          }
          return null;
        };
        const blend = (fg: Rgba, bg: Rgba, alpha: number): Rgba => ({
          r: fg.r * alpha + bg.r * (1 - alpha),
          g: fg.g * alpha + bg.g * (1 - alpha),
          b: fg.b * alpha + bg.b * (1 - alpha),
          a: 1,
        });
        const lum = ({ r, g, b }: Rgba) => {
          const ch = (v: number) => {
            const s = v / 255;
            return s <= 0.04045 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4);
          };
          return 0.2126 * ch(r) + 0.7152 * ch(g) + 0.0722 * ch(b);
        };
        // fg: nearest computed color with alpha (walks up for inheritance).
        let fg: Rgba | null = null;
        for (let n: HTMLElement | SVGElement | null = el; n && !fg; n = n.parentElement) {
          const c = parse(getComputedStyle(n).color);
          if (c && c.a > 0) fg = c;
        }
        if (!fg) return { ratio: 0, dark: false, reason: "no fg" };
        // bg: stack this element's translucent background(s) outward until
        // an opaque ancestor layer (the bubble), then composite inward.
        const layers: Rgba[] = [];

        for (let n: HTMLElement | SVGElement | null = el; n instanceof HTMLElement; n = n.parentElement) {
          const c = parse(getComputedStyle(n).backgroundColor);
          if (c && c.a > 0) {
            layers.push(c);
            if (c.a >= 1) break;
          }
        }
        if (layers.length === 0) return { ratio: 0, dark: false, reason: "no bg layers" };
        let bg = layers[layers.length - 1]!;
        for (let i = layers.length - 2; i >= 0; i--) bg = blend(layers[i]!, bg, layers[i]!.a);
        const l1 = lum(fg);
        const l2 = lum(bg);
        const ratio = (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05);
        return { ratio, dark: document.documentElement.classList.contains("dark") };
      });

      expect(measured.reason).toBeUndefined();
      expect(measured.dark).toBe(theme === "dark");
      expect(measured.ratio).toBeGreaterThanOrEqual(4.5);
    });
  }
});
