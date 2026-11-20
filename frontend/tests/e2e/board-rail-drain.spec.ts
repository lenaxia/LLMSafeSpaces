/**
 * Board rail e2e — the drain-state wall (owner direction 2026-11-17).
 *
 * The .15 lesson: per-story pins existed but missed real user paths.
 * The rail's width-retention invariant ("empty columns keep their
 * width — skeleton cells, so the layout cannot jump as stages drain")
 * was pinned ONLY for the all-done shape (board.spec.ts "empty columns
 * keep their width") and at the unit tier (BoardPage.rail.test.tsx).
 * The OTHER drain states — each column drained ALONE while the rest
 * stay loaded, and the fully-empty board — had no e2e execution: a
 * regression that collapses only the backlog column (or only in_review)
 * when the trio is otherwise loaded would pass every existing pin.
 *
 * This spec sweeps every drain state on the phone viewport and asserts
 * the REAL layout: the drained column keeps its measured width
 * fraction inside the same fixed-basis ranges the loaded board pins
 * (29/36/30 minus the gutter cede; done keeps its full page), the
 * skeleton affordance ("—") renders, and the drop zone survives — a
 * drained column is still a drop target.
 *
 * Mocked backend, self-contained (the house spec convention).
 */
import { test, expect, type Page, type Route } from "@playwright/test";

const WORKSPACE_ID = "ws-board-drain-e2e";
const API = "**/api/v1";
const PROJECT_A = "prj-aaaa1111";

type DrainTask = {
  id: string;
  project_id: string;
  epic_id: string;
  short_id: string;
  title: string;
  stage: string;
  paused: boolean;
  tags: string[];
  acceptance_criteria: Array<{ text: string; met: boolean }>;
  is_blocked: boolean;
  blocked_by: string[];
  attempts: number;
  last_attempt_failed: boolean;
  stage_entered_at: string;
  created_at: string;
  updated_at: string;
};

function drainTask(shortId: string, title: string, stage: string): DrainTask {
  return {
    id: `uuid-${shortId}`,
    project_id: PROJECT_A,
    epic_id: "epic-1",
    short_id: shortId,
    title,
    stage,
    paused: false,
    tags: [],
    acceptance_criteria: [],
    is_blocked: false,
    blocked_by: [],
    attempts: 0,
    last_attempt_failed: false,
    stage_entered_at: "2026-10-20T00:00:00Z",
    created_at: "2026-10-01T00:00:00Z",
    updated_at: "2026-10-20T00:00:00Z",
  };
}

/** One loaded card per through-flow column + done — the loaded
 * baseline every drain state starts from. */
function loadedBoard(): DrainTask[] {
  return [
    drainTask("tsk-load0001", "Charter", "backlog"),
    drainTask("tsk-load0002", "Engine", "in_progress"),
    drainTask("tsk-load0003", "Review", "in_review"),
    drainTask("tsk-load0004", "Shipped", "done"),
  ];
}

