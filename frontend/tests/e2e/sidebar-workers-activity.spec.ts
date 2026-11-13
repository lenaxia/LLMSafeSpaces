/**
 * E2E: the Workers-group header activity spinner (tsk-upland-marigold-masks).
 * The collapsed-by-default Workers group shows an animated spinner next
 * to its count iff at least one worker row is busy. Driven through the
 * REAL shell: sessions seeded on the listing payload (origin: "worker"),
 * busy/idle delivered as session.status events over the user SSE stream
 * (the session-activity.spec.ts deferred-fulfill pattern), assertions on
 * the real sidebar tree (the sidebar-hierarchy.spec.ts setup pattern).
 *
 * Pins the wiring no mocked-provider unit test can see: SSE → provider
 * busy map → aggregation hook → the collapsed header glyph, including
 * the worker-SCOPING (a busy user session never lights it) and the
 * idle extinguish.
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

const WORKSPACE_ID = "ws-workers-activity-e2e";
const USER_SESSION = "ses_user_chat";
const WORKER_A = "ses_lane_worker_a";
const WORKER_B = "ses_lane_worker_b";
const API_PREFIX = "**/api/v1";

/**
 * Deferred SSE fulfiller for the user event stream: each connection
 * parks until the test emits its next body (session-activity.spec.ts
 * test 40's pattern, sequenced so a test can deliver several events —
 * after one fulfill the reader sees EOF and reconnects, re-entering
 * the route handler for the next body). Emitted bodies QUEUE while no
 * connection is parked (the app may connect later than the first
 * emit), so no event is ever dropped to a timing race.
 */
class DeferredUserSSE {
  private waiters: Array<(body: string) => void> = [];
  private queue: string[] = [];

  readonly routeHandler = async (r: Route) => {
    await new Promise<void>((resolve) => {
      const deliver = (body: string) => {
        void r.fulfill({
          status: 200,
          headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
          body,
        });
        resolve();
      };
      const queued = this.queue.shift();
      if (queued !== undefined) deliver(queued);
      else this.waiters.push(deliver);
    });
  };

  emit = (body: string): void => {
    if (this.waiters.length === 0) {
      this.queue.push(body);
      return;
    }
    // Deliver to EVERY parked connection (the live-update-1687.spec.ts
    // pattern): StrictMode double-mounts park two fetches and the older
    // one may already be aborted — the live connection must see the
    // body no matter which is which.
    for (const deliver of this.waiters.splice(0)) deliver(body);
  };

  statusEvent = (sessionId: string, status: string): void =>
    this.emit(`data: ${JSON.stringify({ type: "session.status", workspace_id: WORKSPACE_ID, session_id: sessionId, status })}\n\n`);
}

async function setupAPIMocks(page: Page, userSse: DeferredUserSSE) {
  await page.route(`${API_PREFIX}/auth/me`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "testuser", email: "t@t.com", role: "user", active: true }) });
  });
  await page.route(`${API_PREFIX}/auth/config`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false, instanceName: "test" }) });
  });
  await page.route(`${API_PREFIX}/workspaces`, async (route: Route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WORKSPACE_ID, name: "Workers Activity Test", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
    } else { await route.continue(); }
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/status`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" }, sessions: [] }) });
  });
  // Two workers + one user chat: the count badge reads 2 and the
  // scoping negative (busy user) has a real target.
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions?*`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify([
        { id: USER_SESSION, title: "Main chat", messageCount: 4, status: "idle" },
        { id: WORKER_A, title: "lane worker alpha", origin: "worker", messageCount: 8, status: "idle" },
        { id: WORKER_B, title: "lane worker beta", origin: "worker", messageCount: 3, status: "idle" },
      ]),
    });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/session-origins`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ origins: [] }) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions/*/message`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/session-events`, async (route) => {
    await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" });
  });
  await mockIdleContractStream(page, `${API_PREFIX}/workspaces/${WORKSPACE_ID}/contract-events`, USER_SESSION);
  // The user event stream: parked connections the test fulfills.
  await page.route(`${API_PREFIX}/events`, userSse.routeHandler);
}

test.describe("Sidebar Workers group — header activity spinner", () => {
  test("busy worker lights the collapsed header spinner; busy user does not; idle extinguishes", async ({ page }) => {
    const userSse = new DeferredUserSSE();
    await setupAPIMocks(page, userSse);
    await page.goto(`/chat/${WORKSPACE_ID}/${USER_SESSION}`);

    // The group renders collapsed, count 2, no spinner.
    const group = page.getByTestId("workers-group");
    await expect(group).toBeVisible({ timeout: 15_000 });
    await expect(group.getByText("2")).toBeVisible({ timeout: 15_000 });
    const headerSpinner = page.getByTestId("workers-group-spinner");
    await expect(headerSpinner).toBeHidden();
    await expect(page.getByText("lane worker alpha")).not.toBeVisible();

    // UNHAPPY/scoping: a busy USER session lands (its row badges — the
    // landed-proof for this negative) while the workers header stays dark.
    userSse.statusEvent(USER_SESSION, "busy");
    await expect(page.locator("button:has-text('Main chat') .animate-spin").first()).toBeVisible({ timeout: 10_000 });
    await expect(headerSpinner).toBeHidden();

    // HAPPY: worker alpha goes busy → the collapsed header lights.
    userSse.statusEvent(WORKER_A, "busy");
    await expect(headerSpinner).toBeVisible({ timeout: 10_000 });
    await expect(group.getByText("2")).toBeVisible(); // count stays honest

    // UNHAPPY: alpha goes idle (beta never busy) → the header extinguishes.
    userSse.statusEvent(WORKER_A, "idle");
    await expect(headerSpinner).toBeHidden({ timeout: 10_000 });
  });
});
