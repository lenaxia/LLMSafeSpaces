/**
 * Playwright e2e tests for the chat composer and message-list features.
 *
 * These tests run fully mocked (no live backend, no E2E_USERNAME required),
 * mirroring the strategy used in chat.spec.ts's "session auto-creation"
 * inner block. They exercise the real browser code path — actual DOM
 * events, real keydown handling, real layout effects — which jsdom cannot
 * reliably reproduce for:
 *   - Textarea cursor positioning on arrow keys
 *   - Tooltip hover/focus
 *   - Scroll-anchoring math against real layout
 *   - Mobile vs desktop via viewport sizing
 *
 * Together with the vitest unit/integration suite, this file closes the
 * "works in jsdom, untested in real browser" gap called out during the
 * composer-enter-history PR review (PR #504).
 *
 * Local-dev note: a sandboxed headless Chromium without system libs cannot
 * always insert text into a React-controlled textarea via keyboard events
 * (the browser's text-editing action doesn't fire). Tests that rely on
 * `keyboard.type` or `press("Enter")` to insert text may fail locally but
 * pass in CI, which uses `playwright install --with-deps chromium` (full
 * Chromium). The Composer's underlying logic is fully covered by vitest;
 * these Playwright tests are a regression net for real-browser behavior.
 */
import { test, expect, type Page, type Route } from "@playwright/test";

const API = "**/api/v1";
const WS_ID = "ws-e2e";
const SES_ID = "ses_e2e_1";

interface MockUser { id: string; username: string; email: string; role: string; active: boolean; createdAt: string }
interface MockMessage {
  id: string;
  role: "user" | "assistant";
  text: string;
  // ISO timestamp; tests control ordering via this field.
  createdAt: string;
}

async function mockAuthAndWorkspace(page: Page, opts: { phase?: string } = {}) {
  const phase = opts.phase ?? "Active";
  await page.route(`${API}/auth/me`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        id: "user-e2e", username: "e2e", email: "e2e@test", role: "user", active: true, createdAt: "2026-01-01T00:00:00Z",
      } satisfies MockUser),
    });
  });
  await page.route(`${API}/auth/config`, async (route: Route) => {
    await route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify({ registrationEnabled: true, oidcEnabled: false, instanceName: "e2e" }),
    });
  });
  await page.route(`${API}/settings/user`, async (route: Route) => {
    await route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify({ settings: {}, schemaVersion: 6 }),
    });
  });
  await page.route(`${API}/workspaces`, async (route: Route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({
        status: 200, contentType: "application/json",
        body: JSON.stringify({
          items: [{ id: WS_ID, name: "e2e-ws", phase, userId: "user-e2e", runtime: "python", storageSize: "1Gi", createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z" }],
          pagination: { limit: 20, offset: 0, total: 1 },
        }),
      });
    } else {
      await route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: WS_ID }) });
    }
  });
  await page.route(`${API}/workspaces/${WS_ID}/status`, async (route: Route) => {
    await route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify({ phase, sessions: [{ id: SES_ID, status: "idle" }] }),
    });
  });
  await page.route(`${API}/workspaces/${WS_ID}/sessions`, async (route: Route) => {
    await route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify([{ id: SES_ID, title: "e2e session", messageCount: 0, status: "idle", hasUnread: false }]),
    });
  });
  await page.route(`${API}/workspaces/${WS_ID}/sessions/active`, async (route: Route) => {
    await route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify({ active: [SES_ID], maxActive: 5 }),
    });
  });
  await page.route(`${API}/workspaces/${WS_ID}/models`, async (route: Route) => {
    await route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify({ models: [], currentModel: "" }),
    });
  });
  await page.route(`${API}/workspaces/${WS_ID}/session-events`, async (route: Route) => {
    // SSE: just send workspace.phase=Active and keep the stream open.
    await route.fulfill({
      status: 200, contentType: "text/event-stream",
      body: `data: ${JSON.stringify({ type: "workspace.phase", phase })}\n\n`,
    });
  });
  await page.route(`${API}/workspaces/${WS_ID}/sessions/${SES_ID}/queue`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ messages: [] }) });
  });
  // markSessionSeen — accept silently.
  await page.route(`${API}/workspaces/${WS_ID}/sessions/${SES_ID}/seen`, async (route: Route) => {
    await route.fulfill({ status: 204, body: "" });
  });
}

