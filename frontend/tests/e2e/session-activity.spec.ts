/**
 * E2E tests for Epic 37: Session Activity & Unread State UX (tests 40–46 + 53)
 *
 * Tests 40–46 cover the session activity indicators that surface across
 * workspaces:
 *   40 – Activity spinner across workspaces
 *   41 – Unread pulsation after completion
 *   42 – Pulsation clears on navigate
 *   43 – New messages divider
 *   44 – Divider gone on revisit
 *   45 – Persistence across page refresh
 *   46 – Collapsed workspace spinner
 *   53 – Mobile swipeable sidebar shows indicators (regression)
 *
 * All backend APIs are intercepted via Playwright route mocking. The
 * user-scoped SSE endpoint (/api/v1/events) is used to inject session.status
 * events in real-time. The workspace-scoped SSE endpoint is kept alive and
 * silent to avoid interfering with the chat UI.
 */
import { test, expect, type Page, type Route } from "@playwright/test";

const WS_A = "ws-sa-a";   // workspace A — active session
const WS_B = "ws-sa-b";   // workspace B — unread session
const SESS_A1 = "ses_sa_a1";  // session in WS_A — will become busy then idle
const SESS_B1 = "ses_sa_b1";  // session in WS_B — will become unread
const API = "**/api/v1";

// ---------------------------------------------------------------------------
// Standard mock setup
// ---------------------------------------------------------------------------

async function setupBase(page: Page, opts: {
  sessAStatus?: "active" | "idle";
  sessBHasUnread?: boolean;
  sessALastSeenAt?: string;
  sessAMessages?: unknown[];
} = {}) {
  const sessAStatus = opts.sessAStatus ?? "idle";
  const sessBHasUnread = opts.sessBHasUnread ?? false;
  const sessAMessages = opts.sessAMessages ?? [];

  // Auth
  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true }) }));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false }) }));

  // Workspace list — WS_A expanded with session A1; WS_B collapsed
  await page.route(`${API}/workspaces`, (r: Route) => {
    if (r.request().method() !== "GET") { r.continue(); return; }
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({
      items: [
        { id: WS_A, name: "Alpha", userId: "u1", runtime: "base", storageSize: "1Gi", phase: "Active" },
        { id: WS_B, name: "Beta",  userId: "u1", runtime: "base", storageSize: "1Gi", phase: "Active" },
      ],
      pagination: { limit: 50, offset: 0, total: 2 },
    })});
  });

  // Status endpoints
  await page.route(`${API}/workspaces/${WS_A}/status`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0" }, sessions: [] }) }));
  await page.route(`${API}/workspaces/${WS_B}/status`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0" }, sessions: [] }) }));

  // Session lists
  await page.route(`${API}/workspaces/${WS_A}/sessions`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([
      { id: SESS_A1, title: "Task Alpha", status: sessAStatus, hasUnread: false, messageCount: 2, lastSeenAt: opts.sessALastSeenAt },
    ])}));
  await page.route(`${API}/workspaces/${WS_B}/sessions`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([
      { id: SESS_B1, title: "Task Beta", status: "idle", hasUnread: sessBHasUnread, messageCount: 3, lastSeenAt: "2026-06-10T10:00:00Z" },
    ])}));

  // Message history — default empty; callers can pass sessAMessages to override
  await page.route(`${API}/workspaces/${WS_A}/sessions/${SESS_A1}/message**`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(sessAMessages) }));

  // Models
  await page.route(`${API}/workspaces/${WS_A}/models`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ models: [], currentModel: "" }) }));

  // Mark-seen (fire-and-forget)
  await page.route(`${API}/workspaces/*/sessions/*/seen`, (r: Route) =>
    r.fulfill({ status: 204, body: "" }));

  // Workspace-scoped SSE — silent keep-alive
  await page.route(`${API}/workspaces/*/session-events`, (r: Route) =>
    r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));
}

// ---------------------------------------------------------------------------
// Tests 40–46 + 53
// ---------------------------------------------------------------------------

