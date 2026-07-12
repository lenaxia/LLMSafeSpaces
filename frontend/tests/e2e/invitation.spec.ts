import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

const API = "**/api/v1";

async function mockAuthenticated(page: Page) {
  await page.route(`${API}/auth/me`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        id: "e2e-user",
        username: "e2e",
        email: "invitee@e2e.test",
        role: "member",
        active: true,
      }),
    });
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

test.describe("Invitation acceptance", () => {
  test.beforeEach(async ({ page }) => {
    await mockAuthenticated(page);
  });

  test("renders invitation detail and accepts successfully", async ({ page }) => {
    // Mock the invitation detail fetch.
    await page.route(`${API}/invitations/e2e-token`, async (route: Route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          orgName: "Acme E2E",
          orgSlug: "acme-e2e",
          inviterName: "admin",
          role: "member",
          expiresAt: new Date(Date.now() + 7 * 86400000).toISOString(),
        }),
      });
    });

    // Mock the accept endpoint.
    let acceptCalled = false;
    await page.route(
      `${API}/invitations/e2e-token/accept`,
      async (route: Route) => {
        acceptCalled = true;
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            membership: {
              orgId: "org-e2e",
              userId: "e2e-user",
              username: "e2e",
              email: "invitee@e2e.test",
              role: "member",
              emailVerified: true,
              createdAt: new Date().toISOString(),
            },
          }),
        });
      },
    );

    await page.goto("/invitations/e2e-token");

    // Should see the invitation details.
    await expect(page.getByText("Organisation Invitation")).toBeVisible({
      timeout: 10000,
    });
    await expect(page.getByText("Acme E2E")).toBeVisible();
    await expect(page.getByText("admin")).toBeVisible();
    await expect(page.getByText("Member")).toBeVisible();

    // Click Accept.
    await page.getByRole("button", { name: "Accept" }).click();

    // Should see the success message with org name.
    await expect(page.getByText("Invitation accepted!")).toBeVisible({
      timeout: 10000,
    });
    await expect(page.getByText("Acme E2E")).toBeVisible();

    expect(acceptCalled).toBe(true);
  });

  test("shows decline confirmation", async ({ page }) => {
    await page.route(`${API}/invitations/e2e-token`, async (route: Route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          orgName: "Acme E2E",
          orgSlug: "acme-e2e",
          inviterName: "admin",
          role: "member",
          expiresAt: new Date(Date.now() + 7 * 86400000).toISOString(),
        }),
      });
    });

    await page.route(
      `${API}/invitations/e2e-token/decline`,
      async (route: Route) => {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ status: "declined" }),
        });
      },
    );

    await page.goto("/invitations/e2e-token");
    await expect(page.getByText("Organisation Invitation")).toBeVisible({
      timeout: 10000,
    });

    await page.getByRole("button", { name: "Decline" }).click();

    await expect(page.getByText("Invitation declined")).toBeVisible({
      timeout: 10000,
    });
  });

  test("shows 404 error for invalid token", async ({ page }) => {
    await page.route(`${API}/invitations/bad-token`, async (route: Route) => {
      await route.fulfill({
        status: 404,
        contentType: "application/json",
        body: JSON.stringify({ error: "not found" }),
      });
    });

    await page.goto("/invitations/bad-token");

    await expect(
      page.getByText("Invitation not found or has expired."),
    ).toBeVisible({ timeout: 10000 });
  });

  test("shows inline error on 403 wrong-email", async ({ page }) => {
    await page.route(`${API}/invitations/e2e-token`, async (route: Route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          orgName: "Acme E2E",
          orgSlug: "acme-e2e",
          inviterName: "admin",
          role: "member",
          expiresAt: new Date(Date.now() + 7 * 86400000).toISOString(),
        }),
      });
    });

    // Accept returns 403 — wrong email.
    await page.route(
      `${API}/invitations/e2e-token/accept`,
      async (route: Route) => {
        await route.fulfill({
          status: 403,
          contentType: "application/json",
          body: JSON.stringify({ error: "wrong email" }),
        });
      },
    );

    await page.goto("/invitations/e2e-token");
    await expect(page.getByText("Accept")).toBeVisible({ timeout: 10000 });

    await page.getByRole("button", { name: "Accept" }).click();

    // Error should appear inline but page should still show the detail.
    await expect(
      page.getByText(/different email address/),
    ).toBeVisible({ timeout: 10000 });
    // Accept/Decline buttons should still be present.
    await expect(page.getByRole("button", { name: "Accept" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Decline" })).toBeVisible();
  });

  test("shows Sign in / Create account links when unauthenticated", async ({
    page,
  }) => {
    // Override auth — return 401 from /auth/me.
    await page.route(`${API}/auth/me`, async (route: Route) => {
      await route.fulfill({ status: 401 });
    });

    await page.route(`${API}/invitations/e2e-token`, async (route: Route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          orgName: "Acme E2E",
          orgSlug: "acme-e2e",
          inviterName: "admin",
          role: "member",
          expiresAt: new Date(Date.now() + 7 * 86400000).toISOString(),
        }),
      });
    });

    await page.goto("/invitations/e2e-token");

    // Should still show the invitation detail (public).
    await expect(page.getByText("Organisation Invitation")).toBeVisible({
      timeout: 10000,
    });

    // Should show auth prompt instead of Accept/Decline.
    await expect(
      page.getByText(/Sign in or create an account/),
    ).toBeVisible({ timeout: 5000 });

    // Accept/Decline should NOT be present.
    await expect(
      page.getByRole("button", { name: "Accept" }),
    ).not.toBeVisible();
    await expect(
      page.getByRole("button", { name: "Decline" }),
    ).not.toBeVisible();

    // Sign in link should preserve the return_to back to the invitation.
    const signInLink = page.getByRole("link", { name: "Sign in" });
    await expect(signInLink).toBeVisible();
    const href = await signInLink.getAttribute("href");
    expect(href).toContain("return_to=%2Finvitations%2Fe2e-token");
  });
});
