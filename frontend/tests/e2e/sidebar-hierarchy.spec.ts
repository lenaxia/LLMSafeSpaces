/**
 * E2E test for sidebar hierarchy: subagent (subtask) sessions render
 * nested under their parent, expand/collapse works, and orphaned sessions
 * appear in the synthetic group.
 *
 * This is the user-facing scenario for the parent_session_id migration
 * (worklog 0123). Without this fix, subagent sessions appear as flat
 * top-level entries and the user can't tell which were spawned by which
 * `task` invocation.
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

const WORKSPACE_ID = "ws-hierarchy-e2e";
const ROOT_SESSION = "ses_root";
const CHILD_SESSION = "ses_child";
const ORPHAN_SESSION = "ses_orphan";
const API_PREFIX = "**/api/v1";

async function setupAPIMocks(page: Page) {
  await page.route(`${API_PREFIX}/auth/me`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "testuser", email: "t@t.com", role: "user", active: true }) });
  });
  await page.route(`${API_PREFIX}/auth/config`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false, instanceName: "test" }) });
  });
  await page.route(`${API_PREFIX}/workspaces`, async (route: Route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WORKSPACE_ID, name: "Hierarchy Test", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
    } else { await route.continue(); }
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/status`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" }, sessions: [] }) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify([
        { id: ROOT_SESSION, title: "Main task", messageCount: 5, status: "idle" },
        { id: CHILD_SESSION, title: "Find files (@explore)", parentId: ROOT_SESSION, messageCount: 3, status: "idle" },
        { id: ORPHAN_SESSION, title: "Orphaned task", parentId: "ses_deleted_parent", messageCount: 1, status: "idle" },
      ]),
    });
  });
  // Sessions default to empty body for GET history; not relevant to this test.
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions/*/message`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
  });
  // SSE: keep alive, send nothing.
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/session-events`, async (route) => {
    await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" });
  });
  // Contract stream (US-69.10 cutover) — minimal idle snapshot; every body
  // must open with a snapshot frame or the client reconnects.
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/contract-events`, async (route) => {
    await route.fulfill({
      status: 200, contentType: "text/event-stream",
      body: `data: ${JSON.stringify({ snapshot: { atSeq: "1", snapshot: { sessions: [{ sessionId: ROOT_SESSION, status: "SESSION_STATUS_IDLE", inFlightParts: [], queueDepth: 0, pendingInputs: [] }] } } })}\n\n`,
    });
  });
}

test.describe("Sidebar hierarchy", () => {
  test.beforeEach(async ({ page }) => {
    await setupAPIMocks(page);
  });

  test("subtask child is auto-expanded when navigating to parent; chevron collapses and re-expands it", async ({ page }) => {
    await page.goto(`/chat/${WORKSPACE_ID}/${ROOT_SESSION}`);

    await expect(page.getByText("Main task")).toBeVisible({ timeout: 10_000 });
    // autoExpandChildren=true (default): navigating to a parent auto-expands its children.
    await expect(page.getByText("Find files (@explore)")).toBeVisible();

    // Click the chevron to collapse the subtree.
    await page.getByLabel("Collapse subtasks").first().click();
    await expect(page.getByText("Find files (@explore)")).not.toBeVisible();

    // Click again to expand.
    await page.getByLabel("Expand subtasks").first().click();
    await expect(page.getByText("Find files (@explore)")).toBeVisible();
  });

  test("navigating directly to a subtask auto-expands its parent", async ({ page }) => {
    await page.goto(`/chat/${WORKSPACE_ID}/${CHILD_SESSION}`);

    await expect(page.getByText("Main task")).toBeVisible({ timeout: 10_000 });
    // Without auto-expand the user couldn't see where they are in the
    // tree. The parent must be expanded so the child is visible.
    await expect(page.getByText("Find files (@explore)")).toBeVisible();
  });

  test("orphaned subtasks appear in the 'Orphaned subtasks' group", async ({ page }) => {
    await page.goto(`/chat/${WORKSPACE_ID}/${ROOT_SESSION}`);

    await expect(page.getByText("Orphaned subtasks")).toBeVisible({ timeout: 10_000 });
    // Group is collapsed; orphan is hidden.
    await expect(page.getByText("Orphaned task")).not.toBeVisible();

    await page.getByLabel("Expand orphaned subtasks").click();
    await expect(page.getByText("Orphaned task")).toBeVisible();
  });

  test("orphans group is hidden when there are no orphans", async ({ page }) => {
    // Override the default mock with a list that has no orphans.
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions`, async (route: Route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify([
          { id: ROOT_SESSION, title: "Main task", messageCount: 5, status: "idle" },
        ]),
      });
    });

    await page.goto(`/chat/${WORKSPACE_ID}/${ROOT_SESSION}`);

    await expect(page.getByText("Main task")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText("Orphaned subtasks")).not.toBeVisible();
  });
});
