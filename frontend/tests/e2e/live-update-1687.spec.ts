/**
 * #1687 — frontend live-update reliability e2e (mocked backend):
 *   resync_reconciles_busy_e2e        — the broker's resync (admitted event
 *                                       loss on a HEALTHY connection) clears
 *                                       a latched busy badge from REST truth,
 *                                       with no reconnect to mask the heal.
 *   contract_idle_bridge_e2e          — the contract stream's SESSION_STATUS
 *                                       IDLE clears a user-stream-latched
 *                                       busy badge while the user stream's
 *                                       idle frame is lost; the next send
 *                                       goes DIRECT (no queue hold).
 *   midturn_reconnect_buffer_e2e      — a contract-stream reconnect MID-TURN
 *                                       (fold says BUSY) keeps the live
 *                                       streaming buffer; the REAL end of
 *                                       turn still clears it (history owns
 *                                       the turn).
 *
 * All backend APIs are mocked via Playwright route interception. SSE bodies
 * are fulfilled as complete responses: body end = "server closed" to the
 * client, which reconnects with backoff — the same harness shape as
 * contract-stream.spec.ts. Connections that must stay "open" are held
 * (never fulfilled).
 */
import { test, expect, type Page, type Route } from "@playwright/test";

const WORKSPACE_ID = "ws-e2e-1687";
const SESSION_ID = "sess-e2e-1687";
const API = "**/api/v1";

function userHistoryMessage(text: string): Record<string, unknown> {
  return { id: `msg_u_${text.length}`, type: "user", parts: [{ type: "text", text }] };
}
function assistantHistoryMessage(text: string): Record<string, unknown> {
  return { id: `msg_a_${text.length}`, type: "assistant", parts: [{ type: "text", text }] };
}

/** Standard chat-page mocks; SSE routes (user + contract streams) are
 * per-test. Returns recorders for the direct-send vs queue decision. */
async function setupChat(page: Page, opts: {
  history: () => Array<Record<string, unknown>>;
  sessions: () => Array<Record<string, unknown>>;
}) {
  const promptPosts: string[] = [];
  const queuePosts: string[] = [];

  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true }) }));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false }) }));
  await page.route(`${API}/workspaces`, (r: Route) => {
    if (r.request().method() !== "GET") { r.continue(); return; }
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({
      items: [{ id: WORKSPACE_ID, name: "Live Update WS", userId: "u1", runtime: "base", storageSize: "1Gi", phase: "Active" }],
      pagination: { limit: 50, offset: 0, total: 1 },
    }) });
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/status`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" }, sessions: [] }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions?*`, (r: Route) => {
    if (r.request().method() === "POST") {
      r.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, sessionId: SESSION_ID }) });
      return;
    }
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(opts.sessions()) });
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/ensure`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/message*`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(opts.history()) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/models`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ models: [], currentModel: "" }) }));
  await page.route(`${API}/workspaces/*/sessions/*/seen`, (r: Route) =>
    r.fulfill({ status: 204, body: "" }));

  // The direct-send vs queue decision recorders.
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/prompt`, (r: Route) => {
    promptPosts.push(r.request().postData() ?? "");
    r.fulfill({ status: 200, contentType: "application/json", body: "{}" });
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/queue`, (r: Route) => {
    if (r.request().method() === "POST") queuePosts.push(r.request().postData() ?? "");
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ messages: [] }) });
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/queue/*`, (r: Route) =>
    r.fulfill({ status: 204, body: "" }));

  // Platform stream: silent heartbeat bodies (no platform events).
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, (r: Route) =>
    r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: ":\n\n" }));

  return { promptPosts, queuePosts };
}

