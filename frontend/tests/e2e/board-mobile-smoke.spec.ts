/**
 * Phone-viewport smoke for the .15 board UI surfaces (owner direction
 * 2026-11-17: "thin but broad").
 *
 * The rail itself has the trusted-touch wall (board-rail-touch.spec.ts)
 * and the layout wall (board.spec.ts + board-rail-drain.spec.ts). The
 * OTHER .15 phone surfaces — the task-card kebab actions, the done cap
 + show-all drawer, the project kebab, the settings pages — were pinned
 * only at desktop viewports or the unit tier. A phone-class regression
 * (the kebab opening off-screen, the drawer unscrollable, a settings
 * surface overflowing the viewport) would pass every existing pin.
 *
 * Thin by design: each row asserts the surface OPENS and its primary
 * action FIRES on the 375px viewport — depth stays with the per-feature
 * suites. Mocked backend, self-contained (the house spec convention).
 */
import { test, expect, type Page, type Route } from "@playwright/test";

const WORKSPACE_ID = "ws-board-mobile-e2e";
const API = "**/api/v1";
const PROJECT_A = "prj-aaaa1111";

type SmokeTask = {
  id: string;
  project_id: string;
  epic_id: string;
  short_id: string;
  title: string;
  description: string;
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

function smokeTask(shortId: string, title: string, stage: string, overrides: Partial<SmokeTask> = {}): SmokeTask {
  return {
    id: `uuid-${shortId}`,
    project_id: PROJECT_A,
    epic_id: "epic-1",
    short_id: shortId,
    title,
    description: "",
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
    ...overrides,
  };
}

/** The visible-card fixture: backlog carries two cards so the FIRST
 * rail view (the trio) has a kebab to fire without scrolling. */
function smokeTasks(): SmokeTask[] {
  return [
    smokeTask("tsk-mob0001", "Kebab target", "backlog"),
    smokeTask("tsk-mob0002", "Second row", "backlog"),
    smokeTask("tsk-mob0003", "In flight", "in_progress"),
  ];
}

async function openMobileBoard(page: Page, tasks: SmokeTask[]) {
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
      items: [{ id: WORKSPACE_ID, name: "Mobile E2E", userId: "u1", runtime: "python", storageSize: "1Gi", phase: "Active" }],
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

test.describe("phone-viewport smoke — .15 board UI surfaces (375px)", () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize({ width: 375, height: 667 });
  });

  test("task kebab opens on-screen and its action fires the write", async ({ page }) => {
    await openMobileBoard(page, smokeTasks());

    const kebab = page.getByTestId("task-kebab-tsk-mob0001");
    await expect(kebab).toBeVisible();
    await kebab.click();

    // The menu opens within the viewport (the sidebar-kebab-viewport
    // discipline applied to the board card) and offers the backlog
    // matrix's Pause arm.
    const menu = page.getByRole("menu");
    await expect(menu).toBeVisible();
    const pauseItem = menu.getByRole("menuitem", { name: /Pause story/i });
    await expect(pauseItem).toBeVisible();
    await expect(pauseItem).toBeInViewport();

    // The action FIRES: POST /me/tasks/:id/pause leaves the page.
    const pauseP = page.waitForRequest(
      (r) => r.method() === "POST" && /\/me\/tasks\/tsk-mob0001\/pause$/.test(r.url()),
    );
    await pauseItem.click();
    await pauseP;
    await expect(menu).toHaveCount(0);
  });

  test("done cap + show-all drawer: reachable from the done page on the phone, opens, closes", async ({ page }) => {
    // 13 done rows: the inline cap holds 10, the affordance carries the
    // full count, and the drawer opens FROM THE DONE PAGE (the phone
    // user must be able to reach done at all — scroll/keyboard parity
    // is the rail wall's job; this row pins the drawer's own surface).
    const doneRows = Array.from({ length: 13 }, (_, i) =>
      smokeTask(`tsk-mdone${String(i + 1).padStart(4, "0")}`, `Done ${i + 1}`, "done", {
        stage_entered_at: `2026-10-${String(i + 1).padStart(2, "0")}T00:00:00Z`,
        updated_at: `2026-10-${String(i + 1).padStart(2, "0")}T00:00:00Z`,
      }),
    );
    const tasks = [...smokeTasks().filter((t) => t.stage !== "done"), ...doneRows];
    await openMobileBoard(page, tasks);

    // Reach the done page by deep-scrolling the rail and letting
    // snap-mandatory settle — the exact idiom board.spec.ts's snap test
    // trusts in CI (navigation parity itself is the rail wall's job:
    // board-rail-touch.spec.ts + board.spec.ts; this row pins the
    // drawer's own surface at the phone class).
    const rail = page.getByTestId(`stage-grid-${PROJECT_A}`);
    await rail.evaluate((el) => el.scrollTo({ left: el.scrollWidth }));

    const affordance = page.getByTestId(`done-show-all-${PROJECT_A}`);
    await expect(affordance).toBeVisible();
    await expect(affordance).toContainText("Show all 13 done");
    await expect(affordance).toBeInViewport();
    await affordance.click();

    const drawer = page.getByTestId("done-drawer-list");
    await expect(drawer).toBeVisible();
    await expect(drawer.locator("[data-task-card]")).toHaveCount(13);
    // The drawer's close affordance is reachable on the phone.
    const close = page.getByTestId("done-drawer-close");
    await expect(close).toBeInViewport();
    await close.click();
    await expect(page.getByTestId("done-drawer-list")).toHaveCount(0);
  });

  test("project kebab opens on the phone and its lifecycle action fires", async ({ page }) => {
    await openMobileBoard(page, smokeTasks());

    const kebab = page.getByTestId(`project-kebab-${PROJECT_A}`);
    await expect(kebab).toBeVisible();
    await kebab.click();

    const menu = page.getByRole("menu");
    await expect(menu).toBeVisible();
    const archive = menu.getByRole("menuitem", { name: /archive/i }).first();
    await expect(archive).toBeInViewport();

    const archiveP = page.waitForRequest(
      (r) => r.method() === "POST" && /\/me\/projects\/prj-aaaa1111\/archive$/.test(r.url()),
    );
    await archive.click();
    await archiveP;
  });
});

