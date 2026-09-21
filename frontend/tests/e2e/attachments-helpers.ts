// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// The shared mock/setup surface of the Epic 68 attachments e2e —
// extracted (#1520) so the attachFiles helper's own regression rows can
// compose the same environment without duplicating the mocks.

import { expect, type Page, type Route } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

export const API = "**/api/v1";
export const WS_ID = "ws-e2e";
export const SES_ID = "ses_e2e_1";
export const UPLOADED_PATH = "/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt";

interface MockUser { id: string; username: string; email: string; role: string; active: boolean; createdAt: string }

export async function mockAuthAndWorkspace(page: Page, opts: { settings?: Record<string, unknown> } = {}) {
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
  // Contract stream (US-69.10 cutover) — minimal idle snapshot; every body
  // must open with a snapshot frame or the client reconnects.
  await mockIdleContractStream(page, `${API}/workspaces/${WS_ID}/contract-events`, SES_ID);
  await page.route(`${API}/workspaces/${WS_ID}/sessions/${SES_ID}/queue`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ messages: [] }) });
  });
  await page.route(`${API}/workspaces/${WS_ID}/sessions/${SES_ID}/seen`, async (route: Route) => {
    await route.fulfill({ status: 204, body: "" });
  });
}

export async function mockHistory(page: Page, msgs: Array<{ id: string; role: "user" | "assistant"; text: string; createdAt: string }>) {
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
export async function mockUpload(page: Page, uploaded: string[]) {
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
export async function capturePrompts(page: Page, prompts: Array<Record<string, unknown>>) {
  await page.route(`${API}/workspaces/${WS_ID}/sessions/${SES_ID}/prompt`, async (route: Route) => {
    if (route.request().method() === "POST") {
      const raw = route.request().postData();
      if (raw) prompts.push(JSON.parse(raw));
    }
    await route.fulfill({ status: 202, body: "" });
  });
}

export async function gotoChat(page: Page) {
  await page.goto(`/chat/${WS_ID}/${SES_ID}`);
  await expect(page.getByPlaceholder("Type a message...")).toBeVisible({ timeout: 10_000 });
  await expect(page.getByPlaceholder("Type a message...")).toBeEnabled({ timeout: 10_000 });
}
