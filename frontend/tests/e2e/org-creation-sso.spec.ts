/**
 * E2E for the platform-admin org-creation form under Option B (design 0065
 * §4 / #1679): every organisation requires OIDC, and the initial SSO
 * configuration is collected inline, in the same step as creation. Verifies
 * the full wiring (form → API client → POST /orgs body shape) in a real
 * browser with a mocked backend, plus the client-side gate and the backend
 * 400 sso.<field> detail rendering.
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

const API_PREFIX = "**/api/v1";

async function mockAdminAuth(page: Page) {
  await page.route(`${API_PREFIX}/auth/me`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "admin-1", email: "admin@test.com", role: "admin" }) });
  });
  await page.route(`${API_PREFIX}/auth/config`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: true, oidcEnabled: true, instanceName: "test" }) });
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
  await page.route(`${API_PREFIX}/admin/orgs*`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ items: [], pagination: { total: 0, start: 0, end: 0, limit: 20, offset: 0 } }),
    });
  });
}

async function openCreateForm(page: Page) {
  await page.goto("/admin/organisations");
  await expect(page.getByText("Organisations").first()).toBeVisible({ timeout: 10_000 });
  await page.getByRole("button", { name: "New Organisation" }).click();
  await expect(page.getByText("New Organisation").nth(1)).toBeVisible();
}

test.describe("Org creation requires OIDC at creation (Option B, #1679)", () => {
  test("create posts the SSO block in the same request", async ({ page }) => {
    await mockAdminAuth(page);
    await openCreateForm(page);

    let createBody: Record<string, unknown> | undefined;
    let createCalls = 0;
    await page.route(`${API_PREFIX}/orgs`, async (route: Route) => {
      if (route.request().method() === "POST") {
        createCalls += 1;
        createBody = route.request().postDataJSON();
        await route.fulfill({
          status: 201,
          contentType: "application/json",
          body: JSON.stringify({ id: "org-new", name: "Acme", slug: "acme", status: "active", planId: "enterprise", memberCount: 1 }),
        });
        return;
      }
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
    });

    await page.getByPlaceholder(/owner email/i).fill("owner@example.com");
    await page.getByPlaceholder(/organisation name/i).fill("Acme");
    // Progressive disclosure: the IdP section starts collapsed; opening it
    // reveals the required OIDC fields.
    await page.getByRole("button", { name: /identity provider/i }).click();
    await expect(page.getByText("not configured")).toBeVisible();
    await page.getByPlaceholder(/discovery url/i).fill("https://idp.acme.example/.well-known/openid-configuration");
    await page.getByPlaceholder(/^client id$/i).fill("acme-idp-client");
    await page.getByPlaceholder(/^client secret$/i).fill("idp-secret-1");
    await expect(page.getByText("configured", { exact: true })).toBeVisible();

    await page.getByRole("button", { name: "Create", exact: true }).click();

    await expect.poll(() => createCalls).toBe(1);
    expect(createBody).toMatchObject({
      name: "Acme",
      ownerEmail: "owner@example.com",
      sso: {
        discoveryUrl: "https://idp.acme.example/.well-known/openid-configuration",
        clientId: "acme-idp-client",
        clientSecret: "idp-secret-1",
      },
    });
    // Success closes the form.
    await expect(page.getByRole("button", { name: "Create", exact: true })).toHaveCount(0);
  });

  test("submit is blocked and the SSO section auto-opens when the IdP is unconfigured", async ({ page }) => {
    await mockAdminAuth(page);
    await openCreateForm(page);

    let createCalls = 0;
    await page.route(`${API_PREFIX}/orgs`, async (route: Route) => {
      if (route.request().method() === "POST") {
        createCalls += 1;
      }
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
    });

    await page.getByPlaceholder(/owner email/i).fill("owner@example.com");
    await page.getByPlaceholder(/organisation name/i).fill("Acme");
    // SSO section left collapsed/unconfigured — Option B must block.
    await page.getByRole("button", { name: "Create", exact: true }).click();

    await expect(page.getByText(/discovery URL is required/i)).toBeVisible();
    // The gate auto-opens the collapsed section so the required work is visible.
    await expect(page.getByPlaceholder(/discovery url/i)).toBeVisible();
    await page.waitForTimeout(300);
    expect(createCalls).toBe(0);
  });

  test("backend 400 sso.<field> details render with a friendly label", async ({ page }) => {
    await mockAdminAuth(page);
    await openCreateForm(page);

    await page.route(`${API_PREFIX}/orgs`, async (route: Route) => {
      if (route.request().method() === "POST") {
        await route.fulfill({
          status: 400,
          contentType: "application/json",
          body: JSON.stringify({
            error: "validation failed",
            details: { "sso.discoveryUrl": "Must be a valid URL" },
          }),
        });
        return;
      }
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
    });

    await page.getByPlaceholder(/owner email/i).fill("owner@example.com");
    await page.getByPlaceholder(/organisation name/i).fill("Acme");
    await page.getByRole("button", { name: /identity provider/i }).click();
    await page.getByPlaceholder(/discovery url/i).fill("https://idp.acme.example/.well-known/openid-configuration");
    await page.getByPlaceholder(/^client id$/i).fill("acme-idp-client");
    await page.getByPlaceholder(/^client secret$/i).fill("idp-secret-1");
    await page.getByRole("button", { name: "Create", exact: true }).click();

    await expect(page.getByText(/^Discovery URL: Must be a valid URL/i)).toBeVisible({ timeout: 10_000 });
  });
});