test.describe("phone-viewport smoke — settings surfaces (375px)", () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize({ width: 375, height: 667 });
    const j = (body: unknown) => ({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(body),
    });
    await page.route(`${API}/auth/me`, (r: Route) =>
      r.fulfill(j({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true })));
    await page.route(`${API}/auth/config`, (r: Route) =>
      r.fulfill(j({ registrationEnabled: false, oidcEnabled: false })));
    // The canonical settings mock set (settings.spec.ts shapes): the
    // schema endpoint serves an ARRAY of setting descriptors — a
    // hand-rolled {properties:{}} shape renders a malformed form that
    // overflows the phone viewport (the r2 CI finding: mock bug, not a
    // main bug).
    await page.route(`${API}/users/me/settings`, (r: Route) =>
      r.fulfill(j({ settings: {}, schemaVersion: 1 })));
    await page.route(`${API}/users/me/settings/schema`, (r: Route) =>
      r.fulfill(j({ settings: [], schemaVersion: 1 })));
    await page.route(`${API}/provider-credentials`, (r: Route) => r.fulfill(j([])));
    await page.route(`${API}/orgs`, (r: Route) => r.fulfill(j([])));
    await page.route(`${API}/secrets`, (r: Route) => r.fulfill(j([])));
    await page.route(`${API}/auth/api-keys`, (r: Route) => r.fulfill(j([])));
  });

  /** No horizontal overflow: the page's content fits the phone width
   * (the #1629 class — a surface wider than the viewport is unusable
   * and unscrollable under overflow-hidden shells). */
  async function expectNoHorizontalOverflow(page: Page) {
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow, "the surface must not overflow the 375px viewport horizontally").toBeLessThanOrEqual(1);
  }

  for (const path of ["/settings/preferences", "/settings/secrets", "/settings/api-keys"]) {
    test(`${path} renders and fits the phone viewport`, async ({ page }) => {
      await page.goto(path);
      await expect(page).toHaveURL(new RegExp(path.replace("/", "\\/")));
      // The surface rendered real chrome (a heading or form control),
      // not a blank shell.
      await expect(page.locator("main").first()).toBeVisible();
      await expectNoHorizontalOverflow(page);
    });
  }
});
