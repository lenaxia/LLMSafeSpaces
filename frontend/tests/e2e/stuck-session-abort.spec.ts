/**
 * E2E tests for the stuck-session auto-abort gating (PR #852, fold-driven
 * after the US-69.10 hard cutover).
 *
 * Uses Playwright route interception to mock the backend, including the
 * contract stream (`/api/v1/workspaces/:id/contract-events`) whose stamped
 * snapshots are the projection-authoritative evidence. Two scenarios the
 * unit tests cannot cover end-to-end:
 *
 *  1. Reconnect into a BUSY session with a LIVE pending question — the
 *     fold's snapshot CARRIES the pendingInput → the prompt renders and
 *     stays answerable, and the "Session was interrupted" banner must NOT
 *     appear.
 *  2. Reconnect into a genuinely stuck session (opencode restarted and
 *     lost the question): the fold says BUSY with a running question tool
 *     in history while snapshots NEVER carry pendingInputs → after the
 *     1.5s dwell the auto-abort fires and the banner appears.
 */
import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

const WORKSPACE_ID = "ws-e2e-abort";
const SESSION_ID = "ses_e2e_abort";
const API_PREFIX = "**/api/v1";

const HISTORY_WITH_RUNNING_QUESTION = [
  { id: "msg_u1", type: "user", createdAt: "2026-08-14T00:00:00Z", parts: [{ type: "text", text: "push to github" }] },
  {
    id: "msg_a1",
    type: "assistant",
    createdAt: "2026-08-14T00:00:05Z",
    parts: [
      {
        type: "tool",
        tool: {
          name: "question",
          state: { status: "running" },
          input: { question: "GitHub auth required" },
        },
      },
    ],
  },
];

// --- contract-frame builders (mirrors contract-stream.spec.ts) ---------------

function snapshotFrame(atSeq: number | bigint, sessions: Array<Record<string, unknown>>): string {
  return `data: ${JSON.stringify({
    snapshot: { atSeq: String(atSeq), snapshot: { sessions } },
  })}\n\n`;
}

function busySession(pendingInputs: Array<Record<string, unknown>>): Record<string, unknown> {
  return { sessionId: SESSION_ID, status: "SESSION_STATUS_BUSY", inFlightParts: [], queueDepth: 0, pendingInputs };
}

const LIVE_QUESTION_INPUT = {
  id: "que_live_e2e",
  sessionId: SESSION_ID,
  rootSessionId: SESSION_ID,
  kind: "INPUT_KIND_QUESTION",
  question: "GitHub auth required — approve?",
  header: "GitHub auth",
  options: [{ label: "Yes", description: "" }, { label: "No", description: "" }],
};