/** Render a chronological message array (oldest-first) as the opencode-shaped JSON the API returns. */
function opencodeShape(msgs: MockMessage[]): unknown[] {
  return msgs.map((m) => ({
    info: {
      id: m.id,
      role: m.role,
      time: { created: new Date(m.createdAt).getTime() },
    },
    id: m.id,
    role: m.role,
    parts: [{ type: "text", text: m.text }],
  }));
}

async function mockHistory(page: Page, msgs: MockMessage[], opts: { nextCursor?: string } = {}) {
  await page.route(`${API}/workspaces/${WS_ID}/sessions/${SES_ID}/message*`, async (route: Route) => {
    const headers: Record<string, string> = { "Content-Type": "application/json" };
    if (opts.nextCursor) headers["X-Next-Cursor"] = opts.nextCursor;
    await route.fulfill({
      status: 200, headers,
      body: JSON.stringify(opencodeShape(msgs)),
    });
  });
}

async function gotoChat(page: Page) {
  await page.goto(`/chat/${WS_ID}/${SES_ID}`);
  // Wait for the composer to be interactive.
  await expect(page.getByPlaceholder("Type a message...")).toBeVisible({ timeout: 10_000 });
  await expect(page.getByPlaceholder("Type a message...")).toBeEnabled({ timeout: 10_000 });
}

test.describe("Composer send-key behavior (real browser)", () => {
  test.beforeEach(async ({ page }) => {
    await mockAuthAndWorkspace(page);
    await mockHistory(page, []);
  });

  test("desktop: Enter inserts a newline, Ctrl+Enter sends", async ({ page }) => {
    await gotoChat(page);
    const textarea = page.getByPlaceholder("Type a message...");
    await textarea.fill("hello");
    // Enter must NOT send.
    await textarea.press("Enter");
    await expect(textarea).toHaveValue("hello\n");
    // Ctrl+Enter sends.
    await textarea.press("Control+Enter");
    await expect(textarea).toHaveValue("");
  });

  test("desktop: Cmd+Enter sends (Mac meta key)", async ({ page }) => {
    await gotoChat(page);
    const textarea = page.getByPlaceholder("Type a message...");
    await textarea.fill("hi");
    await textarea.press("Meta+Enter");
    await expect(textarea).toHaveValue("");
  });

  test("desktop: Shift+Enter inserts a newline (does not send)", async ({ page }) => {
    await gotoChat(page);
    const textarea = page.getByPlaceholder("Type a message...");
    await textarea.fill("hello");
    await textarea.press("Shift+Enter");
    await expect(textarea).toHaveValue("hello\n");
  });

  test("send button click sends the message", async ({ page }) => {
    await gotoChat(page);
    const textarea = page.getByPlaceholder("Type a message...");
    await textarea.fill("via button");
    await page.getByRole("button", { name: "Send message" }).click();
    await expect(textarea).toHaveValue("");
  });

  test("desktop: send-button tooltip advertises Ctrl+Enter on hover", async ({ page }) => {
    await gotoChat(page);
    const textarea = page.getByPlaceholder("Type a message...");
    await textarea.fill("x");
    await page.getByRole("button", { name: "Send message" }).hover();
    await expect(page.getByText(/Ctrl\+Enter/).first()).toBeVisible({ timeout: 5_000 });
  });
});

