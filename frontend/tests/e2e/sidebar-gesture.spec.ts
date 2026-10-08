import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

// #1629 — the full-edge sidebar gesture, end to end in a real browser:
// the full stack (index.css root overscroll policy → app boot → AppShell
// wiring → hook recognition → drawer state) against real layout and the
// real CSS cascade.
//
// Scope honesty: Playwright cannot emulate the OS back-gesture itself
// (that lane is device QA on iOS Safari / Android Chrome — tracked on
// the issue). What this spec proves is the app-level contract: the CSS
// mechanism is actually applied at runtime (computed style on the root
// scroller), the deliberate full-edge gesture opens the drawer 100%
// here, and non-edge swipes stay unclaimed.

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

test.describe("sidebar full-edge gesture (#1629)", () => {
  test("viewport meta carries viewport-fit=cover so safe-area insets engage", async ({ page }) => {
    await mockAuthenticated(page);
    await page.goto("/chat/ws-1/sess-1");
    const meta = page.locator('meta[name="viewport"]');
    await expect(meta).toHaveAttribute("content", /viewport-fit=cover/);
  });

  test("overscroll-behavior-x: none is applied at runtime on the root scroller (html and body)", async ({ page }) => {
    await mockAuthenticated(page);
    await page.goto("/chat/ws-1/sess-1");
    await expect(page.getByRole("button", { name: "Open menu" })).toBeVisible();

    const computed = await page.evaluate(() => ({
      html: getComputedStyle(document.documentElement).overscrollBehaviorX,
      body: getComputedStyle(document.body).overscrollBehaviorX,
    }));
    // The CSS half of the fix: this is what removes the browser's
    // horizontal swipe-back navigation at the platform level.
    expect(computed.html).toBe("none");
    expect(computed.body).toBe("none");
  });

  test("full-edge swipe opens the drawer through the full stack", async ({ page }) => {
    await mockAuthenticated(page);
    await page.goto("/chat/ws-1/sess-1");
    await expect(page.getByRole("button", { name: "Open menu" })).toBeVisible();

    // The absolute left edge — the #1626 strip is gone; with
    // overscroll-behavior-x owning the gesture, the edge is the app's.
    await dispatchSwipe(page, 8, 160, 300);
    await expect(page.getByRole("button", { name: "Close menu" })).toBeVisible({ timeout: 5000 });
  });

  test("non-edge swipe does not open the drawer (content drags stay unopened)", async ({ page }) => {
    await mockAuthenticated(page);
    await page.goto("/chat/ws-1/sess-1");
    await expect(page.getByRole("button", { name: "Open menu" })).toBeVisible();

    await dispatchSwipe(page, 60, 200, 300);
    // Give any mis-recognition a moment to (wrongly) settle.
    await page.waitForTimeout(300);
    await expect(page.getByRole("button", { name: "Open menu" })).toBeVisible();
  });

  test("hamburger opens the drawer from both halves of the button (the always-works path)", async ({ page }) => {
    await mockAuthenticated(page);
    await page.goto("/chat/ws-1/sess-1");
    const toggle = page.getByRole("button", { name: "Open menu" });
    await expect(toggle).toBeVisible();

    // The button's left half sits inside the 30px edge band (header
    // px-3 + 36px button). The #1626 r2 review proved a touchstart
    // preventDefault over a tappable control suppresses its synthetic
    // click — so the #1629 claim must skip interactive targets. Tap the
    // LEFT edge of the button first (the r2 regression class), then the
    // center.
    const box = await toggle.boundingBox();
    expect(box).not.toBeNull();
    await page.touchscreen.tap(box!.x + 6, box!.y + box!.height / 2);
    await expect(page.getByRole("button", { name: "Close menu" })).toBeVisible({ timeout: 5000 });

    // Close (tap the close button) and re-open from the center.
    const close = page.getByRole("button", { name: "Close menu" });
    const closeBox = await close.boundingBox();
    await page.touchscreen.tap(closeBox!.x + closeBox!.width / 2, closeBox!.y + closeBox!.height / 2);
    await expect(page.getByRole("button", { name: "Open menu" })).toBeVisible({ timeout: 5000 });
    await page.touchscreen.tap(box!.x + box!.width / 2, box!.y + box!.height / 2);
    await expect(page.getByRole("button", { name: "Close menu" })).toBeVisible({ timeout: 5000 });
  });
});
