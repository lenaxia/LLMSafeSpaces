/**
 * E2E tests for history rendering with realistic opencode 1.18.10 wire data.
 *
 * Proves in a real browser:
 *   1. REST history with reasoning parts renders the "Thinking" section (#752 F1)
 *   2. REST history with model info renders the model badge (#752 F2)
 *   3. REST history with tool parts renders tool-use display
 *
 * The mocked history data mirrors the backend's platform-session contract
 * (pkg/session.Message JSON shape) — NOT the raw opencode wire shape. The
 * adapter (pkg/agent/opencode/translate.go) normalizes opencode's wire shape
 * into this contract before the frontend sees it.
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

const WORKSPACE_ID = "ws-history-e2e";
const SESSION_ID = "ses-history-e2e";
const API = "**/api/v1";

async function setupAPIMocks(page: Page, messages: unknown[]) {
  await page.route(`${API}/auth/login`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ token: "e2e-token", user: { id: "u1", username: "tester", role: "user" } }) });
  });
  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true }) }));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false, instanceName: "test" }) }));

  await page.route(`${API}/workspaces`, async (r: Route) => {
    if (r.request().method() !== "GET") { await r.continue(); return; }
    await r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WORKSPACE_ID, name: "History E2E", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
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
    await r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([{ id: SESSION_ID, title: "History Test", messageCount: 3, status: "idle", hasUnread: false }]) });
  });

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/ensure`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/seen`, (r: Route) => r.fulfill({ status: 204 }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, title: "History Test" }) }));

  // REST history — returns the platform-session contract shape (flat array)
  // Wildcard at end catches query string (?limit=50) appended by getHistoryPage.
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/message*`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(messages) }));

  // SSE — return empty stream (no live events needed)
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, (r: Route) =>
    r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));

  // Contract stream (US-69.10 cutover) — minimal idle snapshot; every body
  // must open with a snapshot frame or the client reconnects.
  await await mockIdleContractStream(page, `${API}/workspaces/${WORKSPACE_ID}/contract-events`, SESSION_ID);}

test.describe("History rendering with realistic wire data (#752 F1/F2)", () => {
  test("reasoning parts render in the transcript (#752 F1)", async ({ page }) => {
    const messages = [
      {
        id: "msg_user_1",
        type: "user",
        parts: [{ type: "text", text: "Explain your approach" }],
      },
      {
        id: "msg_asst_1",
        type: "assistant",
        model: { id: "glm-5.2", provider: "thekaocloud" },
        parts: [
          { type: "reasoning", reasoning: "Let me think about this step by step. The user wants an explanation." },
          { type: "text", text: "Here's my approach to solving this problem." },
        ],
      },
    ];

    await setupAPIMocks(page, messages);
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // Reasoning renders inside a collapsed <details> with a "Thinking" summary.
    // The summary being visible proves the reasoning part was parsed and rendered.
    await expect(page.getByText("Thinking").first()).toBeVisible({ timeout: 10000 });
    // Expand and verify content.
    await page.getByText("Thinking").first().click();
    await expect(page.getByText("Let me think about this step by step")).toBeVisible({ timeout: 5000 });
  });

  test("model badge renders when history includes model info (#752 F2)", async ({ page }) => {
    const messages = [
      {
        id: "msg_user_1",
        type: "user",
        parts: [{ type: "text", text: "Hello" }],
      },
      {
        id: "msg_asst_1",
        type: "assistant",
        model: { id: "glm-5.2", provider: "thekaocloud" },
        parts: [{ type: "text", text: "Hi there!" }],
      },
    ];

    await setupAPIMocks(page, messages);
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // The model badge must appear with the model ID.
    await expect(page.locator('[data-testid="message-model"]')).toContainText("glm-5.2", { timeout: 10000 });
  });

  test("history without model info does not crash or show empty badge", async ({ page }) => {
    const messages = [
      {
        id: "msg_user_1",
        type: "user",
        parts: [{ type: "text", text: "Hello" }],
      },
      {
        id: "msg_asst_1",
        type: "assistant",
        parts: [{ type: "text", text: "Hi there!" }],
      },
    ];

    await setupAPIMocks(page, messages);
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // Should render the assistant text without crashing.
    await expect(page.getByText("Hi there!")).toBeVisible({ timeout: 10000 });
    // No model badge should be present.
    await expect(page.locator('[data-testid="message-model"]')).toHaveCount(0);
  });
});

// Design 0050 D5 (#892): elapsed-time badges on running tools. The
// incident's motivating scenario — distinguishing a live-silent tool
// ("42s") from dead state ("3h") at a glance. Contract shape carries
// state.startedAt (ISO); the badge renders for running tools and is
// absent for completed tools or payloads without the field.
test("running tool with startedAt renders the elapsed-time badge (#892 D5)", async ({ page }) => {
  const messages = [
    {
      id: "msg_user_1",
      type: "user",
      parts: [{ type: "text", text: "run the build" }],
    },
    {
      id: "msg_asst_1",
      type: "assistant",
      parts: [
        {
          type: "tool",
          tool: {
            name: "bash",
            input: { command: "make build" },
            state: {
              status: "running",
              startedAt: new Date(Date.now() - 42_000).toISOString(),
            },
          },
        },
      ],
    },
  ];

  await setupAPIMocks(page, messages);
  await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

  // Coarse elapsed text (tolerant to render timing): 4xs.
  await expect(page.locator('[aria-label="elapsed time"]')).toHaveText(/^4[0-9]s$/, { timeout: 10000 });
});

test("history without startedAt degrades to today's UI — no badge, no crash (#892 D5)", async ({ page }) => {
  const messages = [
    {
      id: "msg_user_1",
      type: "user",
      parts: [{ type: "text", text: "run the build" }],
    },
    {
      id: "msg_asst_1",
      type: "assistant",
      parts: [
        {
          type: "tool",
          tool: {
            name: "bash",
            input: { command: "make build" },
            state: { status: "running" },
          },
        },
      ],
    },
  ];

  await setupAPIMocks(page, messages);
  await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

  // The tool still renders; no badge appears; nothing crashes.
  await expect(page.getByText("bash").first()).toBeVisible({ timeout: 10000 });
  await expect(page.locator('[aria-label="elapsed time"]')).toHaveCount(0);
});
