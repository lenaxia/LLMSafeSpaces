import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

const API = "**/api/v1";

async function mockUnauthenticated(page: Page) {
  await page.route(`${API}/auth/me`, async (route: Route) => {
    await route.fulfill({ status: 401 });
  });
  await page.route(`${API}/auth/config`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        registrationEnabled: true,
        oidcEnabled: false,
        instanceName: "E2E Test",
      }),
    });
  });
}

async function mockLoginSuccess(page: Page) {
  await page.route(`${API}/auth/login`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        token: "e2e-token",
        user: {
          id: "e2e-user",
          username: "e2e",
          email: "e2e@test.com",
          role: "member",
        },
      }),
    });
  });
}

test.describe("return_to redirect", () => {
  test("401 redirect preserves current path as return_to", async ({ page }) => {
    // Simulate session expiry: /auth/me succeeds initially, but once
    // a protected endpoint returns 401, the session is gone.
    let sessionExpired = false;
    await page.route(`${API}/auth/me`, async (route: Route) => {
      if (sessionExpired) {
        await route.fulfill({ status: 401 });
      } else {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            id: "e2e-user",
            username: "e2e",
            email: "e2e@test.com",
            role: "member",
            active: true,
          }),
        });
      }
    });

    await page.route(`${API}/auth/config`, async (route: Route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          registrationEnabled: true,
          oidcEnabled: false,
          instanceName: "E2E Test",
        }),
      });
    });

    // When /workspaces returns 401, the session is considered expired.
    await page.route(`${API}/workspaces`, async (route: Route) => {
      sessionExpired = true;
      await route.fulfill({ status: 401 });
    });

    // Also stub other endpoints the chat page might hit.
    await page.route(`${API}/events`, async (route: Route) => {
      await route.fulfill({ status: 200 });
    });

    // Navigate to /chat — which will trigger a workspace list fetch.
    // When the 401 comes back, the client should redirect to /login?return_to=<path>.
    await page.goto("/chat");

    // The 401 handler sets window.location.href — wait for the navigation.
    await expect(page).toHaveURL(/\/login\?return_to=/, { timeout: 10000 });

    // Verify the return_to value is /chat (URL-encoded).
    const url = page.url();
    const returnTo = new URL(url).searchParams.get("return_to");
    expect(returnTo).toContain("/chat");
  });

  test("login with return_to navigates back to target", async ({ page }) => {
    await mockUnauthenticated(page);
    await mockLoginSuccess(page);

    // Visit login with a return_to.
    await page.goto("/login?return_to=%2Fsettings");

    await expect(page.getByPlaceholder("Email")).toBeVisible({
      timeout: 10000,
    });
    await expect(page.getByPlaceholder("Password")).toBeVisible();

    // Fill and submit.
    await page.getByPlaceholder("Email").fill("e2e@test.com");
    await page.getByPlaceholder("Password").fill("password123");
    await page.getByRole("button", { name: "Sign in" }).click();

    // Should navigate to /settings after login.
    await expect(page).toHaveURL(/\/settings/, { timeout: 10000 });
  });

  test("\"Create an account\" link preserves return_to", async ({ page }) => {
    await mockUnauthenticated(page);

    await page.goto("/login?return_to=%2Fchat");

    await expect(
      page.getByText("Create an account"),
    ).toBeVisible({ timeout: 5000 });

    const link = page.getByText("Create an account");
    const href = await link.getAttribute("href");
    expect(href).toContain("return_to=%2Fchat");
  });

  test("malicious return_to is sanitised from sign-in link", async ({
    page,
  }) => {
    await mockUnauthenticated(page);

    // Visit login with a protocol-relative evil redirect.
    await page.goto("/login?return_to=%2F%2Fevil.com");

    await expect(
      page.getByText(/Welcome to/),
    ).toBeVisible({ timeout: 5000 });

    // The "Create an account" link should NOT contain the evil URL.
    const link = page.getByText("Create an account");
    const href = await link.getAttribute("href");
    expect(href).not.toContain("evil");
  });
});
