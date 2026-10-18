/**
 * E2E for #1686: the disk-recovery strip's surfacing rules in the real
 * ChatPage composition, desktop AND mobile viewport.
 *
 *   1 — below the 95% threshold the strip renders NOTHING (the
 *       pre-#1686 permanent "Free up disk space" line is gone).
 *   2 — at >=95% a compact single-line banner renders (inline actions).
 *   3 — mobile: the metrics Hide/Metrics toggle still works with the
 *       strip mounted at critical (the old dedicated line sat under the
 *       toggle and broke the perceived hide), and Dismiss quiets the
 *       banner.
 *
 * All backend APIs are route-mocked (the archive.spec.ts harness shape);
 * the workspace status carries the disk fields the strip renders from.
 */
import { test, expect, type Page, type Route } from "@playwright/test";
import { mockIdleContractStream } from "./helpers/contractStream";

const WS = "ws-disk";
const SESS = "ses_disk";
const API = "**/api/v1";

async function setupChat(page: Page, disk: { used: number; total: number }) {
  await page.route(`${API}/auth/me`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "u1", username: "tester", email: "t@t.com", role: "user", active: true }) }));
  await page.route(`${API}/auth/config`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ registrationEnabled: false, oidcEnabled: false }) }));

  await page.route(`${API}/workspaces`, (r: Route) => {
    if (r.request().method() !== "GET") { r.continue(); return; }
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({
      items: [{ id: WS, name: "DiskSpace", userId: "u1", runtime: "base", storageSize: "1Gi", phase: "Active" }],
      pagination: { limit: 50, offset: 0, total: 1 },
    })});
  });

  await page.route(`${API}/workspaces/${WS}/status`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({
      phase: "Active",
      credentialState: { available: true },
      agentHealth: { status: "healthy", agentVersion: "1.0" },
      sessions: [],
      diskUsedBytes: disk.used,
      diskTotalBytes: disk.total,
    }) }));

  await page.route(`${API}/workspaces/${WS}/sessions?*`, (r: Route) => {
    if (r.request().method() === "GET") {
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([
        { id: SESS, title: "Disk session", status: "idle", hasUnread: false, messageCount: 1 },
      ])});
      return;
    }
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) });
  });

  await page.route(`${API}/workspaces/${WS}/models`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ models: [], currentModel: "" }) }));
  await page.route(`${API}/workspaces/*/sessions/*/seen`, (r: Route) =>
    r.fulfill({ status: 204, body: "" }));
  await page.route(`${API}/workspaces/${WS}/sessions/*/message**`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }));
  await page.route(`${API}/workspaces/${WS}/sessions/*`, (r: Route) => {
    if (r.request().method() !== "GET") { r.fulfill({ status: 204, body: "" }); return; }
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: SESS, title: "" }) });
  });
  await page.route(`${API}/user-timezone`, (r: Route) => r.fulfill({ status: 204, body: "" }));
  await page.route(`${API}/workspaces/${WS}/session-origins`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ origins: [] }) }));
  await page.route(`${API}/workspaces/${WS}/runs/active`, (r: Route) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ runs: [] }) }));

  await mockIdleContractStream(page, `${API}/workspaces/*/contract-events`, SESS);
}

test.describe("#1686: disk-recovery strip surfacing", () => {
  test("below 95% nothing renders — no on-demand line anywhere", async ({ page }) => {
    await setupChat(page, { used: 70_000_000_000, total: 100_000_000_000 });
    await page.goto(`/chat/${WS}/${SESS}`);
    await expect(page.getByText("Disk session")).toBeVisible({ timeout: 10_000 });
    await expect(page.getByTestId("disk-recovery-strip")).toHaveCount(0);
    await expect(page.getByTestId("disk-recover-on-demand")).toHaveCount(0);
  });

  test("at >=95% the compact banner renders with inline actions", async ({ page }) => {
    await setupChat(page, { used: 97_000_000_000, total: 100_000_000_000 });
    await page.goto(`/chat/${WS}/${SESS}`);
    const strip = page.getByTestId("disk-recovery-strip");
    await expect(strip).toBeVisible({ timeout: 10_000 });
    await expect(strip).toContainText(/97%/);
    await expect(strip.getByTestId("disk-recover-review")).toBeVisible();
    await expect(strip.getByTestId("disk-recovery-dismiss")).toBeVisible();
  });

});

test.describe("#1686: mobile viewport", () => {
  test.use({ viewport: { width: 390, height: 844 } });

  test("Hide toggle still works with the critical banner; Dismiss quiets it", async ({ page }) => {
    await setupChat(page, { used: 97_000_000_000, total: 100_000_000_000 });
    await page.goto(`/chat/${WS}/${SESS}`);
    await expect(page.getByTestId("disk-recovery-strip")).toBeVisible({ timeout: 10_000 });

    // The metrics row's Hide/Metrics toggle must remain functional with
    // the banner mounted (the #1686 mobile complaint) — pinned as an
    // actual STATE FLIP (review r1: a both-labels regex would pass a
    // no-op click). Disk at 97% is itself a critical metric, so the
    // mobile drawer AUTO-OPENS and the toggle starts as "Hide".
    const hideToggle = page.getByRole("button", { name: "Hide", exact: true });
    await expect(hideToggle).toBeVisible();
    await hideToggle.click();
    const metricsToggle = page.getByRole("button", { name: "Metrics" });
    await expect(metricsToggle).toBeVisible();
    await expect(hideToggle).toHaveCount(0);
    await metricsToggle.click();
    await expect(page.getByRole("button", { name: "Hide", exact: true })).toBeVisible();

    // Dismiss hides the banner for this critical episode.
    await page.getByTestId("disk-recovery-dismiss").click();
    await expect(page.getByTestId("disk-recovery-strip")).toHaveCount(0);
    // The metrics toggle survives the dismissal (drawer is open from the
    // flip above, so the label reads "Hide") — nothing disk-related
    // lingers under it.
    await expect(page.getByRole("button", { name: "Hide", exact: true })).toBeVisible();
  });
});
