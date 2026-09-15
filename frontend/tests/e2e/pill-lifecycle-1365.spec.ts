/**
 * E2E for #1365 — pill lifecycle browser acceptance beyond the optimistic
 * clear: the unhappy reply path (5xx keeps the pill), the incident's
 * dead-pill-shadows-live-ask view (both render; refresh converges to the
 * live ask), and the stale-stream row (a dead user stream heals by
 * reconnect — the snapshot flight re-runs and the pending set converges
 * without a manual refresh). Pure route-mock strategy
 * (walk-away.spec.ts precedent): no live backend.
 */
import { test, expect, type Page, type Route } from "@playwright/test";

const WS = "ws-pill1365";
const SES = "sess-pill1365";
const API = "**/api/v1";

function snapshotFrame(atSeq: number | bigint, pendingInputs: Array<Record<string, unknown>>): string {
  return `data: ${JSON.stringify({
    snapshot: { atSeq: String(atSeq), snapshot: { sessions: [{ sessionId: SES, status: "SESSION_STATUS_BUSY", inFlightParts: [], queueDepth: 0, pendingInputs }] } },
  })}\n\n`;
}

function permissionInput(id: string, patterns: string[]): Record<string, unknown> {
  return { id, sessionId: SES, rootSessionId: SES, kind: "INPUT_KIND_PERMISSION", permission: "external_directory", patterns, always: [] };
}

const empty = (body: unknown) => async (route: Route) => {
  await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
};

async function stubShell(page: Page, opts: { contractBody?: string; userEvents?: (hit: number) => string }) {
  await page.route(`${API}/auth/me`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "e2e", email: "e2e@test", role: "user", status: "active" }) });
  });
  await page.route(`${API}/workspaces`, empty({ items: [{ id: WS, name: "pill1365", phase: "Active", userId: "u1" }], pagination: { limit: 20, offset: 0, total: 1 } }));
  await page.route(`${API}/workspaces/${WS}/runs/active`, empty({ runs: [] }));
  await page.route(`${API}/workspaces/${WS}/agent-role`, empty({ role: null }));
  await page.route(`${API}/workspaces/${WS}/status`, empty({ phase: "Active", podName: "pod-1" }));
  await page.route(`${API}/workspaces/${WS}/alerts`, empty({ alerts: [] }));
  await page.route(`${API}/workspaces/${WS}/files`, empty({ files: [] }));
  await page.route(`${API}/orgs`, empty([]));
  await page.route(`${API}/image-factory/configs`, empty({ configs: [] }));
  await page.route(`${API}/admin/agent-roles`, empty([]));
  await page.route(`${API}/workspaces/${WS}/sessions`, empty([]));
  await page.route(`${API}/workspaces/${WS}/models`, empty({ models: [], currentModel: "", currentModelProviderID: "" }));
  await page.route(`${API}/workspaces/${WS}/sessions/${SES}/message*`, empty([]));
  await page.route(`${API}/workspaces/${WS}/sessions/${SES}`, empty({ id: SES, status: "idle" }));
  await page.route(`${API}/workspaces/${WS}/session-events`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "text/event-stream", body: ":\n\n" });
  });
  await page.route(`${API}/workspaces/${WS}/contract-events`, async (route: Route) => {
    await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: opts.contractBody ?? snapshotFrame(1, []) });
  });
  await page.route(`${API}/events`, async (route: Route) => {
    eventsHits++;
    const body = opts.userEvents?.(eventsHits) ?? ":\n\n";
    await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
  });
}

let eventsHits = 0;

test.describe("#1365 pill lifecycle — browser acceptance", () => {
  test.beforeEach(() => {
    eventsHits = 0;
  });

  test("unhappy path: a 5xx reply keeps the pill (no optimistic clear on failure)", async ({ page }) => {
    await stubShell(page, { contractBody: snapshotFrame(3, [permissionInput("per_5xx", ["/etc/shadow"])]) });
    await page.route(`${API}/workspaces/${WS}/permission/per_5xx/reply`, async (route: Route) => {
      await route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "upstream gone" }) });
    });

    await page.goto(`/chat/${WS}/${SES}`);
    await expect(page.getByText("Read file").or(page.getByText("external_directory"))).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText("/etc/shadow")).toBeVisible();

    await page.getByText("Allow always").click();
    // The failed reply must NOT clear — the pill stays actionable so the
    // user can retry; the error surfaces inline.
    await page.waitForTimeout(1000);
    await expect(page.getByText("/etc/shadow")).toBeVisible();
    await expect(page.getByText("upstream gone")).toBeVisible();
  });

  test("incident view: the stale pill does not shadow the live ask; refresh converges to the live ask", async ({ page }) => {
    const stale = permissionInput("per_stale_shadow", ["/old/dead-pattern"]);
    const live = permissionInput("per_live_ask", ["/tmp/live-clone"]);
    await stubShell(page, { contractBody: snapshotFrame(3, [stale, live]) });

    await page.goto(`/chat/${WS}/${SES}`);
    // Both asks render — the incident's defect was the live ask NEVER
    // appearing while the stale pill held the screen.
    await expect(page.getByText("/old/dead-pattern")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText("/tmp/live-clone")).toBeVisible();

    // Refresh with a fresh authoritative snapshot carrying only the live
    // ask (the stale projection is gone server-side post-resolve).
    await page.route(`${API}/workspaces/${WS}/contract-events`, async (route: Route) => {
      await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: snapshotFrame(4, [live]) });
    });
    await page.reload();
    await expect(page.getByText("/tmp/live-clone")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText("/old/dead-pattern")).not.toBeVisible();
  });

  test("stale stream: a dead user stream heals by reconnect — the flight re-runs and the pending set converges", async ({ page }) => {
    // Connection 1-2 (StrictMode double-mount): a heartbeat then the
    // connection ENDS — the silently-dead stream of the incident. The
    // reconnect (3+) delivers the snapshot flight exactly as the API's
    // connect path re-runs it: begin → whileAway permission → complete ok.
    const flightEvents = [
      { type: "agent.input.snapshot_begin", workspace_id: WS, snapshot_id: "heal-1" },
      {
        type: "agent.permission",
        workspace_id: WS,
        session_id: SES,
        request_id: "per_healed",
        whileAway: true,
        data: {
          id: "per_healed",
          sessionId: SES,
          rootSessionId: SES,
          kind: "permission",
          permission: "external_directory",
          patterns: ["/tmp/healed-clone"],
          always: [],
        },
      },
      { type: "agent.input.snapshot_complete", workspace_id: WS, snapshot_id: "heal-1", snapshot_ok: true },
    ];
    await stubShell(page, {
      userEvents: (hit: number) => (hit <= 2 ? ":\n\n" : flightEvents.map((e) => `data: ${JSON.stringify(e)}\n\n`).join("")),
    });
    await page.route(`${API}/workspaces/${WS}/permission/per_healed/reply`, async (route: Route) => {
      await route.fulfill({ status: 202, contentType: "application/json", body: JSON.stringify({ status: "queued", clientMessageID: "inbox-per_healed-answer", messageID: "ob_h" }) });
    });

    await page.goto(`/chat/${WS}/${SES}`);
    // No reload: the stream heals itself and the pending set converges.
    await expect(page.getByText("/tmp/healed-clone")).toBeVisible({ timeout: 15_000 });

    await page.getByText("Allow always").click();
    await expect(page.getByText("/tmp/healed-clone")).not.toBeVisible({ timeout: 10_000 });
  });
});
