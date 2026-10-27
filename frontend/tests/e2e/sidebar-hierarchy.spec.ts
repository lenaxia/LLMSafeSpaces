/**
 * E2E test for the sidebar session list, post-#1966: subagent (subtask)
 * sessions no longer nest under their parent — they live in the
 * collapsed Workers folder alongside board/engine-spawned workers
 * (origin-class 'worker'), keeping the main list user-facing. The
 * synthetic 'Orphaned subtasks' group is gone (orphaned subagents row
 * under Workers too).
 *
 * Historical note: this spec originally pinned the parent_session_id
 * nesting UX (worklog 0123); #1966 superseded it — the folder replaces
 * the nesting as the answer to subagent clutter.
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

const WORKSPACE_ID = "ws-hierarchy-e2e";
const ROOT_SESSION = "ses_root";
const CHILD_SESSION = "ses_child";
const ORPHAN_SESSION = "ses_orphan";
const WORKER_SESSION = "ses_board_worker";
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
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions?*`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify([
        { id: ROOT_SESSION, title: "Main task", messageCount: 5, status: "idle" },
        { id: CHILD_SESSION, title: "Find files (@explore)", parentId: ROOT_SESSION, messageCount: 3, status: "idle" },
        { id: ORPHAN_SESSION, title: "Orphaned task", parentId: "ses_deleted_parent", messageCount: 1, status: "idle" },
        { id: WORKER_SESSION, title: "lane-1966 charter worker", messageCount: 8, status: "idle" },
      ]),
    });
  });
  // #1966: the origins endpoint carries the worker marks — the board
  // spawner stamps origin='worker' on every engine spawn.
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/session-origins`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ origins: [{ sessionId: WORKER_SESSION, origin: "worker" }] }),
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
  await mockIdleContractStream(page, `${API_PREFIX}/workspaces/${WORKSPACE_ID}/contract-events`, ROOT_SESSION);
}

test.describe("Sidebar workers folder (#1966)", () => {
  test.beforeEach(async ({ page }) => {
    await setupAPIMocks(page);
  });

  test("subtasks and workers live in the collapsed Workers folder; the main list stays user-facing", async ({ page }) => {
    await page.goto(`/chat/${WORKSPACE_ID}/${ROOT_SESSION}`);

    await expect(page.getByText("Main task")).toBeVisible({ timeout: 10_000 });

    // The Workers folder renders with a count badge (parented subtask +
    // orphan + board worker = 3) and is collapsed by default: none of
    // the worker titles is visible. Explicit timeout: cold-start
    // (dev-server compile) can exceed the 5s default on the first run.
    const folder = page.getByTestId("workers-group");
    await expect(folder).toBeVisible({ timeout: 15_000 });
    await expect(folder.getByText("3")).toBeVisible({ timeout: 15_000 });
    await expect(page.getByText("Find files (@explore)")).not.toBeVisible();
    await expect(page.getByText("lane-1966 charter worker")).not.toBeVisible();
    await expect(page.getByText("Orphaned task")).not.toBeVisible();

    // One click expands the folder; every worker rows inside.
    await page.getByLabel("Expand worker sessions").click();
    await expect(page.getByText("Find files (@explore)")).toBeVisible();
    await expect(page.getByText("lane-1966 charter worker")).toBeVisible();
    await expect(page.getByText("Orphaned task")).toBeVisible();

    // Collapsing hides them again.
    await page.getByLabel("Collapse worker sessions").click();
    await expect(page.getByText("Find files (@explore)")).not.toBeVisible();
  });

  test("navigating directly to a foldered session auto-expands the Workers folder", async ({ page }) => {
    await page.goto(`/chat/${WORKSPACE_ID}/${CHILD_SESSION}`);

    await expect(page.getByText("Main task")).toBeVisible({ timeout: 10_000 });
    // The active session must be visible — the folder auto-expands for
    // it (the tree's auto-expand-the-active-chain precedent).
    await expect(page.getByText("Find files (@explore)")).toBeVisible();
  });

  test("the synthetic 'Orphaned subtasks' group no longer renders", async ({ page }) => {
    await page.goto(`/chat/${WORKSPACE_ID}/${ROOT_SESSION}`);

    await expect(page.getByText("Main task")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText("Orphaned subtasks")).not.toBeVisible();
  });

  test("no Workers folder when every session is user-facing", async ({ page }) => {
    // Override the default mock with a worker-free list.
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions?*`, async (route: Route) => {
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
    await expect(page.getByTestId("workers-group")).not.toBeVisible();
  });
});
