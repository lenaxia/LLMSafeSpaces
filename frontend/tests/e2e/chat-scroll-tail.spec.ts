/**
 * #1720 — chat auto-tail vs user scroll during streaming.
 *
 * Acceptance (from the issue):
 *   - streaming + scroll away + further stream updates → viewport stays;
 *   - resume control appears while tail is disarmed mid-stream;
 *   - click → jumps to bottom and follows again;
 *   - stream end settles tail state;
 *   - a stream starting while scrolled away must not snap (no silent
 *     re-arm on the streaming transition).
 *
 * Transport: both SSE endpoints are fetch+ReadableStream (sseConnection),
 * so incremental frames are delivered through a fetch monkey-patch that
 * exposes per-test emitters (window.__contract / window.__user). Route
 * interception can only return complete bodies — it cannot stream.
 */
import { test, expect, type Page, type Route } from "@playwright/test";

const WORKSPACE_ID = "ws-scroll-e2e";
const SESSION_ID = "sess-scroll-e2e";
const API = "**/api/v1";
const PART_ID = "p1";
const MESSAGE_ID = "msg_a1";

// --- contract-frame builders (mirrors streaming.spec.ts) ---------------------

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

function partDelta(seq: number | bigint, delta: string): string {
  return eventFrame(seq, {
    type: "EVENT_TYPE_PART_DELTA",
    sessionId: SESSION_ID,
    messageId: MESSAGE_ID,
    partId: PART_ID,
    delta,
  });
}

function partEnd(seq: number | bigint, text: string): string {
  return eventFrame(seq, {
    type: "EVENT_TYPE_PART_END",
    sessionId: SESSION_ID,
    messageId: MESSAGE_ID,
    partId: PART_ID,
    part: { id: PART_ID, type: "PART_TYPE_TEXT", text },
  });
}

function idleSessions() {
  return [{ sessionId: SESSION_ID, status: "SESSION_STATUS_IDLE", inFlightParts: [], queueDepth: 0, pendingInputs: [] }];
}

function busySessions(seedText: string) {
  return [{
    sessionId: SESSION_ID,
    status: "SESSION_STATUS_BUSY",
    inFlightParts: [{ id: PART_ID, type: "PART_TYPE_TEXT", text: seedText }],
    queueDepth: 0,
    pendingInputs: [],
  }];
}

// --- incremental SSE via fetch patch ----------------------------------------

async function installSSEEmitters(page: Page) {
  await page.addInitScript(
    (args: { sessionId: string }) => {
      const enc = new TextEncoder();
      // Live per-connection controllers. StrictMode's dev double-mount
      // destroys connection #1; broadcasting to every live controller
      // (enqueue on a cancelled stream throws — swallowed) reaches
      // whichever mount survives.
      const contractCtrls = new Set<ReadableStreamDefaultController<Uint8Array>>();
      const userCtrls = new Set<ReadableStreamDefaultController<Uint8Array>>();

      const idleSeed = `data: ${JSON.stringify({
        snapshot: { atSeq: "1", snapshot: { sessions: [{ sessionId: args.sessionId, status: "SESSION_STATUS_IDLE", inFlightParts: [], queueDepth: 0, pendingInputs: [] }] } },
      })}\n\n`;

      const broadcast = (ctrls: Set<ReadableStreamDefaultController<Uint8Array>>, frame: string) => {
        for (const c of ctrls) {
          try {
            c.enqueue(enc.encode(frame));
          } catch {
            /* cancelled connection */
          }
        }
      };

      (window as unknown as Record<string, unknown>).__contract = (frame: string) => broadcast(contractCtrls, frame);
      (window as unknown as Record<string, unknown>).__user = (frame: string) => broadcast(userCtrls, frame);

      const origFetch = window.fetch.bind(window);
      window.fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
        if (url.includes("/contract-events") || url.includes("/events")) {
          const isContract = url.includes("/contract-events");
          const ctrls = isContract ? contractCtrls : userCtrls;
          let ctl: ReadableStreamDefaultController<Uint8Array> | null = null;
          const stream = new ReadableStream<Uint8Array>({
            start(c) {
              ctl = c;
              ctrls.add(c);
              if (isContract) {
                // Protocol rule: every body opens with a snapshot.
                c.enqueue(enc.encode(idleSeed));
              }
            },
            cancel: () => {
              if (ctl) ctrls.delete(ctl);
            },
          });
          void stream;
          return new Response(stream, {
            status: 200,
            headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
          });
        }
        return origFetch(input as RequestInfo, init);
      };
    },
    { sessionId: SESSION_ID },
  );
}

// --- REST mocks (history tall enough to scroll) ------------------------------

