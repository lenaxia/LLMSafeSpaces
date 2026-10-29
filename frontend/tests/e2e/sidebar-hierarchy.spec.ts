/**
 * E2E test for the sidebar session list, ruling 6 (2026-10-28 owner
 * batch): the "Workers" group is populated by worker PROVENANCE
 * (origin-class 'worker', carried ON the session list payload — the
 * server-side session_origins join; the /session-origins endpoint is
 * the compatibility fallback), never by parentage. Subagent (subtask)
 * sessions nest under their parent in the main tree; orphaned subtasks
 * promote to top-level rows (the synthetic 'Orphaned subtasks' group
 * stays dead); board workers leave the flat roots for the collapsed
 * group, whose expand/collapse owns their visibility.
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
  // Ruling 6: the worker mark rides the LISTING payload itself
  // (SessionListItem.origin — the server-side session_origins join the
  // board spawner's spawn stamp feeds).
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions?*`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify([
        { id: ROOT_SESSION, title: "Main task", messageCount: 5, status: "idle" },
        { id: CHILD_SESSION, title: "Find files (@explore)", parentId: ROOT_SESSION, messageCount: 3, status: "idle" },
        { id: ORPHAN_SESSION, title: "Orphaned task", parentId: "ses_deleted_parent", messageCount: 1, status: "idle" },
        { id: WORKER_SESSION, title: "lane-1966 charter worker", origin: "worker", messageCount: 8, status: "idle" },
      ]),
    });
  });
  // The origins fallback endpoint returns EMPTY — the payload-carried
  // origin must suffice on its own (the join is the primary carrier).
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/session-origins`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ origins: [] }),
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

test.describe("Sidebar Workers group (#1966/ruling 6)", () => {
  test.beforeEach(async ({ page }) => {
    await setupAPIMocks(page);
  });

  test("workers group by provenance only; subagents nest under parents; orphans promote to top-level", async ({ page }) => {
    await page.goto(`/chat/${WORKSPACE_ID}/${ROOT_SESSION}`);

    await expect(page.getByText("Main task")).toBeVisible({ timeout: 10_000 });

    // The Workers group renders with a count badge (1 — the
    // provenance-marked worker ONLY; the parented subagent and the
    // orphan do NOT count) and is collapsed by default. Explicit
    // timeout: cold-start (dev-server compile) can exceed the 5s
    // default on the first run.
    const group = page.getByTestId("workers-group");
    await expect(group).toBeVisible({ timeout: 15_000 });
    await expect(group.getByText("Workers")).toBeVisible();
    await expect(group.getByText("1")).toBeVisible({ timeout: 15_000 });
    await expect(page.getByText("lane-1966 charter worker")).not.toBeVisible();

    // The active root's nested subagent auto-expands beneath it
    // (parentage nests, never folders), and the orphan promotes to a
    // visible top-level row.
    await expect(page.getByText("Find files (@explore)")).toBeVisible();
    await expect(page.getByText("Orphaned task")).toBeVisible();

    // One click expands the group; the worker rows inside.
    await page.getByLabel("Expand worker sessions").click();
    await expect(page.getByText("lane-1966 charter worker")).toBeVisible();

    // Collapsing hides it again.
    await page.getByLabel("Collapse worker sessions").click();
    await expect(page.getByText("lane-1966 charter worker")).not.toBeVisible();
  });

  test("navigating directly to a board worker auto-expands the group; a user subagent does not", async ({ page }) => {
    await page.goto(`/chat/${WORKSPACE_ID}/${WORKER_SESSION}`);

    // The active worker must be visible — the group auto-expands for
    // it (the tree's auto-expand-the-active-chain precedent).
    await expect(page.getByText("Main task")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText("lane-1966 charter worker")).toBeVisible({ timeout: 10_000 });

    // A user session's subagent stays a main-tree row: direct
    // navigation shows it under its auto-expanded parent while the
    // workers group stays collapsed.
    await page.goto(`/chat/${WORKSPACE_ID}/${CHILD_SESSION}`);
    await expect(page.getByText("Main task")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText("Find files (@explore)")).toBeVisible();
    await expect(page.getByText("lane-1966 charter worker")).not.toBeVisible();
  });

  test("the synthetic 'Orphaned subtasks' group no longer renders", async ({ page }) => {
    await page.goto(`/chat/${WORKSPACE_ID}/${ROOT_SESSION}`);

    await expect(page.getByText("Main task")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText("Orphaned subtasks")).not.toBeVisible();
  });

  test("no Workers group when every session is user-facing", async ({ page }) => {
    // Override the default mock with a worker-free list.
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions?*`, async (route: Route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify([
          { id: ROOT_SESSION, title: "Main task", messageCount: 5, status: "idle" },
          { id: CHILD_SESSION, title: "Find files (@explore)", parentId: ROOT_SESSION, messageCount: 3, status: "idle" },
        ]),
      });
    });

    await page.goto(`/chat/${WORKSPACE_ID}/${ROOT_SESSION}`);

    await expect(page.getByText("Main task")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId("workers-group")).not.toBeVisible();
    // The parented subagent nests under its parent — no folder needed.
    await expect(page.getByText("Find files (@explore)")).toBeVisible();
  });
});