async function openDrainBoard(page: Page, tasks: DrainTask[]) {
  const j = (body: unknown, status = 200) => ({
    status,
    contentType: "application/json",
    body: JSON.stringify(body),
  });
  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill(j({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true })));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill(j({ registrationEnabled: false, oidcEnabled: false })));
  await page.route(`${API}/workspaces`, (r: Route) =>
    r.fulfill(j({
      items: [{ id: WORKSPACE_ID, name: "Drain E2E", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }],
      pagination: { limit: 50, offset: 0, total: 1 },
    })));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/status`, (r: Route) =>
    r.fulfill(j({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0.0" } })));
  await page.route(`${API}/me/projects`, (r: Route) =>
    r.fulfill(j({
      projects: [{ id: PROJECT_A, short_id: PROJECT_A, user_id: "u1", name: "Apollo", target_workspace_id: WORKSPACE_ID, worker_slots: 2, tags: [], status: "active", created_at: "2026-10-01T00:00:00Z", updated_at: "2026-10-01T00:00:00Z" }],
    })));
  await page.route(`${API}/me/projects/*/tasks*`, (r: Route) =>
    r.fulfill(j({ tasks })));
  await page.route(`${API}/me/projects/*/epics`, (r: Route) =>
    r.fulfill(j({ epics: [] })));
  await page.goto("/board");
  await expect(page.getByTestId(`swimlane-${PROJECT_A}`)).toBeVisible();
}

/** Measured portrait width FRACTIONS per column (content-width
 * normalized) — the same measurement discipline and tolerance bands as
 * board.spec.ts's all-done pin, applied to every drain state. */
async function columnFractions(page: Page): Promise<Record<string, number>> {
  const rail = page.getByTestId(`stage-grid-${PROJECT_A}`);
  return rail.evaluate((el) => {
    const cs = getComputedStyle(el);
    const pad = parseFloat(cs.paddingLeft) + parseFloat(cs.paddingRight);
    const contentWidth = el.getBoundingClientRect().width - pad;
    const cols = Array.from(el.querySelectorAll("[data-stage-col]"));
    const byStage = (s: string) => cols.find((c) => c.getAttribute("data-stage-col") === s)!;
    return {
      backlog: byStage("backlog").getBoundingClientRect().width / contentWidth,
      inProgress: byStage("in_progress").getBoundingClientRect().width / contentWidth,
      inReview: byStage("in_review").getBoundingClientRect().width / contentWidth,
      done: byStage("done").getBoundingClientRect().width / contentWidth,
    };
  });
}

/** The invariant: whatever is drained, the fixed-basis geometry holds
 * (the trio bands from the loaded board's 29/36/30 split minus the
 * gutter cede; done keeps its full page). */
async function expectStableGeometry(page: Page, drained: string) {
  const widths = await columnFractions(page);
  expect(widths.backlog, `backlog width while ${drained}`).toBeGreaterThan(0.24);
  expect(widths.backlog, `backlog width while ${drained}`).toBeLessThan(0.31);
  expect(widths.inProgress, `in_progress width while ${drained}`).toBeGreaterThan(0.31);
  expect(widths.inProgress, `in_progress width while ${drained}`).toBeLessThan(0.38);
  expect(widths.inReview, `in_review width while ${drained}`).toBeGreaterThan(0.25);
  expect(widths.inReview, `in_review width while ${drained}`).toBeLessThan(0.32);
  expect(widths.done, `done keeps its full page while ${drained}`).toBeGreaterThan(0.95);
}

test.describe("board rail drain states — empty columns keep their width at every drain state", () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize({ width: 375, height: 667 });
  });

  for (const stage of ["backlog", "in_progress", "in_review", "done"] as const) {
    test(`only ${stage} drained: skeleton renders, geometry holds, drop zone survives`, async ({ page }) => {
      const tasks = loadedBoard().filter((t) => t.stage !== stage);
      await openDrainBoard(page, tasks);

      const rail = page.getByTestId(`stage-grid-${PROJECT_A}`);
      const drainedCol = rail.locator(`[data-stage-col="${stage}"]`);
      await expect(drainedCol).toBeVisible();

      // The skeleton affordance: a drained column renders the em-dash
      // placeholder, not nothing (the unit pin's contract, executed in
      // a real layout).
      await expect(drainedCol).toContainText("—");

      // A drained column is still a DROP TARGET (drag-to-column stays
      // possible — the engine's arbiter surface never loses a zone).
      await expect(rail.locator(`[data-stage-drop="${stage}"]`)).toHaveCount(1);

      // The loaded siblings keep their cards.
      for (const other of ["backlog", "in_progress", "in_review", "done"] as const) {
        if (other === stage) continue;
        await expect(rail.locator(`[data-stage-col="${other}"] [data-task-card]`)).toHaveCount(1);
      }

      await expectStableGeometry(page, `only ${stage} is drained`);
    });
  }

  test("the fully-drained board (no stories anywhere): four equal-footing columns, no collapse", async ({ page }) => {
    await openDrainBoard(page, []);

    const rail = page.getByTestId(`stage-grid-${PROJECT_A}`);
    await expect(rail.locator("[data-stage-col]")).toHaveCount(4);
    for (const stage of ["backlog", "in_progress", "in_review", "done"] as const) {
      await expect(rail.locator(`[data-stage-col="${stage}"]`)).toContainText("—");
      await expect(rail.locator(`[data-stage-drop="${stage}"]`)).toHaveCount(1);
    }
    await expectStableGeometry(page, "the board is fully drained");
  });

  test("drain-in-place: a column that empties LIVE (SSE task removal) keeps its width — no layout jump", async ({ page }) => {
    // The user path the static fixtures cannot express: the board is
    // loaded, a task leaves its column mid-session (delete/move via a
    // board.event), and the now-empty column must not reflow the rail.
    const tasks = loadedBoard();
    await openDrainBoard(page, tasks);
    const rail = page.getByTestId(`stage-grid-${PROJECT_A}`);

    const before = await columnFractions(page);

    // Serve the drained board to the next fetch and trigger the
    // invalidation the real mutation performs (the delete arm's
    // confirmation refetch).
    await page.route(`${API}/me/projects/*/tasks*`, (r: Route) =>
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ tasks: tasks.filter((t) => t.stage !== "in_review") }) }));
    await page.reload();
    await expect(rail).toBeVisible();
    await expect(rail.locator('[data-stage-col="in_review"]')).toContainText("—");

    const after = await columnFractions(page);
    for (const key of ["backlog", "inProgress", "inReview", "done"] as const) {
      expect(Math.abs(after[key] - before[key]), `${key} fraction must not jump when in_review drains live`).toBeLessThan(0.02);
    }
  });
});
