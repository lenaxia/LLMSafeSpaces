import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

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

  // Stub settings API so UserSettingsTab renders.
  await page.route(`${API}/users/me/settings`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ settings: {}, schemaVersion: 1 }),
    });
  });
  await page.route(`${API}/users/me/settings/schema`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ settings: [], schemaVersion: 1 }),
    });
  });

  // Stub provider credentials.
  await page.route(`${API}/provider-credentials`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify([]),
    });
  });

  // Stub orgs so MyOrganisationTab doesn't crash.
  await page.route(`${API}/orgs`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify([]),
    });
  });

  // Stub secrets.
  await page.route(`${API}/secrets`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify([]),
    });
  });

  // Stub API keys.
  await page.route(`${API}/auth/api-keys`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify([]),
    });
  });
}

test.describe("Settings deep-linking", () => {
  test.beforeEach(async ({ page }) => {
    await mockAuthenticated(page);
  });

  test("/settings redirects to /settings/preferences", async ({ page }) => {
    await page.goto("/settings");

    // URL should be /settings/preferences after redirect.
    await expect(page).toHaveURL(/\/settings\/preferences/, { timeout: 5000 });

    // The sidebar should render the Settings heading.
    await expect(page.getByText("Settings")).toBeVisible();
  });

  test("can deep-link to /settings/secrets", async ({ page }) => {
    await page.goto("/settings/secrets");

    await expect(page).toHaveURL(/\/settings\/secrets/, { timeout: 5000 });

    // The Secrets tab should render.
    await expect(page.getByText("Secrets")).toBeVisible();
  });

  test("can deep-link to /settings/api-keys", async ({ page }) => {
    await page.goto("/settings/api-keys");

    await expect(page).toHaveURL(/\/settings\/api-keys/, { timeout: 5000 });

    // The API Keys tab should render.
    await expect(page.getByText("API Keys")).toBeVisible();
  });

  test("can deep-link to /settings/my-organisation", async ({ page }) => {
    await page.goto("/settings/my-organisation");

    await expect(page).toHaveURL(/\/settings\/my-organisation/, {
      timeout: 5000,
    });

    await expect(page.getByText("My Organisation")).toBeVisible();
  });

  test("can navigate between tabs via sidebar links", async ({ page }) => {
    await page.goto("/settings/preferences");

    await expect(page.getByText("Settings")).toBeVisible({ timeout: 5000 });

    // Click "Secrets" in the sidebar.
    await page.getByText("Secrets").first().click();

    await expect(page).toHaveURL(/\/settings\/secrets/, { timeout: 5000 });

    // Click "API Keys" in the sidebar.
    await page.getByText("API Keys").first().click();

    await expect(page).toHaveURL(/\/settings\/api-keys/, { timeout: 5000 });
  });
});
