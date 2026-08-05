import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

const API = "**/api/v1";
const ORG_ID = "org-e2e-1";

const ORG = {
  id: ORG_ID,
  name: "E2E Org",
  slug: "e2e-org",
  createdBy: "admin-1",
  createdAt: "2026-01-01T00:00:00Z",
  updatedAt: "2026-01-01T00:00:00Z",
  status: "active",
  planId: "team",
  subscriptionStatus: "active",
  userRole: "admin",
  memberCount: 3,
};

async function mockOrgAdmin(page: Page, overrides: Partial<typeof ORG> = {}) {
  const org = { ...ORG, ...overrides };

  await page.route(`${API}/auth/me`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ id: "admin-1", email: "admin@test.com", role: "member" }),
    });
  });
  await page.route(`${API}/auth/config`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ registrationEnabled: true, oidcEnabled: false, instanceName: "E2E" }),
    });
  });
  await page.route(`${API}/orgs/${ORG_ID}`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(org),
    });
  });

  // Stub org policies.
  await page.route(`${API}/orgs/${ORG_ID}/policies`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify([
        { key: "max_workspaces_per_member", value: 10, updatedAt: "2026-01-01T00:00:00Z" },
        { key: "max_active_workspaces_per_member", value: 5, updatedAt: "2026-01-01T00:00:00Z" },
        { key: "max_mcp_servers_per_workspace", value: 3, updatedAt: "2026-01-01T00:00:00Z" },
        { key: "allow_user_prompt", value: true, updatedAt: "2026-01-01T00:00:00Z" },
        { key: "allow_user_mcp_servers", value: true, updatedAt: "2026-01-01T00:00:00Z" },
      ]),
    });
  });

  // Stub org prompt config.
  await page.route(`${API}/orgs/${ORG_ID}/prompt`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ prompt: "", allowUserPrompt: true }),
    });
  });

  // Stub MCP servers.
  await page.route(`${API}/orgs/${ORG_ID}/mcp-servers`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ servers: [] }),
    });
  });

  // Stub other commonly-fetched endpoints.
  for (const ep of [
    "/users/me/settings",
    "/secrets",
    "/provider-credentials",
    "/auth/api-keys",
  ]) {
    await page.route(`${API}${ep}`, async (route: Route) => {
      await route.fulfill({ status: 200, contentType: "application/json", body: "[]" });
    });
  }
  await page.route(`${API}/users/me/settings/schema`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ settings: [], schemaVersion: 1 }) });
  });
  // Override the generic /users/me/settings stub with the object shape.
  await page.unroute(`${API}/users/me/settings`);
  await page.route(`${API}/users/me/settings`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ settings: {}, schemaVersion: 1 }) });
  });
  // Stub agent roles (fetched by Agent Config tab).
  await page.route(`${API}/orgs/${ORG_ID}/agent-roles`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: "[]" });
  });
  await page.route(`${API}/admin/agent-roles`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: "[]" });
  });
  await page.route(`${API}/orgs`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: "[]" });
  });
  await page.route(`${API}/events`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: "[]" });
  });
}

