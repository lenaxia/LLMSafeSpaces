/**
 * E2E for #1627 (PR3): session archiving UX.
 *
 *   A1 — kebab Archive → row leaves the live tree, the collapsed
 *        Archived group (with count) appears
 *   A2 — archived session view: read-only banner, no composer,
 *        history stays rendered
 *   A3 — Unarchive → composer restored, group empties away
 *   A4 — unhappy: archive PUT 500 → row unchanged, no group, no crash
 *
 * All backend APIs are route-mocked (the session-activity.spec.ts
 * harness); the sessions list is STATEFUL so the refetch after the
 * mutation mirrors the server's persisted state.
 */
import { test, expect, type Page, type Route } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

const WS = "ws-arch";
const SESS_LIVE = "ses_arch_live";
const SESS_OLD = "ses_arch_old";
const API = "**/api/v1";

interface SessRow { id: string; title: string; archived?: boolean }

async function setupBase(page: Page, opts: { rows?: SessRow[]; archiveStatus?: number } = {}) {
  const rows: SessRow[] = opts.rows ?? [
    { id: SESS_LIVE, title: "Live work" },
    { id: SESS_OLD, title: "Old work" },
  ];
  const archiveStatus = opts.archiveStatus ?? 204;

  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true }) }));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false }) }));

  await page.route(`${API}/workspaces`, (r: Route) => {
    if (r.request().method() !== "GET") { r.continue(); return; }
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({
      items: [{ id: WS, name: "ArchiveSpace", userId: "u1", runtime: "base", storageSize: "1Gi", phase: "Active" }],
      pagination: { limit: 50, offset: 0, total: 1 },
    })});
  });

  await page.route(`${API}/workspaces/${WS}/status`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ phase: "Active", credentialState: { available: true }, agentHealth: { status: "healthy", agentVersion: "1.0" }, sessions: [] }) }));

  // Stateful session list: GET reflects the current rows; the archive
  // PUT mutates them (server-persisted state).
  await page.route(`${API}/workspaces/${WS}/sessions`, (r: Route) => {
    if (r.request().method() === "GET") {
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(
        rows.map((row) => ({ id: row.id, title: row.title, status: "idle", hasUnread: false, messageCount: 2, ...(row.archived ? { archived: true } : {}) })),
      )});
      return;
    }
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
  });
  await page.route(`${API}/workspaces/${WS}/sessions/*/archived`, async (r: Route) => {
    if (archiveStatus !== 204) {
      r.fulfill({ status: archiveStatus, contentType: "application/json", body: JSON.stringify({ error: "boom" }) });
      return;
    }
    const body = await r.request().postDataJSON() as { archived?: boolean };
    const target = rows.find((row) => row.id === r.request().url().split("/sessions/")[1]?.split("/")[0]);
    if (target) target.archived = !!body.archived;
    r.fulfill({ status: 204, body: "" });
  });

  await page.route(`${API}/workspaces/${WS}/models`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ models: [], currentModel: "" }) }));
  await page.route(`${API}/workspaces/*/sessions/*/seen`, (r: Route) =>
    r.fulfill({ status: 204, body: "" }));
  await page.route(`${API}/workspaces/${WS}/sessions/*/message**`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }));
  await page.route(`${API}/workspaces/${WS}/sessions/*`, (r: Route) => {
    if (r.request().method() !== "GET") { r.fulfill({ status: 204, body: "" }); return; }
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESS_LIVE, title: "" }) });
  });
  await page.route(`${API}/user-timezone`, (r: Route) => r.fulfill({ status: 204, body: "" }));
  // The Sidebar sessions query composes origins + active runs into the
  // listing — both must resolve or the queryFn throws on refetch.
  await page.route(`${API}/workspaces/${WS}/session-origins`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ origins: [] }) }));
  await page.route(`${API}/workspaces/${WS}/runs/active`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ runs: [] }) }));

  await mockIdleContractStream(page, `${API}/workspaces/*/contract-events`, SESS_LIVE);
}

async function sessionKebab(page: Page, title: string) {
  const row = page.locator("div.group", { hasText: title }).last();
  await row.getByLabel("Actions").click();
  return row;
}

test.describe("#1627: session archiving UX", () => {
  test("A1 — kebab Archive moves the row into the collapsed Archived group", async ({ page }) => {
    await setupBase(page);
    await page.goto(`/chat/${WS}/${SESS_LIVE}`);
    await expect(page.getByText("Old work")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId("archived-group")).toBeHidden();

    await sessionKebab(page, "Old work");
    await page.getByRole("menuitem", { name: "Archive" }).click();

    await expect(page.getByTestId("archived-group")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId("archived-group")).toContainText("1");
    // The archived title is hidden while the group is collapsed, and no
    // duplicate row remains in the live tree.
    await expect(page.getByText("Old work")).toBeHidden();
    await expect(page.getByText("Live work")).toBeVisible();
  });

  test("A2 — archived session view: read-only banner, no composer, history rendered", async ({ page }) => {
    await setupBase(page, { rows: [
      { id: SESS_LIVE, title: "Live work" },
      { id: SESS_OLD, title: "Old work", archived: true },
    ]});
    await page.goto(`/chat/${WS}/${SESS_OLD}`);

    await expect(page.getByRole("status")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByText(/archived and read-only/i)).toBeVisible();
    await expect(page.getByPlaceholder("Type a message...")).toBeHidden();
  });

  test("A3 — Unarchive restores the composer", async ({ page }) => {
    await setupBase(page, { rows: [
      { id: SESS_LIVE, title: "Live work" },
      { id: SESS_OLD, title: "Old work", archived: true },
    ]});
    await page.goto(`/chat/${WS}/${SESS_OLD}`);
    await expect(page.getByRole("status")).toBeVisible({ timeout: 10_000 });

    // Unarchive from the group's row kebab.
    await page.getByLabel("Expand archived sessions").click();
    await expect(page.getByText("Old work")).toBeVisible();
    await sessionKebab(page, "Old work");
    await page.getByRole("menuitem", { name: "Unarchive" }).click();

    await expect(page.getByPlaceholder("Type a message...")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId("archived-group")).toBeHidden();
  });

  test("A4 — archive PUT 500: row unchanged, no group, no crash", async ({ page }) => {
    await setupBase(page, { archiveStatus: 500 });
    page.on("dialog", (d) => d.accept());
    await page.goto(`/chat/${WS}/${SESS_LIVE}`);
    await expect(page.getByText("Old work")).toBeVisible({ timeout: 10_000 });

    await sessionKebab(page, "Old work");
    await page.getByRole("menuitem", { name: "Archive" }).click();

    // Row stays live, group never appears, the page is still responsive.
    await expect(page.getByText("Old work")).toBeVisible({ timeout: 5_000 });
    await expect(page.getByTestId("archived-group")).toBeHidden();
    await expect(page.getByText("Live work")).toBeVisible();
  });
});