function sseBody(...frames: string[]): string {
  return frames.join("");
}
function snapshotFrame(atSeq: number | bigint, sessions: Array<Record<string, unknown>>): string {
  return `data: ${JSON.stringify({ snapshot: { atSeq: String(atSeq), snapshot: { sessions } } })}\n\n`;
}
function eventFrame(seq: number | bigint, event: Record<string, unknown>): string {
  return `data: ${JSON.stringify({ event: { seq: String(seq), event } })}\n\n`;
}
function partDelta(seq: number | bigint, partId: string, delta: string): string {
  return eventFrame(seq, { type: "EVENT_TYPE_PART_DELTA", sessionId: SESSION_ID, messageId: "msg_a1", partId, delta });
}
function sessionStatus(seq: number | bigint, status: "SESSION_STATUS_IDLE" | "SESSION_STATUS_BUSY"): string {
  return eventFrame(seq, { type: "EVENT_TYPE_SESSION_STATUS", sessionId: SESSION_ID, status });
}
function busySessionRow(): Record<string, unknown> {
  return { id: SESSION_ID, title: "Live update session", messageCount: 1, status: "busy" };
}
function idleSessionRow(): Record<string, unknown> {
  return { id: SESSION_ID, title: "Live update session", messageCount: 1, status: "idle", hasUnread: false };
}
function busySnapshotSession(): Record<string, unknown> {
  return { sessionId: SESSION_ID, status: "SESSION_STATUS_BUSY", inFlightParts: [], queueDepth: 0, pendingInputs: [] };
}

