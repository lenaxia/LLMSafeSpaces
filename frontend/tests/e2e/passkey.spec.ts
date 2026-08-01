import { test, expect } from "@playwright/test";
import type { Page, Route, CDPSession } from "@playwright/test";

// Passkey Playwright e2e tests.
//
// Strategy: uses Chrome DevTools Protocol (CDP) WebAuthn domain to create
// a virtual authenticator that responds to navigator.credentials.create()
// and .get() without user interaction. This is the ONLY way to test the
// real browser WebAuthn flow (origin validation, RP ID matching, actual
// navigator.credentials API) without a physical security key or platform
// biometric prompt.
//
// The API endpoints are mocked via page.route so the tests run offline.
// The virtual authenticator + real browser WebAuthn API exercise the
// @simplewebauthn/browser library's actual serialization path.

const API_PREFIX = "**/api/v1";
const RP_ID = "localhost";
const ORIGIN = "http://localhost:5173";

interface VirtualAuthenticatorOptions {
  protocol: string;
  transport: string;
  hasResidentKey: boolean;
  isUserVerified: boolean;
}

async function setupVirtualAuthenticator(page: Page): Promise<{ cdp: CDPSession; authenticatorId: string }> {
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("WebAuthn.enable", {});
  const result = await cdp.send("WebAuthn.addVirtualAuthenticator", {
    options: {
      protocol: "ctap2",
      transport: "internal",
      hasResidentKey: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    } as VirtualAuthenticatorOptions,
  });
  return { cdp, authenticatorId: result.authenticatorId };
}

async function mockPasskeyApi(page: Page): Promise<{ loggedIn: boolean }> {
  const state = { loggedIn: false };

  await page.route(`${API_PREFIX}/auth/config`, async (route: Route) => {
    await route.fulfill({
      status: 200, contentType: "application/json",
      body: JSON.stringify({ registrationEnabled: true, oidcEnabled: false, passkeyEnabled: true, passkeyDefaultSignup: true, instanceName: "E2E Test" }),
    });
  });
  await page.route(`${API_PREFIX}/auth/me`, async (route: Route) => {
    if (state.loggedIn) {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: "user-e2e", username: "e2euser", email: "e2e@test.com", role: "user", active: true, createdAt: "2026-01-01T00:00:00Z" }) });
    } else {
      await route.fulfill({ status: 401, contentType: "application/json", body: JSON.stringify({ error: "unauthorized" }) });
    }
  });
  await page.route("**/env.json", async (route: Route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ apiBaseUrl: "/api/v1" }) });
  });
  return state;
}

