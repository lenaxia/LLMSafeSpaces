/**
 * E2E: dev_preview_url tool output renders as an in-chat action button.
 *
 * Proves in a real browser (mocked API, real component tree):
 *   Happy: tool_result + tool_use panes carrying the LSP_DEV_PREVIEW_V1
 *          sentinel render the "Open dev preview :<port>" button with the
 *          correct href/target, in both origin-mode and path-mode shapes.
 *   Unhappy: marker-less output, organic text that merely STARTS with
 *          "DEV_PREVIEW …" (collision guard), and a marker with a missing
 *          link line all fall back to plain rendering — no button, no crash.
 *
 * Mock shape mirrors the platform-session contract (see history-rendering.spec.ts).
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

const WORKSPACE_ID = "ws-devpreview-e2e";
const SESSION_ID = "ses-devpreview-e2e";
const API = "**/api/v1";

const ORIGIN_OUTPUT = [
  "LSP_DEV_PREVIEW_V1 port=5173 origin=safespaces.dev",
  "[Open dev preview :5173](https://api.example.com/api/v1/workspaces/ws-devpreview-e2e/dev-preview-bootstrap/5173)",
  "Opens the per-workspace preview origin in a new tab. Requires dev preview enabled and an owner login.",
].join("\n");

const PATH_OUTPUT = [
  "LSP_DEV_PREVIEW_V1 port=3000 mode=path",
  "[Open dev preview :3000](https://api.example.com/api/v1/workspaces/ws-devpreview-e2e/dev-preview/3000/)",
  "Opens the dev preview tunnel.",
].join("\n");

async function setupAPIMocks(page: Page, messages: unknown[], apiBaseUrl = "/api/v1") {
  // /env.json drives the app's API base — the relative-link resolution
  // under test reads it. Default mirrors the same-origin deployment.
  await page.route("**/env.json", (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ apiBaseUrl, turnstileSiteKey: "" }) }));
  await page.route(`${API}/auth/login`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ token: "e2e-token", user: { id: "u1", username: "tester", role: "user" } }) }));
  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true }) }));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false, instanceName: "test" }) }));
  await page.route(`${API}/workspaces`, (r: Route) => {
    if (r.request().method() !== "GET") { void r.continue(); return; }
    void r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WORKSPACE_ID, name: "DevPreview E2E", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/status`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" } }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/activate`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ resumed: WORKSPACE_ID }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/models`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ models: [], currentModel: "" }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions`, (r: Route) => {
    if (r.request().method() === "POST") {
      void r.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID }) });
      return;
    }
    void r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([{ id: SESSION_ID, title: "DevPreview Test", messageCount: 2, status: "idle", hasUnread: false }]) });
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/ensure`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/seen`, (r: Route) => r.fulfill({ status: 204 }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, title: "DevPreview Test" }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/message*`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(messages) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, (r: Route) =>
    r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));
  // Contract stream (US-69.10 cutover) — minimal idle snapshot; every body
  // must open with a snapshot frame or the client reconnects.
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/contract-events`, (r: Route) =>
    r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
      body: `data: ${JSON.stringify({ snapshot: { atSeq: "1", snapshot: { sessions: [{ sessionId: SESSION_ID, status: "SESSION_STATUS_IDLE", inFlightParts: [], queueDepth: 0, pendingInputs: [] }] } } })}\n\n` }));
}

