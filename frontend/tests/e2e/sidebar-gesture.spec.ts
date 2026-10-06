import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

// #1623 — the sidebar gesture zone, end to end in a real browser: the
// full stack (index.html viewport meta → app boot → AppShell wiring →
// hook recognition → drawer state) against real layout (the handle's
// live rect, not a mocked one).
//
// Scope honesty: Playwright cannot emulate the OS back-gesture itself
// (that lane is device QA on iOS Safari / Android Chrome — tracked on
// the issue). What this spec proves is the app-level contract: the
// deliberate gesture opens the drawer 100% here, and the absolute-edge
// gesture is left untouched by app code.

const API = "**/api/v1";

async function mockAuthenticated(page: Page) {
  await page.route(`${API}/auth/me`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        id: "e2e-user",
        username: "e2e",
        email: "e2e@test.com",
        role: "member",
        active: true,
      }),
    });
  });
  await page.route(`${API}/auth/config`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        registrationEnabled: true,
        oidcEnabled: false,
        instanceName: "E2E Test",
      }),
    });
  });
  await page.route(`${API}/workspaces`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ items: [], pagination: { limit: 20, offset: 0, total: 0 } }),
    });
  });
}

// dispatchSwipe synthesizes a horizontal touch sequence in the browser,
// starting on whatever element actually sits at (fromX, y) — real hit
// testing, real propagation.
async function dispatchSwipe(page: Page, fromX: number, toX: number, y: number) {
  await page.evaluate<void, [number, number, number]>(([fx, tx, yy]) => {
    const mk = (x: number) => new Touch({ identifier: 1, target: document.elementFromPoint(x, yy)!, clientX: x, clientY: yy });
    const opts = (x: number) => ({ touches: [mk(x)], changedTouches: [mk(x)], bubbles: true, cancelable: true });
    const el = document.elementFromPoint(fx, yy) as HTMLElement;
    el.dispatchEvent(new TouchEvent("touchstart", opts(fx)));
    el.dispatchEvent(new TouchEvent("touchmove", opts(tx)));
    el.dispatchEvent(new TouchEvent("touchend", { touches: [], changedTouches: [mk(tx)], bubbles: true, cancelable: true }));
  }, [fromX, toX, y]);
}

test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

test.describe("sidebar gesture zone (#1623)", () => {
  test("viewport meta carries viewport-fit=cover so safe-area insets engage", async ({ page }) => {
    await mockAuthenticated(page);
    await page.goto("/chat/ws-1/sess-1");
    const meta = page.locator('meta[name="viewport"]');
    await expect(meta).toHaveAttribute("content", /viewport-fit=cover/);
  });

  test("handle-zone swipe opens the drawer through the full stack", async ({ page }) => {
    await mockAuthenticated(page);
    await page.goto("/chat/ws-1/sess-1");
    await expect(page.getByRole("button", { name: "Open menu" })).toBeVisible();

    // The handle strip: 28px wide, 16px in (safe-area inset is 0 on the
    // desktop browser, so the live rect is exactly 16..44).
    const handle = page.locator("div.touch-none");
    await expect(handle).toBeAttached();
    const box = await handle.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.x).toBeGreaterThanOrEqual(16);

    await dispatchSwipe(page, 30, 160, 300);
    await expect(page.getByRole("button", { name: "Close menu" })).toBeVisible({ timeout: 5000 });
  });

  test("absolute-edge swipe does not open the drawer (the OS back-gesture zone stays untouched)", async ({ page }) => {
    await mockAuthenticated(page);
    await page.goto("/chat/ws-1/sess-1");
    await expect(page.getByRole("button", { name: "Open menu" })).toBeVisible();

    await dispatchSwipe(page, 8, 160, 300);
    // Give any mis-recognition a moment to (wrongly) settle.
    await page.waitForTimeout(300);
    await expect(page.getByRole("button", { name: "Open menu" })).toBeVisible();
  });
});
