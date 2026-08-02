import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

const API = "**/api/v1";

async function mockUnauthenticated(page: Page) {
  await page.route(`${API}/auth/config`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        registrationEnabled: true,
        oidcEnabled: true,
        instanceName: "E2E Test",
      }),
    });
  });
  await page.route(`${API}/auth/me`, async (route: Route) => {
    await route.fulfill({ status: 401 });
  });
}

test.describe("SSO org-slug shortcut /sso/:orgSlug", () => {
  test("redirects to backend SSO start endpoint for valid org", async ({ page }) => {
    await mockUnauthenticated(page);

    const idpAuthorizeURL = "https://idp.example.com/authorize?client_id=cid&redirect_uri=...";
    await page.route(`${API}/auth/sso/acme/start`, async (route: Route) => {
      await route.fulfill({
        status: 302,
        headers: { Location: idpAuthorizeURL },
      });
    });

    await page.goto("/sso/acme");

    await expect(page).toHaveURL(idpAuthorizeURL, { timeout: 10000 });
  });

  test("shows not_configured error on login page for org without SSO", async ({ page }) => {
    await mockUnauthenticated(page);

    await page.route(`${API}/auth/sso/ghost/start`, async (route: Route) => {
      await route.fulfill({
        status: 302,
        headers: { Location: "/?sso=not_configured" },
      });
    });

    await page.goto("/sso/ghost");

    await expect(page).toHaveURL(/\/login/, { timeout: 10000 });
    await expect(
      page.getByText("This organisation does not have single sign-on configured."),
    ).toBeVisible({ timeout: 10000 });
  });
});
