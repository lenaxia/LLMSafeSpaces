/**
 * E2E test for the SSE streaming pipeline.
 *
 * Walks the full flow (post US-69.10 hard cutover):
 *   backend (agent event) → contract stream (protojson StreamFrame SSE on
 *   /contract-events) → useContractStream fold → ChatPage → ChatView
 *
 * All backend APIs are mocked via Playwright route interception, including
 * the SSE endpoints which return real SSE-formatted data. The contract
 * stream carries snapshot-first StreamFrames; the workspace stream
 * (/session-events) is platform-events-only after the cutover.
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

const WORKSPACE_ID = "ws-e2e-1";
const SESSION_ID = "sess-e2e-1";
const API_PREFIX = "**/api/v1";

// --- contract-frame builders (mirrors contract-stream.spec.ts) ---------------

function snapshotFrame(atSeq: number | bigint, sessions: Array<Record<string, unknown>>): string {
  return `data: ${JSON.stringify({
    snapshot: { atSeq: String(atSeq), snapshot: { sessions } },
  })}\n\n`;
}

function eventFrame(seq: number | bigint, event: Record<string, unknown>): string {
  return `data: ${JSON.stringify({
    event: { seq: String(seq), event },
  })}\n\n`;
}

function partDelta(seq: number | bigint, partId: string, delta: string, messageId = "msg_a1"): string {
  return eventFrame(seq, {
    type: "EVENT_TYPE_PART_DELTA",
    sessionId: SESSION_ID,
    messageId,
    partId,
    delta,
  });
}

function partEnd(seq: number | bigint, partId: string, text: string, messageId = "msg_a1"): string {
  return eventFrame(seq, {
    type: "EVENT_TYPE_PART_END",
    sessionId: SESSION_ID,
    messageId,
    partId,
    part: { id: partId, type: "PART_TYPE_TEXT", text },
  });
}

function idleSnapshot(): string {
  return snapshotFrame(1, [{ sessionId: SESSION_ID, status: "SESSION_STATUS_IDLE", inFlightParts: [], queueDepth: 0, pendingInputs: [] }]);
}

/**
 * Set up API route mocks for a fully mocked backend pipeline.
 */
async function setupAPIMocks(page: Page) {
  // Auth
  await page.route(`${API_PREFIX}/auth/login`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ token: "e2e-test-token", user: { id: "u1", username: "testuser", role: "user" } }) });
  });
  await page.route(`${API_PREFIX}/auth/me`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "testuser", email: "test@example.com", role: "user", active: true }) });
  });
  await page.route(`${API_PREFIX}/auth/config`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false }) });
  });

  // Workspaces
  await page.route(`${API_PREFIX}/workspaces`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WORKSPACE_ID, name: "E2E Test WS", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/status`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" } }) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/activate`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ resumed: WORKSPACE_ID }) });
  });

  // Sessions
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions`, async (route: Route) => {
    if (route.request().method() === "POST") {
      await route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, sessionId: SESSION_ID }) });
    } else {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([{ id: SESSION_ID, title: "E2E Test Session", messageCount: 0, status: "idle" }]) });
    }
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions/*/ensure`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID }) });
  });

  // Message history
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/message`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
  });

  // Platform stream — platform events only (US-69.10 hard cutover: the
  // session-state dialect moved to the contract stream).
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/session-events`, async (route: Route) => {
    const events = [{ type: "workspace.phase", phase: "Active" }];
    const sseBody = events.map((e) => `data: ${JSON.stringify(e)}\n`).join("\n");
    await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: sseBody });
  });

  // Contract stream — snapshot-first StreamFrames (default: idle; tests
  // override with streamed content). Every body opens with a snapshot or
  // the client reconnects (protocol rule).
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/contract-events`, async (route: Route) => {
    await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: idleSnapshot() });
  });
}

test.describe("SSE streaming pipeline (mock backend)", () => {
  test.beforeEach(async ({ page }) => {
    await setupAPIMocks(page);
  });

  test("page loads with mocked backend and renders workspace", async ({ page }) => {
    // Navigate to a workspace without a session
    await page.goto(`/chat/${WORKSPACE_ID}`);
    // The page should show the workspace header
    await expect(page.locator("h2")).toContainText("E2E Test WS", { timeout: 10000 });
  });

  test("SSE endpoints are intercepted and return event-stream content type", async ({ page }) => {
    // Both SSE endpoints the chat consumes must be intercepted: the
    // platform stream (/session-events) and the contract stream
    // (/contract-events) which now drives the streamed content.
    let platformRouteHit = false;
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/session-events`, async (route: Route) => {
      platformRouteHit = true;
      const events = [{ type: "workspace.phase", phase: "Active" }];
      const sseBody = events.map((e) => `data: ${JSON.stringify(e)}\n`).join("\n");
      await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: sseBody });
    });
    let contractRouteHit = false;
    let contractHits = 0;
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/contract-events`, async (route: Route) => {
      contractRouteHit = true;
      contractHits++;
      // Deliver on the first TWO hits (React StrictMode's dev double-mount
      // destroys connection #1 before its body is processed), then hold
      // forever — no delta re-delivery on reconnect cycles.
      if (contractHits <= 2) {
        const body =
          snapshotFrame(5, [{ sessionId: SESSION_ID, status: "SESSION_STATUS_BUSY", inFlightParts: [{ id: "p1", type: "PART_TYPE_TEXT", text: "Hello from" }], queueDepth: 0, pendingInputs: [] }]) +
          partDelta(6, "p1", " SSE") +
          partEnd(7, "p1", "Hello from SSE stream!");
        await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
        return;
      }
      await new Promise<never>(() => {}); // hold — no further delivery
    });
    // User stream: busy status seeds the provider's busy map so the
    // streaming bubble renders while the turn is in flight.
    await page.route(`${API_PREFIX}/events`, async (route: Route) => {
      const body = `data: ${JSON.stringify({ type: "session.status", workspace_id: WORKSPACE_ID, session_id: SESSION_ID, status: "busy" })}\n\n`;
      await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
    });
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.getByText("Hello from SSE stream!").first()).toBeVisible({ timeout: 10_000 });
    expect(platformRouteHit).toBe(true);
    expect(contractRouteHit).toBe(true);
  });

  test("page handles SSE connection without errors", async ({ page }) => {
    // Collect console errors
    const consoleErrors: string[] = [];
    page.on("console", (msg) => {
      if (msg.type() === "error") {
        consoleErrors.push(msg.text());
      }
    });

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.locator("h2")).toContainText("E2E Test WS", { timeout: 10000 });

    // Wait for SSE connection to be established
    await page.waitForTimeout(2000);

    // There should be no SSE-related errors
    const sseErrors = consoleErrors.filter((e) => e.includes("EventSource") || e.includes("SSE") || e.includes("events"));
    expect(sseErrors).toEqual([]);
  });
});
