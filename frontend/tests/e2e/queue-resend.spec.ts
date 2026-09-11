import { test, expect, type Page, type Route } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

// Queue re-send e2e pins (#1320 items 3/5) — the browser-level
// acceptance bar for the contended-delivery 503 semantics (#1318) and
// the send-path dedupe identity (D3/#907). Pure route-mock strategy
// (register-turnstile.spec.ts precedent): no live backend, deterministic
// in CI. What only a real browser catches here: the ChatPage → hook →
// api wiring (a disconnected onRetry/onDismiss handler passes every
// hook test), and the actual wire bodies carrying clientMessageID.

const WS = "ws-queue-e2e";
const SES = "sess-queue-e2e";
const API = "**/api/v1";

type QueueEntry = {
  id: string;
  text: string;
  session_id: string;
  workspace_id: string;
  enqueued_at: string;
  retry_count: number;
  status?: string;
  lastError?: string;
  clientMessageID?: string;
};

async function stubAuth(page: Page) {
  await page.route(`${API}/auth/me`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ id: "u1", username: "e2e", email: "e2e@test", role: "user", status: "active" }),
    });
  });
}

async function stubWorkspace(page: Page, opts: { eventsStub?: boolean } = {}) {
  await page.route(`${API}/workspaces`, async (route: Route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          items: [{ id: WS, name: "queue-e2e", phase: "Active", userId: "u1" }],
          pagination: { limit: 20, offset: 0, total: 1 },
        }),
      });
    } else {
      await route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: WS }) });
    }
  });
  await page.route(`${API}/workspaces/${WS}/status`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active" }) });
  });
  await page.route(`${API}/workspaces/${WS}/sessions/new`, async (route: Route) => {
    if (route.request().method() === "POST") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ workspaceId: WS, sessionId: SES, resumed: false, workspacePhase: "Active" }),
      });
    } else {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
    }
  });
  await page.route(`${API}/workspaces/${WS}/session-events`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "text/event-stream",
      body: `data: ${JSON.stringify({ type: "workspace.phase", phase: "Active" })}\n\n`,
    });
  });
  await page.route(`${API}/workspaces/${WS}/sessions`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
  });
  await page.route(`${API}/workspaces/${WS}/models`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ models: [], currentModel: "", currentModelProviderID: "" }),
    });
  });
  // History + session detail — without these the "Chat history
  // unavailable" alert renders its own Retry button and steals the
  // pill-scoped clicks.
  await page.route(`${API}/workspaces/${WS}/sessions/${SES}/message*`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
  });
  await page.route(`${API}/workspaces/${WS}/sessions/${SES}`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SES, status: "idle" }) });
  });
  await mockIdleContractStream(page, `${API}/workspaces/${WS}/contract-events`, SES);
  // r8: the USER event stream must be stubbed in EVERY arm — unstubbed,
  // it falls through to the dev server, fails as SSE, and reconnects
  // forever; that churn is what starved the clicks in four cold runs.
  // Hold-style: two empty 200 streams (StrictMode's double-connect),
  // then hold later connections. Arms that drive busy-state through
  // /events (the clearAll Abort path) opt OUT and own the route — the
  // provider's snapshot-staging means their busy event must ride the
  // FIRST connection to land live.
  if (opts.eventsStub !== false) {
    let eventHits = 0;
    await page.route(`${API}/events`, async (route: Route) => {
      eventHits++;
      if (eventHits <= 2) {
        await route.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: ":\n\n" });
        return;
      }
      await new Promise<never>(() => {});
    });
  }
}

