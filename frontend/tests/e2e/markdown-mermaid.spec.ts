/**
 * E2E for #1694: markdown rendering for user messages + mermaid diagram
 * rendering. Proves in a REAL browser (real mermaid, bundled/offline —
 * no network) what the jsdom pins mock:
 *   1. user text renders through the markdown pipeline (bold → <strong>)
 *   2. the user bubble keeps its inverted (bg-primary) surface
 *   3. a ```mermaid fence renders an svg diagram via the lazy chunk
 *
 * History data uses the platform-session contract shape (same as
 * history-rendering.spec.ts — the adapter normalizes the wire shape).
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

const WORKSPACE_ID = "ws-markdown-e2e";
const SESSION_ID = "ses-markdown-e2e";
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
    await r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WORKSPACE_ID, name: "Markdown E2E", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
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
    await r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([{ id: SESSION_ID, title: "Markdown Test", messageCount: 2, status: "idle", hasUnread: false }]) });
  });

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/ensure`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/seen`, (r: Route) => r.fulfill({ status: 204 }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, title: "Markdown Test" }) }));

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/message*`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(messages) }));

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, (r: Route) =>
    r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));

  await mockIdleContractStream(page, `${API}/workspaces/${WORKSPACE_ID}/contract-events`, SESSION_ID);
}

test.describe("user markdown + mermaid rendering (#1694)", () => {
  test("user message renders markdown inside the inverted bubble", async ({ page }) => {
    const messages = [
      {
        id: "msg_user_1",
        type: "user",
        parts: [{ type: "text", text: "**urgent** — review before merge" }],
      },
    ];
    await setupAPIMocks(page, messages);
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // Markdown pipeline: **urgent** renders as <strong>, not literal asterisks
    const strong = page.locator("strong", { hasText: "urgent" });
    await expect(strong).toBeVisible({ timeout: 10000 });

    // The inverted bubble surface survives the prose scoping (#1694)
    const bubble = strong.locator("xpath=ancestor::div[contains(@class, 'bg-primary')][1]");
    await expect(bubble).toBeVisible();
    await expect(bubble).toHaveClass(/text-primary-foreground/);
  });

  test("assistant mermaid fence renders a real svg diagram", async ({ page }) => {
    const messages = [
      {
        id: "msg_user_1",
        type: "user",
        parts: [{ type: "text", text: "draw it" }],
      },
      {
        id: "msg_asst_1",
        type: "assistant",
        parts: [
          { type: "text", text: "```mermaid\nflowchart TD\nA[Start] --> B{Choice}\nB -->|yes| C[Done]\n```" },
        ],
      },
    ];
    await setupAPIMocks(page, messages);
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // Real mermaid render (lazy chunk, offline) → svg inside the pinned container
    const diagram = page.locator('[data-testid="mermaid-diagram"] svg');
    await expect(diagram).toBeVisible({ timeout: 15000 });
  });

  // r1: the jsdom pins mock mermaid.render's rejection — only a real
  // browser exercises mermaid 12's actual error path, including the
  // #d<id> diagnostic div it leaves in <body> on parse failure.
  test("malformed assistant mermaid fence shows the inline error state — no crash, no diagnostic bombsite (r1)", async ({ page }) => {
    const messages = [
      {
        id: "msg_user_1",
        type: "user",
        parts: [{ type: "text", text: "draw it" }],
      },
      {
        id: "msg_asst_1",
        type: "assistant",
        parts: [
          { type: "text", text: "```mermaid\nthis is ((( not a diagram\n```" },
        ],
      },
    ];
    await setupAPIMocks(page, messages);
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    await expect(page.locator('[data-testid="mermaid-error"]')).toBeVisible({ timeout: 15000 });
    // the transcript stays alive (no crash) — the user message still renders
    await expect(page.getByText("draw it")).toBeVisible();
    // mermaid's failed-parse diagnostic divs (#d<lsp-mermaid-*>) are
    // removed by the component; none accumulate in <body>
    await expect(page.locator('body > div[id^="dlsp-mermaid-"]')).toHaveCount(0);
  });

  // r1: real-browser unhappy path for the user-markdown workflow —
  // dangerous HTML in a user message must not execute and must not
  // render as literal markup through the markdown path.
  test("user message with dangerous HTML renders safe — no execution, no literal markup (r1)", async ({ page }) => {
    const messages = [
      {
        id: "msg_user_1",
        type: "user",
        parts: [{ type: "text", text: "<script>window.__pwned = 1</script>\n\n**safe** text" }],
      },
    ];
    await setupAPIMocks(page, messages);
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // the markdown around it still renders
    await expect(page.locator("strong", { hasText: "safe" })).toBeVisible({ timeout: 10000 });
    // the script never executed
    expect(await page.evaluate(() => (window as unknown as Record<string, unknown>).__pwned)).toBeUndefined();
    // the raw markup is not visible as literal text
    await expect(page.getByText("<script>window.__pwned = 1</script>")).toHaveCount(0);
  });
});