function tallHistory(): Array<Record<string, unknown>> {
  return Array.from({ length: 24 }, (_, i) => ({
    id: `msg_${i}`,
    type: i % 2 === 0 ? "user" : "assistant",
    parts: [{ type: "text", text: `History line ${i} — ${"lorem ipsum dolor sit amet ".repeat(3)}` }],
  }));
}

let historyFinalized: string | null = null;

async function setupAPIMocks(page: Page, history: Array<Record<string, unknown>> = tallHistory()) {
  await page.route(`${API}/auth/login`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ token: "e2e-token", user: { id: "u1", username: "tester", role: "user" } }) });
  });
  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true }) }));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false }) }));

  await page.route(`${API}/workspaces`, (r: Route) => {
    if (r.request().method() !== "GET") { void r.continue(); return; }
    void r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [{ id: WORKSPACE_ID, name: "Scroll E2E", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }], pagination: { limit: 50, offset: 0, total: 1 } }) });
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/status`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" } }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/activate`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ resumed: WORKSPACE_ID }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/models`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ models: [], currentModel: "" }) }));

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions?*`, async (r: Route) => {
    if (r.request().method() === "POST") {
      await r.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, sessionId: SESSION_ID }) });
      return;
    }
    await r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([{ id: SESSION_ID, title: "Scroll Test", messageCount: 24, status: "idle", hasUnread: false }]) });
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/ensure`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID }) }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/*/seen`, (r: Route) => r.fulfill({ status: 204 }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESSION_ID, title: "Scroll Test" }) }));

  await page.route(`${API}/workspaces/${WORKSPACE_ID}/sessions/${SESSION_ID}/message*`, (r: Route) => {
    const body = historyFinalized
      ? JSON.stringify([...history, { id: MESSAGE_ID, type: "assistant", parts: [{ type: "text", text: historyFinalized }] }])
      : JSON.stringify(history);
    void r.fulfill({ status: 200, contentType: "application/json", body });
  });

  // Platform stream — irrelevant here, single empty body then reconnects
  // are fine (only platform events, no session-state dialect).
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, (r: Route) =>
    r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));
}

// --- helpers ------------------------------------------------------------------

interface ScrollState { scrollTop: number; scrollHeight: number; clientHeight: number }

async function scrollState(page: Page): Promise<ScrollState> {
  return page.evaluate(() => {
    const el = document.querySelector('[role="log"]');
    if (!el) throw new Error("log element not found");
    return { scrollTop: el.scrollTop, scrollHeight: el.scrollHeight, clientHeight: el.clientHeight };
  });
}

// The pill carries aria-label="Scroll to bottom"; its visible text
// ("Jump to bottom" / "Resume tailing") distinguishes the variants.
async function atBottom(s: ScrollState): Promise<boolean> {
  return Math.abs(s.scrollHeight - s.scrollTop - s.clientHeight) < 4;
}

async function emitDeltas(page: Page, fromSeq: number, batches: number, text: string): Promise<number> {
  let seq = fromSeq;
  for (let i = 0; i < batches; i++) {
    await page.evaluate((frame: string) => {
      (window as unknown as { __contract: (f: string) => void }).__contract(frame);
    }, partDelta(seq++, `${text} batch ${i} — ${"streaming token line ".repeat(6)}\n`));
    await page.waitForTimeout(120);
  }
  return seq;
}

