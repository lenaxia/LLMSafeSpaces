/**
 * E2E for the platform admin "Versions" tab. Verifies the full wiring
 * (frontend tab → API client → backend /admin/platform-info → rendered table)
 * in a real browser with a mocked backend.
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

const API_PREFIX = "**/api/v1";

async function mockAdminAuth(page: Page) {
  await page.route(`${API_PREFIX}/auth/me`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "admin-1", email: "admin@test.com", role: "admin" }) });
  });
  await page.route(`${API_PREFIX}/auth/config`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: true, oidcEnabled: false, instanceName: "test" }) });
  });
  await page.route(`${API_PREFIX}/users/me/settings`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ settings: {}, schemaVersion: 1 }) });
  });
  await page.route(`${API_PREFIX}/users/me/settings/schema`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ settings: [], schemaVersion: 1 }) });
  });
  await page.route(`${API_PREFIX}/orgs`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
  });
  await page.route(`${API_PREFIX}/events`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
  });
}

const MOCK_VERSIONS = {
  api: "0.4.5",
  controller: "0.4.5",
  frontend: "0.4.5",
  relayRouter: "0.4.3",
  baseRuntime: "0.4.5",
};

test.describe("Platform Versions tab", () => {
  test("displays all component versions from the API", async ({ page }) => {
    await mockAdminAuth(page);
    await page.route(`${API_PREFIX}/admin/platform-info`, async (route: Route) => {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(MOCK_VERSIONS) });
    });

    await page.goto("/admin/versions");

    // Title + table render.
    await expect(page.getByText("Platform Versions")).toBeVisible({ timeout: 10_000 });
    // Every component label + its version is rendered.
    await expect(page.getByText("API")).toBeVisible();
    await expect(page.getByText("Controller")).toBeVisible();
    await expect(page.getByText("Frontend")).toBeVisible();
    await expect(page.getByText("Relay Router")).toBeVisible();
    await expect(page.getByText("Base Runtime")).toBeVisible();
    // The mixed version (relayRouter: 0.4.3) renders distinctly.
    await expect(page.getByText("0.4.3")).toBeVisible();
  });

  test("shows error + retry when the API fails", async ({ page }) => {
    await mockAdminAuth(page);
    await page.route(`${API_PREFIX}/admin/platform-info`, async (route: Route) => {
      await route.fulfill({ status: 500 });
    });

    await page.goto("/admin/versions");

    await expect(page.getByText(/failed to load/i)).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText("Retry")).toBeVisible();
  });
});