test.describe("Epic 37: Session Activity & Unread State UX", () => {

  // Tests 41-46, 53 are marked .fixMe: they depend on Playwright
  // intercepting fetch-based SSE connections (createSSEConnection uses
  // fetch + ReadableStream, not EventSource). Playwright's route.fulfill
  // returns a complete body, so the SSE reader immediately gets done=true,
  // triggering reconnection loops and reader.cancel errors. The unit +
  // integration tests in SessionActivityProvider.test.tsx and
  // session-activity.test.tsx cover the same logic without the SSE
  // transport layer. These E2E tests need a Playwright SSE mock helper
  // (route handler that streams chunks) to work — tracked separately.

  // Test 40: Activity spinner visible on session row when status:active arrives via SSE.
  test("40 — activity spinner appears on session row when busy", async ({ page }) => {
    await setupBase(page);

    let userSSEFulfill: ((body: string) => void) | null = null;
    await page.route(`${API}/events`, async (r: Route) => {
      await new Promise<void>((resolve) => {
        userSSEFulfill = (body: string) => {
          r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
          resolve();
        };
      });
    });

    await page.goto(`/chat/${WS_A}/${SESS_A1}`);
    await expect(page.getByText("Task Alpha")).toBeVisible({ timeout: 10_000 });

    // Emit session.status busy via user SSE
    userSSEFulfill?.(
      `data: ${JSON.stringify({ type: "session.status", workspace_id: WS_A, session_id: SESS_A1, status: "busy" })}\n\n`,
    );

    // Spinner should replace the MessageSquare icon on the session row
    await expect(page.locator(".animate-spin").first()).toBeVisible({ timeout: 5_000 });
  });

  // Test 41: Unread pulsation appears on a different session after its agent completes.
  test.fixme("41 — unread pulsation appears after session completion in another workspace", async ({ page }) => {
    await setupBase(page, { sessBHasUnread: true });

    await page.route(`${API}/events`, (r: Route) =>
      r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));

    await page.goto(`/chat/${WS_A}/${SESS_A1}`);
    await expect(page.getByText("Task Beta")).toBeVisible({ timeout: 10_000 });

    // Beta session loaded with hasUnread:true — its title should pulse
    await expect(page.locator(".animate-unread-pulse").first()).toBeVisible({ timeout: 5_000 });
  });

  // Test 42: Pulsation clears on navigate to the unread session.
  test.fixme("42 — pulsation clears when navigating to the unread session", async ({ page }) => {
    await setupBase(page, { sessBHasUnread: true });

    await page.route(`${API}/events`, (r: Route) =>
      r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));
    // Seen endpoint — 204
    await page.route(`${API}/workspaces/${WS_B}/sessions/${SESS_B1}/seen`, (r: Route) =>
      r.fulfill({ status: 204, body: "" }));
    // WS_B session message history
    await page.route(`${API}/workspaces/${WS_B}/sessions/${SESS_B1}/message**`, (r: Route) =>
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }));
    await page.route(`${API}/workspaces/${WS_B}/models`, (r: Route) =>
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ models: [], currentModel: "" }) }));

    await page.goto(`/chat/${WS_A}/${SESS_A1}`);
    await expect(page.locator(".animate-unread-pulse").first()).toBeVisible({ timeout: 10_000 });

    // Navigate to the unread session in Beta
    await page.getByText("Task Beta").click();
    await expect(page.url()).toContain(SESS_B1);

    // Pulsation should clear — no more animate-unread-pulse elements
    await expect(page.locator(".animate-unread-pulse")).toHaveCount(0, { timeout: 3_000 });
  });

  // Test 43: New messages divider appears when session has unseen messages.
  test("43 — new messages divider appears in chat when lastSeenAt is in the past", async ({ page }) => {
    const lastSeenAt = "2026-06-10T10:00:00Z";
    await setupBase(page, {
      sessALastSeenAt: lastSeenAt,
      sessAMessages: [
        { id: "msg-old", role: "user", parts: [{ type: "text", text: "Old question" }], info: { role: "user", id: "msg-old", time: { created: new Date("2026-06-10T09:59:00Z").getTime() } } },
        { id: "msg-new", role: "user", parts: [{ type: "text", text: "New question" }], info: { role: "user", id: "msg-new", time: { created: new Date("2026-06-10T10:01:00Z").getTime() } } },
      ],
    });

    await page.route(`${API}/events`, (r: Route) =>
      r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));

    await page.goto(`/chat/${WS_A}/${SESS_A1}`);

    // Wait for sidebar sessions to load (confirms sessions query is in cache before
    // asserting the divider — lastSeenAt is only reactive once the cache is populated).
    await expect(page.getByText("Task Alpha")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText("Old question")).toBeVisible({ timeout: 5_000 });
    await expect(page.getByText("New question")).toBeVisible({ timeout: 3_000 });

    // The "New messages" divider should appear once lastSeenAt is in cache.
    await expect(page.getByRole("separator", { name: "New messages" })).toBeVisible({ timeout: 8_000 });
  });

  // Test 44: Divider is gone on revisit after mark-seen fires.
  test("44 — divider gone on revisit after mark-seen fires", async ({ page }) => {
    const lastSeenAt = "2026-06-11T12:00:00Z"; // future — all messages already seen
    await setupBase(page, {
      sessALastSeenAt: lastSeenAt,
      sessAMessages: [
        { id: "msg-1", role: "user", parts: [{ type: "text", text: "Question" }], createdAt: "2026-06-10T09:00:00Z" },
      ],
    });

    await page.route(`${API}/events`, (r: Route) =>
      r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));

    await page.goto(`/chat/${WS_A}/${SESS_A1}`);
    await expect(page.getByText("Question")).toBeVisible({ timeout: 10_000 });

    // lastSeenAt is in the future — all messages already seen → no divider
    await expect(page.getByRole("separator", { name: "New messages" })).not.toBeVisible();
  });

  // Test 45: Persistence across page refresh — hasUnread from REST survives reload.
  test.fixme("45 — unread indicator persists across page refresh", async ({ page }) => {
    await setupBase(page, { sessBHasUnread: true });

    await page.route(`${API}/events`, (r: Route) =>
      r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));

    await page.goto(`/chat/${WS_A}/${SESS_A1}`);
    await expect(page.locator(".animate-unread-pulse").first()).toBeVisible({ timeout: 10_000 });

    // Reload
    await page.reload();
    await expect(page.locator(".animate-unread-pulse").first()).toBeVisible({ timeout: 10_000 });
  });

  // Test 46: Collapsed workspace shows spinner when a session is busy.
  test.fixme("46 — collapsed workspace shows spinner badge when session is busy", async ({ page }) => {
    await setupBase(page, { sessAStatus: "active" });

    await page.route(`${API}/events`, (r: Route) =>
      r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));

    // Navigate to WS_B so WS_A is collapsed
    await page.route(`${API}/workspaces/${WS_B}/sessions/${SESS_B1}/message**`, (r: Route) =>
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }));
    await page.route(`${API}/workspaces/${WS_B}/models`, (r: Route) =>
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ models: [], currentModel: "" }) }));

    await page.goto(`/chat/${WS_B}/${SESS_B1}`);
    await expect(page.getByText("Alpha")).toBeVisible({ timeout: 10_000 });

    // Click the workspace header button (not the session row) to collapse it.
    // Use getByRole to target the workspace group button specifically, avoiding
    // strict-mode violation from "Task Alpha" also matching "Alpha".
    await page.getByRole("button", { name: "Alpha", exact: true }).click();

    // Collapsed Alpha workspace should show the blue spinner (text-blue-500)
    await expect(page.locator(".animate-spin.text-blue-500").first()).toBeVisible({ timeout: 3_000 });
  });

  // Test 53: Mobile sidebar swipeable — activity indicators visible after swipe open.
  test.fixme("53 — mobile sidebar shows unread pulsation after swipe open", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 }); // iPhone 14
    await setupBase(page, { sessBHasUnread: true });

    await page.route(`${API}/events`, (r: Route) =>
      r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));

    await page.goto(`/chat/${WS_A}/${SESS_A1}`);

    // On mobile, sidebar starts closed. Simulate a left-edge swipe using
    // mouse events only — touchscreen.tap requires hasTouch on the context
    // which is not set in the default Playwright project config.
    await page.mouse.move(10, 400);
    await page.mouse.down();
    await page.mouse.move(200, 400, { steps: 10 });
    await page.mouse.up();

    // Sidebar should show Task Beta with unread pulsation
    await expect(page.locator(".animate-unread-pulse").first()).toBeVisible({ timeout: 5_000 });
  });
});