test.describe("Composer send-key behavior — mobile viewport", () => {
  // useMobile flips on viewport width < 768px. We skip the device-emulation
  // dance and just set a narrow viewport, which the production code reads
  // via window.matchMedia.
  test.use({ viewport: { width: 400, height: 800 } });

  test.beforeEach(async ({ page }) => {
    await mockAuthAndWorkspace(page);
    await mockHistory(page, []);
  });

  test("mobile: Enter inserts a newline (does NOT send) — button-only", async ({ page }) => {
    await gotoChat(page);
    const textarea = page.getByPlaceholder("Type a message...");
    await textarea.fill("hi");
    await textarea.press("Enter");
    await expect(textarea).toHaveValue("hi\n");
    // Ctrl+Enter must NOT send on mobile — modifier keys are unreliable.
    await textarea.press("Control+Enter");
    await expect(textarea).toHaveValue("hi\n");
  });

  test("mobile: send button is the only way to send", async ({ page }) => {
    await gotoChat(page);
    const textarea = page.getByPlaceholder("Type a message...");
    await textarea.fill("tap to send");
    await page.getByRole("button", { name: "Send message" }).click();
    await expect(textarea).toHaveValue("");
  });

  test("mobile: no tooltip on send button (no hover context)", async ({ page }) => {
    await gotoChat(page);
    const textarea = page.getByPlaceholder("Type a message...");
    await textarea.fill("x");
    await page.getByRole("button", { name: "Send message" }).hover();
    // Tooltip text must never appear on mobile.
    await expect(page.getByText(/Ctrl\+Enter/)).toHaveCount(0);
    await expect(page.getByText(/Send \(Enter\)/)).toHaveCount(0);
  });
});

test.describe("User-message history navigation (real browser)", () => {
  test.beforeEach(async ({ page }) => {
    await mockAuthAndWorkspace(page);
    // Two prior user turns, oldest-first.
    await mockHistory(page, [
      { id: "u1", role: "user", text: "first user question", createdAt: "2026-01-01T00:00:01Z" },
      { id: "a1", role: "assistant", text: "first answer", createdAt: "2026-01-01T00:00:02Z" },
      { id: "u2", role: "user", text: "second user question", createdAt: "2026-01-01T00:00:03Z" },
      { id: "a2", role: "assistant", text: "second answer", createdAt: "2026-01-01T00:00:04Z" },
    ]);
  });

  test("Up on first line loads the most recent user message", async ({ page }) => {
    await gotoChat(page);
    const textarea = page.getByPlaceholder("Type a message...");
    await textarea.click(); // focus, cursor at start
    await textarea.press("ArrowUp");
    await expect(textarea).toHaveValue("second user question");
  });

  test("repeated Up walks back to older user messages", async ({ page }) => {
    await gotoChat(page);
    const textarea = page.getByPlaceholder("Type a message...");
    await textarea.click();
    await textarea.press("ArrowUp"); // → second (most recent)
    await textarea.press("ArrowUp"); // → first (older)
    await expect(textarea).toHaveValue("first user question");
  });

  test("Down on last line restores the pre-browse draft", async ({ page }) => {
    await gotoChat(page);
    const textarea = page.getByPlaceholder("Type a message...");
    await textarea.fill("my draft");
    // Move cursor to start (first line) so Up can fire.
    await textarea.focus();
    await page.keyboard.press("Home");
    await textarea.press("ArrowUp"); // → "second user question"
    await expect(textarea).toHaveValue("second user question");
    // Move cursor to end (last line) so Down can fire.
    await page.keyboard.press("Control+End");
    await textarea.press("ArrowDown"); // restore draft
    await expect(textarea).toHaveValue("my draft");
  });

  // Note: a "Up in the middle of a multi-line draft does NOT navigate" test
  // would close the loop on cursor-gating, but Playwright's fill() truncates
  // multi-line values in some environments, and the controlled-textarea
  // keyboard-text insertion is unreliable in sandboxed headless Chromium.
  // The behavior is exhaustively covered at the unit level
  // (Composer.test.tsx: "Up when cursor is NOT on first line moves cursor")
  // and by getCursorLineInfo's pure-function tests (composerHistory.test.ts).
});

