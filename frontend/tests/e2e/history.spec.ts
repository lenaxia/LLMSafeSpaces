/**
 * E2E test for the "show more history" feature ("Load earlier messages").
 *
 * Walks the full browser flow against a fully-mocked backend:
 *   GET /workspaces/<ws>/sessions/<ses>/message?limit=50[&before=<id>]
 *     → real messagesApi.getHistoryPage (URL build + transformHistory +
 *       X-Next-Cursor header parsing) → real useMessageHistory (TanStack
 *       useInfiniteQuery) → real ChatPage wiring → real MessageList
 *       "Load earlier messages" button → click → fetchNextPage → prepend.
 *
 * The mocked message-history endpoint implements the SAME pagination
 * contract as the Go backend (paginateOpencodeHistory in
 * api/internal/handlers/proxy_handlers.go): returns the newest `limit`
 * displayable messages (or up to `limit` strictly before `before`),
 * oldest-first, with X-Next-Cursor = oldest-id-of-page iff more remain.
 *
 * All other backend APIs (auth, workspaces, sessions, SSE) are mocked too,
 * mirroring streaming.spec.ts, so the test runs with no live backend.
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

const WORKSPACE_ID = "ws-hist-e2e";
const SESSION_ID = "sess-hist-e2e";
const API = "**/api/v1";
const PAGE_LIMIT = 50;

type Role = "user" | "assistant";
interface RawMsg {
  info: { role: Role; id: string; time?: { created?: number } };
  parts: Array<{ type: string; text?: string }>;
}

function buildHistory(count: number): RawMsg[] {
  return Array.from({ length: count }, (_, i) => ({
    info: {
      role: (i % 2 === 0 ? "user" : "assistant") as Role,
      id: `msg_${String(i).padStart(4, "0")}`,
      time: { created: (i + 1) * 1000 },
    },
    parts: [{ type: "text", text: `body-${i}` }],
  }));
}

// Stateful message-history handler. Reads ?before / ?limit off the request URL
// and serves the matching page, emitting X-Next-Cursor exactly when the real
// server would. `legacyNoCursor` simulates the pre-#440 server (return
// everything, never emit the cursor) to guard that regression in the browser.
function makeHistoryHandler(all: RawMsg[], legacyNoCursor: boolean) {
  return async (route: Route) => {
    if (legacyNoCursor) {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(all) });
      return;
    }
    const url = new URL(route.request().url());
    const before = url.searchParams.get("before") ?? "";
    const limit = Number(url.searchParams.get("limit") ?? PAGE_LIMIT);

    let endExclusive = all.length;
    if (before) {
      const idx = all.findIndex((m) => m.info.id === before);
      if (idx < 0) {
        await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
        return;
      }
      endExclusive = idx;
    }
    let start = endExclusive - limit;
    if (start < 0) start = 0;
    const page = all.slice(start, endExclusive);

    const headers: Record<string, string> = {};
    if (start > 0 && page.length > 0) headers["X-Next-Cursor"] = page[0].info.id;

    await route.fulfill({
      status: 200,
      contentType: "application/json",
      headers,
      body: JSON.stringify(page),
    });
  };
}

async function setupAPIMocks(page: Page, opts: { messageCount: number; legacyNoCursor?: boolean }) {
  // Auth
  await page.route(`${API}/auth/login`, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ token: "e2e-token", user: { id: "u1", username: "tester", role: "user" } }),
    });
  });
  await page.route(`${API}/auth/me`, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ id: "u1", username: "tester", email: "t@example.com", role: "user", active: true }),
    });
  });
  await page.route(`${API}/auth/config`, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false }),
    });
  });

  // Workspaces + status (Active so ChatPage is "ready" and history loads)
  await page.route(`${API}/workspaces`, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        items: [{ id: WORKSPACE_ID, name: "History E2E", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }],
        pagination: { limit: 50, offset: 0, total: 1 },
      }),
    });
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/status`, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" } }),
    });
  });

  // Sessions (the session in the URL must exist so ChatPage treats it as live)
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions`, async (route) => {
    if (route.request().method() === "POST") {
      await route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, sessionId: SESSION_ID }) });
    } else {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify([{ id: SESSION_ID, title: "History E2E", messageCount: opts.messageCount, status: "idle" }]),
      });
    }
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/ensure`, async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID }) });
  });

  // Models (ModelSelector + ChatPage subscribe)
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/models`, async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ models: [], currentModel: "" }) });
  });

  // Message history — paginated fake backend
  await page.route(
    `${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/message`,
    makeHistoryHandler(buildHistory(opts.messageCount), !!opts.legacyNoCursor),
  );

  // SSE — idle, no streaming interference
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, async (route) => {
    const events = [{ type: "session.status", session_id: SESSION_ID, status: "idle" }];
    const body = events.map((e) => `data: ${JSON.stringify(e)}\n`).join("\n");
    await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
  });
}

test.describe("'Load earlier messages' (show more history)", () => {
  test("renders the button when more history exists, and prepends older messages on click", async ({ page }) => {
    test.setTimeout(30_000);
    // 60 messages → page 1 = newest 50 (msg_0010..msg_0059), cursor msg_0010;
    // page 2 = oldest 10 (msg_0000..msg_0009), no cursor.
    await setupAPIMocks(page, { messageCount: 60 });
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // Workspace ready → history page 1 loaded.
    await expect(page.locator("h2")).toContainText("History E2E", { timeout: 15_000 });

    // Newest message from page 1 visible; oldest (still on page 2) not yet.
    await expect(page.getByText("body-59").first()).toBeVisible();
    expect(await page.getByText("body-0").count()).toBe(0);

    // The affordance is rendered.
    await expect(page.getByRole("button", { name: /load earlier messages/i })).toBeVisible();

    // Click it → the older page is fetched (?before=msg_0010) and prepended.
    await page.getByRole("button", { name: /load earlier messages/i }).click();
    await expect(page.getByText("body-0").first()).toBeVisible({ timeout: 10_000 });

    // No duplicate of the boundary message (msg_0010 appears once).
    await expect(page.getByText("body-10")).toHaveCount(1);

    // End of history → button is gone.
    await expect(page.getByRole("button", { name: /load earlier messages/i })).toBeHidden();
  });

  test("does not render the button when the full history fits in a single page", async ({ page }) => {
    test.setTimeout(30_000);
    await setupAPIMocks(page, { messageCount: 5 });
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.locator("h2")).toContainText("History E2E", { timeout: 15_000 });

    await expect(page.getByText("body-4").first()).toBeVisible();
    await expect(page.getByRole("button", { name: /load earlier messages/i })).toBeHidden();
  });

  test("REGRESSION: never renders the button when the server omits X-Next-Cursor (pre-#440 server)", async ({ page }) => {
    test.setTimeout(30_000);
    // Legacy server returns all 84 messages in one shot with no cursor. The
    // frontend must not offer "Load earlier" (it has no signal that more
    // exist). This is the documented production bug; the guard is that the
    // SERVER must paginate. If the API regresses, the user sees everything at
    // once and no button — this test pins that observed behaviour so a future
    // change to either side is caught.
    await setupAPIMocks(page, { messageCount: 84, legacyNoCursor: true });
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.locator("h2")).toContainText("History E2E", { timeout: 15_000 });

    await expect(page.getByText("body-83").first()).toBeVisible();
    await expect(page.getByText("body-0").first()).toBeVisible();
    await expect(page.getByRole("button", { name: /load earlier messages/i })).toBeHidden();
  });
});