test.describe("Passkey e2e", () => {
  // Ceremony tests need longer timeouts — the virtual authenticator + browser
  // WebAuthn API is async and may take several seconds.
  test.setTimeout(60_000);
  test("register page defaults to passkey mode when enabled", async ({ page }) => {
    const mockState = await mockPasskeyApi(page);
    await page.goto("/register");

    await expect(page.getByText("Create account with passkey")).toBeVisible();
    await expect(page.getByPlaceholder("Email")).toBeVisible();
    // Password field should NOT be visible in passkey-default mode.
    await expect(page.getByPlaceholder("Password")).not.toBeVisible();
  });

  test("login page defaults to passkey mode when enabled", async ({ page }) => {
    const mockState = await mockPasskeyApi(page);
    await page.goto("/login");

    await expect(page.getByText("Sign in with passkey")).toBeVisible();
    await expect(page.getByPlaceholder("Email")).toBeVisible();
    await expect(page.getByPlaceholder("Password")).not.toBeVisible();
  });

  test("register page switches to password mode", async ({ page }) => {
    const mockState = await mockPasskeyApi(page);
    await page.goto("/register");

    await page.getByText("Use password instead").click();

    await expect(page.getByPlaceholder("Password")).toBeVisible();
    await expect(page.getByPlaceholder("Username")).toBeVisible();
    await expect(page.getByText("Sign up with passkey")).toBeVisible();
  });

  test("login page switches to password mode", async ({ page }) => {
    const mockState = await mockPasskeyApi(page);
    await page.goto("/login");

    await page.getByText("Use password instead").click();

    await expect(page.getByPlaceholder("Password")).toBeVisible();
    await expect(page.getByText("Sign in with passkey")).toBeVisible();
  });

  test("login page shows recovery code link when passkey enabled", async ({ page }) => {
    const mockState = await mockPasskeyApi(page);
    await page.goto("/login");

    await expect(page.getByText(/recovery code/i)).toBeVisible();
  });

  test("login page hides recovery code link when passkey disabled", async ({ page }) => {
    await page.route(`${API_PREFIX}/auth/config`, async (route: Route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          registrationEnabled: true,
          oidcEnabled: false,
          passkeyEnabled: false,
          instanceName: "E2E Test",
        }),
      });
    });
    await page.route(`${API_PREFIX}/auth/me`, async (route: Route) => {
      await route.fulfill({ status: 401, body: JSON.stringify({ error: "unauthorized" }) });
    });

    await page.goto("/login");

    await expect(page.getByPlaceholder("Password")).toBeVisible();
    await expect(page.getByText(/recovery code/i)).not.toBeVisible();
  });

  test("full passkey registration ceremony via virtual authenticator", async ({ browser }) => {
    // Uses Chrome DevTools Protocol (CDP) WebAuthn domain to create a
    // virtual authenticator. This works in headless Chromium — CI's
    // Playwright runner already installs Chromium.
    const page = await browser.newPage();
    const { cdp, authenticatorId } = await setupVirtualAuthenticator(page);
    const mockState = await mockPasskeyApi(page);

    // Mock the registration endpoints.
    await page.route(`${API_PREFIX}/auth/passkey/register/begin`, async (route: Route) => {
      const body = route.request().postDataJSON();
      const challenge = btoa(String.fromCharCode(...crypto.getRandomValues(new Uint8Array(32))));
      // Return flat PublicKeyCredentialCreationOptionsJSON (what
      // @simplewebauthn/browser v13 expects in optionsJSON — NOT wrapped
      // in { publicKey: ... }).
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          options: {
            rp: { name: "E2E Test" },
            user: {
              id: btoa("user-e2e"),
              name: body?.email || "e2e@test.com",
              displayName: body?.name || "E2E User",
            },
            challenge,
            pubKeyCredParams: [{ type: "public-key", alg: -7 }],
            authenticatorSelection: {
              authenticatorAttachment: "platform",
              userVerification: "preferred",
              residentKey: "preferred",
            },
            timeout: 60000,
            attestation: "none",
          },
          sessionToken: "e2e-session-tok",
        }),
      });
    });

    await page.route(`${API_PREFIX}/auth/passkey/register/finish`, async (route: Route) => {
      mockState.loggedIn = true;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        headers: { "Set-Cookie": "lsp_session=e2e-jwt-token; Path=/; HttpOnly" },
        body: JSON.stringify({
          token: "e2e-jwt-token",
          recoveryCodes: ["RECOVERY1", "RECOVERY2", "RECOVERY3"],
        }),
      });
    });

    await page.goto("/register");

    // Wait for the passkey form to be rendered (mode switches from the
    // default "password" to "passkey" only after /auth/config resolves).
    await expect(page.getByText("Create account with passkey")).toBeVisible({ timeout: 10000 });

    // Fill email and submit the passkey form.
    await page.getByPlaceholder("Email").fill("e2e@test.com");
    await page.getByText("Create account with passkey").click();

    // The virtual authenticator should auto-respond to navigator.credentials.create().
    // Recovery codes should be displayed.
    await expect(page.getByText("Save your recovery codes")).toBeVisible({ timeout: 10000 });
    await expect(page.getByText("RECOVERY1")).toBeVisible();
    await expect(page.getByText("RECOVERY2")).toBeVisible();

    // Acknowledge and continue.
    await page.getByRole("checkbox").check();
    await page.getByText("Continue").click();

    // Cleanup.
    await cdp.send("WebAuthn.removeVirtualAuthenticator", { authenticatorId });
    await cdp.detach();
    await page.close();
  });

  test("full passkey login ceremony via virtual authenticator", async ({ browser }) => {
    // This test registers a credential first (so the virtual authenticator
    // has a discoverable credential to use for login), then performs a
    // full login assertion. This proves the end-to-end browser ceremony
    // works: navigator.credentials.create() → navigator.credentials.get().
    const page = await browser.newPage();
    const { cdp, authenticatorId } = await setupVirtualAuthenticator(page);
    const mockState = await mockPasskeyApi(page);

    // PHASE 1: Register a credential (same as the registration test).
    await page.route(`${API_PREFIX}/auth/passkey/register/begin`, async (route: Route) => {
      const challenge = btoa(String.fromCharCode(...crypto.getRandomValues(new Uint8Array(32))));
      await route.fulfill({
        status: 200, contentType: "application/json",
        body: JSON.stringify({
          options: {
            rp: { name: "E2E Test" },
            user: { id: btoa("user-e2e"), name: "e2e@test.com", displayName: "E2E User" },
            challenge, pubKeyCredParams: [{ type: "public-key", alg: -7 }],
            authenticatorSelection: { authenticatorAttachment: "platform", userVerification: "preferred", residentKey: "required" },
            timeout: 60000, attestation: "none",
          },
          sessionToken: "e2e-reg-tok",
        }),
      });
    });
    await page.route(`${API_PREFIX}/auth/passkey/register/finish`, async (route: Route) => {
      await route.fulfill({
        status: 200, contentType: "application/json",
        headers: { "Set-Cookie": "lsp_session=e2e-jwt-token; Path=/; HttpOnly" },
        body: JSON.stringify({ token: "e2e-jwt-token", recoveryCodes: ["RC1", "RC2"] }),
      });
    });

    await page.goto("/register");
    // Wait for the passkey form (mode switches from "password" to "passkey"
    // after /auth/config resolves).
    await expect(page.getByText("Create account with passkey")).toBeVisible({ timeout: 10000 });
    await page.getByPlaceholder("Email").fill("e2e@test.com");
    await page.getByText("Create account with passkey").click();
    // Wait for recovery codes (proves registration succeeded).
    await expect(page.getByText("Save your recovery codes")).toBeVisible({ timeout: 15000 });
    // Acknowledge and continue.
    await page.getByRole("checkbox").check();
    await page.getByText("Continue").click();

    // PHASE 2: Log out, then log back in via passkey assertion.
    mockState.loggedIn = false;
    await page.goto("/login");

    // Mock login endpoints.
    await page.route(`${API_PREFIX}/auth/passkey/login/begin`, async (route: Route) => {
      const challenge = btoa(String.fromCharCode(...crypto.getRandomValues(new Uint8Array(32))));
      await route.fulfill({
        status: 200, contentType: "application/json",
        body: JSON.stringify({
          options: {
            rpId: RP_ID,
            challenge,
            allowCredentials: [],
            userVerification: "preferred",
            timeout: 60000,
          },
          sessionToken: "e2e-login-tok",
        }),
      });
    });
    await page.route(`${API_PREFIX}/auth/passkey/login/finish`, async (route: Route) => {
      mockState.loggedIn = true;
      await route.fulfill({
        status: 200, contentType: "application/json",
        headers: { "Set-Cookie": "lsp_session=e2e-jwt-token; Path=/; HttpOnly" },
        body: JSON.stringify({
          token: "e2e-jwt-token",
          user: { id: "user-e2e", username: "e2euser", email: "e2e@test.com", role: "user", active: true, createdAt: "2026-01-01T00:00:00Z" },
        }),
      });
    });

    // Perform login. The email field is required, so fill it first. Wait for
    // the passkey form specifically — "Sign in with passkey" also appears as
    // a switch button in password mode, but the "Lost your passkey?" link only
    // exists on the passkey form.
    await expect(page.getByText(/Lost your passkey/i)).toBeVisible({ timeout: 10000 });
    await page.getByPlaceholder("Email").fill("e2e@test.com");
    await page.getByText("Sign in with passkey").click();

    // The virtual authenticator should respond to navigator.credentials.get()
    // with the credential registered in PHASE 1. After login, the page
    // should navigate away from /login.
    await expect(page).not.toHaveURL(/\/login/, { timeout: 15000 });

    await cdp.send("WebAuthn.removeVirtualAuthenticator", { authenticatorId });
    await cdp.detach();
    await page.close();
  });

  test("recovery code flow", async ({ page }) => {
    const mockState = await mockPasskeyApi(page);

    await page.route(`${API_PREFIX}/auth/passkey/recover`, async (route: Route) => {
      mockState.loggedIn = true;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        headers: { "Set-Cookie": "lsp_session=recovered-token; Path=/; HttpOnly" },
        body: JSON.stringify({
          token: "recovered-token",
          user: {
            id: "user-recovered",
            username: "recovered",
            email: "recovered@test.com",
            role: "user",
            active: true,
            createdAt: "2026-01-01T00:00:00Z",
          },
          mustEnrollPasskey: true,
        }),
      });
    });

    await page.goto("/login");

    // Click recovery link.
    await page.getByText(/recovery code/i).click();

    // Fill recovery form.
    await expect(page.getByPlaceholder("Recovery code")).toBeVisible();
    await page.getByPlaceholder("Email").fill("recovered@test.com");
    await page.getByPlaceholder("Recovery code").fill("RECOVERYCODE123");
    await page.getByText("Recover account").click();

    // Should navigate away from /login on success.
    await expect(page).not.toHaveURL(/\/login/, { timeout: 5000 });
  });

  test("invalid recovery code shows error", async ({ page }) => {
    const mockState = await mockPasskeyApi(page);

    await page.route(`${API_PREFIX}/auth/passkey/recover`, async (route: Route) => {
      await route.fulfill({
        status: 401,
        contentType: "application/json",
        body: JSON.stringify({ error: "invalid recovery code" }),
      });
    });

    await page.goto("/login");
    await page.getByText(/recovery code/i).click();

    await page.getByPlaceholder("Email").fill("bad@test.com");
    await page.getByPlaceholder("Recovery code").fill("WRONGCODE");
    await page.getByText("Recover account").click();

    await expect(page.getByText(/Invalid recovery code/i)).toBeVisible();
    // Should stay on the recovery page.
    await expect(page.getByPlaceholder("Recovery code")).toBeVisible();
  });

  test("unsupported browser shows fallback in passkey mode", async ({ page }) => {
    // Override navigator.credentials to simulate unsupported browser.
    await page.addInitScript(() => {
      // Remove the WebAuthn API to trigger browserSupportsWebAuthn() === false.
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      delete (window as any).PublicKeyCredential;
    });

    const mockState = await mockPasskeyApi(page);
    await page.goto("/login");

    // Should show the fallback message + password switch.
    await expect(page.getByText(/does not support passkeys/i)).toBeVisible();
    await expect(page.getByText("Use password instead")).toBeVisible();
  });
});

