import { test, expect, type Page, type Route } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

// Walk-away-walk-back e2e (#1313 / epic-71 3a) — the browser-level
// acceptance bar for the unanswered-question inbox. Pure route-mock
// strategy (queue-resend.spec.ts precedent): no live backend. The user
// stream delivers the snapshot flight (begin → whileAway question →
// complete ok) exactly as the API does when a recorded ask's live half
// is gone; what only a real browser catches: the SessionActivityProvider
// → ChatPage → QuestionPrompt wiring (a disconnected submit handler
// passes every vitest), the WhileYouWereAway chrome, and the actual wire
// routes for late-answer reply and inbox dismiss.

const WS = "ws-walkaway-e2e";
const SES = "sess-walkaway-e2e";
const API = "**/api/v1";

function whileAwayFlight(): string {
  const begin = { type: "agent.input.snapshot_begin", workspace_id: WS, snapshot_id: "flight-1" };
  const question = {
    type: "agent.question",
    workspace_id: WS,
    session_id: SES,
    request_id: "que_e2e_away1",
    data: {
      id: "que_e2e_away1",
      session_id: SES,
      root_session_id: SES,
      questions: [
        {
          question: "Should we ship the widget tonight?",
          header: "Ship it?",
          options: [
            { label: "Ship tonight", description: "deploy now" },
            { label: "Hold", description: "wait" },
          ],
        },
      ],
      whileAway: true,
    },
  };
  const complete = { type: "agent.input.snapshot_complete", workspace_id: WS, snapshot_id: "flight-1", snapshot_ok: true };
  return [begin, question, complete].map((e) => `data: ${JSON.stringify(e)}\n\n`).join("");
}

async function stubAuth(page: Page) {
  await page.route(`${API}/auth/me`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ id: "u1", username: "e2e", email: "e2e@test", role: "user", status: "active" }),
    });
  });
}

async function stubChatShell(page: Page, userEventsBody: string) {
  await page.route(`${API}/workspaces`, async (route: Route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          items: [{ id: WS, name: "walkaway-e2e", phase: "Active", userId: "u1" }],
          pagination: { limit: 20, offset: 0, total: 1 },
        }),
      });
    } else {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ workspaceId: WS, sessionId: SES, resumed: false, workspacePhase: "Active" }) });
    }
  });
  await page.route(`${API}/workspaces/${WS}/runs/active`, empty({ runs: [] }));
  await page.route(`${API}/workspaces/${WS}/agent-role`, empty({ role: null }));
  await page.route(`${API}/workspaces/${WS}/status`, empty({ phase: "Active", podName: "pod-1" }));
  await page.route(`${API}/workspaces/${WS}/alerts`, empty({ alerts: [] }));
  await page.route(`${API}/workspaces/${WS}/files`, empty({ files: [] }));
  await page.route(`${API}/orgs`, empty([]));
  await page.route(`${API}/image-factory/configs`, empty({ configs: [] }));
  await page.route(`${API}/admin/agent-roles`, empty([]));
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
  await page.route(`${API}/workspaces/${WS}/sessions/${SES}/message*`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
  });
  await page.route(`${API}/workspaces/${WS}/sessions/${SES}`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SES, status: "idle" }) });
  });
  await mockIdleContractStream(page, `${API}/workspaces/${WS}/contract-events`, SES);
  // The user stream carries the walk-away flight on the first
  // connections (StrictMode double-mount), then holds.
  let hits = 0;
  await page.route(`${API}/events`, async (route: Route) => {
    hits++;
    if (hits <= 2) {
      await route.fulfill({
        status: 200,
        headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
        body: userEventsBody,
      });
      return;
    }
    await new Promise<never>(() => {});
  });
}

const empty = (body: unknown) => async (route: Route) => {
  await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
};

test.describe("walk-away-walk-back (#1313 whileAway inbox)", () => {
  test("the whileAway prompt renders and the late answer posts to the reply route", async ({ page }) => {
    await stubAuth(page);
    await stubChatShell(page, whileAwayFlight());

    const replyBodies: string[] = [];
    await page.route(`${API}/workspaces/${WS}/question/que_e2e_away1/reply`, async (route: Route) => {
      replyBodies.push(route.request().postData() ?? "");
      await route.fulfill({ status: 202, contentType: "application/json", body: JSON.stringify({ status: "queued", clientMessageID: "inbox-que_e2e_away1-answer", messageID: "ob_1" }) });
    });

    await page.goto(`/chat/${WS}/${SES}`);

    await expect(page.getByText(/while you were away/i)).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText("Should we ship the widget tonight?")).toBeVisible();
    await expect(page.getByText(/waited unanswered/i)).toBeVisible();

    await page.getByRole("button", { name: "Ship tonight" }).click();
    await page.getByRole("button", { name: "Submit answers" }).click();

    await expect
      .poll(() => replyBodies.length, { message: "late answer must POST to the reply route" })
      .toBeGreaterThanOrEqual(1);
    expect(replyBodies[0]).toContain("Ship tonight");
  });

  test("dismiss routes to the inbox DELETE endpoint with session scoping", async ({ page }) => {
    await stubAuth(page);
    await stubChatShell(page, whileAwayFlight());

    const deletes: string[] = [];
    await page.route(`${API}/workspaces/${WS}/sessions/${SES}/inbox/que_e2e_away1`, async (route: Route) => {
      if (route.request().method() === "DELETE") {
        deletes.push(route.request().url());
        await route.fulfill({ status: 204, body: "" });
      } else {
        await route.fulfill({ status: 405, body: "" });
      }
    });
    // The live reject route must NOT fire for an inbox-only prompt.
    let rejectFired = 0;
    await page.route(`${API}/workspaces/${WS}/question/que_e2e_away1/reject`, async (route: Route) => {
      rejectFired++;
      await route.fulfill({ status: 200, contentType: "application/json", body: "true" });
    });

    await page.goto(`/chat/${WS}/${SES}`);

    await expect(page.getByText(/while you were away/i)).toBeVisible({ timeout: 10_000 });
    await page.getByText("Dismiss", { exact: true }).click();

    await expect
      .poll(() => deletes.length, { message: "dismiss must DELETE the inbox record" })
      .toBeGreaterThanOrEqual(1);
    expect(rejectFired).toBe(0);
    expect(deletes[0]).toContain(`/sessions/${SES}/inbox/que_e2e_away1`);
  });
});