async function setupAPIMocks(page: Page, opts?: { sessionEventsFirstBody?: string }) {
  let sessionStreamHits = 0;

  await page.route(`${API_PREFIX}/auth/me`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "testuser", email: "t@t.com", role: "user", active: true }) });
  });
  await page.route(`${API_PREFIX}/auth/config`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false, instanceName: "test" }) });
  });
  await page.route(`${API_PREFIX}/workspaces`, async (route: Route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WORKSPACE_ID, name: "Abort Test", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
    } else { await route.continue(); }
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/status`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" }, sessions: [{ id: SESSION_ID, status: "busy" }] }) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([{ id: SESSION_ID, title: "Abort Test", messageCount: 2, status: "busy" }]) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions/*/message*`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(HISTORY_WITH_RUNNING_QUESTION) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions/*/ensure`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID }) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions/*/seen`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: "true" });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions/*/title`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: "true" });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/input-snapshot`, async (route: Route) => {
    await route.fulfill({ status: 202, contentType: "application/json", body: JSON.stringify({ status: "snapshot requested" }) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/models`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ models: [], currentModel: "" }) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions/*/queue`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ messages: [] }) });
  });
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/runs/active`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ runs: [] }) });
  });
  await page.route(`${API_PREFIX}/image-factory/configs`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [] }) });
  });
  await page.route(`${API_PREFIX}/orgs`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
  });
  // Workspace SSE: ONE finite response, then hold forever. Finite bodies make
  // the browser reconnect-loop and re-deliver the full event body every ~1-3s
  // (production replays are id-filtered) — that churn makes these specs
  // timing-dependent. A handler that never fulfills keeps the reconnect
  // "connecting" (no onConnect → no onReconnect churn), pinning state.
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/session-events`, async (route: Route) => {
    sessionStreamHits++;
    // Deliver on the first TWO hits (React StrictMode's dev double-mount
    // destroys connection #1 before its body is processed; #2 survives),
    // then hold forever — a never-fulfilling handler keeps later reconnects
    // "connecting" (no onConnect → no onReconnect churn, no re-delivery).
    if (sessionStreamHits <= 2) {
      await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: opts?.sessionEventsFirstBody ?? ":ok\n\n" });
    } else {
      await new Promise<never>(() => {}); // hold — no further delivery
    }
  });
  // question reply endpoint — the live-prompt scenario must be answerable
  await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/question/que_live_e2e/reply`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: "true" });
  });

}

// mockUserStream installs the scenario-specific user-event-stream mock.
// Delivery is deterministic-by-construction:
//  - hit 1 delivers `firstDelivery` and ends;
//  - if `markersAfterSnapshotPost` is given, hit 2+ WAITS until the on-demand
//    /input-snapshot POST fired (i.e. reconnect mode armed) and then delivers
//    the markers exactly once — markers can never precede arming;
//  - every other hit holds forever (never fulfills), so the browser's
//    reconnect loop neither re-delivers events nor triggers onReconnect
//    churn — unlike production's id-filtered replay, a finite mock body
//    would otherwise redeliver the FULL event set every ~1-3s cycle and make
//    the specs timing-dependent.
function mockUserStream(page: Page, firstDelivery: object[], markersAfterSnapshotPost?: object[]) {
  let hits = 0;
  let markersDelivered = false;
  let resolveSnapshotRequested!: () => void;
  const snapshotRequested = new Promise<void>((resolve) => { resolveSnapshotRequested = resolve; });
  page.on("request", (req) => {
    if (req.method() === "POST" && req.url().includes(`/workspaces/${WORKSPACE_ID}/input-snapshot`)) {
      resolveSnapshotRequested();
    }
  });
  return page.route(`${API_PREFIX}/events`, async (route: Route) => {
    hits++;
    if (hits === 1) {
      await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: sseBody(firstDelivery) });
      return;
    }
    if (markersAfterSnapshotPost && !markersDelivered) {
      await snapshotRequested;
      markersDelivered = true;
      await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: sseBody(markersAfterSnapshotPost) });
      return;
    }
    await new Promise<never>(() => {}); // hold — no further delivery
  });
}

function sseBody(events: object[]): string {
  return events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join("");
}

test.describe("Stuck-session auto-abort gating (PR #852, mocked backend)", () => {
  test("live pending question survives a fold snapshot carrying it — no interrupted banner, prompt answerable", async ({ page }) => {
    await setupAPIMocks(page);
    // The fold's snapshot is projection-authoritative: the LIVE question is
    // carried as a pendingInput, so the prompt renders from the snapshot
    // alone and the stuck-tool evidence never holds.
    let resolveReply!: () => void;
    const replyGate = new Promise<void>((resolve) => { resolveReply = resolve; });
    let contractHits = 0;
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/contract-events`, async (route: Route) => {
      contractHits++;
      if (contractHits <= 2) {
        // React StrictMode's dev double-mount destroys connection #1 before
        // its body is processed — deliver on the first two hits.
        const body = snapshotFrame(3, [busySession([LIVE_QUESTION_INPUT])]);
        await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
        return;
      }
      // After the answer lands, the fold drops the input (production emits
      // INPUT_RESOLVED / a fresh snapshot without it) — the prompt stays gone.
      await replyGate;
      const body = snapshotFrame(4, [{ sessionId: SESSION_ID, status: "SESSION_STATUS_IDLE", inFlightParts: [], queueDepth: 0, pendingInputs: [] }]);
      await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
    });
    // User stream: busy seed for the provider's busy map (unchanged path).
    await mockUserStream(page, [
      { type: "session.status", session_id: SESSION_ID, workspace_id: WORKSPACE_ID, status: "busy" },
    ]);

    let abortCalled = false;
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/abort`, async (route: Route) => {
      abortCalled = true;
      await route.fulfill({ status: 200, contentType: "application/json", body: "true" });
    });
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/question/que_live_e2e/reply`, async (route: Route) => {
      resolveReply();
      await route.fulfill({ status: 200, contentType: "application/json", body: "true" });
    });

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // The question prompt must render and stay answerable.
    await expect(page.getByText("GitHub auth required — approve?")).toBeVisible({ timeout: 10_000 });

    // Long enough for the abort dwell (1.5s) + margin — no banner, no abort.
    await page.waitForTimeout(4000);
    await expect(page.getByText(/Session was interrupted/i)).not.toBeVisible();
    expect(abortCalled).toBe(false);

    // Answerable: clicking an option + submit hits the reply endpoint.
    await page.getByRole("button", { name: "Yes" }).click();
    await page.getByText("Submit answers").click();
    await expect(page.getByText("GitHub auth required — approve?")).not.toBeVisible({ timeout: 10_000 });
  });

  test("genuinely stuck session (fold busy, snapshots never carry pendingInputs) is still auto-aborted with banner", async ({ page }) => {
    await setupAPIMocks(page);
    // opencode restarted and lost the question: the fold stays BUSY with a
    // running question tool in history while NO snapshot ever carries a
    // pendingInput — the fold evidence holds through the 1.5s dwell and the
    // auto-abort fires.
    let contractHits = 0;
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/contract-events`, async (route: Route) => {
      contractHits++;
      if (contractHits <= 2) {
        const body = snapshotFrame(2, [busySession([])]);
        await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body });
        return;
      }
      await new Promise<never>(() => {}); // hold — snapshots never carry inputs, no churn
    });
    // User stream: busy seed for the provider's busy map (unchanged path).
    await mockUserStream(page, [
      { type: "session.status", session_id: SESSION_ID, workspace_id: WORKSPACE_ID, status: "busy" },
    ]);

    let abortCalled = false;
    await page.route(`${API_PREFIX}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/abort`, async (route: Route) => {
      abortCalled = true;
      await route.fulfill({ status: 200, contentType: "application/json", body: "true" });
    });

    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);

    // Abort must fire after the dwell (+ history load + polling margin),
    // and the banner must appear.
    await expect(page.getByText(/Session was interrupted/i)).toBeVisible({ timeout: 10_000 });
    expect(abortCalled).toBe(true);
  });
});
