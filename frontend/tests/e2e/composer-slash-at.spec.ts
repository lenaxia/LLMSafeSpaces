/**
 * E2E for the composer's slash commands and @-prompt recall (#1496).
 *
 * Proves in a real browser: the slash palette opens/filters/executes
 * against the mocked API surface (compact action + rename title PUT are
 * asserted at the network boundary), unknown slashes stay literal, and
 * the @-recall popup filters/keyboard-navigates/expands against the
 * prompt-library contract endpoint (#1499's named envelope, mocked —
 * the sibling lane owns the backend).
 */
import { test, expect, type Page, type Route } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

const WORKSPACE_ID = "ws-slash-e2e";
const SESSION_ID = "ses-slash-e2e";
const API = "**/api/v1";

const PROMPTS = {
  prompts: [
    { id: "p1", name: "deploy-check", content: "Run the deploy checklist:\n1. green CI\n2. tag", createdAt: "2026-09-20T00:00:00Z", updatedAt: "2026-09-20T00:00:00Z" },
    { id: "p2", name: "review-notes", content: "Review focus: error paths first.", createdAt: "2026-09-20T00:00:00Z", updatedAt: "2026-09-20T00:00:00Z" },
  ],
};

async function setupAPIMocks(page: Page) {
  await page.route(`${API}/auth/login`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ token: "e2e-token", user: { id: "u1", username: "tester", role: "user" } }) }));
  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true }) }));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false, instanceName: "test" }) }));
  await page.route(`${API}/workspaces`, (r: Route) => {
    if (r.request().method() !== "GET") { void r.continue(); return; }
    void r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WORKSPACE_ID, name: "Slash E2E", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/status`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" } }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/activate`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ resumed: WORKSPACE_ID }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions`, (r: Route) => {
    if (r.request().method() === "POST") {
      void r.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ sessionId: SESSION_ID, workspaceId: WORKSPACE_ID, workspacePhase: "Active", resumed: false }) });
      return;
    }
    void r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([{ id: SESSION_ID, title: "Slash session", messageCount: 0, status: "idle" }]) });
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/ensure`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/seen`, (r: Route) => r.fulfill({ status: 204 }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, title: "Slash session" }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/message*`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: "[]" }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, (r: Route) =>
    r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: `data: ${JSON.stringify({ type: "workspace.phase", phase: "Active" })}\n\n` }));
  // Model list for the picker surface.
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/models`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ models: [{ id: "m1", providerID: "p", label: "Model One", contextWindow: 128000, freeTier: false, tier: "paid" }] }) }));

  // The two network-asserted command endpoints.
  const compactCalls: unknown[][] = [];
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/actions`, (r: Route) => {
    compactCalls.push(r.request().postDataJSON());
    void r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({}) });
  });
  const renameCalls: { title: string }[] = [];
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/title`, (r: Route) => {
    renameCalls.push(r.request().postDataJSON());
    void r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({}) });
  });

  // The prompt-library contract endpoint (#1499 shape).
  await page.route(`${API}/me/prompts`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(PROMPTS) }));

  await mockIdleContractStream(page, `${API}/workspaces/${WORKSPACE_ID}/contract-events`, SESSION_ID);
  return { compactCalls, renameCalls };
}

async function openChat(page: Page) {
  await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
  const box = page.getByRole("textbox");
  await expect(box).toBeVisible({ timeout: 10_000 });
  return box;
}

test.describe("composer slash commands + @-prompt recall (#1496)", () => {
  test("slash palette opens, filters, and /compact routes the typed action", async ({ page }) => {
    const { compactCalls } = await setupAPIMocks(page);
    const box = await openChat(page);

    await box.type("/");
    await expect(page.getByTestId("slash-palette")).toBeVisible();
    await box.type("com");
    await expect(page.getByTestId("slash-palette").locator('[data-command="compact"]')).toBeVisible();
    await box.press("Enter");
    await expect(compactCalls[0]).toEqual({ compact: {} });
    await expect(page.getByTestId("command-notice")).toContainText("Compaction scheduled");
    await expect(box).toHaveValue("");
  });

  test("/rename with args writes the title at the network boundary", async ({ page }) => {
    const { renameCalls } = await setupAPIMocks(page);
    const box = await openChat(page);

    await box.type("/rename Renamed by slash");
    await expect(page.getByTestId("slash-palette")).toBeVisible(); // args keep the palette armed
    await box.press("Enter");
    // useSessionTitle legitimately persists the current title on mount
    // (startup PUT); assert the command's payload is PRESENT, not first.
    await expect(renameCalls).toContainEqual({ title: "Renamed by slash" });
    await expect(page.getByTestId("command-notice")).toContainText("Renamed by slash");
  });

  test("unknown slash stays literal text and sends", async ({ page }) => {
    await setupAPIMocks(page);
    const box = await openChat(page);

    await box.type("/notacommand");
    await expect(page.getByTestId("slash-palette")).toHaveCount(0);
    await box.press("Control+Enter");
    // The literal message lands in the transcript as a user message.
    await expect(page.getByText("/notacommand")).toBeVisible({ timeout: 10_000 });
  });

  test("@ recall filters, keyboard-navigates, and expands inline", async ({ page }) => {
    await setupAPIMocks(page);
    const box = await openChat(page);

    await box.type("run @dep");
    const popup = page.getByTestId("at-recall-popup");
    await expect(popup).toBeVisible();
    await expect(popup.locator('[data-prompt="p1"]')).toBeVisible();
    await expect(popup.locator('[data-prompt="p2"]')).toHaveCount(0); // filtered
    await box.press("Enter");
    await expect(box).toHaveValue("run Run the deploy checklist:\n1. green CI\n2. tag");
    await expect(popup).toHaveCount(0); // closed after expansion
  });

  test("@ expansion content containing @ does not reopen the popup", async ({ page }) => {
    await setupAPIMocks(page);
    // Re-register AFTER setup so this list wins (last route wins in PW).
    await page.route(`${API}/me/prompts`, (r: Route) =>
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ prompts: [{ id: "p9", name: "chain", content: "first @second tail" }] }) }));
    const box = await openChat(page);
    await box.type("@ch");
    await box.press("Enter");
    await expect(box).toHaveValue("first @second tail");
    await expect(page.getByTestId("at-recall-popup")).toHaveCount(0);
  });

  test("expanding a prompt ENDING in an @token does not reopen the popup", async ({ page }) => {
    await setupAPIMocks(page);
    await page.route(`${API}/me/prompts`, (r: Route) =>
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ prompts: [{ id: "p8", name: "ping", content: "now ping @deploy" }] }) }));
    const box = await openChat(page);
    await box.type("@pi");
    await box.press("Enter");
    await expect(box).toHaveValue("now ping @deploy");
    // The expanded content's TRAILING @token is live by the token rules —
    // suppression must hold; a reopen here is the recursion bug.
    await expect(page.getByTestId("at-recall-popup")).toHaveCount(0);
  });

  test("a failing prompt library never opens the popup and never crashes", async ({ page }) => {
    await setupAPIMocks(page);
    await page.route(`${API}/me/prompts`, (r: Route) =>
      r.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "boom" }) }));
    const box = await openChat(page);
    await box.type("@any");
    await expect(page.getByTestId("at-recall-popup")).toHaveCount(0);
    // The composer still works for normal text.
    await box.fill("plain message");
    await box.press("Control+Enter");
    await expect(page.getByText("plain message")).toBeVisible({ timeout: 10_000 });
  });

  test("Esc dismisses the popup; a fresh @ re-arms", async ({ page }) => {
    await setupAPIMocks(page);
    const box = await openChat(page);

    await box.type("@rev");
    await expect(page.getByTestId("at-recall-popup")).toBeVisible();
    await box.press("Escape");
    await expect(page.getByTestId("at-recall-popup")).toHaveCount(0);
    await box.press("End");
    await box.type("iew-notes"); // same @ anchor: stays dismissed
    await expect(page.getByTestId("at-recall-popup")).toHaveCount(0);
    await box.fill("");
    await box.type("again @re"); // fresh @ anchor: re-armed
    await expect(page.getByTestId("at-recall-popup")).toBeVisible();
  });
});
