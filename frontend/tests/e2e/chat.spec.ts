import { test, expect, type Page } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

async function setupMockWorkspace(page: Page, workspaceId: string) {
  // Intercept workspace list to return one workspace
  await page.route("**/api/v1/workspaces", async (route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          items: [{ id: workspaceId, name: "test-ws", phase: "Active", userId: "test" }],
          pagination: { limit: 20, offset: 0, total: 1 },
        }),
      });
    } else {
      await route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: workspaceId }) });
    }
  });
  // Intercept status endpoint — return Active
  await page.route(`**/api/v1/workspaces/${workspaceId}/status`, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ phase: "Active" }),
    });
  });
  // Intercept session creation
  await page.route(`**/api/v1/workspaces/${workspaceId}/sessions/new`, async (route) => {
    if (route.request().method() === "POST") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ workspaceId, sessionId: "sess-auto-1", resumed: false, workspacePhase: "Active" }),
      });
    } else {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
    }
  });
  // Intercept SSE events — immediately send workspace.phase=Active
  await page.route(`**/api/v1/workspaces/${workspaceId}/session-events`, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "text/event-stream",
      body: `data: ${JSON.stringify({ type: "workspace.phase", phase: "Active" })}\n\n`,
    });
  });
  // Contract stream (US-69.10 cutover) — minimal idle snapshot; every body
  // must open with a snapshot frame or the client reconnects.
  await mockIdleContractStream(page, `**/api/v1/workspaces/${workspaceId}/contract-events`, "sess-auto-1");
}

async function loginAs(page: import("@playwright/test").Page, username: string, password: string) {
  await page.goto("/login");
  await page.getByPlaceholder("Username").fill(username);
  await page.getByPlaceholder("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  // Wait for either redirect to /chat OR error message
  await Promise.race([
    page.waitForURL(/\/chat/, { timeout: 5000 }),
    page.getByText(/invalid|error/i).waitFor({ timeout: 5000 }),
  ]);
  // If we're still on /login, login failed
  if (page.url().includes("/login")) {
    throw new Error("Login failed — check E2E_USERNAME/E2E_PASSWORD and that the user exists");
  }
}

test.describe("Chat page (authenticated)", () => {
  test.skip(
    () => !process.env.E2E_USERNAME || !process.env.E2E_PASSWORD,
    "Skipped: E2E_USERNAME and E2E_PASSWORD env vars required",
  );

  test.beforeEach(async ({ page }) => {
    await loginAs(page, process.env.E2E_USERNAME!, process.env.E2E_PASSWORD!);
  });

  test("shows workspace selection prompt when no workspace selected", async ({ page }) => {
    await expect(page.getByText("Select a workspace to start chatting")).toBeVisible();
  });

  test("sidebar shows workspace list", async ({ page }) => {
    const sidebar = page.locator("aside");
    await expect(sidebar).toBeVisible();
    await expect(sidebar.getByText("Safe Space")).toBeVisible();
  });

  test("settings page renders tabs", async ({ page }) => {
    await page.goto("/settings");
    await expect(page.getByRole("button", { name: "API Keys" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Appearance" })).toBeVisible();
  });

  test("theme toggle works", async ({ page }) => {
    await page.goto("/settings");
    await page.getByRole("button", { name: "Appearance" }).click();
    await page.getByRole("button", { name: "Dark" }).click();
    await expect(page.locator("html")).toHaveClass(/dark/);
    await page.getByRole("button", { name: "Light" }).click();
    await expect(page.locator("html")).not.toHaveClass(/dark/);
  });

  test("logout clears session and redirects to login", async ({ page }) => {
    await page.getByLabel("Log out").click();
    await expect(page).toHaveURL(/\/login/);
  });

  test.describe("session auto-creation", () => {
    test("navigating to workspace with Active status auto-creates a session", async ({ page }) => {
      test.setTimeout(15000);
      const wsId = "test-e2e-auto-1";
      await setupMockWorkspace(page, wsId);
      // Navigate to workspace page
      await page.goto(`/chat?workspace=${wsId}`);
      // Wait for session to appear in URL
      await expect(page).toHaveURL(/session=/, { timeout: 10000 });
      // Verify the auto-created session ID is present
      const url = page.url();
      expect(url).toContain("session=sess-auto-1");
    });

    test("workspace without Active phase does not auto-create session", async ({ page }) => {
      test.setTimeout(15000);
      const wsId = "test-e2e-auto-2";
      await page.route(`**/api/v1/workspaces/${wsId}/status`, async (route) => {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ phase: "Pending" }),
        });
      });
      await page.route("**/api/v1/workspaces", async (route) => {
        if (route.request().method() === "GET") {
          await route.fulfill({
            status: 200,
            contentType: "application/json",
            body: JSON.stringify({
              items: [{ id: wsId, name: "test-ws", phase: "Pending", userId: "test" }],
              pagination: { limit: 20, offset: 0, total: 1 },
            }),
          });
        } else {
          await route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: wsId }) });
        }
      });
      // Navigate to workspace page — session should NOT be created
      await page.goto(`/chat?workspace=${wsId}`);
      // Give it time to potentially create a session
      await page.waitForTimeout(3000);
      // URL should not contain session=
      expect(page.url()).not.toContain("session=");
    });
  });
});