test.describe("Passkey settings", () => {
  test("settings page shows passkey list and Add passkey button", async ({ page }) => {
    const mockState = await mockPasskeyApi(page);
    mockState.loggedIn = true;

    await page.route(`${API_PREFIX}/account/passkeys`, async (route: Route) => {
      await route.fulfill({
        status: 200, contentType: "application/json",
        body: JSON.stringify({ passkeys: [{ id: "pk-1", name: "YubiKey", createdAt: "2026-01-01T00:00:00Z" }] }),
      });
    });

    await page.goto("/settings/passkeys");
    await expect(page.getByRole("heading", { name: "Passkeys" })).toBeVisible({ timeout: 5000 });
    await expect(page.getByText("YubiKey")).toBeVisible();
    await expect(page.getByText("Add passkey")).toBeVisible();
    await expect(page.getByText("Regenerate recovery codes")).toBeVisible();
  });

  test("settings page shows empty state with enrollment prompt", async ({ page }) => {
    const mockState = await mockPasskeyApi(page);
    mockState.loggedIn = true;

    await page.route(`${API_PREFIX}/account/passkeys`, async (route: Route) => {
      await route.fulfill({
        status: 200, contentType: "application/json",
        body: JSON.stringify({ passkeys: [] }),
      });
    });

    await page.goto("/settings/passkeys");
    await expect(page.getByText(/No passkeys registered/i)).toBeVisible({ timeout: 5000 });
    await expect(page.getByText("Add passkey")).toBeVisible();
  });

  test("settings page shows must_enroll_passkey banner", async ({ page }) => {
    const mockState = await mockPasskeyApi(page);
    mockState.loggedIn = true;

    await page.route(`${API_PREFIX}/account/passkeys`, async (route: Route) => {
      await route.fulfill({
        status: 200, contentType: "application/json",
        body: JSON.stringify({ passkeys: [] }),
      });
    });

    await page.goto("/settings/passkeys?must_enroll_passkey=1");
    await expect(page.getByText(/recovery code to sign in/i)).toBeVisible({ timeout: 5000 });
  });
});

  test("Add passkey ceremony via virtual authenticator", async ({ browser }) => {
    const page = await browser.newPage();
    const { cdp, authenticatorId } = await setupVirtualAuthenticator(page);
    const mockState = await mockPasskeyApi(page);
    mockState.loggedIn = true;

    await page.route(`${API_PREFIX}/account/passkeys`, async (route: Route) => {
      const method = route.request().method();
      if (method === "GET") {
        await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ passkeys: [{ id: "pk-new", name: "New Key", createdAt: "2026-01-01T00:00:00Z" }] }) });
      } else {
        await route.fulfill({ status: 200 });
      }
    });
    await page.route(`${API_PREFIX}/account/passkeys/enroll/begin`, async (route: Route) => {
      const challenge = btoa(String.fromCharCode(...crypto.getRandomValues(new Uint8Array(32))));
      await route.fulfill({
        status: 200, contentType: "application/json",
        body: JSON.stringify({
          options: { rp: { name: "Test" }, user: { id: btoa("u1"), name: "user", displayName: "user" }, challenge, pubKeyCredParams: [{ type: "public-key", alg: -7 }], timeout: 60000, attestation: "none" },
          sessionToken: "enroll-tok",
        }),
      });
    });
    await page.route(`${API_PREFIX}/account/passkeys/enroll/finish`, async (route: Route) => {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ enrolled: true }) });
    });

    await page.goto("/settings/passkeys");
    await page.getByText("Add passkey").click();

    // The virtual authenticator auto-responds. After enrollment, the list
    // refreshes and shows the new passkey.
    await expect(page.getByText("New Key")).toBeVisible({ timeout: 15000 });

    await cdp.send("WebAuthn.removeVirtualAuthenticator", { authenticatorId });
    await cdp.detach();
    await page.close();
  });

  test("Add passkey ceremony failure shows error", async ({ browser }) => {
    const page = await browser.newPage();
    const mockState = await mockPasskeyApi(page);
    mockState.loggedIn = true;

    await page.route(`${API_PREFIX}/account/passkeys`, async (route: Route) => {
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ passkeys: [] }) });
    });
    // Mock enroll/finish to fail.
    await page.route(`${API_PREFIX}/account/passkeys/enroll/begin`, async (route: Route) => {
      await route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "internal" }) });
    });

    await page.goto("/settings/passkeys");
    await page.getByText("Add passkey").click();

    // Should show an error message.
    await expect(page.getByText(/Failed to add passkey/i)).toBeVisible({ timeout: 5000 });

    await page.close();
  });

  test("full recovery → re-enrollment flow via virtual authenticator", async ({ browser }) => {
    test.setTimeout(60_000);
    // Tests the complete "lost passkey" workflow:
    // 1. User has no passkey (uses recovery code to log in)
    // 2. Lands on settings page with must_enroll_passkey banner
    // 3. Clicks "Add passkey" and enrolls a new credential
    // 4. New passkey appears in the list
    const page = await browser.newPage();
    const { cdp, authenticatorId } = await setupVirtualAuthenticator(page);
    const mockState = await mockPasskeyApi(page);

    // Mock recovery endpoint.
    await page.route(`${API_PREFIX}/auth/passkey/recover`, async (route: Route) => {
      mockState.loggedIn = true;
      await route.fulfill({
        status: 200, contentType: "application/json",
        headers: { "Set-Cookie": "lsp_session=recovered-token; Path=/; HttpOnly" },
        body: JSON.stringify({
          token: "recovered-token",
          user: { id: "user-e2e", username: "e2euser", email: "e2e@test.com", role: "user", active: true, createdAt: "2026-01-01T00:00:00Z" },
          mustEnrollPasskey: true,
        }),
      });
    });

    // PHASE 2: Mock passkey list + enroll endpoints.
    // First GET returns empty list; after enroll, GET returns the new passkey.
    // Registered before navigation so the initial GET by PasskeySettings is mocked.
    let enrollDone = false;
    await page.route(`${API_PREFIX}/account/passkeys`, async (route: Route) => {
      if (route.request().method() === "GET") {
        const passkeys = enrollDone
          ? [{ id: "pk-new", name: "New Passkey", createdAt: "2026-01-01T00:00:00Z" }]
          : [];
        await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ passkeys }) });
      } else {
        await route.fulfill({ status: 200 });
      }
    });
    await page.route(`${API_PREFIX}/account/passkeys/enroll/begin`, async (route: Route) => {
      const challenge = btoa(String.fromCharCode(...crypto.getRandomValues(new Uint8Array(32))));
      await route.fulfill({
        status: 200, contentType: "application/json",
        body: JSON.stringify({
          options: {
            rp: { name: "E2E Test" },
            user: { id: btoa("user-e2e"), name: "e2euser", displayName: "E2E User" },
            challenge, pubKeyCredParams: [{ type: "public-key", alg: -7 }],
            timeout: 60000, attestation: "none",
          },
          sessionToken: "enroll-tok",
        }),
      });
    });
    await page.route(`${API_PREFIX}/account/passkeys/enroll/finish`, async (route: Route) => {
      enrollDone = true;
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ enrolled: true }) });
    });

    // PHASE 1: Recover via recovery code.
    await page.goto("/login");
    await page.getByText(/recovery code/i).click();
    await page.getByPlaceholder("Email").fill("e2e@test.com");
    await page.getByPlaceholder("Recovery code").fill("VALIDCODE123456");
    await page.getByText("Recover account").click();

    // Should land on settings page with enrollment banner.
    await expect(page).toHaveURL(/\/settings\/passkeys/, { timeout: 5000 });
    await expect(page.getByText(/recovery code to sign in/i)).toBeVisible();

    // Click "Add passkey".
    await page.getByRole("button", { name: "Add passkey" }).click();

    // After enrollment, the new passkey should appear.
    await expect(page.getByText("New Passkey")).toBeVisible({ timeout: 15000 });

    await cdp.send("WebAuthn.removeVirtualAuthenticator", { authenticatorId });
    await cdp.detach();
    await page.close();
  });
