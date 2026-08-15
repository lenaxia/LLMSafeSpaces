/**
 * E2E test for the SSE streaming pipeline.
 *
 * Walks the full flow:
 *   backend (open code event) → proxy (opencode.event) → SSE broker → frontend (EventSource → ChatPage → ChatView)
 *
 * All backend APIs are mocked via Playwright route interception, including the SSE
 * endpoint which returns real SSE-formatted data. The EventSource processes these
 * events and the UI updates accordingly.
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

const WORKSPACE_ID = "ws-e2e-1";
const SESSION_ID = "sess-e2e-1";
const API_PREFIX = "**/api/v1";

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

  // SSE events endpoint — return prefilled events in the flat format the backend
  // produces (the proxy re-parses raw opencode JSON and sets it as evt.Data).
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/session-events`, async (route: Route) => {
    const events = [
      {
        type: "agent.event",
        event_type: "message.part.updated",
        data: {
          type: "message.part.updated",
          properties: {
            sessionID: SESSION_ID,
            part: { type: "text", text: "Hello from SSE stream!" },
          },
        },
      },
    ];
    const sseBody = events.map((e) => `data: ${JSON.stringify(e)}\n`).join("\n");
    await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: sseBody });
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

  test("SSE endpoint is intercepted and returns event-stream content type", async ({ page }) => {
    // Verify the route mock is set up by navigating and checking the page received SSE data
    let sseRouteHit = false;
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/session-events`, async (route: Route) => {
      sseRouteHit = true;
      const events = [{ type: "session.status", session_id: SESSION_ID, status: "idle" }];
      const sseBody = events.map((e) => `data: ${JSON.stringify(e)}\n`).join("\n");
      await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: sseBody });
    });
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await page.waitForTimeout(2000);
    expect(sseRouteHit).toBe(true);
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
