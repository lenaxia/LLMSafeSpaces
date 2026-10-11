import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

// #1651 browser-level pins for the email-verification and password-reset
// frontend. The register→check-email test guards the GuestOnly interaction
// (an unverified registration must NOT be bounced to /chat before the user
// reads the guidance) — a class of bug the unit suite cannot see because it
// never mounts the production router with a live AuthProvider.
//
// Mirrors register-turnstile.spec.ts: all API endpoints are route-mocked so
// the tests run offline and deterministically.

const API_PREFIX = "**/api/v1";

async function mockConfigAndMe(page: Page) {
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
      body: JSON.stringify({ apiBaseUrl: "/api/v1", turnstileSiteKey: "" }),
    });
  });
}

test.describe("Register → email verification gated signup", () => {
  test("emailVerified=false registration shows check-your-email card, not a bounce to /chat", async ({ page }) => {
    await mockConfigAndMe(page);
    await page.route(`${API_PREFIX}/auth/register`, async (route: Route) => {
      await route.fulfill({
        status: 201,
        contentType: "application/json",
        body: JSON.stringify({
          token: "jwt",
          user: {
            id: "u1", username: "bob", email: "bob@test.com", active: true,
            role: "user", emailVerified: false,
          },
        }),
      });
    });

    await page.goto("/register");
    await page.getByPlaceholder("Username").fill("bob");
    await page.getByPlaceholder("Email").fill("bob@test.com");
    await page.getByPlaceholder("Password").fill("password123");
    await page.getByRole("button", { name: /Create account/ }).click();

    await expect(page.getByText(/check your email/i)).toBeVisible({ timeout: 5000 });
    // Still on /register — GuestOnly must not bounce the unverified user.
    await expect(page).toHaveURL(/\/register/);
    // The verify link carries the registered email for resend prefill.
    const verifyLink = page.getByText(/verify or resend/i);
    await expect(verifyLink).toBeVisible();
    await expect(verifyLink).toHaveAttribute("href", /email=bob%40test\.com/);
  });
});

test.describe("/verify-email interstitial", () => {
  test("consumes the token on mount and links to sign in", async ({ page }) => {
    await mockConfigAndMe(page);
    let seenToken = "";
    await page.route(`${API_PREFIX}/auth/verify-email`, async (route: Route) => {
      seenToken = route.request().postDataJSON().token;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ verified: true }),
      });
    });

    await page.goto("/verify-email?token=e2e-token");
    await expect(page.getByText(/email verified/i)).toBeVisible({ timeout: 5000 });
    expect(seenToken).toBe("e2e-token");
    await expect(page.getByRole("link", { name: /continue to sign in/i })).toHaveAttribute(
      "href",
      "/login",
    );
    // Terminal result: the single-use token is stripped from the URL so a
    // refresh cannot re-POST it.
    await expect(page).toHaveURL(/\/verify-email\?*$/);
    await expect(page).not.toHaveURL(/token=/);
  });

  test("expired token shows resend with prefilled email, and resend is neutral", async ({ page }) => {
    await mockConfigAndMe(page);
    await page.route(`${API_PREFIX}/auth/verify-email`, async (route: Route) => {
      await route.fulfill({
        status: 410,
        contentType: "application/json",
        body: JSON.stringify({ error: "token expired" }),
      });
    });
    await page.route(`${API_PREFIX}/auth/verify-email/resend`, async (route: Route) => {
      await route.fulfill({ status: 202, body: "" });
    });

    await page.goto("/verify-email?token=stale&email=bob%40test.com");
    await expect(page.getByText(/verification link has expired/i)).toBeVisible({ timeout: 5000 });
    await expect(page.getByPlaceholder("Email")).toHaveValue("bob@test.com");

    await page.getByRole("button", { name: /send new link/i }).click();
    // Neutral always-202 copy — nothing that implies account existence.
    await expect(page.getByText(/if that email needs verification/i)).toBeVisible({
      timeout: 5000,
    });
  });

  test("respects return_to on the sign-in link", async ({ page }) => {
    await mockConfigAndMe(page);
    await page.route(`${API_PREFIX}/auth/verify-email`, async (route: Route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ verified: true }),
      });
    });

    await page.goto("/verify-email?token=t&return_to=%2Fchat");
    await expect(page.getByRole("link", { name: /continue to sign in/i })).toHaveAttribute(
      "href",
      "/login?return_to=%2Fchat",
      { timeout: 5000 },
    );
  });
});

