/**
 * US-69.10 part 2 — the contract-stream reconnect suite (design 0055 S3):
 *   midturn_reconnect_e2e        — stream killed mid-turn → reconnect →
 *                                  rendered transcript exactly equals the
 *                                  store (no dup/missing parts).
 *   standing_question_reconnect_e2e — question asked → reconnect → the
 *                                  snapshot alone answers it (no extra
 *                                  fetch beyond the stream).
 *   api_rolling_deploy_e2e       — API replica churn mid-turn: browsers
 *                                  converge via fresh stamped snapshots;
 *                                  overlap events are discarded exactly; no
 *                                  stuck spinner.
 *
 * All backend APIs are mocked via Playwright route interception. The
 * contract stream serves protojson StreamFrame SSE bodies (camelCase —
 * byte-compatible with the protoc-gen-es types the frontend parses with).
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

const WORKSPACE_ID = "ws-e2e-cs";
const SESSION_ID = "sess-e2e-cs";
const API_PREFIX = "**/api/v1";

// --- contract-frame builders -------------------------------------------------

function snapshotFrame(atSeq: number | bigint, sessions: Array<Record<string, unknown>>): string {
  return `data: ${JSON.stringify({
    snapshot: { atSeq: String(atSeq), snapshot: { sessions } },
  })}\n\n`;
}

function eventFrame(seq: number | bigint, event: Record<string, unknown>): string {
  return `data: ${JSON.stringify({
    event: { seq: String(seq), event },
  })}\n\n`;
}

function partEnd(seq: number | bigint, partId: string, text: string, messageId = "msg_a1"): string {
  return eventFrame(seq, {
    type: "EVENT_TYPE_PART_END",
    sessionId: SESSION_ID,
    messageId,
    partId,
    part: { id: partId, type: "PART_TYPE_TEXT", text },
  });
}

function partDelta(seq: number | bigint, partId: string, delta: string, messageId = "msg_a1"): string {
  return eventFrame(seq, {
    type: "EVENT_TYPE_PART_DELTA",
    sessionId: SESSION_ID,
    messageId,
    partId,
    delta,
  });
}

function sessionStatus(seq: number | bigint, status: "SESSION_STATUS_IDLE" | "SESSION_STATUS_BUSY"): string {
  return eventFrame(seq, {
    type: "EVENT_TYPE_SESSION_STATUS",
    sessionId: SESSION_ID,
    status,
  });
}

function busySnapshotInput(id: string, question: string): Record<string, unknown> {
  return {
    id,
    sessionId: SESSION_ID,
    kind: "INPUT_KIND_QUESTION",
    question,
    header: "Question",
    options: [{ label: "yes", description: "" }, { label: "no", description: "" }],
  };
}

// --- harness -----------------------------------------------------------------

interface HarnessOptions {
  /** History returned by the messages endpoint; may be mutated between
   * calls by supplying a getter. */
  history: () => Array<Record<string, unknown>>;
  /** Sessions list REST body (drives the provider busy map). */
  sessions?: () => Array<Record<string, unknown>>;
  /** Body for the user stream (/events). */
  userStreamBody?: string;
  /** Dynamic user-stream body (takes precedence over userStreamBody). */
  userStream?: () => string;
}

