// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import type { Page } from "@playwright/test";

export interface AttachFile {
  name: string;
  mimeType: string;
  content: Buffer;
}

/**
 * Dispatch a file selection on the composer's hidden file input WITHOUT
 * Playwright's `setInputFiles` action machinery.
 *
 * Why this exists (the #1523 investigation): Playwright's setInputFiles
 * resolves the element, then sets `.files` and dispatches `input`/`change`
 * from its injected script — across a CDP round-trip. In the dev-server
 * e2e environment the composer subtree re-renders during that window
 * (React StrictMode's dev double-mount churn + the post-load
 * status/context re-render wave), and the dispatched change lands on a
 * detached node: the event never reaches even a document-capture
 * listener, React's root-delegated onChange never fires, and the upload
 * POST is never issued — the chip-never-appears flake (E1/E4/E5/E6).
 * Reproduced deterministically: a document-capture change listener
 * observed ZERO events across all failing iterations while Playwright's
 * API reported success; the same listener saw every event when the
 * dispatch ran inside a single evaluate turn.
 *
 * The fix is the single-turn dispatch: query the LIVE node, set its
 * files via DataTransfer, and fire bubbling input+change events — all
 * in one JS turn inside the page, so no round-trip window exists for a
 * remount to orphan. This mirrors what a real user's file dialog does
 * (the browser dispatches change on the attached node synchronously).
 */
export async function attachFiles(page: Page, files: AttachFile[]): Promise<void> {
  // Bounded wait for the input: the composer's attach fragment mounts
  // in the same commit tree as the textarea the tests gate on, but
  // React StrictMode's dev double-mount (and the mobile viewport's
  // late fragment commit) leaves brief windows where the input is not
  // yet (or no longer, mid-remount) in the DOM — an immediate query
  // throws and the row flakes (#1523's residual, isolated as
  // "composer-file-input not found").
  await page.waitForSelector('[data-testid="composer-file-input"]', { state: "attached", timeout: 10_000 });

  const payload = files.map((f) => ({
    name: f.name,
    type: f.mimeType,
    dataBase64: f.content.toString("base64"),
  }));
  await page.evaluate((specs: Array<{ name: string; type: string; dataBase64: string }>) => {
    const input = document.querySelector<HTMLInputElement>('[data-testid="composer-file-input"]');
    if (!input) throw new Error("composer-file-input not found in the DOM");
    const dt = new DataTransfer();
    for (const spec of specs) {
      const bin = atob(spec.dataBase64);
      const bytes = new Uint8Array(bin.length);
      for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
      dt.items.add(new File([bytes], spec.name, { type: spec.type }));
    }
    input.files = dt.files;
    input.dispatchEvent(new Event("input", { bubbles: true }));
    input.dispatchEvent(new Event("change", { bubbles: true }));
  }, payload);
}
