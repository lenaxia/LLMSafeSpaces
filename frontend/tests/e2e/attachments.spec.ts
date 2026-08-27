/**
 * Epic 67 US-67.5 — composer attachments e2e (fully mocked, no live backend).
 *
 * Covers the three browser-only scenarios from the epic test plan:
 *   - E1 happy path: attach → chip → send → prompt payload asserted via stub
 *     (files[] top-level, parts text NOT mutated — D11)
 *   - E5 mobile viewport 375×812: drawer collapsed by default (D12 media-
 *     query-aware default), chevron opens it, chips do not overflow the composer
 *   - E6 chip removed before send → no files[] in the prompt payload
 *
 * Mirrors the stub strategy of composer.spec.ts (auth/workspace/history/SSE
 * route mocks) plus the Epic 67 upload route stub.
 */
import { test, expect, type Page, type Route } from "@playwright/test";

const API = "**/api/v1";
const WS_ID = "ws-e2e";
const SES_ID = "ses_e2e_1";
const UPLOADED_PATH = "/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt";

interface MockUser { id: string; username: string; email: string; role: string; active: boolean; createdAt: string }

async function mockAuthAndWorkspace(page: Page, opts: { settings?: Record<string, unknown> } = {}) {
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
  await page.route(`${API}/users/me/settings`, async (route: Route) => {
    await route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify({ settings: opts.settings ?? {}, schemaVersion: 14 }),
    });
  });
  await page.route(`${API}/users/me/settings/*`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ ok: true }) });
  });
  await page.route(`${API}/workspaces`, async (route: Route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({
        status: 200, contentType: "application/json",
        body: JSON.stringify({
          items: [{ id: WS_ID, name: "e2e-ws", phase: "Active", userId: "user-e2e", runtime: "python", storageSize: "1Gi", createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z" }],
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
      body: JSON.stringify({ phase: "Active", sessions: [{ id: SES_ID, status: "idle" }] }),
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
      body: JSON.stringify({ active: [], maxActive: 5 }),
    });
  });
  await page.route(`${API}/workspaces/${WS_ID}/models`, async (route: Route) => {
    await route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify({
        models: [{ id: "test-model", name: "Test Model", providerID: "test", tier: "free", freeTier: true }],
        currentModel: "test-model",
      }),
    });
  });
  await page.route(`${API}/admin/agent-roles`, async (route: Route) => {
    await route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify([{ id: "role-1", name: "Reviewer", description: "Reviews code" }]),
    });
  });
  await page.route(`${API}/workspaces/${WS_ID}/agent-role`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: "null" });
  });
  await page.route(`${API}/session-events`, async (route: Route) => { await route.abort(); });
  await page.route(`${API}/workspaces/${WS_ID}/session-events`, async (route: Route) => {
    await route.fulfill({
      status: 200, contentType: "text/event-stream",
      body: `data: ${JSON.stringify({ type: "workspace.phase", phase: "Active" })}\n\n`,
    });
  });
  await page.route(`${API}/workspaces/${WS_ID}/sessions/${SES_ID}/queue`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ messages: [] }) });
  });
  await page.route(`${API}/workspaces/${WS_ID}/sessions/${SES_ID}/seen`, async (route: Route) => {
    await route.fulfill({ status: 204, body: "" });
  });
}

async function mockHistory(page: Page, msgs: Array<{ id: string; role: "user" | "assistant"; text: string; createdAt: string }>) {
  await page.route(`${API}/workspaces/${WS_ID}/sessions/${SES_ID}/message*`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(msgs.map((m) => ({
        id: m.id, type: m.role, createdAt: m.createdAt, parts: [{ type: "text", text: m.text }],
      }))),
    });
  });
}

/** Stub the upload route; records uploaded filenames. */
async function mockUpload(page: Page, uploaded: string[]) {
  await page.route(`${API}/workspaces/${WS_ID}/uploads`, async (route: Route) => {
    const body = route.request().postDataBuffer();
    const disposition = body ? Buffer.from(body).toString("latin1").match(/filename="([^"]*)"/) : null;
    const name = disposition?.[1] ?? "unknown";
    uploaded.push(name);
    await route.fulfill({
      status: 201, contentType: "application/json",
      body: JSON.stringify({ path: UPLOADED_PATH, name, size: 3 }),
    });
  });
}

/** Capture prompt POST bodies. */
async function capturePrompts(page: Page, prompts: Array<Record<string, unknown>>) {
  await page.route(`${API}/workspaces/${WS_ID}/sessions/${SES_ID}/prompt`, async (route: Route) => {
    if (route.request().method() === "POST") {
      const raw = route.request().postData();
      if (raw) prompts.push(JSON.parse(raw));
    }
    await route.fulfill({ status: 202, body: "" });
  });
}

