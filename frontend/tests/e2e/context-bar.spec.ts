/**
 * E2E tests for the context usage bar (DiskUsageBar) rendered in ChatPage.
 *
 * Proves in a real browser:
 *   1. Context bar renders with token count from session list contextUsed
 *   2. Context bar shows progress bar when contextTotal > 0
 *   3. Context bar shows "Unknown" badge when contextTotal is 0
 *   4. Context bar always visible (even with 0/Unknown)
 *   5. Context updates in real-time via SSE session.next.step.ended
 *   6. Compaction banner appears when contextUsed drops >50% via SSE
 *   7. Compaction banner can be dismissed
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

const WORKSPACE_ID = "ws-ctx-e2e";
const SESSION_ID = "ses-ctx-e2e";
const API = "**/api/v1";

async function setupAPIMocks(
  page: Page,
  opts: { contextTotal?: number; sessionContextUsed?: number | null } = {},
) {
  const contextTotal = opts.contextTotal ?? 200000;
  const sessionContextUsed = opts.sessionContextUsed;

  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "testuser", email: "t@t.com", role: "user", active: true }) }));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false, instanceName: "test" }) }));

  await page.route(`${API}/workspaces`, async (r: Route) => {
    if (r.request().method() !== "GET") { await r.continue(); return; }
    await r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WORKSPACE_ID, name: "CTX Test WS", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
  });

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/status`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" }, contextTotal }) }));

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/models`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ models: [], currentModel: "" }) }));

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions`, async (r: Route) => {
    if (r.request().method() === "POST") {
      await r.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, sessionId: SESSION_ID }) });
      return;
    }
    const session: Record<string, unknown> = { id: SESSION_ID, title: "CTX Test Session", messageCount: 1, status: "idle", hasUnread: false };
    if (sessionContextUsed != null) session.contextUsed = sessionContextUsed;
    await r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([session]) });
  });

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/seen`, (r: Route) => r.fulfill({ status: 204 }));

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/message`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ messages: [], nextCursor: null }) }));

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, title: "CTX Test Session" }) }));

  // Default SSE — silent (individual tests override when they need events)
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, (r: Route) =>
    r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));

  await page.route(`${API}/events`, (r: Route) =>
    r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));
}

function stepEndedSSE(sessionID: string, input: number, cacheRead = 0, cacheWrite = 0): string {
  const evt = {
    type: "agent.event",
    event_type: "session.next.step.ended",
    data: {
      type: "session.next.step.ended",
      properties: {
        sessionID,
        tokens: { input, output: 100, reasoning: 0, cache: { read: cacheRead, write: cacheWrite } },
      },
    },
  };
  return `data: ${JSON.stringify(evt)}\n\n`;
}

test.describe("Context bar (DiskUsageBar) — real browser", () => {
  test("always visible with 0/Unknown when session has no prior context data", async ({ page }) => {
    await setupAPIMocks(page, { contextTotal: 0, sessionContextUsed: null });
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // Bar must be present — shows Context label and Unknown badge
    await expect(page.getByText(/Context/i).first()).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText(/Unknown/i).first()).toBeVisible({ timeout: 5_000 });
  });

  test("shows token count from sessions list contextUsed (cold start)", async ({ page }) => {
    await setupAPIMocks(page, { contextTotal: 200000, sessionContextUsed: 45000 });
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // 45000 tokens → formatted as "45K"
    await expect(page.getByText(/45K/).first()).toBeVisible({ timeout: 10_000 });
  });

  test("shows progress bar with percentage when contextTotal > 0", async ({ page }) => {
    await setupAPIMocks(page, { contextTotal: 200000, sessionContextUsed: 100000 });
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // 100K / 200K = 50%
    await expect(page.getByText(/50%/).first()).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText(/100K/).first()).toBeVisible({ timeout: 5_000 });
  });

  test("shows Unknown badge when contextTotal is 0 even with non-zero contextUsed", async ({ page }) => {
    await setupAPIMocks(page, { contextTotal: 0, sessionContextUsed: 50000 });
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    await expect(page.getByText(/50K/).first()).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText(/Unknown/i).first()).toBeVisible({ timeout: 5_000 });
  });

  test("updates context bar in real-time via SSE session.next.step.ended", async ({ page }) => {
    await setupAPIMocks(page, { contextTotal: 200000, sessionContextUsed: null });

    await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, (r: Route) =>
      r.fulfill({
        status: 200,
        headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
        body: stepEndedSSE(SESSION_ID, 80000, 5000, 2000),
      }));

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // 80000 + 5000 + 2000 = 87000 → "87K"
    await expect(page.getByText(/87K/).first()).toBeVisible({ timeout: 10_000 });
  });

  test("compaction banner appears when contextUsed drops >50% via SSE", async ({ page }) => {
    await setupAPIMocks(page, { contextTotal: 200000, sessionContextUsed: 100000 });

    let sendSSE: ((body: string) => void) | null = null;
    await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, async (r: Route) => {
      await new Promise<void>((resolve) => {
        sendSSE = (body: string) => {
          r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
          resolve();
        };
      });
    });

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // Wait for cold-start value — prevContextUsedRef is now 100000
    await expect(page.getByText(/100K/).first()).toBeVisible({ timeout: 10_000 });

    // Fire SSE event: 40K < 50% of 100K → compaction detected
    sendSSE?.(stepEndedSSE(SESSION_ID, 40000));

    await expect(page.getByText(/context compacted/i)).toBeVisible({ timeout: 10_000 });
  });

  test("compaction banner can be dismissed", async ({ page }) => {
    await setupAPIMocks(page, { contextTotal: 200000, sessionContextUsed: 100000 });

    // Deferred SSE — hold the connection open until we're ready to send the event.
    // This ensures prevContextUsedRef is set from the cold-start sessions list
    // before the SSE compaction event fires.
    let sendSSE: ((body: string) => void) | null = null;
    await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, async (r: Route) => {
      await new Promise<void>((resolve) => {
        sendSSE = (body: string) => {
          r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
          resolve();
        };
      });
    });

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // Wait for cold-start value — prevContextUsedRef is now 100000
    await expect(page.getByText(/100K/).first()).toBeVisible({ timeout: 10_000 });

    // Now fire the SSE event that triggers compaction (40K < 50% of 100K)
    sendSSE?.(stepEndedSSE(SESSION_ID, 40000));

    await expect(page.getByText(/context compacted/i)).toBeVisible({ timeout: 10_000 });
    await page.getByRole("button", { name: /dismiss/i }).click();
    await expect(page.getByText(/context compacted/i)).not.toBeVisible();
  });
});