// The queue surface under test: counters for every method + programmable
// retry/dismiss outcomes.
function stubQueue(page: Page, entries: QueueEntry[]) {
  const live = [...entries];
  const calls = { retry: 0, enqueue: 0, delete: 0, deleteStatus: 503 as number, retryStatus: 503 as number };
  const bodies: string[] = [];
  page.on("request", (req) => {
    if (req.url().includes(`/queue`) && req.method() === "POST" && !req.url().includes("/retry")) {
      bodies.push(req.postData() ?? "");
    }
  });
  return {
    calls,
    bodies,
    async install() {
      await page.route(`${API}/workspaces/${WS}/sessions/${SES}/queue`, async (route: Route) => {
        const method = route.request().method();
        if (method === "GET") {
          await route.fulfill({
            status: 200,
            contentType: "application/json",
            body: JSON.stringify({ messages: live }),
          });
        } else if (method === "POST") {
          calls.enqueue++;
          await route.fulfill({ status: 202, contentType: "application/json", body: JSON.stringify({ messageID: "srv_new" }) });
        }
      });
      await page.route(`${API}/workspaces/${WS}/sessions/${SES}/queue/*/retry`, async (route: Route) => {
        calls.retry++;
        if (calls.retryStatus >= 500) {
          await route.fulfill({
            status: calls.retryStatus,
            contentType: "application/json",
            body: JSON.stringify({ error: "session busy delivering; retry shortly" }),
          });
        } else {
          await route.fulfill({ status: 204, body: "" });
        }
      });
      await page.route(`${API}/workspaces/${WS}/sessions/${SES}/queue/*`, async (route: Route) => {
        if (route.request().method() === "DELETE") {
          calls.delete++;
          const id = route.request().url().split("/").pop()!;
          if (calls.deleteStatus >= 400) {
            await route.fulfill({
              status: calls.deleteStatus,
              contentType: "application/json",
              body: JSON.stringify({ error: "session busy delivering; retry shortly" }),
            });
          } else {
            const idx = live.findIndex((m) => m.id === id);
            if (idx >= 0) live.splice(idx, 1);
            await route.fulfill({ status: 204, body: "" });
          }
        } else {
          await route.fulfill({ status: 404, body: "" });
        }
      });
    },
  };
}

const ERR_ENTRY: QueueEntry = {
  id: "srv_1",
  text: "proceed with the split",
  session_id: SES,
  workspace_id: WS,
  enqueued_at: new Date().toISOString(),
  retry_count: 2,
  status: "error",
  lastError: "context deadline exceeded",
  clientMessageID: "cmid-srv-1",
};

async function openChatWithQueue(page: Page, entries: QueueEntry[], opts: { eventsStub?: boolean } = {}) {
  await stubAuth(page);
  await stubWorkspace(page, opts);
  const queue = stubQueue(page, entries);
  await queue.install();
  await page.goto(`/chat/${WS}`);
  // The route param selects the workspace; session auto-creation fires
  // exactly like a real user's first entry.
  await expect(page.getByText("1 message queued")).toBeVisible({ timeout: 10000 });
  // The section can land COLLAPSED (the toggle's open state races the
  // first pill render) — expand it before any pill-button interaction.
  const retryBtn = page.getByRole("button", { name: "Retry" }).first();
  try {
    await retryBtn.waitFor({ state: "visible", timeout: 1500 });
  } catch {
    await page.getByRole("button", { name: /messages queued/ }).click({ timeout: 5000 });
    await retryBtn.waitFor({ state: "visible", timeout: 5000 });
  }
  return queue;
}

// clickUntil — r6/r7: actionability starvation (r5) and the force-click's
// lost-click no-op (r6) are both symptoms of clicking a node mid-churn.
// The robust strategy retries the click UNTIL ITS EFFECT fires — with the
// EFFECT CHECKED FIRST each iteration, so a successful-but-slow click is
// never followed by a second POST (the double-click hazard of a naive
// retry loop). Locators arrive pre-bound (no page parameter — TS6133).
async function clickUntil(selector: () => import("@playwright/test").Locator, effect: () => Promise<unknown>) {
  await expect(async () => {
    // Effect present already? Done — no click, no duplicate POST.
    try {
      await effect();
      return;
    } catch {
      // not yet — fall through and click
    }
    await selector().click({ timeout: 5_000 }).catch(() => {});
    await effect();
  }).toPass({ timeout: 45_000 });
}

