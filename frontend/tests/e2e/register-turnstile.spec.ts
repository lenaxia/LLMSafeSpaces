import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

// Turnstile register e2e tests.
//
// Committed after PR #501 review rounds 3–5 in response to reviewer
// feedback: the CSP lockout (rounds 2–4) and the 401→/login redirect
// (round 5) both escaped unit + integration coverage precisely because
// they only surface when a real browser loads the widget script and
// walks the full request/response flow through the frontend's global
// fetch wrapper. A browser-level test would have caught both blockers
// in round 1.
//
// Strategy: intercept the Cloudflare Turnstile script + API endpoints
// so the tests run offline (deterministic, no cross-origin CDN
// dependency in CI). The stub script installs a minimal window.turnstile
// with programmable `success` / `token` outcomes, matching the shape
// the real Cloudflare script exposes.

const API_PREFIX = "**/api/v1";
// Wildcard for Playwright page.route — matches http/https and any query
// params Cloudflare might append. The bare-string form doesn't intercept
// cross-origin URLs reliably.
const TURNSTILE_SCRIPT_URL = "**/challenges.cloudflare.com/turnstile/v0/api.js*";
const TEST_SITE_KEY = "0xTEST_SITE_KEY";
const VALID_TOKEN = "test-token-valid";

async function mockAuthConfigWithTurnstile(page: Page) {
  await page.route(`${API_PREFIX}/auth/config`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        registrationEnabled: true,
        oidcEnabled: false,
        instanceName: "test",
      }),
    });
  });
  await page.route(`${API_PREFIX}/auth/me`, async (route: Route) => {
    await route.fulfill({
      status: 401,
      contentType: "application/json",
      body: JSON.stringify({ error: "unauthorized" }),
    });
  });
  // Override the runtime env.json to inject a site key. The frontend's
  // getEnv() reads /env.json at boot; overriding here forces the
  // Turnstile widget to render.
  await page.route("**/env.json", async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        apiBaseUrl: "/api/v1",
        turnstileSiteKey: TEST_SITE_KEY,
      }),
    });
  });
}

// mockTurnstileScript intercepts the Cloudflare Turnstile client
// script and replaces it with a stub that immediately renders a
// widget + fires the callback with a configurable outcome. Mirrors
// the shape of the real window.turnstile API.
async function mockTurnstileScript(page: Page, opts: {
  onRender?: (widgetId: string) => void;
  autoFireToken?: string | null; // null → don't auto-fire (simulate pending)
} = {}) {
  const { autoFireToken = VALID_TOKEN } = opts;
  await page.route(TURNSTILE_SCRIPT_URL, async (route: Route) => {
    const stub = `
      (function() {
        window.turnstile = {
          render: function(container, options) {
            var el = typeof container === 'string' ? document.querySelector(container) : container;
            if (el) {
              el.setAttribute('data-turnstile-rendered', 'true');
              el.setAttribute('data-sitekey', options.sitekey);
            }
            var widgetId = 'stub-widget-' + Math.random().toString(36).slice(2);
            ${
              autoFireToken !== null
                ? `setTimeout(function() { if (options.callback) options.callback(${JSON.stringify(autoFireToken)}); }, 50);`
                : ""
            }
            return widgetId;
          },
          reset: function() {},
          remove: function() {},
        };
      })();
    `;
    await route.fulfill({
      status: 200,
      contentType: "application/javascript",
      body: stub,
    });
  });
}

test.describe("Register flow — Turnstile happy path", () => {
  test("valid Turnstile token → registration succeeds", async ({ page }) => {
    await mockAuthConfigWithTurnstile(page);
    await mockTurnstileScript(page);

    // API accepts the register call and returns a valid session.
    let registerCallCount = 0;
    let receivedTurnstileHeader = "";
    await page.route(`${API_PREFIX}/auth/register`, async (route: Route) => {
      registerCallCount++;
      receivedTurnstileHeader =
        route.request().headerValue("cf-turnstile-response") as never;
      // Extract synchronously — Playwright's headerValue is async, so
      // we use headers() to avoid the await race.
      const headers = route.request().headers();
      receivedTurnstileHeader = headers["cf-turnstile-response"] || "";
      await route.fulfill({
        status: 201,
        contentType: "application/json",
        body: JSON.stringify({
          token: "jwt",
          user: { id: "u1", username: "bob", email: "bob@test.com", active: true, role: "user" },
        }),
      });
    });

    await page.goto("/register");
    await expect(page.locator("[data-turnstile-rendered='true']")).toBeVisible();

    await page.getByPlaceholder("Username").fill("bob");
    await page.getByPlaceholder("Email").fill("bob@test.com");
    await page.getByPlaceholder("Password").fill("password123");

    // Wait for the auto-fired token to unlock submit.
    const button = page.getByRole("button", { name: /Create account/ });
    await expect(button).toBeEnabled({ timeout: 5000 });
    await button.click();

    // Wait for the register call to complete.
    await expect(page).not.toHaveURL(/\/register/, { timeout: 5000 });
    expect(registerCallCount).toBe(1);
    expect(receivedTurnstileHeader).toBe(VALID_TOKEN);
  });
});