test.describe("dev_preview_url chat button (epic-66 UX round 2)", () => {
  test("tool_result with origin-mode sentinel renders the button", async ({ page }) => {
    await setupAPIMocks(page, [
      { id: "m1", type: "user", parts: [{ type: "text", text: "preview my app" }] },
      { id: "m2", type: "assistant", parts: [{ type: "tool", tool: { callId: "c1", name: "dev_preview_url", output: ORIGIN_OUTPUT, state: { status: "completed" } } }] },
    ]);
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    const btn = page.getByTestId("dev-preview-button");
    await expect(btn).toBeVisible({ timeout: 10_000 });
    await expect(btn).toHaveAttribute("href", "https://api.example.com/api/v1/workspaces/ws-devpreview-e2e/dev-preview-bootstrap/5173");
    await expect(btn).toHaveAttribute("target", "_blank");
    await expect(btn).toHaveAttribute("rel", /noopener/);
    await expect(btn).toContainText("Open dev preview :5173");
    // Origin hint mentions the preview domain.
    await expect(page.getByText("-preview.safespaces.dev")).toBeVisible();
  });

  test("tool_result with path-mode sentinel renders the button", async ({ page }) => {
    await setupAPIMocks(page, [
      { id: "m1", type: "user", parts: [{ type: "text", text: "preview on 3000" }] },
      { id: "m2", type: "assistant", parts: [{ type: "tool", tool: { callId: "c2", name: "dev_preview_url", output: PATH_OUTPUT, state: { status: "completed" } } }] },
    ]);
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    const btn = page.getByTestId("dev-preview-button");
    await expect(btn).toBeVisible({ timeout: 10_000 });
    await expect(btn).toHaveAttribute("href", "https://api.example.com/api/v1/workspaces/ws-devpreview-e2e/dev-preview/3000/");
    await expect(btn).toContainText("Open dev preview :3000");
  });

  test("tool_use pane with sentinel output renders the button", async ({ page }) => {
    await setupAPIMocks(page, [
      { id: "m2", type: "assistant", parts: [
        { type: "tool", tool: { callId: "c3", name: "dev_preview_url", input: { port: 5173 }, output: ORIGIN_OUTPUT, state: { status: "completed" } } },
      ] },
    ]);
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    const btn = page.getByTestId("dev-preview-button");
    await expect(btn).toBeVisible({ timeout: 10_000 });
    await expect(btn).toContainText("Open dev preview :5173");
  });

  test("marker-less legacy output renders as plain text, no button", async ({ page }) => {
    const legacy = "https://api.example.com/api/v1/workspaces/ws/dev-preview/5173/\n\nDev preview must be enabled for this URL to work.";
    await setupAPIMocks(page, [
      { id: "m2", type: "assistant", parts: [{ type: "tool", tool: { callId: "c4", name: "dev_preview_url", output: legacy, state: { status: "completed" } } }] },
    ]);
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // Legacy output renders in the standard (collapsed) tool pane: open it,
    // then verify plain text and the absence of a button.
    const summary = page.getByText("dev_preview_url").first();
    await expect(summary).toBeVisible({ timeout: 10_000 });
    await summary.click();
    await expect(page.getByText("Dev preview must be enabled", { exact: false })).toBeVisible({ timeout: 5_000 });
    await expect(page.getByTestId("dev-preview-button")).toHaveCount(0);
  });

  test("organic text starting with DEV_PREVIEW does NOT trigger the button (collision guard)", async ({ page }) => {
    // The pre-V1 bare sentinel was 'DEV_PREVIEW <port> …' — organic tool
    // output could plausibly start that way. Only the exact namespaced,
    // versioned, key=value sentinel may render a button.
    const organic = "DEV_PREVIEW 5173 safespaces.dev\nsome tool listing ports\nDEV_PREVIEW is a mode in our framework";
    await setupAPIMocks(page, [
      { id: "m2", type: "assistant", parts: [{ type: "tool", tool: { callId: "c5", name: "some_other_tool", output: organic, state: { status: "completed" } } }] },
    ]);
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // The invariant under test: organic output whose first line resembles
    // the pre-V1 marker must NOT produce a button. The button (when the
    // sentinel parses) renders as an always-visible early-return OUTSIDE
    // any collapsed pane — so button-absence is assertable directly,
    // without depending on LazyDetails open/close internals. The tool pane
    // header must be present (the part rendered); its content visibility
    // is not this test's concern.
    const summary = page.locator("summary", { hasText: "some_other_tool" }).first();
    await expect(summary).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId("dev-preview-button")).toHaveCount(0);
  });

  test("RELATIVE bootstrap link renders the button with the ABSOLUTE API href (split deployment)", async ({ page }) => {
    // Regression (2026-08-21): production pods emit /api/v1/... links
    // (no absolute API URL in env); absolute-only parsing rendered plain
    // text. With a split-deployment env (api on its own origin), the
    // button must point at the API origin — never the frontend origin.
    const rel = [
      "LSP_DEV_PREVIEW_V1 port=5173 origin=safespaces.dev",
      "[Open dev preview :5173](/api/v1/workspaces/ws-devpreview-e2e/dev-preview-bootstrap/5173)",
      "Opens the preview.",
    ].join("\n");
    await setupAPIMocks(page, [
      { id: "m2", type: "assistant", parts: [{ type: "tool", tool: { callId: "c7", name: "dev_preview_url", output: rel, state: { status: "completed" } } }] },
    ], "https://api.example.com/api/v1");

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    const btn = page.getByTestId("dev-preview-button");
    await expect(btn).toBeVisible({ timeout: 10_000 });
    await expect(btn).toHaveAttribute("href", "https://api.example.com/api/v1/workspaces/ws-devpreview-e2e/dev-preview-bootstrap/5173");
  });

  test("RELATIVE link stays relative under the same-origin default (correct there) — unhappy split", async ({ page }) => {
    // Same-origin deployment (default env): the relative link is correct
    // as-is and the button must still render. The 'unhappy' scenario for
    // resolution is a malformed link line, asserted after.
    const rel = [
      "LSP_DEV_PREVIEW_V1 port=3000 mode=path",
      "[Open dev preview :3000](/api/v1/workspaces/ws-devpreview-e2e/dev-preview-bootstrap/3000)",
      "Opens the preview.",
    ].join("\n");
    await setupAPIMocks(page, [
      { id: "m2", type: "assistant", parts: [{ type: "tool", tool: { callId: "c8", name: "dev_preview_url", output: rel, state: { status: "completed" } } }] },
    ]);

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    const btn = page.getByTestId("dev-preview-button");
    await expect(btn).toBeVisible({ timeout: 10_000 });
    await expect(btn).toHaveAttribute("href", "/api/v1/workspaces/ws-devpreview-e2e/dev-preview-bootstrap/3000");
  });

  test("malformed link line (empty target) renders no button", async ({ page }) => {
    const bad = "LSP_DEV_PREVIEW_V1 port=5173 origin=safespaces.dev\n[Open dev preview :5173]()\nOpens the preview.";
    await setupAPIMocks(page, [
      { id: "m2", type: "assistant", parts: [{ type: "tool", tool: { callId: "c9", name: "dev_preview_url", output: bad, state: { status: "completed" } } }] },
    ]);

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.getByTestId("dev-preview-button")).toHaveCount(0);
  });

  test("sentinel with missing link line falls back to plain, no crash", async ({ page }) => {
    const truncated = "LSP_DEV_PREVIEW_V1 port=5173 origin=safespaces.dev";
    await setupAPIMocks(page, [
      { id: "m2", type: "assistant", parts: [{ type: "tool", tool: { callId: "c6", name: "dev_preview_url", output: truncated, state: { status: "completed" } } }] },
    ]);
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // No button (no link to open), but the page rendered without crashing
    // and the raw text is visible.
    await expect(page.getByTestId("dev-preview-button")).toHaveCount(0);
    await expect(page.getByText("LSP_DEV_PREVIEW_V1", { exact: false }).first()).toBeVisible({ timeout: 10_000 });
  });
});
