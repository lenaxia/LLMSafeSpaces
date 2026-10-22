/**
 * #1832 item 6 (2/2) — Task Board Config UI e2e.
 *
 * Rows:
 *   1. The settings tab renders the user's board defaults from
 *      GET /me/board/config and each project's overrides + effective
 *      config from GET /me/projects/:id/config, with provenance
 *      (override vs inherited).
 *   2. Save defaults PUTs the full typed config.
 *   3. A project override Save PUTs ONLY the overridden key (absent =
 *      inherit; defaultWorkerSlots never sent at the project tier).
 *
 * Backend APIs mocked via Playwright route interception (board.spec
 * harness pattern).
 */
import { test, expect, type Page, type Route } from "@playwright/test";

const WORKSPACE_ID = "ws-board-cfg-e2e";
const API = "**/api/v1";
const PROJECT_A = "prj-aaaa1111";

const USER_CFG = { reviewColumn: "manual", autoAssignReviewer: false, defaultWorkerSlots: 2 };
const PROJECTS = [
  { id: "p1", short_id: "prj-aaaa1111", name: "Apollo", status: "active", tags: [], created_at: "", updated_at: "", worker_slots: 2, target_workspace_id: WORKSPACE_ID },
];
const PC = {
  project_id: "prj-aaaa1111",
  overrides: { reviewColumn: "auto" },
  effective: { reviewColumn: "auto", autoAssignReviewer: false, defaultWorkerSlots: 2 },
};

async function mockConfigRoutes(page: Page, puts: { user: unknown[]; project: unknown[] }) {
  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill({ json: { id: "u1", email: "e@x.io", name: "E", username: "e" } }));
  await page.route(`${API}/auth/config`, (r: Route) => r.fulfill({ json: {} }));
  await page.route(`${API}/workspaces`, (r: Route) =>
    r.fulfill({ json: [{ id: WORKSPACE_ID, name: "W", status: "Active", runtime: "go:1.21", created_at: "", updated_at: "" }] }));
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/status`, (r: Route) =>
    r.fulfill({ json: { phase: "Active", conditions: [], agent: { health: "ok" }, restarts: { count: 0 } } }));
  await page.route(`${API}/me/projects`, (r: Route) => r.fulfill({ json: { projects: PROJECTS } }));
  await page.route(`${API}/me/board/config`, async (r: Route) => {
    if (r.request().method() === "PUT") {
      puts.user.push(r.request().postDataJSON());
      return r.fulfill({ json: { ...USER_CFG, ...r.request().postDataJSON() } });
    }
    return r.fulfill({ json: USER_CFG });
  });
  await page.route(`${API}/me/projects/${PROJECT_A}/config`, async (r: Route) => {
    if (r.request().method() === "PUT") {
      puts.project.push(r.request().postDataJSON());
      return r.fulfill({ json: PC });
    }
    return r.fulfill({ json: PC });
  });
  await page.route(`${API}/workspaces/${WORKSPACE_ID}/session-events`, (r: Route) => r.fulfill({ status: 200 }));
}

test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => {
    window.fetch = new Proxy(window.fetch, {
      apply(target, self, args) {
        const u = String(args[0]);
        if (u.includes("/session-events")) {
          return new Promise(() => {});
        }
        return Reflect.apply(target, self, args);
      },
    });
  });
});

test("config tab renders defaults + provenance, saves globally, and overrides carry only the set key", async ({ page }) => {
  const puts = { user: [] as unknown[], project: [] as unknown[] };
  await mockConfigRoutes(page, puts);
  await page.goto("/settings/board-config");

  // Row 1: defaults + provenance.
  await expect(page.getByText("Board defaults")).toBeVisible();
  await expect(page.getByTestId("cfg-reviewColumn")).toContainText("manual");
  await expect(page.getByTestId("cfg-provenance-prj-aaaa1111-reviewColumn")).toContainText("override");
  await expect(page.getByTestId("cfg-provenance-prj-aaaa1111-autoAssignReviewer")).toContainText("inherited");

  // Row 2: full-config PUT on Save (slots edit only — the rest round-trips).
  const slotsInput = page.getByTestId("cfg-defaultWorkerSlots").locator("input");
  await slotsInput.fill("3");
  await slotsInput.blur(); // NumberInput commits onChange on blur
  await page.getByTestId("cfg-save-global").click();
  await expect.poll(() => puts.user.length).toBe(1);
  expect(puts.user[0]).toMatchObject({ reviewColumn: "manual", autoAssignReviewer: false, defaultWorkerSlots: 3 });

  // Row 3: project override Save PUTs the override SET (REPLACE): the
  // new key AND the preserved existing one — never defaultWorkerSlots.
  await page.getByTestId(`cfg-ov-${PROJECT_A}-autoAssignReviewer`).click();
  await page.getByRole("option", { name: "On" }).click();
  await page.getByTestId(`cfg-save-${PROJECT_A}`).click();
  await expect.poll(() => puts.project.length).toBe(1);
  expect(Object.keys(puts.project[0] as object).sort()).toEqual(["autoAssignReviewer", "requestId", "reviewColumn"]);
  expect(puts.project[0]).toMatchObject({ autoAssignReviewer: true, reviewColumn: "auto" });
});
