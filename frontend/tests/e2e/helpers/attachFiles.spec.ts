// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { test, expect } from "@playwright/test";
import { attachFiles } from "./attachFiles";
import { mockAuthAndWorkspace, mockHistory, gotoChat, mockUpload } from "../attachments-helpers";

// The #1523 regression rows. The dispatch must be single-turn (live-node
// query + DataTransfer + bubbling events inside ONE evaluate) — Playwright's
// setInputFiles round-trip orphans the dispatch on remounted nodes (the
// chip-never-appears flake class). These rows prove the helper works
// end-to-end through the real composer, including the multi-file case.
test.describe("attachFiles helper (#1523)", () => {
  test("single file dispatch reaches React and produces an attached chip", async ({ page }) => {
    const uploaded: string[] = [];
    await mockAuthAndWorkspace(page);
    await mockHistory(page, []);
    await mockUpload(page, uploaded);
    await gotoChat(page);

    await attachFiles(page, [{ name: "notes.txt", mimeType: "text/plain", content: Buffer.from("abc") }]);

    const chip = page.locator('[data-testid^="composer-chip-"][data-status="attached"]', { hasText: "notes.txt" });
    await expect(chip).toBeVisible({ timeout: 15_000 });
    expect(uploaded).toEqual(["notes.txt"]);
  });

  test("multiple files dispatch produces one chip per file", async ({ page }) => {
    const uploaded: string[] = [];
    await mockAuthAndWorkspace(page);
    await mockHistory(page, []);
    await mockUpload(page, uploaded);
    await gotoChat(page);

    await attachFiles(page, [
      { name: "a.txt", mimeType: "text/plain", content: Buffer.from("x") },
      { name: "b.bin", mimeType: "application/octet-stream", content: Buffer.from("y") },
    ]);

    await expect(page.locator('[data-testid^="composer-chip-"][data-status="attached"]')).toHaveCount(2, { timeout: 15_000 });
    expect(uploaded.sort()).toEqual(["a.txt", "b.bin"]);
  });
});
