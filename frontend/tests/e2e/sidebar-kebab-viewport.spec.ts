/**
 * E2E for viewport-aware kebab menu positioning. Regression for the bug
 * where the kebab on the bottom-most sidebar session opened its menu past
 * the viewport bottom edge, making it partially unreadable/unusable.
 *
 * The menu is portaled to document.body with position: fixed, so it is
 * positioned relative to the viewport. The assertion is that the menu's
 * bottom edge never exceeds the viewport height, regardless of where the
 * trigger sits in the scrollable sidebar.
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

const WORKSPACE_ID = "ws-kebab-viewport-e2e";
const API_PREFIX = "**/api/v1";

// Generate enough sessions that the last one sits near the bottom of a
// short viewport, exercising the "open near the viewport bottom" path.
const SESSIONS = Array.from({ length: 12 }, (_, i) => ({
  id: `ses_${i}`,
  title: `Session ${i + 1}`,
  messageCount: 1,
  status: "idle",
}));

// Scoped to the sidebar (<aside aria-label="Navigation">) so we target a
// session/workspace kebab inside the nav, NOT the ChatPage header kebab
// (which also has aria-label="Actions" but renders outside the aside).
const sidebarKebab = (page: import("@playwright/test").Page) =>
  page.locator('aside[aria-label="Navigation"] button[aria-label="Actions"]');

async function setupAPIMocks(page: Page) {
  await page.route(`${API_PREFIX}/auth/me`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "testuser", email: "t@t.com", role: "user", active: true }) });
  });
  await page.route(`${API_PREFIX}/auth/config`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false, instanceName: "test" }) });
  });
  await page.route(`${API_PREFIX}/workspaces`, async (route: Route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WORKSPACE_ID, name: "Kebab Viewport Test", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
    } else { await route.continue(); }
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/status`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" }, sessions: [] }) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(SESSIONS) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions/*/message`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/session-events`, async (route: Route) => {
    await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" });
  });
}

test.describe("Sidebar kebab menu viewport awareness", () => {
  test.beforeEach(async ({ page }) => {
    await setupAPIMocks(page);
    // Short viewport so the bottom-most session's kebab is near the bottom
    // edge — the exact condition that used to overflow the menu off-screen.
    await page.setViewportSize({ width: 1024, height: 500 });
  });

  test("menu opened from the bottom-most session stays within the viewport", async ({ page }) => {
    await page.goto(`/chat/${WORKSPACE_ID}/ses_0`);

    // Wait for the sidebar to render the sessions.
    await expect(page.getByText("Session 12")).toBeVisible({ timeout: 10_000 });

    // The last sidebar kebab = the bottom-most session's kebab. Scoped to
    // the sidebar aside so we don't grab the ChatPage header kebab.
    const lastSidebarKebab = sidebarKebab(page).last();
    await lastSidebarKebab.scrollIntoViewIfNeeded();
    await lastSidebarKebab.click();

    const menu = page.getByRole("menu");
    await expect(menu).toBeVisible();

    // The menu must fit inside the viewport: its bottom edge (y + height)
    // must not exceed the viewport height. Before the fix, the menu opened
    // directly below the trigger and overflowed past the bottom edge.
    const box = await menu.boundingBox();
    expect(box).not.toBeNull();
    const viewportHeight = page.viewportSize()?.height ?? 0;
    const menuBottom = box!.y + box!.height;
    expect(menuBottom).toBeLessThanOrEqual(viewportHeight);
    // And the top edge must be within the viewport too.
    expect(box!.y).toBeGreaterThanOrEqual(0);
  });

  test("tall menu in a very short viewport is capped and scrollable, not overflowing", async ({ page }) => {
    // Very short viewport so even a normal-sized menu is taller than the
    // available room — exercises the maxHeight + overflow-y-auto path.
    await page.setViewportSize({ width: 1024, height: 120 });
    await page.goto(`/chat/${WORKSPACE_ID}/ses_0`);

    await expect(page.getByText("Session 1", { exact: true })).toBeVisible({ timeout: 10_000 });

    await sidebarKebab(page).first().click();
    const menu = page.getByRole("menu");
    await expect(menu).toBeVisible();

    const box = await menu.boundingBox();
    expect(box).not.toBeNull();
    const viewportHeight = page.viewportSize()?.height ?? 0;
    // The cap must keep the menu within the viewport even though its
    // natural height exceeds it.
    expect(box!.y + box!.height).toBeLessThanOrEqual(viewportHeight);
    // overflow-y-auto is applied so the capped menu scrolls internally.
    const overflowY = await menu.evaluate((el) => getComputedStyle(el).overflowY);
    expect(["auto", "scroll"]).toContain(overflowY);
  });
});
