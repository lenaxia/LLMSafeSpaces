import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

const API = "**/api/v1";

async function mockPlatformAdmin(page: Page) {
  await page.route(`${API}/auth/me`, async (route: Route) => {
    await route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify({ id: "admin-1", email: "admin@test.com", role: "admin" }),
    });
  });
  await page.route(`${API}/auth/config`, async (route: Route) => {
    await route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify({ registrationEnabled: true, oidcEnabled: false, instanceName: "E2E" }),
    });
  });
  await page.route(`${API}/users/me/settings`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ settings: {}, schemaVersion: 1 }) });
  });
  await page.route(`${API}/users/me/settings/schema`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ settings: [], schemaVersion: 1 }) });
  });
  for (const ep of ["/orgs", "/secrets", "/provider-credentials", "/auth/api-keys", "/events"]) {
    await page.route(`${API}${ep}`, async (route: Route) => {
      await route.fulfill({ status: 200, contentType: "application/json", body: "[]" });
    });
  }
}

test.describe("Platform admin Image Factory", () => {
  test.beforeEach(async ({ page }) => {
    await mockPlatformAdmin(page);

    // Stub image factory endpoints.
    await page.route(`${API}/image-factory/catalog`, async (route: Route) => {
      await route.fulfill({
        status: 200, contentType: "application/json",
        body: JSON.stringify({
          architectures: ["linux/amd64"],
          bases: [{ name: "bookworm", version: "0.6.0", image: "img", tag: "0.6.0", isDefault: true }],
          extensions: [{ id: "ffmpeg", type: "apt", value: "ffmpeg", supportedBases: ["bookworm"] }],
          knownFailures: [],
        }),
      });
    });
    await page.route(`${API}/image-factory/configs`, async (route: Route) => {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
    });
  });

  test("renders the Image Factory tab with config builder", async ({ page }) => {
    await page.goto("/admin/image-factory");
    await page.waitForLoadState("networkidle");

    // The platform Image Factory tab renders the config builder.
    await expect(page.getByRole("heading", { name: "Create Platform Image" })).toBeVisible({ timeout: 8000 });
  });

  test("deep-link to /admin/image-factory works", async ({ page }) => {
    await page.goto("/admin");
    await page.waitForLoadState("networkidle");

    // Navigate via sidebar.
    await page.getByRole("link", { name: "Image Factory" }).click();
    await expect(page).toHaveURL(/\/admin\/image-factory/, { timeout: 5000 });
    await expect(page.getByRole("heading", { name: "Create Platform Image" })).toBeVisible();
  });
});