async function gotoChat(page: Page) {
  await page.goto(`/chat/${WS_ID}/${SES_ID}`);
  await expect(page.getByPlaceholder("Type a message...")).toBeVisible({ timeout: 10_000 });
  await expect(page.getByPlaceholder("Type a message...")).toBeEnabled({ timeout: 10_000 });
}

test.describe("Composer attachments (Epic 67)", () => {
  test("E1 happy path: attach → chip → send → payload carries files[], text unmutated", async ({ page }) => {
    const uploaded: string[] = [];
    const prompts: Array<Record<string, unknown>> = [];
    await mockAuthAndWorkspace(page);
    await mockHistory(page, []);
    await mockUpload(page, uploaded);
    await capturePrompts(page, prompts);

    await gotoChat(page);

    await page.setInputFiles('[data-testid="composer-file-input"]', {
      name: "notes.txt", mimeType: "text/plain", buffer: Buffer.from("abc"),
    });

    const chip = page.locator('[data-testid^="composer-chip-"][data-status="attached"]', { hasText: "notes.txt" });
    await expect(chip).toBeVisible({ timeout: 5_000 });
    await expect(chip).toContainText("3 B");
    expect(uploaded).toEqual(["notes.txt"]);

    await page.getByPlaceholder("Type a message...").fill("read the attached file");
    await page.getByRole("button", { name: "Send message" }).click();

    await expect(prompts).toHaveLength(1);
    expect(prompts[0]).toMatchObject({
      parts: [{ type: "text", text: "read the attached file" }],
      files: [UPLOADED_PATH],
    });
    expect(JSON.stringify(prompts[0])).not.toContain("llmsafespaces:attachment");
    await expect(page.locator('[data-testid^="composer-chip-"]')).toHaveCount(0);
  });

  test("E6 chip removed before send → no files[] in the prompt payload", async ({ page }) => {
    const uploaded: string[] = [];
    const prompts: Array<Record<string, unknown>> = [];
    await mockAuthAndWorkspace(page);
    await mockHistory(page, []);
    await mockUpload(page, uploaded);
    await capturePrompts(page, prompts);

    await gotoChat(page);

    await page.setInputFiles('[data-testid="composer-file-input"]', {
      name: "notes.txt", mimeType: "text/plain", buffer: Buffer.from("abc"),
    });
    await expect(page.locator('[data-testid^="composer-chip-"][data-status="attached"]')).toBeVisible({ timeout: 5_000 });

    await page.getByRole("button", { name: "Remove attachment notes.txt" }).click();
    await expect(page.locator('[data-testid^="composer-chip-"]')).toHaveCount(0);

    await page.getByPlaceholder("Type a message...").fill("no attachments now");
    await page.getByRole("button", { name: "Send message" }).click();

    await expect(prompts).toHaveLength(1);
    expect(prompts[0]).toMatchObject({ parts: [{ type: "text", text: "no attachments now" }] });
    expect(Object.keys(prompts[0]!)).not.toContain("files");
  });

  test("history user bubble strips the manifest and renders a chip", async ({ page }) => {
    await mockAuthAndWorkspace(page);
    await mockHistory(page, [
      {
        id: "u1",
        role: "user",
        text: `Please review.\n\n[llmsafespaces:attachment path="${UPLOADED_PATH}" name="notes.txt"]\n`,
        createdAt: "2026-01-01T00:00:01Z",
      },
    ]);

    await gotoChat(page);

    // Attachment (not visibility) — the message list scrolls and the
    // single message may sit above the fold (same convention as
    // composer.spec.ts's "Load earlier" tests).
    await expect(page.getByText("Please review.")).toHaveCount(1);
    await expect(page.getByTestId("history-attachment-chip")).toHaveText(/notes\.txt/);
    await expect(page.getByText(/llmsafespaces:attachment/)).toHaveCount(0);
  });

  // E3 (browser half): a 26 MiB file is rejected with a friendly composer
  // error surfaced on the chip + notice — the server-side half (nothing
  // lands on the PVC, no .tmp residue) is covered by the upload handler
  // and agentd test suites (U1.1.4/U1.2.5) at the Go layer.
  test("E3 oversize: 26 MiB file → friendly composer error, chip error state, no files[]", async ({ page }) => {
    const uploaded: string[] = [];
    const prompts: Array<Record<string, unknown>> = [];
    await mockAuthAndWorkspace(page);
    await mockHistory(page, []);
    await page.route(`${API}/workspaces/${WS_ID}/uploads`, async (route: Route) => {
      const body = route.request().postDataBuffer();
      const disposition = body ? Buffer.from(body).toString("latin1").match(/filename="([^"]*)"/) : null;
      uploaded.push(disposition?.[1] ?? "unknown");
      await route.fulfill({
        status: 413, contentType: "application/json",
        body: JSON.stringify({ error: "file exceeds size cap" }),
      });
    });
    await capturePrompts(page, prompts);

    await gotoChat(page);

    const big = Buffer.alloc(26 * 1024 * 1024, 0x61);
    await page.setInputFiles('[data-testid="composer-file-input"]', {
      name: "big.bin", mimeType: "application/octet-stream", buffer: big,
    });

    const chip = page.locator('[data-testid^="composer-chip-"][data-status="error"]', { hasText: "big.bin" });
    await expect(chip).toBeVisible({ timeout: 10_000 });
    await expect(page.getByLabel("upload-error-notice")).toContainText(/big\.bin/);
    await expect(page.getByLabel("upload-error-notice")).toContainText(/file exceeds size cap/);
    expect(uploaded).toEqual(["big.bin"]);

    // Send still works (failed chips are the user's explicit choice — D17),
    // and the failed upload is NOT referenced in the payload.
    await page.getByPlaceholder("Type a message...").fill("trying with the big one");
    await page.getByRole("button", { name: "Send message" }).click();
    await expect(prompts).toHaveLength(1);
    expect(Object.keys(prompts[0]!)).not.toContain("files");
  });

  // E4: upload against a suspended workspace → the 409 is surfaced in the
  // composer with the phase hint (Epic 67 D5).
  test("E4 suspended workspace: upload 409 surfaced with phase hint", async ({ page }) => {
    const uploaded: string[] = [];
    await mockAuthAndWorkspace(page);
    await mockHistory(page, []);
    await page.route(`${API}/workspaces/${WS_ID}/uploads`, async (route: Route) => {
      const body = route.request().postDataBuffer();
      const disposition = body ? Buffer.from(body).toString("latin1").match(/filename="([^"]*)"/) : null;
      uploaded.push(disposition?.[1] ?? "unknown");
      await route.fulfill({
        status: 409, contentType: "application/json",
        body: JSON.stringify({ error: "workspace not active", phase: "Suspended" }),
      });
    });

    await gotoChat(page);

    await page.setInputFiles('[data-testid="composer-file-input"]', {
      name: "notes.txt", mimeType: "text/plain", buffer: Buffer.from("abc"),
    });

    const chip = page.locator('[data-testid^="composer-chip-"][data-status="error"]', { hasText: "notes.txt" });
    await expect(chip).toBeVisible({ timeout: 5_000 });
    await expect(chip).toContainText("workspace not active (phase: Suspended)");
    await expect(page.getByLabel("upload-error-notice")).toContainText("workspace not active (phase: Suspended)");
    expect(uploaded).toEqual(["notes.txt"]);
  });
});

