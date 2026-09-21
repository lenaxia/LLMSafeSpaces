/**
 * Epic 68 US-68.5 — composer attachments e2e (fully mocked, no live backend).
 *
 * Covers the three browser-only scenarios from the epic test plan:
 *   - E1 happy path: attach → chip → send → prompt payload asserted via stub
 *     (files[] top-level, parts text NOT mutated — D11)
 *   - E5 mobile viewport 375×812: drawer collapsed by default (D12 media-
 *     query-aware default), chevron opens it, chips do not overflow the composer
 *   - E6 chip removed before send → no files[] in the prompt payload
 *
 * Mirrors the stub strategy of composer.spec.ts (auth/workspace/history/SSE
 * route mocks) plus the Epic 68 upload route stub.
 */
import { test, expect, type Route } from "@playwright/test";
import { attachFiles } from "./helpers/attachFiles";
import {
  API, WS_ID, UPLOADED_PATH,
  mockAuthAndWorkspace, mockHistory, mockUpload, capturePrompts, gotoChat,
} from "./attachments-helpers";

test.describe("Composer attachments (Epic 68)", () => {
  test("E1 happy path: attach → chip → send → payload carries files[], text unmutated", async ({ page }) => {
    const uploaded: string[] = [];
    const prompts: Array<Record<string, unknown>> = [];
    await mockAuthAndWorkspace(page);
    await mockHistory(page, []);
    await mockUpload(page, uploaded);
    await capturePrompts(page, prompts);

    await gotoChat(page);

    await attachFiles(page, [{ name: "notes.txt", mimeType: "text/plain", content: Buffer.from("abc") }]);

    const chip = page.locator('[data-testid^="composer-chip-"][data-status="attached"]', { hasText: "notes.txt" });
    await expect(chip).toBeVisible({ timeout: 15_000 });
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

    await attachFiles(page, [{ name: "notes.txt", mimeType: "text/plain", content: Buffer.from("abc") }]);
    await expect(page.locator('[data-testid^="composer-chip-"][data-status="attached"]')).toBeVisible({ timeout: 15_000 });

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
    await attachFiles(page, [{ name: "big.bin", mimeType: "application/octet-stream", content: big }]);

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
  // composer with the phase hint (Epic 68 D5).
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

    await attachFiles(page, [{ name: "notes.txt", mimeType: "text/plain", content: Buffer.from("abc") }]);

    const chip = page.locator('[data-testid^="composer-chip-"][data-status="error"]', { hasText: "notes.txt" });
    await expect(chip).toBeVisible({ timeout: 15_000 });
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

    // Persona is selectable from the drawer. The dropdown is portaled to
    // document.body (viewport-aware positioning, escapes composer clipping),
    // so its items are queried at page level under the menuitem role.
    await drawer.getByRole("button", { name: /Default/ }).click();
    const reviewerItem = page.getByRole("menuitem", { name: "Reviewer" });
    await expect(reviewerItem).toBeVisible();
    await reviewerItem.click();

    await chevron.click();
    await expect(chevron).toHaveAttribute("aria-expanded", "false");
    await expect(page.getByTestId("composer-options-drawer")).toHaveCount(0);
  });

  test("chips do not overflow the 375px composer", async ({ page }) => {
    const uploaded: string[] = [];
    await mockUpload(page, uploaded);
    await gotoChat(page);

    await attachFiles(page, [
      { name: "a-very-long-filename-that-could-overflow.txt", mimeType: "text/plain", content: Buffer.from("x") },
      { name: "second-file.bin", mimeType: "application/octet-stream", content: Buffer.from("y") },
    ]);
    await expect(page.locator('[data-testid^="composer-chip-"][data-status="attached"]')).toHaveCount(2, { timeout: 15_000 });

    const chipsRow = page.locator('[data-testid^="composer-chip-"]').first().locator("..");
    const rowBox = await chipsRow.boundingBox();
    expect(rowBox).not.toBeNull();
    expect(rowBox!.width).toBeLessThanOrEqual(375);
    expect(rowBox!.x).toBeGreaterThanOrEqual(0);
    await expect(page.locator('[data-testid^="composer-chip-"]').first()).toBeVisible();
  });
});
