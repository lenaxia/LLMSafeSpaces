import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

const API = "**/api/v1";

// Shared auth + settings stubs (mirrors settings.spec.ts).
async function mockAuthenticated(page: Page) {
  await page.route(`${API}/auth/me`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ id: "e2e-user", username: "e2e", email: "e2e@test.com", role: "member", active: true }),
    });
  });
  await page.route(`${API}/auth/config`, async (route: Route) => {
    await route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify({ registrationEnabled: true, oidcEnabled: false, instanceName: "E2E" }),
    });
  });
  await page.route(`${API}/users/me/settings`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ settings: {}, schemaVersion: 1 }) });
  });
  await page.route(`${API}/users/me/settings/schema`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ settings: [], schemaVersion: 1 }) });
  });
  for (const ep of ["/orgs", "/secrets", "/provider-credentials", "/auth/api-keys", "/events"]) {
    await page.route(`${API}${ep}`, async (route: Route) => {
      await route.fulfill({ status: 200, contentType: "application/json", body: "[]" });
    });
  }
}

// Catalog with the migration target present (refresh prefill only
// targets bases the catalog offers).
const CATALOG = {
  architectures: ["linux/amd64"],
  bases: [
    { name: "bookworm", version: "0.6.0", image: "img", tag: "0.6.0" },
    { name: "trixie", version: "0.1.0", image: "img-trixie", tag: "0.1.0", isDefault: true },
  ],
  extensions: [
    { id: "ffmpeg", type: "apt", value: "ffmpeg", supportedBases: ["bookworm", "trixie"] },
  ],
  knownFailures: [],
};

const STALE_CONFIG = {
  id: "cfg-1",
  hash: "s-stale",
  name: "ml-stack",
  selection: ["ffmpeg"],
  resolvedValues: {},
  baseName: "bookworm",
  baseVersion: "0.6.0",
  scope: "member",
  status: "ready",
  updatesAvailable: {
    kind: "base_migration",
    currentBaseName: "bookworm",
    currentBaseVersion: "0.6.0",
    latestBaseVersion: "0.6.0",
    defaultBaseName: "trixie",
    defaultBaseVersion: "0.1.0",
  },
};

async function mockFactory(page: Page, configs: unknown[], createStatus = 201, createBody?: unknown) {
  await page.route(`${API}/image-factory/catalog`, async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(CATALOG) });
  });
  await page.route(`${API}/image-factory/configs`, async (route: Route) => {
    if (route.request().method() === "GET") {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(configs) });
      return;
    }
    // POST — the create the refresh save issues.
    await route.fulfill({
      status: createStatus,
      contentType: "application/json",
      body: JSON.stringify(createBody ?? { error: "failed to save config" }),
    });
  });
}

test.describe("Workspace Images: base-update refresh flow (#928)", () => {
  test.beforeEach(async ({ page }) => {
    await mockAuthenticated(page);
  });

  test("happy: stale pill → refresh → prefill with de-conflicted name → save → new config, original intact", async ({ page }) => {
    const created = { id: "cfg-2", hash: "s-new", name: "ml-stack (trixie 0.1.0)", selection: ["ffmpeg"], resolvedValues: {}, baseName: "trixie", baseVersion: "0.1.0", scope: "member", status: "building" };
    await mockFactory(page, [STALE_CONFIG], 201, created);
    await page.goto("/settings/workspace-images");
    await page.waitForLoadState("networkidle");

    // Stale pill visible on the config row.
    await expect(page.getByText("new base: trixie")).toBeVisible({ timeout: 8000 });

    // Expand → Refresh button → prefill.
    await page.getByText("ml-stack").first().click();
    await page.getByRole("button", { name: /Refresh to trixie/i }).click();

    // Banner + de-conflicted name + base pre-targeted.
    await expect(page.getByText(/Refreshing “ml-stack”/i)).toBeVisible();
    await expect(page.getByPlaceholder("e.g. ml-stack")).toHaveValue("ml-stack (trixie 0.1.0)");

    // Save → toast names the update; the original stays in the list.
    await page.getByRole("button", { name: /Create Personal Image & Build/i }).click();
    await expect(page.getByText(/original is unchanged/i)).toBeVisible({ timeout: 8000 });
    await expect(page.getByText("ml-stack").first()).toBeVisible();
  });

  test("unhappy: save failure surfaces the API error and retains the prefill", async ({ page }) => {
    await mockFactory(page, [STALE_CONFIG], 500, { error: "failed to save config" });
    await page.goto("/settings/workspace-images");
    await page.waitForLoadState("networkidle");

    await page.getByText("ml-stack").first().click();
    await page.getByRole("button", { name: /Refresh to trixie/i }).click();
    await expect(page.getByText(/Refreshing “ml-stack”/i)).toBeVisible();

    await page.getByRole("button", { name: /Create Personal Image & Build/i }).click();
    // Error toast + prefill survives for retry.
    await expect(page.getByText("failed to save config")).toBeVisible({ timeout: 8000 });
    await expect(page.getByPlaceholder("e.g. ml-stack")).toHaveValue("ml-stack (trixie 0.1.0)");
    await expect(page.getByText(/Refreshing “ml-stack”/i)).toBeVisible();
  });

  test("unhappy: catalog without the target base → loud error, no prefill", async ({ page }) => {
    // Catalog WITHOUT trixie; stale config still points at a trixie move.
    const noTargetCatalog = { ...CATALOG, bases: [CATALOG.bases[0]] };
    await page.route(`${API}/image-factory/catalog`, async (route: Route) => {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(noTargetCatalog) });
    });
    await page.route(`${API}/image-factory/configs`, async (route: Route) => {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([STALE_CONFIG]) });
    });
    await page.goto("/settings/workspace-images");
    await page.waitForLoadState("networkidle");

    await page.getByText("ml-stack").first().click();
    await page.getByRole("button", { name: /Refresh to trixie/i }).click();
    await expect(page.getByText(/Update target base not found/i)).toBeVisible({ timeout: 8000 });
    await expect(page.getByText(/Refreshing/i)).toHaveCount(0);
  });
});
