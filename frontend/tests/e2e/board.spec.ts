/**
 * #1702 — Task Board UI e2e (design 0067 §4.8, Lane 4).
 *
 * Rows:
 *   1. Board renders: swimlanes = projects, columns = stages, cards with
 *      short id / pause-waiting chips / tags / blocked badge.
 *   2. Drag = server-arbitrated move_stage: optimistic move + confirm;
 *      refused drag (409 + guard code) snaps back with the code rendered.
 *   3. Done drop collects the evidence URL (required on → done, R2).
 *   4. SSE live update: a board.event on the user stream moves the card
 *      (fetch monkey-patch streams frames — route.fulfill cannot stream;
 *      the known limitation documented in chat-scroll-tail.spec.ts).
 *   5. Scope switcher: project focus + epic chips + search did-you-mean.
 *   6. OR4 pr:N tag validation on the task editor.
 *
 * All backend APIs are mocked via Playwright route interception, the SSE
 * endpoints through the established fetch monkey-patch (per-test emitters).
 */
import { test, expect, type Page, type Route } from "@playwright/test";

const WORKSPACE_ID = "ws-board-e2e";
const API = "**/api/v1";
const PROJECT_A = "prj-aaaa1111";
const PROJECT_B = "prj-bbbb2222";

// --- fixtures -------------------------------------------------------------------

type E2ETask = {
  id: string;
  project_id: string;
  epic_id: string;
  short_id: string;
  title: string;
  description: string;
  stage: string;
  paused: boolean;
  project_name: string;
  epic_name: string;
  tags: string[];
  acceptance_criteria: Array<{ text: string; met: boolean }>;
  is_blocked: boolean;
  blocked_by?: string[];
  attempts: number;
  claim_incremented_attempts: boolean;
  stage_entered_at: string;
  created_at: string;
  updated_at: string;
};

function task(partial: Partial<E2ETask> & { short_id: string; title: string; stage: string; project_id: string; epic_id: string }): E2ETask {
  return {
    id: `uuid-${partial.short_id}`,
    description: "",
    paused: false,
    project_name: partial.project_id === PROJECT_A ? "Apollo" : "Borealis",
    epic_name: partial.epic_id === "epic-1" ? "E1" : "E2",
    tags: [],
    acceptance_criteria: [],
    is_blocked: false,
    attempts: 0,
    claim_incremented_attempts: false,
    stage_entered_at: "2026-10-10T00:00:00Z",
    created_at: "2026-10-10T00:00:00Z",
    updated_at: "2026-10-10T00:00:00Z",
    ...partial,
  };
}

interface BoardState {
  tasks: E2ETask[];
}

function freshState(): BoardState {
  return {
    tasks: [
      task({ short_id: "tsk-aaaa0001", title: "Write charter", stage: "backlog", project_id: PROJECT_A, epic_id: "epic-1" }),
      task({
        short_id: "tsk-bbbb0002", title: "Implement engine", stage: "in_progress", project_id: PROJECT_A, epic_id: "epic-1",
        attempts: 1, acceptance_criteria: [{ text: "tests green", met: false }, { text: "docs", met: true }],
      }),
      task({ short_id: "tsk-cccc0003", title: "Review drag arm", stage: "in_review", project_id: PROJECT_A, epic_id: "epic-1" }),
      task({ short_id: "tsk-dddd0004", title: "Ship design doc", stage: "done", project_id: PROJECT_A, epic_id: "epic-1" }),
      task({ short_id: "tsk-eeee0005", title: "Blocked follow-up", stage: "backlog", project_id: PROJECT_A, epic_id: "epic-2", is_blocked: true, blocked_by: ["tsk-cccc0003"], tags: ["pr:1702"] }),
      task({ short_id: "tsk-ffff0006", title: "Waiting on human", stage: "pending_input", project_id: PROJECT_B, epic_id: "epic-1", attempts: 3 }),
    ],
  };
}

// --- SSE emitters (the chat-scroll-tail fetch monkey-patch shape) ----------------

