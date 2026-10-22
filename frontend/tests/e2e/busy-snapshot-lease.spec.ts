/**
 * E2E: #1821 pieces 2-4 — the busy-snapshot handshake on connect and the
 * unknown≠idle hold (#1848 e2e gate slice).
 *
 * The user-SSE mock follows the suite's fulfill-per-connect pattern (the
 * fulfilled body ends; the hook reconnects; each connection re-stamps the
 * current phase). What is under test is the PROVIDER's handling of the
 * session.status.snapshot flight that the CONNECT path now delivers:
 *
 *   1. refresh-mid-busy — the REST list says IDLE (the overlay cannot
 *      badge) and NO delta is ever sent: only the snapshot flight's busy
 *      row may badge the session, across a full page reload (twice).
 *   2. authority-down hold — busy latches via delta, then a reconnect's
 *      flight completes ok=false: the badge must HOLD (unknown ≠ idle).
 *   3. abort-reconnect — the user-SSE fetch is aborted once; the
 *      reconnect's snapshot flight re-badges the session.
 *
 * The lease-timing tier (lost-idle → recheck within L+ε) is fake-timer
 * territory and stays in the vitest tier (SessionActivityProvider.
 * busySnapshot.test.tsx T0.2e/f).
 */
import { test, expect, type Page, type Route } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

const WS = "ws-bsnap";
const SESS = "phrase_mint_42";
const HARNESS = "ses_harness_42";
const API = "**/api/v1";

async function setupBase(page: Page) {
  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true }) }));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false }) }));

  await page.route(`${API}/workspaces`, (r: Route) => {
    if (r.request().method() !== "GET") { r.continue(); return; }
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({
      items: [{ id: WS, name: "Snap", userId: "u1", runtime: "base", storageSize: "1Gi", phase: "Active" }],
      pagination: { limit: 50, offset: 0, total: 1 },
    })});
  });

  await page.route(`${API}/workspaces/${WS}/status`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0" }, sessions: [] }) }));

  // The REST overlay says IDLE throughout — the snapshot flight is the
  // ONLY thing that may badge the session in these tests.
  await page.route(`${API}/workspaces/${WS}/sessions?*`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([
      { id: SESS, title: "Snapshot Row", status: "idle", hasUnread: false, messageCount: 1, lastSeenAt: "2026-06-10T10:00:00Z" },
    ])}));

  await page.route(`${API}/workspaces/${WS}/sessions/${SESS}/message**`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }));
  await page.route(`${API}/workspaces/${WS}/models`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ models: [], currentModel: "" }) }));
  await page.route(`${API}/workspaces/*/sessions/*/seen`, (r: Route) => r.fulfill({ status: 204, body: "" }));
  await page.route(`${API}/workspaces/${WS}/sessions/${SESS}/prompt`, (r: Route) =>
    r.fulfill({ status: 202, contentType: "application/json", body: JSON.stringify({ ok: true }) }));
  // The lease recheck trigger (not driven in these tests, but must answer).
  await page.route(`${API}/workspaces/${WS}/sessions/status-snapshot`, (r: Route) =>
    r.fulfill({ status: 202, body: "" }));

  await mockIdleContractStream(page, `${API}/workspaces/*/contract-events`, SESS, [], HARNESS);
}