test.describe("#1687 live-update reliability (mocked backend)", () => {
  test("resync clears a mount-seeded busy badge from REST truth — no reconnect masks the heal", async ({ page }) => {
    // Sessions REST: busy until the test fires the resync (the mount
    // seed latches, no matter how many reads land); idle afterwards —
    // the post-resync refetch reads the truth (the session genuinely
    // finished).
    let resyncFired = false;
    let sessionsCalls = 0;
    await setupChat(page, {
      history: () => [userHistoryMessage("prior turn")],
      sessions: () => {
        sessionsCalls++;
        return [resyncFired ? idleSessionRow() : busySessionRow()];
      },
    });

    // User stream: every connection is deferred — held open, silent —
    // until the test fires the broker's resync on it (the LIVE
    // connection, exactly the healthy-stream shape). No reconnect ever
    // happens, so any heal must come from the resync path itself.
    const pendingUserStreams: Array<(body: string) => void> = [];
    await page.route(`${API}/events`, async (r: Route) => {
      await new Promise<void>((resolve) => {
        pendingUserStreams.push((body: string) => {
          void r.fulfill({
            status: 200,
            headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
            body,
          });
          resolve();
        });
      });
    });

    // Contract stream: quiet idle snapshot bodies, then hold.
    let contractHits = 0;
    await page.route(`${API}/workspaces/${WORKSPACE_ID}/contract-events`, async (r: Route) => {
      contractHits++;
      if (contractHits <= 2) {
        await r.fulfill({
          status: 200,
          headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
          body: sseBody(snapshotFrame(1, [{ sessionId: SESSION_ID, status: "SESSION_STATUS_IDLE", inFlightParts: [], queueDepth: 0, pendingInputs: [] }])),
        });
        return;
      }
      await new Promise<never>(() => {});
    });

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.locator("h2")).toContainText("Live Update WS", { timeout: 10_000 });
    await expect(page.getByText("prior turn").first()).toBeVisible({ timeout: 10_000 });

    // The mount seed latched busy (REST said busy at load) — the sidebar
    // session row spins.
    await expect(page.locator(".animate-spin").first()).toBeVisible({ timeout: 10_000 });
    const sessionsCallsBeforeResync = sessionsCalls;

    // The broker overflowed this subscriber: resync = admitted event loss
    // on the healthy connection. Busy must reconcile from REST and the
    // spinner must drop…
    resyncFired = true;
    const resyncBody = sseBody(`data: ${JSON.stringify({ type: "resync" })}\n\n`);
    for (const fire of pendingUserStreams.splice(0)) fire(resyncBody);
    await expect(page.locator(".animate-spin").first()).toHaveCount(0, { timeout: 10_000 });

    // …BECAUSE the resync path refetched sessions (the next read saw
    // idle)…
    await expect.poll(() => sessionsCalls, { timeout: 10_000 }).toBeGreaterThan(sessionsCallsBeforeResync);
    // …and WITHOUT any SSE reconnection (the streams never ended).
    expect(pendingUserStreams).toHaveLength(0);
  });

  test("contract SESSION_STATUS IDLE clears a user-stream-latched badge; the next send goes DIRECT", async ({ page }) => {
    const { promptPosts, queuePosts } = await setupChat(page, {
      history: () => [userHistoryMessage("first turn")],
      sessions: () => [idleSessionRow()],
    });

    // User stream: deferred connections — the test fires the busy frame
    // once the page has settled; the idle frame is NEVER delivered
    // (broker drop / reconnect gap shape).
    const pendingUserStreams: Array<(body: string) => void> = [];
    await page.route(`${API}/events`, async (r: Route) => {
      await new Promise<void>((resolve) => {
        pendingUserStreams.push((body: string) => {
          void r.fulfill({
            status: 200,
            headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
            body,
          });
          resolve();
        });
      });
    });

    // Contract stream: busy snapshots on the mount connections; the
    // reconnect's body (the turn's end — SESSION_STATUS IDLE) is DEFERRED
    // until the test fires it, so the idle can never race the busy latch.
    let contractHits = 0;
    let fireContractIdle: ((body: string) => void) | null = null;
    // Read through an indirection: TS control-flow can't see the route
    // callback's assignment from the test scope.
    const fireIdle = (body: string): void => fireContractIdle?.(body);
    await page.route(`${API}/workspaces/${WORKSPACE_ID}/contract-events`, async (r: Route) => {
      contractHits++;
      if (contractHits <= 2) {
        await r.fulfill({
          status: 200,
          headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
          body: sseBody(snapshotFrame(1, [busySnapshotSession()])),
        });
        return;
      }
      if (contractHits === 3) {
        await new Promise<void>((resolve) => {
          fireContractIdle = (body: string) => {
            void r.fulfill({
              status: 200,
              headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
              body,
            });
            resolve();
          };
        });
        return;
      }
      await new Promise<never>(() => {});
    });

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.locator("h2")).toContainText("Live Update WS", { timeout: 10_000 });
    await expect(page.getByText("first turn").first()).toBeVisible({ timeout: 10_000 });

    // The user-stream busy frame latches the badge.
    const busyBody = sseBody(`data: ${JSON.stringify({ type: "session.status", workspace_id: WORKSPACE_ID, session_id: SESSION_ID, status: "busy" })}\n\n`);
    for (const fire of pendingUserStreams.splice(0)) fire(busyBody);
    await expect(page.locator(".animate-spin").first()).toBeVisible({ timeout: 10_000 });

    // The turn ends on the CONTRACT stream (the user stream's idle frame
    // is lost): the bridge must clear the badge — one stream reporting
    // the turn's end is enough.
    await expect
      .poll(() => contractHits, { timeout: 15_000 })
      .toBeGreaterThanOrEqual(3);
    fireIdle(sseBody(
      snapshotFrame(2, [busySnapshotSession()]),
      sessionStatus(3, "SESSION_STATUS_IDLE"),
    ));
    await expect(page.locator(".animate-spin").first()).toHaveCount(0, { timeout: 10_000 });

    // The turn has visibly ended: the next send must be DIRECT (POST
    // /prompt + optimistic transcript bubble), not held in the queue.
    const textarea = page.getByRole("textbox");
    await expect(textarea).toBeVisible();
    await textarea.fill("second message");
    // Desktop composer binding: Enter is newline, Ctrl+Enter sends.
    await textarea.press("Control+Enter");

    await expect.poll(() => promptPosts.length, { timeout: 10_000 }).toBe(1);
    expect(promptPosts[0]).toContain("second message");
    expect(queuePosts).toEqual([]);
    await expect(page.getByText("second message").first()).toBeVisible({ timeout: 10_000 });
  });

  test("mid-turn contract reconnect keeps the live streaming buffer; the real end of turn still clears it", async ({ page }) => {
    let turnOver = false;
    const streamedText = "Hello world — the beginning of the answer";
    const continuation = " and the middle passage";
    await setupChat(page, {
      // History is NONEMPTY during the turn (the old code's destructive
      // precondition); the full message appears only after the turn ends.
      history: () =>
        turnOver
          ? [userHistoryMessage("draft the release notes"), assistantHistoryMessage(streamedText + continuation)]
          : [userHistoryMessage("draft the release notes")],
      sessions: () => [idleSessionRow()],
    });

    // User stream: busy frames on every connection — the badge latches so
    // the streaming bubble renders (ChatView gates it on `streaming`,
    // which follows the provider busy map), and the idle frame NEVER
    // arrives. Reconnects re-deliver busy; harmless here.
    await page.route(`${API}/events`, (r: Route) => {
      void r.fulfill({
        status: 200,
        headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
        body: sseBody(`data: ${JSON.stringify({ type: "session.status", workspace_id: WORKSPACE_ID, session_id: SESSION_ID, status: "busy" })}\n\n`),
      });
    });

    // Connection 4 (the post-reconnect one) is DEFERRED: the reconnect
    // stays mid-turn until the test fires the turn's end, so the
    // destructive window (reconnect reconcile with the fold still BUSY)
    // is observable rather than closed atomically by an immediate IDLE.
    let fireContractTurnEnd: ((body: string) => void) | null = null;
    // Read through an indirection: TS control-flow can't see the route
    // callback's assignment from the test scope.
    const fireTurnEnd = (body: string): void => fireContractTurnEnd?.(body);

    let contractHits = 0;
    await page.route(`${API}/workspaces/${WORKSPACE_ID}/contract-events`, async (r: Route) => {
      contractHits++;
      if (contractHits <= 2) {
        // Connections 1–2 (StrictMode): mid-turn snapshot (fold BUSY, no
        // in-flight parts carried) + two deltas build the live buffer —
        // then the body ends (server closed) → reconnect MID-TURN.
        await r.fulfill({
          status: 200,
          headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
          body: sseBody(
            snapshotFrame(1, [busySnapshotSession()]),
            partDelta(2, "p1", "Hello world — "),
            partDelta(3, "p1", "the beginning of the answer"),
          ),
        });
        return;
      }
      if (contractHits === 3) {
        // Reconnect, STILL MID-TURN (no IDLE in this body): fresh BUSY
        // snapshot with EMPTY inFlightParts — the pod's snapshot does NOT
        // carry the in-flight text, so the client buffer is the only
        // copy — plus a continuation delta proving the buffer keeps
        // accumulating. The body ends again; the turn has NOT ended.
        await r.fulfill({
          status: 200,
          headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
          body: sseBody(
            snapshotFrame(9, [busySnapshotSession()]),
            partDelta(10, "p1", continuation),
          ),
        });
        return;
      }
      if (contractHits === 4) {
        await new Promise<void>((resolve) => {
          fireContractTurnEnd = (body: string) => {
            void r.fulfill({
              status: 200,
              headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
              body,
            });
            resolve();
          };
        });
        return;
      }
      await new Promise<never>(() => {});
    });

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.locator("h2")).toContainText("Live Update WS", { timeout: 10_000 });

    const streamed = page.getByText(streamedText, { exact: false });
    await expect(streamed.first()).toBeVisible({ timeout: 10_000 });

    // INVARIANT across the mid-turn reconnect (~1–3s backoff after the
    // connection-2 body ends): the streamed text must NEVER disappear —
    // the in-flight message exists nowhere else and history does not
    // contain it while the fold says BUSY. (Old code: the reconnect
    // reconcile wiped the buffer whenever history was nonempty.)
    for (let i = 0; i < 100; i++) {
      expect(await streamed.count()).toBeGreaterThan(0);
      await page.waitForTimeout(100);
    }

    // The buffer kept accumulating across the reconnect.
    await expect(page.getByText(continuation, { exact: false }).first()).toBeVisible({ timeout: 10_000 });

    // The reconnect's connection is held mid-turn; fire the REAL end of
    // turn on it: SESSION_STATUS IDLE, history now owns the message.
    await expect
      .poll(() => contractHits, { timeout: 15_000 })
      .toBeGreaterThanOrEqual(4);
    turnOver = true;
    fireTurnEnd(sseBody(
      snapshotFrame(11, [busySnapshotSession()]),
      partDelta(12, "p1", " …and the end"),
      sessionStatus(13, "SESSION_STATUS_IDLE"),
    ));

    // Exactly one rendered copy remains (the buffer yielded to the store).
    await expect
      .poll(async () => page.getByText(streamedText, { exact: false }).count(), { timeout: 15_000 })
      .toBe(1);
  });
});