test.describe("Composer attachments — mobile viewport 375×812 (E5)", () => {
  test.use({ viewport: { width: 375, height: 812 } });

  test.beforeEach(async ({ page }) => {
    await mockAuthAndWorkspace(page);
    await mockHistory(page, []);
  });

  test("drawer collapsed by default; chevron opens; model + persona selectable", async ({ page }) => {
    await gotoChat(page);

    const chevron = page.getByRole("button", { name: "Toggle composer options" });
    await expect(chevron).toHaveAttribute("aria-expanded", "false");
    await expect(page.getByTestId("composer-options-drawer")).toHaveCount(0);

    await chevron.click();
    await expect(chevron).toHaveAttribute("aria-expanded", "true");
    await expect(page.getByTestId("composer-options-drawer")).toBeVisible();
    const drawer = page.locator("#composer-options-drawer");
    await expect(drawer.getByRole("button", { name: /Test Model/ })).toBeVisible();
    await expect(drawer.getByRole("button", { name: /Default/ })).toBeVisible();

    // Persona is selectable from the drawer.
    await drawer.getByRole("button", { name: /Default/ }).click();
    await expect(drawer.getByRole("button", { name: "Reviewer" })).toBeVisible();
    await drawer.getByRole("button", { name: "Reviewer" }).click();

    await chevron.click();
    await expect(chevron).toHaveAttribute("aria-expanded", "false");
    await expect(page.getByTestId("composer-options-drawer")).toHaveCount(0);
  });

  test("chips do not overflow the 375px composer", async ({ page }) => {
    const uploaded: string[] = [];
    await mockUpload(page, uploaded);
    await gotoChat(page);

    await page.setInputFiles('[data-testid="composer-file-input"]', [
      { name: "a-very-long-filename-that-could-overflow.txt", mimeType: "text/plain", buffer: Buffer.from("x") },
      { name: "second-file.bin", mimeType: "application/octet-stream", buffer: Buffer.from("y") },
    ]);
    await expect(page.locator('[data-testid^="composer-chip-"][data-status="attached"]')).toHaveCount(2, { timeout: 5_000 });

    const chipsRow = page.locator('[data-testid^="composer-chip-"]').first().locator("..");
    const rowBox = await chipsRow.boundingBox();
    expect(rowBox).not.toBeNull();
    expect(rowBox!.width).toBeLessThanOrEqual(375);
    expect(rowBox!.x).toBeGreaterThanOrEqual(0);
    await expect(page.locator('[data-testid^="composer-chip-"]').first()).toBeVisible();
  });
});
