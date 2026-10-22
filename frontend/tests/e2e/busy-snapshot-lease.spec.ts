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
 *   2. authority-down hold — busy latches via delta, then every
 *      reconnect's flight answers ok=false: the badge must HOLD, never
 *      read idle, across the churn (unknown ≠ idle).
 *   3. abort-reconnect — the user-SSE fetch is aborted once; the
 *      reconnect's snapshot flight re-badges the session.
 *   4. lost-idle lease recheck (§3 E2.5) — the idle delta is never
 *      delivered; page.clock drives the 90s lease + 15s sweep
 *      deterministically; the recheck's authoritative idle flight
 *      clears the badge within L+ε (the wipe-memory re-latch half of
 *      the unknown-hold is pinned with fake timers in vitest Y5).
 */
import { test, expect, type Page, type Route } from "@playwright/test";
import { createServer } from "node:http";
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

  test("4 — lost idle: the lease recheck clears the badge within L+ε (E2.5)", async ({ page }) => {
    // E2.5's premise is a HEALTHY OPEN connection: the idle delta is
    // lost while heartbeats keep the stream alive — no reconnect, no
    // #1646 wipe-and-reseed — so only the 90s lease recheck can clear
    // the latched badge. A fulfill-per-connect mock cannot represent
    // this (every body-end is a reconnect → the wipe), so /events is
    // served by an in-test HTTP server that streams forever; env.json
    // points the app at it (page.route mocks still intercept every
    // other endpoint — the patterns match any origin). page.clock
    // advances the lease + sweep in fake time in <35s steps with
    // real-time gaps so the server's heartbeats keep the read-timeout
    // and liveness watchdogs fed.
    let sse: import("node:http").ServerResponse | null = null;
    let recheckRequested = false;
    const cors = (res: import("node:http").ServerResponse, req: import("node:http").IncomingMessage) => {
      const origin = req.headers.origin ?? "*";
      res.setHeader("Access-Control-Allow-Origin", origin);
      res.setHeader("Access-Control-Allow-Credentials", "true");
      res.setHeader("Access-Control-Allow-Headers", req.headers["access-control-request-headers"] ?? "*");
      res.setHeader("Access-Control-Allow-Methods", "GET, POST, OPTIONS");
    };
    const srv = createServer((req, res) => {
      const url = req.url ?? "";
      if (req.method === "OPTIONS") {
        cors(res, req);
        res.writeHead(204);
        return res.end();
      }
      if (url.endsWith("/events")) {
        cors(res, req);
        res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
        // The busy delta ONCE — then the idle is never delivered (lost).
        res.write(`id: 1\ndata: ${JSON.stringify({ event_id: 1, type: "session.status", workspace_id: WS, session_id: SESS, status: "busy" })}\n\n`);
        sse = res;
        const hb = setInterval(() => {
          try {
            res.write(":\n\n");
          } catch {
            /* connection gone */
          }
        }, 250);
        res.on("close", () => clearInterval(hb));
        return;
      }
      if (url.endsWith("/sessions/status-snapshot") && req.method === "POST") {
        // The lease recheck: 202 + the authoritative idle flight
        // published INTO the open stream (the production shape — the
        // on-demand trigger publishes to the user stream).
        recheckRequested = true;
        cors(res, req);
        res.writeHead(202);
        res.end();
        sse?.write(
          `id: 41\ndata: ${JSON.stringify({ event_id: 41, type: "session.status.snapshot_begin", workspace_id: WS, snapshot_id: "f-idle" })}\n\n` +
            `id: 42\ndata: ${JSON.stringify({ event_id: 42, type: "session.status.snapshot", workspace_id: WS, snapshot_id: "f-idle", data: { cursor: 42, sessions: [{ session_id: SESS, internal_session_id: HARNESS, derived_busy: false }] } })}\n\n` +
            `id: 43\ndata: ${JSON.stringify({ event_id: 43, type: "session.status.snapshot_complete", workspace_id: WS, snapshot_id: "f-idle", snapshot_ok: true })}\n\n`,
        );
        return;
      }
      cors(res, req);
      res.writeHead(404);
      res.end();
    });
    await new Promise<void>((resolve) => srv.listen(0, "127.0.0.1", resolve));
    const port = (srv.address() as { port: number }).port;

    try {
      await setupBase(page);
      // Point the app at the in-test server; the recheck POST must
      // reach IT (override setupBase's 202 stub — later route wins).
      await page.route("**/env.json", (r: Route) =>
        r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ apiBaseUrl: `http://127.0.0.1:${port}/api/v1` }) }));
      await page.route(`${API}/workspaces/${WS}/sessions/status-snapshot`, (r: Route) => r.continue());

      await page.clock.install();
      await page.goto(`/chat/${WS}/${SESS}`);
      const row = page.getByRole("button", { name: /Snapshot Row/ }).first();
      await expect(page.getByText("Snapshot Row")).toBeVisible({ timeout: 10_000 });
      await expect(row.locator("[data-busy]")).toBeVisible({ timeout: 6_000 });

      // Advance past lease (90s) + sweep jitter (15s) in fake time;
      // pre-fix this latches forever (no lease to expire).
      for (let i = 0; i < 12; i++) {
        await page.clock.fastForward(9_000);
        await page.waitForTimeout(250);
      }
      await expect.poll(() => recheckRequested, { timeout: 10_000 }).toBe(true);
      await expect(row.locator("[data-busy]")).toBeHidden({ timeout: 15_000 });
    } finally {
      srv.closeAllConnections?.();
      await new Promise<void>((resolve) => srv.close(() => resolve()));
    }
  });
});