async function setupChatMocks(page: Page, opts: HarnessOptions) {
  await page.route(`${API_PREFIX}/auth/login`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ token: "e2e-test-token", user: { id: "u1", username: "testuser", role: "user" } }) });
  });
  await page.route(`${API_PREFIX}/auth/me`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "testuser", email: "test@example.com", role: "user", active: true }) });
  });
  await page.route(`${API_PREFIX}/auth/config`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false }) });
  });
  await page.route(`${API_PREFIX}/workspaces`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WORKSPACE_ID, name: "Contract Stream WS", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/status`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" } }) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions`, async (route: Route) => {
    if (route.request().method() === "POST") {
      await route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, sessionId: SESSION_ID }) });
    } else {
      const body = opts.sessions
        ? opts.sessions()
        : [{ id: SESSION_ID, title: "Contract stream session", messageCount: 0, status: "idle" }];
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
    }
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions/*/ensure`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID }) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/message*`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(opts.history()) });
  });

  // Platform stream: no platform events in these scenarios.
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/session-events`, async (route: Route) => {
    await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: ":\n\n" });
  });

  // User stream: busy status seeds the provider's busy map so the
  // streaming bubble renders while the turn is in flight.
  await page.route(`${API_PREFIX}/events`, async (route: Route) => {
    const busy = `data: ${JSON.stringify({ type: "session.status", workspace_id: WORKSPACE_ID, session_id: SESSION_ID, status: "busy" })}\n\n`;
    const idle = `data: ${JSON.stringify({ type: "session.status", workspace_id: WORKSPACE_ID, session_id: SESSION_ID, status: "idle" })}\n\n`;
    const body = opts.userStream ? opts.userStream() : (opts.userStreamBody ?? busy);
    void busy; void idle;
    await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
  });
}

function userHistoryMessage(text: string): Record<string, unknown> {
  return { id: "msg_u1", type: "user", parts: [{ type: "text", text }] };
}

function assistantHistoryMessage(text: string): Record<string, unknown> {
  return { id: "msg_a1", type: "assistant", parts: [{ type: "text", text }] };
}

test.describe("contract stream reconnects (US-69.10, mocked backend)", () => {
  test("midturn_reconnect_e2e: stream killed mid-turn — transcript reconstructs exactly", async ({ page }) => {
    // History: empty during the turn; the completed message appears only
    // after the turn ends (the store catches up).
    let turnOver = false;
    await setupChatMocks(page, {
      history: () => (turnOver ? [userHistoryMessage("draft the release notes"), assistantHistoryMessage("Hello world")] : [userHistoryMessage("draft the release notes")]),
      // The provider's busy map follows the user stream: busy while the
      // turn is live, idle once the store owns it (production
      // dual-publishes the same shape).
      userStream: () => (turnOver
        ? `data: ${JSON.stringify({ type: "session.status", workspace_id: WORKSPACE_ID, session_id: SESSION_ID, status: "idle" })}\n\n`
        : `data: ${JSON.stringify({ type: "session.status", workspace_id: WORKSPACE_ID, session_id: SESSION_ID, status: "busy" })}\n\n`),
    });

    let contractHits = 0;
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/contract-events`, async (route: Route) => {
      contractHits++;
      if (contractHits === 1) {
        // Connection 1: snapshot@5 mid-turn (in-flight text part), one
        // delta — then the stream dies (body ends = server closed).
        const body =
          snapshotFrame(5, [{ sessionId: SESSION_ID, status: "SESSION_STATUS_BUSY", inFlightParts: [{ id: "p1", type: "PART_TYPE_TEXT", text: "Hello" }], queueDepth: 0, pendingInputs: [] }]) +
          partDelta(6, "p1", " wor");
        await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
        return;
      }
      if (contractHits === 2) {
        // Connection 2 (reconnect): fresh snapshot@9 — the pod advanced
        // while we were gone — plus a STALE overlap event (seq 6, already
        // folded: must be discarded) and the completing events. The
        // discard rule makes the overlap exact.
        const body =
          snapshotFrame(9, [{ sessionId: SESSION_ID, status: "SESSION_STATUS_BUSY", inFlightParts: [{ id: "p1", type: "PART_TYPE_TEXT", text: "Hello wor" }], queueDepth: 0, pendingInputs: [] }]) +
          partDelta(6, "p1", " STALE-DUPLICATE") +
          partDelta(10, "p1", "ld") +
          partEnd(11, "p1", "Hello world") +
          sessionStatus(12, "SESSION_STATUS_IDLE");
        await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
        turnOver = true;
        return;
      }
      // Later connections (post-idle polling churn): idle snapshot.
      const body = snapshotFrame(12, [{ sessionId: SESSION_ID, status: "SESSION_STATUS_IDLE", inFlightParts: [], queueDepth: 0, pendingInputs: [] }]);
      await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
    });

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.locator("h2")).toContainText("Contract Stream WS", { timeout: 10000 });

    // The mid-turn stream renders the in-flight text.
    await expect(page.getByText("Hello wor").first()).toBeVisible({ timeout: 10000 });

    // Reconnect happens on the connection-1 body end (backoff 1–3s); the
    // fresh snapshot + discard rule reconstruct the turn exactly.
    await expect(contractHits).toBeGreaterThanOrEqual(1);
    await expect
      .poll(async () => contractHits, { timeout: 15000 })
      .toBeGreaterThanOrEqual(2);

    // Exact reconstruction vs the store: exactly one user bubble and one
    // assistant bubble with the final text — the stale duplicate delta
    // must NOT have doubled any part, and nothing may be missing.
    await expect(page.getByText("Hello world").first()).toBeVisible({ timeout: 15000 });
    await expect(page.getByText("STALE-DUPLICATE")).toHaveCount(0);
    await expect(page.getByText("draft the release notes")).toHaveCount(1);
    // After idle, the history (store) owns the turn and the streaming
    // bubble yields: exactly one. Poll — the reconcile refetch and the
    // provider busy-clear race under CI load.
    await expect
      .poll(async () => page.getByText("Hello world").count(), { timeout: 15000 })
      .toBe(1);
  });

  test("standing_question_reconnect_e2e: question answerable from the snapshot alone", async ({ page }) => {
    const extraFetches: string[] = [];
    await setupChatMocks(page, {
      history: () => [userHistoryMessage("ship it?")],
      sessions: () => [{ id: SESSION_ID, title: "Standing question", messageCount: 1, status: "busy" }],
    });

    let contractHits = 0;
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/contract-events`, async (route: Route) => {
      contractHits++;
      if (contractHits === 1) {
        // Connection 1: busy turn, no pending input yet — then the stream
        // dies before the question event is observed.
        const body = snapshotFrame(3, [{ sessionId: SESSION_ID, status: "SESSION_STATUS_BUSY", inFlightParts: [], queueDepth: 0, pendingInputs: [] }]);
        await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
        return;
      }
      // Reconnect: the snapshot carries the standing question (I12) —
      // the prompt must render from this frame alone.
      const body = snapshotFrame(7, [{ sessionId: SESSION_ID, status: "SESSION_STATUS_BUSY", inFlightParts: [], queueDepth: 0, pendingInputs: [busySnapshotInput("que_1", "Ship the release now?")] }]);
      await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
    });
    // Any fetch beyond the stream/sessions refresh would disprove I12.
    page.on("request", (req) => {
      const url = req.url();
      if (url.includes("input-snapshot") || url.includes("/question") || url.includes("/permission")) {
        extraFetches.push(url);
      }
    });

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.locator("h2")).toContainText("Contract Stream WS", { timeout: 10000 });

    // The prompt renders from the reconnect snapshot and is answerable.
    await expect(page.getByText("Ship the release now?").first()).toBeVisible({ timeout: 15000 });
    const yes = page.getByRole("button", { name: /yes/i }).first();
    await expect(yes).toBeVisible();
    await expect(extraFetches).toEqual([]);
  });

  test("api_rolling_deploy_e2e: replica churn mid-turn — browser converges, no stuck spinner", async ({ page }) => {
    let turnOver = false;
    await setupChatMocks(page, {
      history: () => (turnOver ? [userHistoryMessage("go"), assistantHistoryMessage("rolled through the deploy")] : [userHistoryMessage("go")]),
      // The provider's busy map follows the user stream: busy while the
      // turn is live, idle once the store owns it — same shape production
      // dual-publishes.
      userStream: () => (turnOver
        ? `data: ${JSON.stringify({ type: "session.status", workspace_id: WORKSPACE_ID, session_id: SESSION_ID, status: "idle" })}\n\n`
        : `data: ${JSON.stringify({ type: "session.status", workspace_id: WORKSPACE_ID, session_id: SESSION_ID, status: "busy" })}\n\n`),
    });

    let contractHits = 0;
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/contract-events`, async (route: Route) => {
      contractHits++;
      if (contractHits === 1) {
        // Replica A: snapshot + first delta, then the deploy kills it.
        const body =
          snapshotFrame(4, [{ sessionId: SESSION_ID, status: "SESSION_STATUS_BUSY", inFlightParts: [{ id: "p1", type: "PART_TYPE_TEXT", text: "rolled" }], queueDepth: 0, pendingInputs: [] }]) +
          partDelta(5, "p1", " through");
        await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
        return;
      }
      if (contractHits === 2) {
        // Replica B: fresh snapshot (the pod advanced past the gap) with
        // overlap events re-delivered — converges via the discard rule,
        // then the turn completes and goes idle.
        const body =
          snapshotFrame(8, [{ sessionId: SESSION_ID, status: "SESSION_STATUS_BUSY", inFlightParts: [{ id: "p1", type: "PART_TYPE_TEXT", text: "rolled through" }], queueDepth: 0, pendingInputs: [] }]) +
          partDelta(5, "p1", " DUPLICATED") +
          partDelta(9, "p1", " the deploy") +
          partEnd(10, "p1", "rolled through the deploy") +
          sessionStatus(11, "SESSION_STATUS_IDLE");
        await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
        turnOver = true;
        return;
      }
      const body = snapshotFrame(11, [{ sessionId: SESSION_ID, status: "SESSION_STATUS_IDLE", inFlightParts: [], queueDepth: 0, pendingInputs: [] }]);
      await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
    });

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.getByText("rolled through").first()).toBeVisible({ timeout: 10000 });

    await expect
      .poll(async () => contractHits, { timeout: 15000 })
      .toBeGreaterThanOrEqual(2);

    // Convergence: final text from the store, no duplicated delta text,
    // and no stuck streaming indicator once idle lands.
    await expect(page.getByText("rolled through the deploy").first()).toBeVisible({ timeout: 15000 });
    await expect(page.getByText("DUPLICATED")).toHaveCount(0);
    await page.waitForTimeout(2500);
    const bounce = page.locator("span.animate-bounce");
    await expect(bounce).toHaveCount(0);
  });
});