async function installSSEEmitters(page: Page) {
  await page.addInitScript(() => {
    const enc = new TextEncoder();
    const userCtrls = new Set<ReadableStreamDefaultController<Uint8Array>>();
    (window as unknown as Record<string, unknown>).__user = (frame: string) => {
      for (const c of userCtrls) {
        try {
          c.enqueue(enc.encode(frame));
        } catch {
          /* cancelled connection */
        }
      }
    };
    const origFetch = window.fetch.bind(window);
    window.fetch = async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url.includes("/events")) {
        let ctl: ReadableStreamDefaultController<Uint8Array> | null = null;
        const stream = new ReadableStream<Uint8Array>({
          start(c) {
            ctl = c;
            userCtrls.add(c);
          },
          cancel: () => {
            if (ctl) userCtrls.delete(ctl);
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
  });
}

// --- REST mocks -------------------------------------------------------------------

async function setupBoardMocks(page: Page, state: BoardState) {
  const j = (body: unknown, status = 200) => ({
    status,
    contentType: "application/json",
    body: JSON.stringify(body),
  });

  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill(j({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true })));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill(j({ registrationEnabled: false, oidcEnabled: false })));
  await page.route(`${API}/workspaces`, (r: Route) => {
    if (r.request().method() !== "GET") {
      void r.continue();
      return;
    }
    void r.fulfill(j({
      items: [{ id: WORKSPACE_ID, name: "Board E2E", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }],
      pagination: { limit: 50, offset: 0, total: 1 },
    }));
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/status`, (r: Route) =>
    r.fulfill(j({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" } })));

  // Board REST surface (#1699): projects list + per-project tasks +
  // move-stage. The move-stage mock mutates the shared state so the
  // post-invalidation refetch returns the authoritative row.
  await page.route(`${API}/me/projects`, (r: Route) =>
    r.fulfill(j({
      projects: [
        { id: PROJECT_A, short_id: PROJECT_A, user_id: "u1", name: "Apollo", target_workspace_id: WORKSPACE_ID, worker_slots: 2, tags: [], status: "active", created_at: "2026-10-01T00:00:00Z", updated_at: "2026-10-01T00:00:00Z" },
        { id: PROJECT_B, short_id: PROJECT_B, user_id: "u1", name: "Borealis", target_workspace_id: WORKSPACE_ID, worker_slots: 1, tags: [], status: "active", created_at: "2026-10-02T00:00:00Z", updated_at: "2026-10-02T00:00:00Z" },
      ],
    })));
  await page.route(`${API}/me/projects/*/tasks*`, (r: Route) => {
    const url = new URL(r.request().url());
    const pid = url.pathname.split("/").pop() === "tasks" ? url.pathname.split("/")[url.pathname.split("/").indexOf("projects") + 1] : "";
    void r.fulfill(j({ tasks: state.tasks.filter((t) => t.project_id === pid) }));
  });
  await page.route(`${API}/me/tasks/*/move-stage`, async (r: Route) => {
    const shortId = r.request().url().split("/tasks/")[1]?.split("/")[0];
    const body = r.request().postDataJSON() as { toStage: string; evidenceUrl?: string };
    const t = state.tasks.find((x) => x.short_id === shortId);
    if (!t) {
      void r.fulfill(j({ error: `board: task "${shortId}" not found` }, 404));
      return;
    }
    // The Done guard, honestly: criteria unmet or no evidence → 409.
    if (body.toStage === "done" && (body.evidenceUrl === "" || t.acceptance_criteria.some((c) => !c.met))) {
      void r.fulfill(j({ error: "board: guard done_guard: all acceptance criteria must be met before done (ruling R2)", code: "done_guard" }, 409));
      return;
    }
    t.stage = body.toStage;
    void r.fulfill(j(t));
  });

  await page.route(`${API}/me/tasks/*/cancel`, async (r: Route) => {
    const shortId = r.request().url().split("/tasks/")[1]?.split("/")[0];
    const t = state.tasks.find((x) => x.short_id === shortId);
    if (t) t.stage = "cancelled";
    void r.fulfill(j(t ?? { error: "not found" }, t ? 200 : 404));
  });
  await page.route(`${API}/me/tasks/*/start`, async (r: Route) => {
    const shortId = r.request().url().split("/tasks/")[1]?.split("/")[0];
    const t = state.tasks.find((x) => x.short_id === shortId);
    if (!t) {
      void r.fulfill(j({ error: `board: task "${shortId}" not found` }, 404));
      return;
    }
    t.stage = "in_progress";
    t.attempts += 1;
    void r.fulfill(j({
      task_id: t.id, short_id: t.short_id, project_id: t.project_id, epic_id: t.epic_id,
      workspace_id: WORKSPACE_ID, claim_token: "tok-e2e", lease_expires_at: "2026-10-18T23:30:00Z",
      escalated: false,
    }));
  });

  await page.route(`${API}/me/tasks/*/dependencies`, (r: Route) => {
    void r.fulfill(j({
      error: "board: dependency cycle: tsk-aaaa0001 → tsk-cccc0003 → tsk-aaaa0001 (did you mean: [tsk-cccc0003]?)",
      code: "cycle",
      suggestions: ["tsk-cccc0003"],
    }, 409));
  });

  // Platform stream — empty body then reconnects are fine.
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, (r: Route) =>
    r.fulfill({ status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" }, body: "" }));
}

async function openBoard(page: Page) {
  const state = freshState();
  await installSSEEmitters(page);
  await setupBoardMocks(page, state);
  await page.goto("/board");
  await expect(page.getByTestId(`swimlane-${PROJECT_A}`)).toBeVisible();
  return state;
}

// --- rows -------------------------------------------------------------------------

test.describe("Task Board UI (#1702)", () => {
  test("renders swimlanes per project, stage columns, and cards (§4.8)", async ({ page }) => {
    await openBoard(page);

    await expect(page.getByTestId(`swimlane-${PROJECT_B}`)).toBeVisible();
    const card = page.getByTestId("task-card-tsk-aaaa0001");
    await expect(card).toBeVisible();
    await expect(card).toContainText("tsk-aaaa0001");
    await expect(card).toContainText("Write charter");

    // Stage columns in the swimlane grid.
    const laneA = page.getByTestId(`swimlane-${PROJECT_A}`);
    await expect(laneA.locator('[data-stage-drop="backlog"]')).toBeVisible();
    await expect(laneA.locator('[data-stage-drop="in_progress"]')).toBeVisible();
    await expect(laneA.locator('[data-stage-drop="in_review"]')).toBeVisible();
    await expect(laneA.locator('[data-stage-drop="done"]')).toBeVisible();

    // Blocked badge (structural, ruling R3) + the OR4 anchor chip.
    await expect(page.getByTestId("blocked-badge-tsk-eeee0005")).toContainText("1");
    await expect(page.getByTestId("task-card-tsk-eeee0005").locator("[data-pr-anchor]")).toHaveText("pr:1702");

    // Slots in the swimlane header (1 in_progress of 2).
    await expect(page.getByTestId(`slots-${PROJECT_A}`)).toContainText("1/2");
  });

  test("drag moves a card via move-stage (optimistic + confirmed)", async ({ page }) => {
    const state = await openBoard(page);
    expect(state.tasks.find((t) => t.short_id === "tsk-cccc0003")!.stage).toBe("in_review");

    await page.dragAndDrop(
      '[data-testid="task-card-tsk-cccc0003"]',
      `[data-swimlane="${PROJECT_A}"] [data-stage-drop="in_progress"]`,
    );

    // Authoritative state after the invalidation refetch.
    await expect(page.getByTestId("task-card-tsk-cccc0003")).toHaveAttribute("data-stage", "in_progress", { timeout: 8000 });
  });

  test("refused drag (409 done_guard) snaps back and renders the guard code", async ({ page }) => {
    const state = await openBoard(page);
    // tsk-bbbb0002 has an unmet criterion: the Done guard must refuse.
    const claimed = state.tasks.find((t) => t.short_id === "tsk-bbbb0002")!;
    expect(claimed.stage).toBe("in_progress");
    expect(claimed.acceptance_criteria.some((c) => !c.met)).toBe(true);

    // Make it reviewable first (in_progress → in_review is token-guarded
    // for the manager surface; move the row server-side to in_review to
    // reach the done-guarded drag arm deterministically).
    claimed.stage = "in_review";
    await page.reload();
    await expect(page.getByTestId("task-card-tsk-bbbb0002")).toHaveAttribute("data-stage", "in_review");

    await page.dragAndDrop(
      '[data-testid="task-card-tsk-bbbb0002"]',
      `[data-swimlane="${PROJECT_A}"] [data-stage-drop="done"]`,
    );

    // Evidence prompt: fill it so the guard's criteria leg is what fires.
    const evidence = page.getByTestId("evidence-url");
    await expect(evidence).toBeVisible();
    // The prompt previews the unmet criterion without vetoing the call.
    await expect(page.getByTestId("done-readiness-view")).toContainText("tests green");
    await evidence.fill("https://github.com/lenaxia/LLMSafeSpaces/pull/1728");
    await page.getByTestId("confirm-done").click();

    // The engine's honest 409: code rendered, card snapped back.
    const notice = page.locator("[data-board-error]");
    await expect(notice).toHaveAttribute("data-board-error", "done_guard", { timeout: 8000 });
    await expect(notice).toContainText("done_guard");
    await expect(page.getByTestId("task-card-tsk-bbbb0002")).toHaveAttribute("data-stage", "in_review");
  });

  test("a board.event on the user stream moves the card (SSE live update)", async ({ page }) => {
    const state = await openBoard(page);
    await expect(page.getByTestId("task-card-tsk-cccc0003")).toHaveAttribute("data-stage", "in_review");

    // The engine moved the row (worker's token-guarded write); the event
    // streams and the read model refetches.
    state.tasks.find((t) => t.short_id === "tsk-cccc0003")!.stage = "done";
    await page.evaluate(() => {
      const emit = (window as unknown as { __user: (f: string) => void }).__user;
      emit(`data: ${JSON.stringify({
        type: "board.event",
        project_id: "prj-aaaa1111",
        id: "evt-e2e-1",
        item_kind: "task",
        item_id: "uuid-tsk-cccc0003",
        action: "move_stage",
        payload: { to_stage: "done" },
      })}\n\n`);
    });

    await expect(page.getByTestId("task-card-tsk-cccc0003")).toHaveAttribute("data-stage", "done", { timeout: 8000 });
  });

  test("scope switcher: project focus, epic chips, search did-you-mean", async ({ page }) => {
    await openBoard(page);

    // Project focus.
    await page.getByTestId("project-focus").selectOption({ label: "Borealis (prj-bbbb2222)" });
    await expect(page.getByTestId(`swimlane-${PROJECT_B}`)).toBeVisible();
    await expect(page.getByTestId(`swimlane-${PROJECT_A}`)).toHaveCount(0);

    // Back to all; epic chip filters.
    await page.getByTestId("project-focus").selectOption({ value: "all" });
    await expect(page.getByTestId(`swimlane-${PROJECT_A}`)).toBeVisible();
    await page.getByText("E2", { exact: true }).click();
    await expect(page.getByTestId("task-card-tsk-eeee0005")).toBeVisible();
    await expect(page.getByTestId("task-card-tsk-aaaa0001")).toHaveCount(0);
    await page.getByTestId("epic-chip-all").click();
    await expect(page.getByTestId("task-card-tsk-aaaa0001")).toBeVisible();

    // Search near-miss → did-you-mean suggestion (0066 shape).
    await page.getByTestId("board-search").fill("tsk-aaaa0009");
    const miss = page.getByTestId("board-search-miss");
    await expect(miss).toContainText("No match");
    await expect(miss).toContainText("tsk-aaaa0001");
    await miss.getByRole("button", { name: "tsk-aaaa0001" }).click();
    await expect(page.getByTestId("task-card-tsk-aaaa0001")).toBeVisible();
  });

  test("OR4 pr:N validation on the task editor", async ({ page }) => {
    await openBoard(page);

    await page.getByTestId("task-card-tsk-aaaa0001").click();
    const tags = page.locator("#task-tags");
    await tags.click();
    await tags.fill("pr:0");
    await tags.press("Enter");

    await expect(page.getByTestId("tag-validation-errors")).toContainText("pr:0");
    await expect(page.getByTestId("task-save")).toBeDisabled();
  });

  test("cancel arm: terminal write lands the card in Cancelled (design §4.8 Lane-4 e2e arm)", async ({ page }) => {
    const state = await openBoard(page);
    expect(state.tasks.find((t) => t.short_id === "tsk-cccc0003")!.stage).toBe("in_review");

    await page.getByTestId("task-card-tsk-cccc0003").click();
    await page.getByTestId("cancel-task").click();

    // The terminal write + invalidation: the dialog closes and the card
    // appears in the collapsible Cancelled side column.
    await expect(page.getByTestId("cancel-task")).toHaveCount(0, { timeout: 8000 });
    await page.getByTestId("side-columns-toggle").first().click();
    await expect(
      page.locator('[data-swimlane="prj-aaaa1111"] [data-stage-drop="cancelled"]').getByTestId("task-card-tsk-cccc0003"),
    ).toBeVisible({ timeout: 8000 });
  });

  test("reclaim arm: a streamed reclaim event converges the row to Backlog (lease lapse)", async ({ page }) => {
    const state = await openBoard(page);
    await expect(page.getByTestId("task-card-tsk-bbbb0002")).toHaveAttribute("data-stage", "in_progress");

    // RC1 reclaimed the lapsed lease server-side; the board.event streams.
    state.tasks.find((t) => t.short_id === "tsk-bbbb0002")!.stage = "backlog";
    await page.evaluate(() => {
      const emit = (window as unknown as { __user: (f: string) => void }).__user;
      emit(`data: ${JSON.stringify({
        type: "board.event",
        project_id: "prj-aaaa1111",
        id: "evt-e2e-reclaim",
        item_kind: "task",
        item_id: "uuid-tsk-bbbb0002",
        action: "reclaim",
        payload: { to_stage: "backlog" },
      })}\n\n`);
    });

    await expect(page.getByTestId("task-card-tsk-bbbb0002")).toHaveAttribute("data-stage", "backlog", { timeout: 8000 });
  });

  test("start arm: task_start dispatches a backlog card and renders the claim notice", async ({ page }) => {
    const state = await openBoard(page);
    expect(state.tasks.find((t) => t.short_id === "tsk-aaaa0001")!.stage).toBe("backlog");

    await page.getByTestId("start-tsk-aaaa0001").click();

    await expect(page.getByTestId("board-notice")).toContainText("fresh worker dispatched", { timeout: 8000 });
    await expect(page.getByTestId("task-card-tsk-aaaa0001")).toHaveAttribute("data-stage", "in_progress", { timeout: 8000 });
  });

  test("refusal arm: cancel 409 keeps the dialog open with the guard code rendered", async ({ page }) => {
    const state = freshState();
    await installSSEEmitters(page);
    await setupBoardMocks(page, state);
    // Override cancel to refuse (already-terminal race, engine class).
    await page.route(`${API}/me/tasks/*/cancel`, (r: Route) =>
      r.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify({ error: "board: guard stage_transition: already terminal", code: "stage_transition" }) }));
    await page.goto("/board");
    await expect(page.getByTestId(`swimlane-${PROJECT_A}`)).toBeVisible();

    await page.getByTestId("task-card-tsk-cccc0003").click();
    await page.getByTestId("cancel-task").click();

    const notice = page.locator("[data-board-error]");
    await expect(notice).toHaveAttribute("data-board-error", "stage_transition", { timeout: 8000 });
    // The dialog STAYS open (the fields survive for a retry).
    await expect(page.getByTestId("cancel-task")).toBeVisible();
    await expect(page.getByTestId("task-title")).toBeVisible();
  });

  test("refusal arm: dependency cycle 409 renders suggestions and the chip fills the input", async ({ page }) => {
    const state = freshState();
    await installSSEEmitters(page);
    await setupBoardMocks(page, state);
    await page.goto("/board");
    await expect(page.getByTestId(`swimlane-${PROJECT_A}`)).toBeVisible();

    await page.getByTestId("task-card-tsk-cccc0003").click();
    const dep = page.getByTestId("dep-input");
    await dep.fill("tsk-aaaa0001");
    await page.getByTestId("add-dependency").click();

    const notice = page.locator("[data-board-error]");
    await expect(notice).toHaveAttribute("data-board-error", "cycle", { timeout: 8000 });
    // The typed id is preserved and the suggestion chip fills the input.
    await expect(dep).toHaveValue("tsk-aaaa0001");
    const chip = page.getByTestId("board-error-suggestions").locator("button");
    await expect(chip).toHaveText("tsk-cccc0003");
    await chip.click();
    await expect(dep).toHaveValue("tsk-cccc0003");
  });

  test("refusal arm: task_start refusal renders at page level with the guard code", async ({ page }) => {
    const state = freshState();
    await installSSEEmitters(page);
    await setupBoardMocks(page, state);
    await page.route(`${API}/me/tasks/*/start`, (r: Route) =>
      r.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify({ error: "board: guard epic_state: paused epics refuse task_start", code: "epic_state" }) }));
    await page.goto("/board");
    await expect(page.getByTestId(`swimlane-${PROJECT_A}`)).toBeVisible();

    await page.getByTestId("start-tsk-aaaa0001").click();

    const notice = page.locator("[data-board-error]");
    await expect(notice).toHaveAttribute("data-board-error", "epic_state", { timeout: 8000 });
    await expect(notice).toContainText("epic_state");
  });
});