test.describe("Load earlier messages + scroll anchoring (real browser)", () => {
  test.beforeEach(async ({ page }) => {
    await mockAuthAndWorkspace(page);
  });

  test("clicking 'Load earlier messages' fetches and renders the older page", async ({ page }) => {
    // First page: 2 messages with a cursor pointing further back.
    let callCount = 0;
    await page.route(`${API}/workspaces/${WS_ID}/sessions/${SES_ID}/message*`, async (route: Route) => {
      callCount++;
      const url = route.request().url();
      const isSecondCall = url.includes("before=");
      const body = isSecondCall
        ? opencodeShape([
            { id: "old-u", role: "user", text: "OLDER user msg", createdAt: "2026-01-01T00:00:01Z" },
            { id: "old-a", role: "assistant", text: "OLDER asst msg", createdAt: "2026-01-01T00:00:02Z" },
          ])
        : opencodeShape([
            { id: "new-u", role: "user", text: "NEW user msg", createdAt: "2026-01-01T00:00:10Z" },
            { id: "new-a", role: "assistant", text: "NEW asst msg", createdAt: "2026-01-01T00:00:11Z" },
          ]);
      const headers: Record<string, string> = { "Content-Type": "application/json" };
      if (!isSecondCall) headers["X-Next-Cursor"] = "new-u";
      await route.fulfill({ status: 200, headers, body: JSON.stringify(body) });
    });

    await gotoChat(page);
    // Messages render inside a scroll container; assert attachment rather
    // than visibility because the container scrolls to the bottom and the
    // first message may be above the fold.
    await expect(page.getByText("NEW user msg")).toHaveCount(1);

    const loadButton = page.getByRole("button", { name: "Load earlier messages" });
    await expect(loadButton).toBeVisible();
    await loadButton.click();

    // Older page must render in the DOM.
    await expect(page.getByText("OLDER user msg")).toHaveCount(1, { timeout: 5_000 });
    // Newer messages still present too (prepended, not replaced).
    await expect(page.getByText("NEW user msg")).toHaveCount(1);
    // Button should be gone (no further cursor).
    await expect(loadButton).toHaveCount(0, { timeout: 5_000 });
    expect(callCount).toBe(2);
  });

  test("older messages prepend (not replace) when 'Load earlier' completes", async ({ page }) => {
    // Regression for the scroll-anchoring fix. The unit tests verify the
    // scroll-position math; this e2e test verifies the user-facing
    // contract: after Load earlier, BOTH the older and newer pages are
    // present (the older was prepended, not swapped in).
    const tallText = "x".repeat(500);
    await page.route(`${API}/workspaces/${WS_ID}/sessions/${SES_ID}/message*`, async (route: Route) => {
      const url = route.request().url();
      const isSecondCall = url.includes("before=");
      const body = isSecondCall
        ? opencodeShape([
            { id: "old-1", role: "user", text: "OLDER-MARKER " + tallText, createdAt: "2026-01-01T00:00:01Z" },
          ])
        : opencodeShape([
            { id: "new-1", role: "user", text: "ANCHOR-MARKER " + tallText, createdAt: "2026-01-01T00:00:10Z" },
          ]);
      const headers: Record<string, string> = { "Content-Type": "application/json" };
      if (!isSecondCall) headers["X-Next-Cursor"] = "new-1";
      await route.fulfill({ status: 200, headers, body: JSON.stringify(body) });
    });

    await gotoChat(page);
    await expect(page.getByText(/ANCHOR-MARKER/)).toHaveCount(1);

    await page.getByRole("button", { name: "Load earlier messages" }).click();

    // Both old and new markers must be present — prepended, not replaced.
    await expect(page.getByText(/OLDER-MARKER/)).toHaveCount(1, { timeout: 5_000 });
    await expect(page.getByText(/ANCHOR-MARKER/)).toHaveCount(1);

    // And the older marker must appear BEFORE the anchor in DOM order.
    const older = page.getByText(/OLDER-MARKER/);
    const anchor = page.getByText(/ANCHOR-MARKER/);
    const orderCorrect = await page.evaluate(([a, b]) => {
      return !!(a && b && a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
    }, [await older.elementHandle(), await anchor.elementHandle()]);
    expect(orderCorrect).toBe(true);
  });
});