test.describe("Org admin portal", () => {
  test.beforeEach(async ({ page }) => {
    await mockOrgAdmin(page);
  });

  test("renders the portal with all admin nav items", async ({ page }) => {
    await page.goto(`/orgs/${ORG_ID}`);
    await page.waitForLoadState("networkidle");

    await expect(page.getByRole("heading", { name: "E2E Org" })).toBeVisible({ timeout: 8000 });
    // All admin nav items visible to admin.
    for (const label of ["Overview", "Members", "Credentials", "MCP Servers", "Workspaces", "Audit", "Billing", "SSO", "Agent Config", "Settings"]) {
      await expect(page.getByRole("link", { name: label })).toBeVisible();
    }
  });

  test("deep-links to /orgs/:id/settings and renders policy cards", async ({ page }) => {
    await page.goto(`/orgs/${ORG_ID}/settings`);
    await page.waitForLoadState("networkidle");

    await expect(page.getByRole("heading", { name: "Workspace Limits" })).toBeVisible({ timeout: 8000 });
    await expect(page.getByRole("heading", { name: "Model & Provider Restrictions" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "MCP & Image Defaults" })).toBeVisible();

    // Loaded policy values render (max_workspaces = 10, max_active = 5).
    await expect(page.locator("input[type='number']").first()).toHaveValue("10");
  });

  test("can navigate from overview to settings via sidebar", async ({ page }) => {
    await page.goto(`/orgs/${ORG_ID}`);
    await page.waitForLoadState("networkidle");

    await expect(page.getByRole("heading", { name: "E2E Org" })).toBeVisible({ timeout: 8000 });

    await page.getByRole("link", { name: "Settings" }).click();
    await expect(page).toHaveURL(/\/orgs\/.*\/settings/, { timeout: 5000 });
    await expect(page.getByRole("heading", { name: "Workspace Limits" })).toBeVisible();
  });

  test("deep-links to /orgs/:id/agent-config and renders toggle", async ({ page }) => {
    await page.goto(`/orgs/${ORG_ID}/agent-config`);
    await page.waitForLoadState("networkidle");

    await expect(page.getByRole("heading", { name: "Member Customization" })).toBeVisible({ timeout: 8000 });
  });

  test("deep-links to /orgs/:id/mcp-servers and renders tab", async ({ page }) => {
    await page.goto(`/orgs/${ORG_ID}/mcp-servers`);
    await page.waitForLoadState("networkidle");

    // "Add Server" button is unique on the MCP tab.
    await expect(page.getByRole("button", { name: "Add Server" })).toBeVisible({ timeout: 8000 });
  });

  test("saving workspace limits PUTs both policies", async ({ page }) => {
    const putBodies: Record<string, string> = {};
    await page.route(`${API}/orgs/${ORG_ID}/policies/*`, async (route: Route) => {
      if (route.request().method() === "PUT") {
        const key = route.request().url().split("/policies/")[1]!;
        putBodies[key] = route.request().postData() ?? "";
      }
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ status: "ok" }) });
    });

    await page.goto(`/orgs/${ORG_ID}/settings`);
    await page.waitForLoadState("networkidle");

    await expect(page.getByRole("button", { name: "Save Limits" })).toBeVisible({ timeout: 8000 });

    // Change the max-workspaces value and save.
    const maxInput = page.locator("input[type='number']").first();
    await maxInput.fill("20");
    await page.getByRole("button", { name: "Save Limits" }).click();

    // Wait for both PUTs to fire (max_workspaces + max_active).
    await expect.poll(() => putBodies["max_workspaces_per_member"]).toBe("20");
    await expect.poll(() => putBodies["max_active_workspaces_per_member"]).toBeDefined();
  });

  test("org load failure shows error message", async ({ page }) => {
    await page.route(`${API}/orgs/${ORG_ID}`, async (route: Route) => {
      await route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify({ error: "not found" }) });
    });

    await page.goto(`/orgs/${ORG_ID}`);
    await page.waitForLoadState("networkidle");

    await expect(page.getByText(/not found|do not have access/i)).toBeVisible({ timeout: 8000 });
  });

  test("save failure shows error toast", async ({ page }) => {
    await page.route(`${API}/orgs/${ORG_ID}/policies/*`, async (route: Route) => {
      if (route.request().method() === "PUT") {
        await route.fulfill({ status: 403, contentType: "application/json", body: JSON.stringify({ error: "feature gate rejected" }) });
      } else {
        await route.continue();
      }
    });

    await page.goto(`/orgs/${ORG_ID}/settings`);
    await page.waitForLoadState("networkidle");

    await expect(page.getByRole("button", { name: "Save Limits" })).toBeVisible({ timeout: 8000 });
    await page.getByRole("button", { name: "Save Limits" }).click();

    await expect(page.getByText("Failed to save")).toBeVisible({ timeout: 8000 });
  });

  test("non-admin member does not see admin-only tabs", async ({ page }) => {
    await mockOrgAdmin(page, { userRole: "member" });

    await page.goto(`/orgs/${ORG_ID}`);
    await page.waitForLoadState("networkidle");

    await expect(page.getByRole("heading", { name: "E2E Org" })).toBeVisible({ timeout: 8000 });
    // Admin-only tabs hidden from members.
    await expect(page.getByRole("link", { name: "Members" })).not.toBeVisible();
    await expect(page.getByRole("link", { name: "Settings" })).not.toBeVisible();
    await expect(page.getByRole("link", { name: "Credentials" })).not.toBeVisible();
    // Non-admin tabs still visible.
    await expect(page.getByRole("link", { name: "Overview" })).toBeVisible();
    await expect(page.getByRole("link", { name: "Workspaces" })).toBeVisible();
  });
});
