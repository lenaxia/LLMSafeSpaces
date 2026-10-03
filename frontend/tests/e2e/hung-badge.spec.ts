/**
 * E2E: D6 (#998) hung badge seeding from persisted alerts — the resolved-
 * history regression and the live-alert direction. Resolution is
 * server-side truth (resolvedAt, set by the D6 sweep when it observes
 * the hang end): RESOLVED history never badges; an UNRESOLVED alert
 * does. The badge renders on COLLAPSED workspace rows, while the
 * sessions query (whose cache drives the seed) populates when the
 * workspace is expanded — so both tests navigate in (expanded), then
 * collapse and assert the badge.
 *
 * No SSE transport is needed in either direction: the page-load seed
 * path under test is REST-only (/sessions + /alerts). The user SSE
 * endpoint is fulfilled empty (the tests-43/44 pattern).
 */
import { test, expect, type Page, type Route } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

const WS_A = "ws-hb-a";
const SESS_A1 = "ses_hb_a1";
const API = "**/api/v1";

async function setupBase(page: Page, opts: { sessAStatus?: "active" | "idle"; alerts: unknown[] }) {
  const sessAStatus = opts.sessAStatus ?? "idle";

  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true }) }));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false }) }));
  await page.route(`${API}/workspaces`, (r: Route) => {
    if (r.request().method() !== "GET") { r.continue(); return; }
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({
      items: [
        { id: WS_A, name: "Alpha", userId: "u1", runtime: "base", storageSize: "1Gi", phase: "Active" },
      ],
      pagination: { limit: 50, offset: 0, total: 1 },
    })});
  });
  await page.route(`${API}/workspaces/${WS_A}/status`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0" }, sessions: [] }) }));
  await page.route(`${API}/workspaces/${WS_A}/sessions`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([
      { id: SESS_A1, title: "Task Alpha", status: sessAStatus, hasUnread: false, messageCount: 2, lastSeenAt: "2026-06-10T10:00:00Z" },
    ])}));
  await page.route(`${API}/workspaces/${WS_A}/alerts`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ alerts: opts.alerts }) }));
  await page.route(`${API}/workspaces/${WS_A}/sessions/${SESS_A1}/message**`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: "[]" }));
  await page.route(`${API}/workspaces/${WS_A}/models`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ models: [], currentModel: "" }) }));
  await page.route(`${API}/workspaces/*/sessions/*/seen`, (r: Route) =>
    r.fulfill({ status: 204, body: "" }));
  await page.route(`${API}/events`, (r: Route) =>
    r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));
  await mockIdleContractStream(page, `${API}/workspaces/*/contract-events`, SESS_A1);
}

async function collapseWorkspace(page: Page) {
  // The workspace row button carries the workspace name.
  await page.getByRole("button", { name: /Alpha/ }).first().click();
}

test.describe("D6 (#998): hung badge seeded from persisted alerts", () => {
  test("stale history does not badge a recovered workspace (the shipped latch)", async ({ page }) => {
    // The regression: the session hung hours ago (alerts persisted in
    // the append-only 24h feed) and has since recovered — the REST
    // list says idle. The seed must NOT latch the badge.
    await setupBase(page, {
      sessAStatus: "idle",
      alerts: [
        { id: "1", workspaceId: WS_A, sessionId: SESS_A1, alert: "session_hung", oldestBusySeconds: 3154, createdAt: "2026-10-03T04:59:06Z", resolvedAt: "2026-10-03T05:15:30Z" },
        { id: "2", workspaceId: WS_A, sessionId: SESS_A1, alert: "session_hung", oldestBusySeconds: 997, createdAt: "2026-10-03T04:23:09Z", resolvedAt: "2026-10-03T05:15:30Z" },
      ],
    });

    await page.goto(`/chat/${WS_A}/${SESS_A1}`);
    await expect(page.getByText("Task Alpha")).toBeVisible({ timeout: 10_000 });
    // Let the alerts fetch resolve against the idle session.
    await page.waitForTimeout(300);

    await collapseWorkspace(page);
    await expect(page.getByTestId("hung-badge")).not.toBeVisible();
  });

  test("a still-busy alerted session badges the workspace (true recovery)", async ({ page }) => {
    await setupBase(page, {
      sessAStatus: "active",
      alerts: [
        { id: "1", workspaceId: WS_A, sessionId: SESS_A1, alert: "session_hung", oldestBusySeconds: 960, createdAt: new Date().toISOString(), resolvedAt: null },
      ],
    });

    await page.goto(`/chat/${WS_A}/${SESS_A1}`);
    await expect(page.getByText("Task Alpha")).toBeVisible({ timeout: 10_000 });

    await collapseWorkspace(page);
    await expect(page.getByTestId("hung-badge")).toBeVisible({ timeout: 5_000 });
  });
});