test.describe("/reset-password interstitial", () => {
  test("login page offers the forgot-password entry link", async ({ page }) => {
    await mockConfigAndMe(page);
    await page.goto("/login");
    const link = page.getByText(/forgot your password\?/i);
    await expect(link).toBeVisible();
    await expect(link).toHaveAttribute("href", "/reset-password");
  });

  test("request mode: always-202 neutral confirmation", async ({ page }) => {
    await mockConfigAndMe(page);
    let seenEmail = "";
    await page.route(`${API_PREFIX}/auth/password-reset/request`, async (route: Route) => {
      seenEmail = route.request().postDataJSON().email;
      await route.fulfill({ status: 202, body: "" });
    });

    // Entered from the login page's "Forgot your password?" link (pinned
    // above); direct navigation keeps this test deterministic.
    await page.goto("/reset-password");
    await page.getByPlaceholder("Email").fill("bob@test.com");
    await page.getByRole("button", { name: /send reset link/i }).click();

    await expect(page.getByText(/if an account exists for that email/i)).toBeVisible({
      timeout: 5000,
    });
    expect(seenEmail).toBe("bob@test.com");
  });

  test("confirm mode: resets and continues to /login?reset=success", async ({ page }) => {
    await mockConfigAndMe(page);
    let seenBody = { token: "", newPassword: "" };
    await page.route(`${API_PREFIX}/auth/password-reset/confirm`, async (route: Route) => {
      seenBody = route.request().postDataJSON();
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ reset: true }),
      });
    });

    await page.goto("/reset-password?token=e2e-reset");
    await page.getByPlaceholder("New password", { exact: true }).fill("newpassword1");
    await page.getByPlaceholder("Confirm new password").fill("newpassword1");
    await page.getByRole("button", { name: /reset password/i }).click();

    await expect(page.getByText(/password has been reset/i)).toBeVisible({ timeout: 5000 });
    expect(seenBody).toEqual({ token: "e2e-reset", newPassword: "newpassword1" });
    await expect(
      page.getByRole("link", { name: /sign in with your new password/i }),
    ).toHaveAttribute("href", "/login?reset=success");
  });

  test("confirm mode: expired link offers request-a-new-link", async ({ page }) => {
    await mockConfigAndMe(page);
    await page.route(`${API_PREFIX}/auth/password-reset/confirm`, async (route: Route) => {
      await route.fulfill({
        status: 410,
        contentType: "application/json",
        body: JSON.stringify({ error: "token expired" }),
      });
    });

    await page.goto("/reset-password?token=stale");
    await page.getByPlaceholder("New password", { exact: true }).fill("newpassword1");
    await page.getByPlaceholder("Confirm new password").fill("newpassword1");
    await page.getByRole("button", { name: /reset password/i }).click();

    await expect(page.getByText(/reset link has expired/i)).toBeVisible({ timeout: 5000 });
    await page.getByRole("button", { name: /request a new link/i }).click();
    await expect(page.getByPlaceholder("Email")).toBeVisible({ timeout: 5000 });
  });
});

test.describe("login 403 email_not_verified", () => {
  test("surfaces verification guidance with prefilled resend link", async ({ page }) => {
    await mockConfigAndMe(page);
    await page.route(`${API_PREFIX}/auth/login`, async (route: Route) => {
      await route.fulfill({
        status: 403,
        contentType: "application/json",
        body: JSON.stringify({
          error: "please verify your email address before logging in",
          emailVerified: false,
        }),
      });
    });

    await page.goto("/login");
    await page.getByPlaceholder("Email").fill("bob@test.com");
    await page.getByPlaceholder("Password").fill("password123");
    await page.getByRole("button", { name: /sign in/i }).click();

    await expect(page.getByText(/verify your email address before logging in/i)).toBeVisible({
      timeout: 5000,
    });
    await expect(page.getByText(/resend verification email/i)).toHaveAttribute(
      "href",
      "/verify-email?email=bob%40test.com",
    );
  });
});