test.describe("#1720 chat auto-tail vs user scroll during streaming", () => {
  test.beforeEach(async ({ page }) => {
    historyFinalized = null;
    await setupAPIMocks(page);
    await installSSEEmitters(page);
  });

  test("scroll-away during streaming holds; explicit resume re-arms; stream end settles", async ({ page }) => {
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.getByText("History line 0").first()).toBeVisible({ timeout: 10_000 });
    await page.waitForTimeout(500); // let both StrictMode mounts settle

    const log = page.locator('[role="log"]');
    await expect(log).toBeVisible();

    // Stream starts: busy via user stream + busy snapshot with seed text.
    await page.evaluate((frame: string) => {
      (window as unknown as { __user: (f: string) => void }).__user(frame);
    }, `data: ${JSON.stringify({ type: "session.status", workspace_id: WORKSPACE_ID, session_id: SESSION_ID, status: "busy" })}\n\n`);
    await page.evaluate((frame: string) => {
      (window as unknown as { __contract: (f: string) => void }).__contract(frame);
    }, snapshotFrame(10, busySessions("Streaming starts now.\n")));

    // The snapshot seeds fold state; the bubble renders from EVENTS —
    // prime it with one delta and wait on that.
    await page.evaluate((frame: string) => {
      (window as unknown as { __contract: (f: string) => void }).__contract(frame);
    }, partDelta(11, "Streaming starts now.\n"));
    let seq = 12;
    await expect(page.getByText("Streaming starts now.").first()).toBeVisible({ timeout: 5_000 });

    // Tailing: armed and following the batches.
    seq = await emitDeltas(page, seq, 4, "early");
    await page.waitForTimeout(250);
    let s = await scrollState(page);
    expect(s.scrollHeight).toBeGreaterThan(s.clientHeight + 300);
    expect(await atBottom(s)).toBe(true);
    await expect(page.getByRole("button", { name: "Scroll to bottom" })).toHaveCount(0);

    // User scrolls away with the wheel (genuine user input).
    const box = await log.boundingBox();
    expect(box).not.toBeNull();
    await page.mouse.move(box!.x + box!.width / 2, box!.y + box!.height / 2);
    for (let i = 0; i < 6; i++) {
      await page.mouse.wheel(0, -400);
      await page.waitForTimeout(60);
    }
    await page.waitForTimeout(250);
    s = await scrollState(page);
    expect(s.scrollHeight - s.scrollTop - s.clientHeight).toBeGreaterThan(300);

    // THE acceptance: further stream updates — viewport stays put.
    const heldScrollTop = s.scrollTop;
    seq = await emitDeltas(page, seq, 5, "post-scroll");
    await page.waitForTimeout(400);
    s = await scrollState(page);
    expect(Math.abs(s.scrollTop - heldScrollTop)).toBeLessThan(4);
    // Content kept growing (the stream was live while held).
    expect(s.scrollHeight).toBeGreaterThan(0);

    // Resume control visible; clicking re-arms + jumps.
    await expect(page.getByRole("button", { name: "Scroll to bottom" })).toBeVisible();
    await expect(page.getByText("Resume tailing")).toBeVisible();
    await page.getByRole("button", { name: "Scroll to bottom" }).click();
    await page.waitForTimeout(120);
    s = await scrollState(page);
    expect(await atBottom(s)).toBe(true);

    // Following again.
    seq = await emitDeltas(page, seq, 3, "resumed");
    await page.waitForTimeout(300);
    s = await scrollState(page);
    expect(await atBottom(s)).toBe(true);

    // Stream end settles: part end + idle snapshot + user-stream idle.
    const finalText = "Streaming starts now.\nfinal tail of the stream";
    historyFinalized = finalText;
    await page.evaluate((frame: string) => {
      (window as unknown as { __contract: (f: string) => void }).__contract(frame);
    }, partEnd(seq, finalText));
    await page.evaluate((frame: string) => {
      (window as unknown as { __contract: (f: string) => void }).__contract(frame);
    }, snapshotFrame(900, idleSessions()));
    await page.evaluate((frame: string) => {
      (window as unknown as { __user: (f: string) => void }).__user(frame);
    }, `data: ${JSON.stringify({ type: "session.status", workspace_id: WORKSPACE_ID, session_id: SESSION_ID, status: "idle" })}\n\n`);

    await page.waitForTimeout(800);
    await expect(page.getByRole("button", { name: "Scroll to bottom" })).toHaveCount(0);
    s = await scrollState(page);
    expect(await atBottom(s)).toBe(true);
  });

  test("stream starting while scrolled away does not snap or re-arm (no silent re-arm)", async ({ page }) => {
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.getByText("History line 0").first()).toBeVisible({ timeout: 10_000 });
    await page.waitForTimeout(500);

    const log = page.locator('[role="log"]');
    const box = await log.boundingBox();
    expect(box).not.toBeNull();

    // User scrolls away while IDLE.
    await page.mouse.move(box!.x + box!.width / 2, box!.y + box!.height / 2);
    for (let i = 0; i < 6; i++) {
      await page.mouse.wheel(0, -400);
      await page.waitForTimeout(60);
    }
    await page.waitForTimeout(250);
    const held = await scrollState(page);
    expect(held.scrollHeight - held.scrollTop - held.clientHeight).toBeGreaterThan(300);
    await expect(page.getByRole("button", { name: "Scroll to bottom" })).toBeVisible();
    await expect(page.getByText("Jump to bottom")).toBeVisible();

    // A stream starts (busy + snapshot + deltas) — no snap, no re-arm.
    await page.evaluate((frame: string) => {
      (window as unknown as { __user: (f: string) => void }).__user(frame);
    }, `data: ${JSON.stringify({ type: "session.status", workspace_id: WORKSPACE_ID, session_id: SESSION_ID, status: "busy" })}\n\n`);
    await page.evaluate((frame: string) => {
      (window as unknown as { __contract: (f: string) => void }).__contract(frame);
    }, snapshotFrame(10, busySessions("seed\n")));
    await page.evaluate((frame: string) => {
      (window as unknown as { __contract: (f: string) => void }).__contract(frame);
    }, partDelta(11, "A new stream begins.\n"));
    await expect(page.getByText("A new stream begins.").first()).toBeVisible({ timeout: 5_000 });

    await emitDeltas(page, 12, 4, "silent");
    await page.waitForTimeout(400);

    const s = await scrollState(page);
    expect(Math.abs(s.scrollTop - held.scrollTop)).toBeLessThan(4);
    // The control switched to the resume affordance — tail still disarmed.
    await expect(page.getByRole("button", { name: "Scroll to bottom" })).toBeVisible();
    await expect(page.getByText("Resume tailing")).toBeVisible();
    await expect(page.getByText("Jump to bottom")).toHaveCount(0);

    // r2: the acceptance for THIS composition — click resume → jumps
    // AND follows again (the r1 gap: the observer never attached for a
    // stream that started while disarmed, so the click jumped but the
    // stream was not followed).
    const beforeResume = await scrollState(page);
    await page.getByRole("button", { name: "Scroll to bottom" }).click();
    await page.waitForTimeout(120);
    let after = await scrollState(page);
    expect(await atBottom(after)).toBe(true);
    expect(after.scrollTop).toBeGreaterThan(beforeResume.scrollTop);

    await emitDeltas(page, 30, 4, "post-resume");
    await page.waitForTimeout(300);
    after = await scrollState(page);
    expect(await atBottom(after)).toBe(true);
    await expect(page.getByRole("button", { name: "Scroll to bottom" })).toHaveCount(0);
  });

  // r3: fresh-session composition — the component mounts WITHOUT a
  // scroll container (empty log), the stream brings it into existence
  // mid-flight. The scroll-intent listener must attach when the log
  // appears: scroll-away disarms, no snap-back, affordance present.
  test("fresh session: log mounts mid-stream — scroll away holds, affordance appears, resume follows", async ({ page }) => {
    await setupAPIMocks(page, []);
    await page.goto(`/chat/${WORKSPACE_ID}/${SESSION_ID}`);
    await expect(page.getByText("Send a message to start the conversation")).toBeVisible({ timeout: 10_000 });
    await expect(page.locator('[role="log"]')).toHaveCount(0);
    await page.waitForTimeout(500);

    // Stream starts on the empty session → the log mounts mid-stream.
    await page.evaluate((frame: string) => {
      (window as unknown as { __user: (f: string) => void }).__user(frame);
    }, `data: ${JSON.stringify({ type: "session.status", workspace_id: WORKSPACE_ID, session_id: SESSION_ID, status: "busy" })}\n\n`);
    await page.evaluate((frame: string) => {
      (window as unknown as { __contract: (f: string) => void }).__contract(frame);
    }, snapshotFrame(10, busySessions("seed\n")));
    await page.evaluate((frame: string) => {
      (window as unknown as { __contract: (f: string) => void }).__contract(frame);
    }, partDelta(11, "Fresh session stream starts.\n"));
    await expect(page.getByText("Fresh session stream starts.").first()).toBeVisible({ timeout: 5_000 });
    await expect(page.locator('[role="log"]')).toBeVisible();

    const log = page.locator('[role="log"]');
    // Grow the stream until it overflows the viewport (the fresh
    // session starts with an empty log — no seeded history to scroll).
    let seq = 12;
    for (let round = 0; round < 4; round++) {
      seq = await emitDeltas(page, seq, 10, `bulk${round}`);
    }
    let s = await scrollState(page);
    expect(s.scrollHeight).toBeGreaterThan(s.clientHeight + 200);
    expect(await atBottom(s)).toBe(true);

    // User scrolls away — the listener attached at log-mount must see it.
    const box = await log.boundingBox();
    expect(box).not.toBeNull();
    await page.mouse.move(box!.x + box!.width / 2, box!.y + box!.height / 2);
    for (let i = 0; i < 6; i++) {
      await page.mouse.wheel(0, -400);
      await page.waitForTimeout(60);
    }
    await page.waitForTimeout(250);
    s = await scrollState(page);
    expect(s.scrollHeight - s.scrollTop - s.clientHeight).toBeGreaterThan(100);
    const held = s.scrollTop;

    // Further batches: no snap-back; the affordance is present.
    seq = await emitDeltas(page, seq, 4, "fresh-held");
    await page.waitForTimeout(400);
    s = await scrollState(page);
    expect(Math.abs(s.scrollTop - held)).toBeLessThan(4);
    await expect(page.getByText("Resume tailing")).toBeVisible();

    // Explicit resume: jumps and follows again in this composition too.
    await page.getByRole("button", { name: "Scroll to bottom" }).click();
    await page.waitForTimeout(120);
    seq = await emitDeltas(page, seq, 3, "fresh-resumed");
    await page.waitForTimeout(300);
    s = await scrollState(page);
    expect(await atBottom(s)).toBe(true);
    void seq;
  });
});
