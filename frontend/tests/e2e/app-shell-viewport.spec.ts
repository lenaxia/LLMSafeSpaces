// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// #1648 mobile viewport regression — served-artifact tier.
//
// Scope honesty (the #1629 precedent): headless Chromium has no browser
// chrome and no soft keyboard — 100vh == 100dvh and env() insets are 0px
// there, so a GEOMETRY assertion (shell ≤ viewport) is green both before
// and after the fix and cannot discriminate. The discriminating e2e
// assertions are about the SERVED artifacts:
//   1. the real stylesheet the browser loaded carries .h-dvh{height:100dvh}
//      (Tailwind v4 emits it ONLY if some source file uses the class —
//      red before the fix, green after),
//   2. the @supports-not 100vh fallback for dvh-less browsers is present,
//   3. the [data-osk-open] keyboard override rule is present and LIVE
//      (beats .h-dvh on specificity when the guard sets the attribute),
//   4. the served viewport meta opts Android into resizes-content.
// The unit pins (AppShell.viewport.test.tsx, styles/index.test.ts,
// useOskViewportGuard.test.ts) carry the mechanism-level discrimination;
// soft-keyboard behavior on real iOS/Android remains owner device QA.
import { test, expect, type Page, type Route } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

const WS = "ws-shell-viewport-e2e";
const SESS = "ses-shell-viewport-e2e";
const API = "**/api/v1";

async function setupShellMocks(page: Page) {
  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "testuser", email: "t@t.com", role: "user", active: true }) }));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false, instanceName: "test" }) }));
  await page.route(`${API}/workspaces`, (r: Route) => {
    if (r.request().method() !== "GET") { r.fulfill({ status: 204, body: "" }); return; }
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WS, name: "Shell Viewport Test", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
  });
  await page.route(`${API}/workspaces/${WS}/status`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" }, sessions: [] }) }));
  await page.route(`${API}/workspaces/${WS}/sessions`, (r: Route) => {
    if (r.request().method() !== "GET") { r.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: SESS }) }); return; }
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
  });
  await page.route(`${API}/workspaces/${WS}/sessions/new`, (r: Route) =>
    r.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: SESS }) }));
  await page.route(`${API}/workspaces/${WS}/sessions/*/message`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }));
  await page.route(`${API}/user-timezone`, (r: Route) => r.fulfill({ status: 204, body: "" }));
  // The sidebar sessions query composes origins + active runs into the
  // listing — both must resolve or the queryFn throws on refetch.
  await page.route(`${API}/workspaces/${WS}/session-origins`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ origins: [] }) }));
  await page.route(`${API}/workspaces/${WS}/runs/active`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ runs: [] }) }));
  await mockIdleContractStream(page, `${API}/workspaces/*/contract-events`, SESS);
}

test.describe("#1648 served mobile shell artifacts", () => {
  test("served viewport meta keeps cover and opts into interactive-widget=resizes-content", async ({ page }) => {
    await setupShellMocks(page);
    await page.goto(`/chat/${WS}`);
    await expect(page.getByRole("main")).toBeVisible();
    const content = await page.locator('meta[name="viewport"]').getAttribute("content");
    expect(content).toContain("viewport-fit=cover");
    expect(content).toContain("interactive-widget=resizes-content");
  });

  test("served stylesheet carries the 100dvh rule, the vh @supports fallback, and the osk override", async ({ page }) => {
    await setupShellMocks(page);
    await page.goto(`/chat/${WS}`);
    await expect(page.getByRole("main")).toBeVisible();

    const found = await page.evaluate(() => {
      const seen = {
        dvhRule: false,
        vhFallbackInGuard: false,
        minHFallbackInGuard: false,
        oskOverride: false,
      };
      const visit = (rules: CSSRuleList) => {
        for (const rule of Array.from(rules)) {
          if (rule instanceof CSSSupportsRule) {
            if (/100dvh/i.test(rule.conditionText)) {
              for (const inner of Array.from(rule.cssRules)) {
                if (inner instanceof CSSStyleRule) {
                  const sel = inner.selectorText;
                  const h = inner.style.getPropertyValue("height").trim();
                  const mh = inner.style.getPropertyValue("min-height").trim();
                  if (sel === ".h-dvh" && h === "100vh") seen.vhFallbackInGuard = true;
                  if (sel === ".min-h-dvh" && mh === "100vh") seen.minHFallbackInGuard = true;
                }
              }
            }
            visit(rule.cssRules);
          }
          if (rule instanceof CSSStyleRule) {
            const h = rule.style.getPropertyValue("height").trim();
            if (rule.selectorText === ".h-dvh" && h === "100dvh") seen.dvhRule = true;
            if (/\[data-osk-open\].*\.h-dvh/.test(rule.selectorText) && h.includes("--shell-visual-height")) seen.oskOverride = true;
          }
          if (rule instanceof CSSLayerBlockRule) visit(rule.cssRules);
          if (rule instanceof CSSMediaRule) visit(rule.cssRules);
        }
      };
      for (const sheet of Array.from(document.styleSheets)) {
        try { visit(sheet.cssRules); } catch { /* cross-origin sheet */ }
      }
      return seen;
    });

    // Tailwind JIT only emits .h-dvh when a source file uses it — this is
    // red before the #1648 fix (class absent everywhere) and green after.
    expect(found.dvhRule).toBe(true);
    expect(found.vhFallbackInGuard).toBe(true);
    expect(found.minHFallbackInGuard).toBe(true);
    expect(found.oskOverride).toBe(true);
  });
});

test.describe("#1648 mobile shell geometry (emulated)", () => {
  test.use({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });

  test("shell fits the viewport; the osk override is live and wins over .h-dvh", async ({ page }) => {
    await setupShellMocks(page);
    await page.goto(`/chat/${WS}`);

    const shell = page.locator("div.overscroll-none").first();
    await expect(shell).toBeVisible();
    await expect(shell).toHaveClass(/h-dvh/);

    // Geometry smoke (non-discriminating in headless — vh == dvh here; see
    // the header): the rendered shell never exceeds the emulated viewport.
    const fits = await shell.evaluate((el) => el.getBoundingClientRect().height <= window.innerHeight);
    expect(fits).toBe(true);

    // The keyboard override is LIVE in the served cascade: with the guard's
    // attribute + var set (as useOskViewportGuard does), the shell's used
    // height must follow the var, not 100dvh.
    const height = await page.evaluate(() => {
      const root = document.documentElement;
      root.dataset.oskOpen = "true";
      root.style.setProperty("--shell-visual-height", "500px");
      const el = document.querySelector("div.overscroll-none") as HTMLElement | null;
      const h = el ? getComputedStyle(el).height : null;
      root.removeAttribute("data-osk-open");
      root.style.removeProperty("--shell-visual-height");
      return h;
    });
    expect(height).toBe("500px");
  });
});