test.describe("Register flow — Turnstile unhappy paths", () => {
  test("submit button stays disabled while Turnstile has no token", async ({ page }) => {
    await mockAuthConfigWithTurnstile(page);
    // autoFireToken=null → widget renders but never issues a token
    await mockTurnstileScript(page, { autoFireToken: null });

    await page.goto("/register");
    await expect(page.locator("[data-turnstile-rendered='true']")).toBeVisible();

    await page.getByPlaceholder("Username").fill("bob");
    await page.getByPlaceholder("Email").fill("bob@test.com");
    await page.getByPlaceholder("Password").fill("password123");

    // Submit button is disabled because token never arrived.
    const button = page.getByRole("button", { name: /Create account/ });
    await expect(button).toBeDisabled();
  });

  test("turnstile_failed 401 → user stays on /register (no redirect to /login)", async ({ page }) => {
    // This is the regression test for round-5 blocker: backend Turnstile
    // 401 was triggering the global handleUnauthorized redirect.
    await mockAuthConfigWithTurnstile(page);
    await mockTurnstileScript(page);

    await page.route(`${API_PREFIX}/auth/register`, async (route: Route) => {
      await route.fulfill({
        status: 401,
        contentType: "application/json",
        body: JSON.stringify({
          error: "turnstile_failed",
          reason: "rejected",
          detail: "invalid-input-response",
        }),
      });
    });

    await page.goto("/register");
    await page.getByPlaceholder("Username").fill("bob");
    await page.getByPlaceholder("Email").fill("bob@test.com");
    await page.getByPlaceholder("Password").fill("password123");

    const button = page.getByRole("button", { name: /Create account/ });
    await expect(button).toBeEnabled({ timeout: 5000 });
    await button.click();

    // Small wait for the error UI to render + potential redirect to
    // trigger. Then assert we're still on /register.
    await page.waitForTimeout(500);
    await expect(page).toHaveURL(/\/register/);
    // Error message from the backend should be visible.
    await expect(page.getByText(/turnstile_failed|rejected|captcha/i)).toBeVisible({
      timeout: 3000,
    });
  });

  test("verify_unavailable 401 → user stays on /register with error visible", async ({ page }) => {
    // Same class of test but for verify-unreachable failure mode.
    await mockAuthConfigWithTurnstile(page);
    await mockTurnstileScript(page);

    await page.route(`${API_PREFIX}/auth/register`, async (route: Route) => {
      await route.fulfill({
        status: 401,
        contentType: "application/json",
        body: JSON.stringify({
          error: "turnstile_failed",
          reason: "verify_unavailable",
          detail: "captcha verification service unavailable",
        }),
      });
    });

    await page.goto("/register");
    await page.getByPlaceholder("Username").fill("bob");
    await page.getByPlaceholder("Email").fill("bob@test.com");
    await page.getByPlaceholder("Password").fill("password123");

    const button = page.getByRole("button", { name: /Create account/ });
    await expect(button).toBeEnabled({ timeout: 5000 });
    await button.click();

    await page.waitForTimeout(500);
    await expect(page).toHaveURL(/\/register/);
  });
});

test.describe("Register flow — Turnstile disabled", () => {
  // When turnstile.enabled=false server-side, the frontend env.json has
  // turnstileSiteKey="" and the widget must not render. Regression guard
  // for a "widget always renders" bug.
  test("no widget rendered when turnstileSiteKey is empty", async ({ page }) => {
    await page.route(`${API_PREFIX}/auth/config`, async (route: Route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          registrationEnabled: true,
          oidcEnabled: false,
          instanceName: "test",
        }),
      });
    });
    await page.route(`${API_PREFIX}/auth/me`, async (route: Route) => {
      await route.fulfill({
        status: 401,
        contentType: "application/json",
        body: JSON.stringify({ error: "unauthorized" }),
      });
    });
    await page.route("**/env.json", async (route: Route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          apiBaseUrl: "/api/v1",
          turnstileSiteKey: "",
        }),
      });
    });

    await page.goto("/register");
    // No widget in the DOM (the component returns null when siteKey is empty).
    await expect(page.locator("[data-turnstile-rendered='true']")).toHaveCount(0);
    // Submit button should be enabled once the form is filled — no
    // Turnstile token requirement.
    await page.getByPlaceholder("Username").fill("bob");
    await page.getByPlaceholder("Email").fill("bob@test.com");
    await page.getByPlaceholder("Password").fill("password123");
    const button = page.getByRole("button", { name: /Create account/ });
    await expect(button).toBeEnabled();
  });
});
