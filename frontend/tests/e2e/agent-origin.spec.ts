/**
 * E2E for agent-originated message rendering (#1465).
 *
 * Proves in a real browser: a user-role message delivered by the agentd
 * send_message tool (carrying the agent-message-v1 provenance sentinel)
 * renders USER-SIDE with the provenance badge — the sentinel line itself
 * never visible, the payload intact, both attestation modes labeled. The
 * mocked history mirrors the platform-session contract shape the adapter
 * emits (pkg/session.Message), exactly what a delivered send_message looks
 * like after agentd composes the sentinel.
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

const WORKSPACE_ID = "ws-agent-origin-e2e";
const SESSION_ID = "ses-agent-origin-e2e";
const API = "**/api/v1";
const FROM_SESSION = "ses_f499ee9e6ffe52BJ8jxc2TEQQJ";

function sentineledUserText(mode: string): string {
  return `<!-- lsp:agent-message-v1 {"fromSession":"${FROM_SESSION}","mode":"${mode}"} -->\nStatus update: the vitest run is green.`;
}

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
    await r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WORKSPACE_ID, name: "Agent Origin E2E", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
  });

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/status`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" } }) }));

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/activate`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ resumed: WORKSPACE_ID }) }));

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions`, (r: Route) => {
    if (r.request().method() === "POST") {
      r.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, sessionId: SESSION_ID }) });
      return;
    }
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([{ id: SESSION_ID, title: "Agent Origin Session", messageCount: messages.length, status: "idle" }]) });
  });

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/ensure`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/seen`, (r: Route) => r.fulfill({ status: 204 }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, title: "Agent Origin Session" }) }));

  // REST history — platform-session contract shape (flat array). The
  // trailing * catches the ?limit=50 query getHistoryPage appends
  // (playwright patterns match the full URL including query).
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/message*`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(messages) }));

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, (r: Route) =>
    r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: `data: ${JSON.stringify({ type: "workspace.phase", phase: "Active" })}\n\n` }));

  await mockIdleContractStream(page, `${API}/workspaces/${WORKSPACE_ID}/contract-events`, SESSION_ID);
}

async function openChat(page: Page) {
  await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
}

test.describe("agent-originated message rendering (#1465)", () => {
  test("sentineled user message renders the provenance badge, sentinel hidden", async ({ page }) => {
    await setupAPIMocks(page, [
      { id: "m1", type: "user", parts: [{ type: "text", text: sentineledUserText("injected") }] },
      { id: "m2", type: "assistant", parts: [{ type: "text", text: "Acknowledged — continuing." }] },
    ]);
    await openChat(page);

    const badge = page.getByTestId("agent-origin-badge");
    await expect(badge).toBeVisible({ timeout: 10_000 });
    await expect(badge).toContainText(`message from session ${FROM_SESSION}`);
    // Injected origin carries no self-declared marker.
    await expect(badge).not.toContainText("self-declared");
    // The payload renders; the machine sentinel never does.
    await expect(page.getByText("Status update: the vitest run is green.")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText(/lsp:agent-message-v1/)).toHaveCount(0);
  });

  test("self-declared origin is visibly labeled (degraded-pod fallback)", async ({ page }) => {
    await setupAPIMocks(page, [
      { id: "m1", type: "user", parts: [{ type: "text", text: sentineledUserText("self-declared") }] },
    ]);
    await openChat(page);

    const badge = page.getByTestId("agent-origin-badge");
    await expect(badge).toBeVisible({ timeout: 10_000 });
    await expect(badge).toContainText(`message from session ${FROM_SESSION}`);
    await expect(badge).toContainText("self-declared origin");
  });

  test("plain human message renders no badge", async ({ page }) => {
    await setupAPIMocks(page, [
      { id: "m1", type: "user", parts: [{ type: "text", text: "Just a human message." }] },
    ]);
    await openChat(page);

    await expect(page.getByText("Just a human message.")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId("agent-origin-badge")).toHaveCount(0);
  });

  // #1465 owner report: the badge TRUNCATED the ID behind an ellipsis
  // (nowrap on the label span) — a bug class text-content assertions
  // cannot see in any harness. This row observes LAYOUT in a real
  // browser at a narrow viewport: the label + 31-char ID (~363px at
  // 11px monospace) cannot fit the ~300px message column, so a healthy
  // badge WRAPS to two+ lines (single line ≈ 20px incl. padding and
  // border; wrapped ≥ ~33px — the 26px threshold splits both with
  // margin), and nothing overflows horizontally. A reintroduced
  // truncate/nowrap anywhere in the badge's constraint chain keeps the
  // box one line tall and this row fails.
  test("the origin session ID wraps at a narrow viewport instead of truncating", async ({ page }) => {
    await page.setViewportSize({ width: 375, height: 812 });
    await setupAPIMocks(page, [
      { id: "m1", type: "user", parts: [{ type: "text", text: sentineledUserText("injected") }] },
    ]);
    await openChat(page);

    const badge = page.getByTestId("agent-origin-badge");
    await expect(badge).toBeVisible({ timeout: 10_000 });
    const box = await badge.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.height).toBeGreaterThan(26);
    // No horizontal overflow inside the badge: if any element in the
    // constraint chain regains nowrap, the badge's scrollWidth exceeds
    // its clientWidth (or the label ellipsizes — either way the wrap
    // height assertion above already caught it; this guards the ID
    // element specifically).
    const idEl = page.getByTestId("agent-origin-session-id");
    await expect(idEl).toBeVisible();
    const noOverflow = await idEl.evaluate((el) => el.scrollWidth <= el.clientWidth);
    expect(noOverflow).toBe(true);
  });
});