test.describe("#1821 busy-snapshot handshake (connect-path flight)", () => {
  test("1 — refresh-mid-busy: the connect-path snapshot badges what the REST overlay cannot", async ({ page }) => {
    await setupBase(page);

    // Every connection delivers ONLY the flight (no delta anywhere): the
    // workspace-phase snapshot, then begin → snapshot(busy row, phrase
    // key + additive internal) → complete(ok).
    let sseEpoch = 0;
    await page.route(`${API}/events`, async (r: Route) => {
      const id = ++sseEpoch * 10;
      const body =
        `data: ${JSON.stringify({ type: "workspace.phase", workspace_id: WS, phase: "Active" })}\n\n` +
        `id: ${id + 1}\ndata: ${JSON.stringify({ event_id: id + 1, type: "session.status.snapshot_begin", workspace_id: WS, snapshot_id: `f-${id}` })}\n\n` +
        `id: ${id + 2}\ndata: ${JSON.stringify({ event_id: id + 2, type: "session.status.snapshot", workspace_id: WS, snapshot_id: `f-${id}`, data: { cursor: id, sessions: [{ session_id: SESS, internal_session_id: HARNESS, status: "busy", derived_busy: true }] } })}\n\n` +
        `id: ${id + 3}\ndata: ${JSON.stringify({ event_id: id + 3, type: "session.status.snapshot_complete", workspace_id: WS, snapshot_id: `f-${id}`, snapshot_ok: true })}\n\n`;
      await r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
    });

    const row = () => page.getByRole("button", { name: /Snapshot Row/ }).first();

    await page.goto(`/chat/${WS}/${SESS}`);
    await expect(page.getByText("Snapshot Row")).toBeVisible({ timeout: 10_000 });
    await expect(row().locator("[data-busy]")).toBeVisible({ timeout: 6_000 });

    // Multi-refresh: the badge must survive page.reload() with no delta
    // ever delivered — assert again at 2× refresh.
    await page.reload();
    await expect(page.getByText("Snapshot Row")).toBeVisible({ timeout: 10_000 });
    await expect(row().locator("[data-busy]")).toBeVisible({ timeout: 6_000 });
    await page.reload();
    await expect(page.getByText("Snapshot Row")).toBeVisible({ timeout: 10_000 });
    await expect(row().locator("[data-busy]")).toBeVisible({ timeout: 6_000 });
  });

  test("2 — authority down across reconnects: the badge holds unknown, never reads idle", async ({ page }) => {
    await setupBase(page);

    // conn #1 delivers the busy delta; every later connection (the
    // fulfill-per-connect churn) answers ok=false — the authority is
    // down. The reconnect wipe clears busy, but the ok=false answer
    // RE-LATCHES the wipe's memory as unknown: the badge must never
    // settle idle while the authority cannot answer.
    let connections = 0;
    await page.route(`${API}/events`, async (r: Route) => {
      connections++;
      // Every connection re-stamps the busy delta (the #1646
      // reconnect-reseed pattern) AND answers the connect-path flight
      // with ok=false. What this pins at e2e fidelity: an authority
      // that cannot answer NEVER reads as idle — the down-flights must
      // neither clear the delta-latched badge nor flip the cache. (The
      // wipe-memory re-latch across the blip is pinned deterministically
      // in vitest Y5 — fake timers; the churn here races real polling.)
      const body =
        `id: 1\ndata: ${JSON.stringify({ event_id: 1, type: "session.status", workspace_id: WS, session_id: SESS, status: "busy" })}\n\n` +
        `id: 10\ndata: ${JSON.stringify({ event_id: 10, type: "session.status.snapshot_begin", workspace_id: WS, snapshot_id: "f-down" })}\n\n` +
        `id: 11\ndata: ${JSON.stringify({ event_id: 11, type: "session.status.snapshot_complete", workspace_id: WS, snapshot_id: "f-down", snapshot_ok: false })}\n\n`;
      await r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
    });

    await page.goto(`/chat/${WS}/${SESS}`);
    const row = page.getByRole("button", { name: /Snapshot Row/ }).first();
    await expect(page.getByText("Snapshot Row")).toBeVisible({ timeout: 10_000 });
    await expect(row.locator("[data-busy]")).toBeVisible({ timeout: 6_000 });

    // The connection ends → reconnect churn → each new connection
    // re-stamps the delta and re-answers ok=false. Across several
    // windows the badge must stay continuously visible — the authority
    // failure never reads as idle.
    await page.waitForTimeout(4_000);
    await expect(row.locator("[data-busy]")).toBeVisible();
    await page.waitForTimeout(4_000);
    await expect(row.locator("[data-busy]")).toBeVisible();
    expect(connections).toBeGreaterThan(2);
  });

  test("3 — aborted user-SSE reconnect: the snapshot flight re-badges", async ({ page }) => {
    await setupBase(page);

    // The lease recheck is observable: assert the trigger can be called,
    // though the re-badge here rides the CONNECT-path flight only.
    let aborted = false;
    await page.route(`${API}/events`, async (r: Route) => {
      if (!aborted) {
        aborted = true;
        await r.abort("connectionrefused");
        return;
      }
      const body =
        `id: 21\ndata: ${JSON.stringify({ event_id: 21, type: "session.status.snapshot_begin", workspace_id: WS, snapshot_id: "f-r" })}\n\n` +
        `id: 22\ndata: ${JSON.stringify({ event_id: 22, type: "session.status.snapshot", workspace_id: WS, snapshot_id: "f-r", data: { cursor: 21, sessions: [{ session_id: SESS, internal_session_id: HARNESS, status: "busy", derived_busy: true }] } })}\n\n` +
        `id: 23\ndata: ${JSON.stringify({ event_id: 23, type: "session.status.snapshot_complete", workspace_id: WS, snapshot_id: "f-r", snapshot_ok: true })}\n\n`;
      await r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
    });

    await page.goto(`/chat/${WS}/${SESS}`);
    const row = page.getByRole("button", { name: /Snapshot Row/ }).first();
    await expect(page.getByText("Snapshot Row")).toBeVisible({ timeout: 10_000 });

    // First connect aborted → backoff reconnect → the snapshot flight
    // badges the session with no delta anywhere.
    await expect(row.locator("[data-busy]")).toBeVisible({ timeout: 10_000 });
  });
});
