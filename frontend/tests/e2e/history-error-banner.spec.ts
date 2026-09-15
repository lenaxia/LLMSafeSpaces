/**
 * E2E: chat-history error banner renders from API-authored bodies only
 * (#1303, the runnable leg of the #490/#486/#488 regression line).
 *
 * Proves in a real browser:
 *   1. A GET history 502 with the API-authored body
 *      {"error":"failed to fetch history"} (proxy_handlers.go GetHistory)
 *      renders the banner with that message — no nested-envelope field
 *      is consulted (the raw passthrough died with #828).
 *   2. A GET history 503 with the API-authored recovery body renders
 *      the "Reconnecting…" state from `message`/`reason`.
 *
 * Deferred kind-cluster leg (not runnable in this environment — no
 * kind/docker): kill/suspend the agent pod mid-session, then load chat
 * history in a real browser against the cluster API and assert the same
 * banner content. Closest runnable command on a provisioned cluster:
 *   ./local/test.sh   # kind suite bootstrap + smoke (local/README)
 *   # then suspend the e2e workspace (kubectl delete pod / API suspend)
 *   # and open /chat/<workspace>/<session> expecting the banner rows
 *   # asserted below against the real 502/503 the API emits.
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

const WORKSPACE_ID = "ws-history-err-e2e";
const SESSION_ID = "ses-history-err-e2e";
const API = "**/api/v1";

type HistoryFailure = { status: number; body: unknown };

async function setupAPIMocks(page: Page, historyFailure: HistoryFailure) {
  await page.route(`${API}/auth/login`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ token: "e2e-token", user: { id: "u1", username: "tester", role: "user" } }) });
  });
  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true }) }));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false, instanceName: "test" }) }));
  await page.route(`${API}/workspaces`, async (r: Route) => {
    if (r.request().method() !== "GET") { await r.continue(); return; }
    await r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WORKSPACE_ID, name: "History Error E2E", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/status`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" } }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/activate`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ resumed: WORKSPACE_ID }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/models`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ models: [], currentModel: "" }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions`, async (r: Route) => {
    if (r.request().method() === "POST") {
      await r.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, sessionId: SESSION_ID }) });
      return;
    }
    await r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([{ id: SESSION_ID, title: "History Error Test", messageCount: 0, status: "idle", hasUnread: false }]) });
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/ensure`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/seen`, (r: Route) => r.fulfill({ status: 204 }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, title: "History Error Test" }) }));

  // The leg under test: GET history fails with the API-authored body.
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/message*`, (r: Route) =>
    r.fulfill({ status: historyFailure.status, contentType: "application/json", body: JSON.stringify(historyFailure.body) }));

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, (r: Route) =>
    r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));
  await mockIdleContractStream(page, `${API}/workspaces/${WORKSPACE_ID}/contract-events`, SESSION_ID);
}

test.describe("History error banner from API-authored bodies (#1303)", () => {
  test("502 {error} body: banner shows the API message, no ref, no nested fallback", async ({ page }) => {
    await setupAPIMocks(page, { status: 502, body: { error: "failed to fetch history" } });
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    await expect(page.getByText("Chat history unavailable")).toBeVisible({ timeout: 15000 });

    await page.getByText("Details").click();
    await expect(page.getByText("HTTP 502")).toBeVisible();
    await expect(page.getByText("failed to fetch history")).toBeVisible();
    // No ref in the API-authored body — none may be invented; the
    // nested envelope fields are never consulted.
    await expect(page.getByText(/^Ref:/)).toHaveCount(0);
    await expect(page.getByText(/^undefined$/)).toHaveCount(0);
  });

  test("503 recovery body: banner shows Reconnecting… from API message/reason", async ({ page }) => {
    await setupAPIMocks(page, {
      status: 503,
      body: {
        error: "workspace connection failed",
        message: "The agent is not responding. Please try again in a moment.",
        reason: "agent_unreachable",
        retryAfter: 10,
      },
    });
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    await expect(page.getByText("Reconnecting…")).toBeVisible({ timeout: 15000 });
    await page.getByText("Details").click();
    await expect(page.getByText("Reason: agent_unreachable")).toBeVisible();
    await expect(page.getByText(/The agent is not responding/)).toBeVisible();
    await expect(page.getByText(/^Ref:/)).toHaveCount(0);
  });
});