test.describe("queue re-send (#1320: contended-delivery 503s + dedupe identity)", () => {
  // Cold-start (first browser boot + app load) can consume most of the
  // default 30s before the assertions begin.
  test.setTimeout(60_000);

  test("retry under persistent 503: pill stays with the busy hint, ZERO re-enqueue POSTs", async ({ page }) => {
    const queue = await openChatWithQueue(page, [ERR_ENTRY]);

    await clickUntil(
      () => page.getByRole("button", { name: "Retry" }).first(),
      () => expect(page.getByText(/busy delivering/i).first()).toBeVisible({ timeout: 6_000 }),
    );

    await expect(page.getByText(ERR_ENTRY.text).first()).toBeVisible();
    // The invariant the whole fix exists for: contention must never mint
    // a second entry. ZERO queue POSTs is the hard assertion; the retry
    // POST count is >= 1 (the effect-first clickUntil makes a second
    // click vanishingly rare, but a slow render after a landed POST can
    // still cost one extra harmless 503 re-POST — an idempotent
    // server-side re-arm of the SAME entry, never a duplicate).
    await page.waitForTimeout(400);
    expect(queue.calls.enqueue).toBe(0);
    expect(queue.calls.retry).toBeGreaterThanOrEqual(1);
    expect(queue.calls.enqueue).toBe(0);
  });

  test("dismiss under 503: pill stays; after contention clears, dismiss removes it", async ({ page }) => {
    const queue = await openChatWithQueue(page, [ERR_ENTRY]);

    await clickUntil(
      () => page.getByRole("button", { name: "Dismiss" }).first(),
      () => expect(page.getByText(/busy delivering/i).first()).toBeVisible({ timeout: 6_000 }),
    );
    await expect(page.getByText(ERR_ENTRY.text).first()).toBeVisible();
    expect(queue.calls.delete).toBe(1);

    // Let the first dismiss's refreshQueue settle (the pill re-renders on
    // every refresh cycle — clicking mid-churn starves the actionability
    // check on a detaching node).
    await page.waitForTimeout(600);
    // Contention clears (delivery finished) — the dismiss now applies.
    queue.calls.deleteStatus = 204;
    await clickUntil(
      () => page.getByRole("button", { name: "Dismiss" }).first(),
      () => expect(page.getByText(ERR_ENTRY.text)).toHaveCount(0, { timeout: 6_000 }),
    );
    expect(queue.calls.delete).toBe(2);
  });

  // r5 missing-case 2: the Abort → clearAll path had no browser-level
  // coverage at all. Contended entries survive the sweep with a hint;
  // confirmed deletes clear.
  test("clearAll (Abort) under 503: contended pill survives hinted, confirmed delete clears", async ({ page }) => {
    // This arm owns /events through ONE gated route: the connection is
    // held QUIETLY during setup (no churn, no state — the r8 root cause
    // stays closed), then the SAME connection answers with the busy
    // event once the queue is up. Busy-from-load suppresses session
    // auto-creation, so the flip must come after setup.
    let releaseBusy!: () => void;
    const busyGate = new Promise<void>((resolve) => { releaseBusy = resolve; });
    await page.route("**/api/v1/events", async (route) => {
      await busyGate;
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: `data: ${JSON.stringify({ type: "session.status", status: "busy", session_id: SES, workspace_id: WS })}\n\n`,
      });
    });
    const queue = await openChatWithQueue(page, [ERR_ENTRY], { eventsStub: false });

    // A second, cleanly-deletable entry joins the queue — the GET stays
    // stateful so BOTH pills survive the refresh cycles.
    const PLAIN: typeof ERR_ENTRY = {
      ...ERR_ENTRY,
      id: "srv_2",
      text: "plain entry",
      status: undefined,
      lastError: undefined,
      clientMessageID: "cmid-srv-2",
    };
    // Stateful (r6): the GET payload must reflect confirmed deletes —
    // clearAll's trailing refreshQueue re-adds any entry the stub still
    // lists, so a stateless stub invalidates the very state under assert.
    const liveEntries: typeof ERR_ENTRY[] = [ERR_ENTRY, PLAIN];
    await page.route("**/api/v1/workspaces/ws-queue-e2e/sessions/sess-queue-e2e/queue", async (route) => {
      if (route.request().method() === "GET") {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ messages: liveEntries }),
        });
      } else {
        queue.calls.enqueue++;
        liveEntries.push(PLAIN);
        await route.fulfill({ status: 202, contentType: "application/json", body: JSON.stringify({ messageID: "srv_2" }) });
      }
    });
    const composer = page.getByPlaceholder("Type a message...");
    await composer.fill("plain entry");
    await composer.press("Control+Enter");
    await expect(page.getByText("2 messages queued")).toBeVisible({ timeout: 10000 });

    // Release the gate: the held /events connection now delivers the
    // busy event (the live authority for the session-activity provider).
    releaseBusy();
    const stop = page.getByRole("button", { name: "Stop generating" }).first();
    await expect(stop).toBeVisible({ timeout: 10000 });
    // The abort call itself must succeed (an unstubbed 404/502 renders a
    // global error banner unrelated to the queue contract under test).
    await page.route("**/api/v1/workspaces/ws-queue-e2e/sessions/sess-queue-e2e/abort", async (route) => {
      await route.fulfill({ status: 200, body: "" });
    });

    // The contended entry (srv_1) 503s; the fresh one (srv_2) confirms —
    // keyed by URL id, not call order (the sweep's pill order races with
    // the optimistic add + refresh re-sync).
    await page.route("**/api/v1/workspaces/ws-queue-e2e/sessions/sess-queue-e2e/queue/*", async (route) => {
      const url = route.request().url();
      if (route.request().method() === "DELETE") {
        queue.calls.delete++;
        if (url.endsWith("/srv_1")) {
          await route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ error: "session busy delivering; retry shortly" }) });
        } else {
          const idx = liveEntries.findIndex((m) => url.endsWith("/" + m.id));
          if (idx >= 0) liveEntries.splice(idx, 1);
          await route.fulfill({ status: 204, body: "" });
        }
      } else {
        await route.fulfill({ status: 404, body: "" });
      }
    });
    await clickUntil(
      () => page.getByRole("button", { name: "Stop generating" }).first(),
      () => expect(page.getByText(/delivery in progress/i).first()).toBeVisible({ timeout: 6_000 }),
    );

    // The queued "plain entry" also renders an optimistic transcript
    // bubble — pill removal is asserted via the QUEUE SECTION, not the
    // global text count: exactly one pill remains (the contended
    // survivor, hinted), the confirmed entry's pill is gone.
    await expect(page.getByText("1 message queued")).toBeVisible({ timeout: 5000 });
    await expect(page.getByText(ERR_ENTRY.text).first()).toBeVisible();
    await expect(page.getByText(/delivery in progress/i).first()).toBeVisible();
    expect(queue.calls.delete).toBe(2);
  });

  test("send retries a 503 with the SAME clientMessageID on the wire", async ({ page }) => {
    await stubAuth(page);
    await stubWorkspace(page);
    await page.route(`${API}/workspaces/${WS}/sessions/${SES}/history**`, async (route: Route) => {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
    });
    const promptBodies: string[] = [];
    let promptCalls = 0;
    await page.route(`${API}/workspaces/${WS}/sessions/${SES}/prompt`, async (route: Route) => {
      promptCalls++;
      promptBodies.push(route.request().postData() ?? "");
      if (promptCalls === 1) {
        await route.fulfill({
          status: 503,
          contentType: "application/json",
          body: JSON.stringify({ error: "workspace_restarting", retryAfter: 1 }),
        });
      } else {
        await route.fulfill({ status: 202, body: "" });
      }
    });

    await page.goto(`/chat/${WS}`);
    const composer = page.getByPlaceholder("Type a message...");
    await composer.fill("dedupe me");
    // Default desktop mode: Enter is newline, Ctrl+Enter sends.
    await composer.press("Control+Enter");

    await expect.poll(() => promptCalls, { timeout: 15000 }).toBe(2);
    const ids = promptBodies.map((b) => (JSON.parse(b) as { clientMessageID?: string }).clientMessageID);
    expect(ids[0]).toBeTruthy();
    expect(ids[1]).toBe(ids[0]);
  });
});
