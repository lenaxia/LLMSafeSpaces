/**
 * E2E for the session_gone typed gone-state (#1340).
 *
 * Proves in a real browser: opening a session whose harness side was
 * deleted renders the typed 410 gone-state (never a generic
 * fetch-failure banner), and the sidebar's ghost row is dropped after
 * the sessions-list invalidation refetch (the API already reaped the
 * row server-side — the first sessions response carries the ghost, the
 * post-invalidation refetch does not).
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

const WORKSPACE_ID = "ws-session-gone-e2e";
const SESSION_ID = "ses-gone-e2e";
const API = "**/api/v1";

async function setupSessionGoneMocks(page: Page) {
  let sessionsFetches = 0;

  await page.route(`${API}/auth/login`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ token: "e2e-token", user: { id: "u1", username: "tester", role: "user" } }) });
  });
  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true }) }));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false, instanceName: "test" }) }));

  await page.route(`${API}/workspaces`, async (r: Route) => {
    if (r.request().method() !== "GET") { await r.continue(); return; }
    await r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WORKSPACE_ID, name: "Session Gone E2E", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/status`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" } }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/activate`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ resumed: WORKSPACE_ID }) }));

  // Sessions list: the FIRST fetch still carries the ghost row (the
  // sidebar rendered it before the harness deletion); every refetch
  // after the gone-verdict invalidation omits it.
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions`, (r: Route) => {
    if (r.request().method() === "POST") {
      r.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, sessionId: SESSION_ID }) });
      return;
    }
    sessionsFetches++;
    const rows =
      sessionsFetches <= 1
        ? [{ id: SESSION_ID, title: "Ghost Row", messageCount: 3, status: "idle", hasUnread: false }]
        : [];
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(rows) });
  });

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/ensure`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/seen`, (r: Route) => r.fulfill({ status: 204 }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, title: "Ghost Row" }) }));

  // The typed 410: the agent's not-found verdict, house-pattern body.
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/message*`, (r: Route) =>
    r.fulfill({ status: 410, contentType: "application/json", body: JSON.stringify({ code: "session_gone", error: "This session no longer exists on the agent — it may have been deleted. It has been removed from your session list." }) }));

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, (r: Route) =>
    r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));
  await mockIdleContractStream(page, `${API}/workspaces/${WORKSPACE_ID}/contract-events`, SESSION_ID);
}

test.describe("session_gone typed gone-state (#1340)", () => {
  test("typed 410 renders the gone-state, never the generic banner, and drops the sidebar ghost", async ({ page }) => {
    await setupSessionGoneMocks(page);
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // The typed gone-state renders — not the generic fetch-failure banner.
    const gone = page.getByTestId("session-gone-banner");
    await expect(gone).toBeVisible({ timeout: 10_000 });
    await expect(gone).toContainText("Session no longer exists");
    await expect(page.getByText("Chat history unavailable")).toHaveCount(0);
    await expect(page.getByRole("button", { name: /retry/i })).toHaveCount(0);

    // The sidebar ghost row disappears after the invalidation refetch
    // (the API reaped the row; the second sessions fetch omits it).
    await expect(page.getByRole("button", { name: "Ghost Row" })).toHaveCount(0, { timeout: 10_000 });
  });
});
