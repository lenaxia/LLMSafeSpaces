import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor, fireEvent, act } from "@testing-library/react";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AuthProvider } from "../providers/AuthProvider";
import { LoginPage } from "./LoginPage";

// Default mocks — individual tests override via mockConfig/mockDomains.
const mockGetConfig = vi.fn();
const mockDomains = vi.fn();
const mockLookup = vi.fn();
const mockLoginApi = vi.fn();

vi.mock("../api/auth", () => ({
  authApi: {
    me: vi.fn().mockRejectedValue(new Error("401")),
    getConfig: () => mockGetConfig(),
    login: (...args: unknown[]) => mockLoginApi(...args),
    lookup: (email: string) => mockLookup(email),
  },
}));

vi.mock("../api/sso", () => ({
  ssoApi: {
    domains: () => mockDomains(),
  },
  ssoRedirectURL: (orgSlug: string) => `/api/v1/auth/sso/${orgSlug}/start`,
}));

function renderLoginPage() {
  return render(
    <AuthProvider>
      <MemoryRouter>
        <LoginPage />
      </MemoryRouter>
    </AuthProvider>,
  );
}

describe("LoginPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockGetConfig.mockResolvedValue({
      registrationEnabled: true, passkeyEnabled: false, passkeyDefaultSignup: false,
      oidcEnabled: false,
      instanceName: "TestSpace",
    });
    mockDomains.mockResolvedValue({ domains: [] });
    mockLookup.mockResolvedValue({ redirectUrl: "https://example.com" });
    // Clear query params between tests
    window.history.replaceState({}, "", "/login");
  });

  it("renders sign in form", async () => {
    renderLoginPage();
    await waitFor(() => expect(screen.getByText("Welcome to TestSpace")).toBeInTheDocument());
    expect(screen.getByPlaceholderText("Email")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Password")).toBeInTheDocument();
  });

  it("shows register link when registration is enabled", async () => {
    renderLoginPage();
    await waitFor(() => expect(screen.getByText("Create an account")).toBeInTheDocument());
  });

  it("shows SSO button when email domain matches claimed domain", async () => {
    mockGetConfig.mockResolvedValue({
      registrationEnabled: true, passkeyEnabled: false, passkeyDefaultSignup: false,
      oidcEnabled: true,
      instanceName: "TestSpace",
    });
    mockDomains.mockResolvedValue({
      domains: [{ domain: "acme.com", orgSlug: "acme", orgName: "Acme" }],
    });

    renderLoginPage();
    await waitFor(() => expect(screen.getByPlaceholderText("Email")).toBeInTheDocument());

    fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "alice@acme.com" } });

    await waitFor(() => {
      expect(screen.getByText("Sign in with Acme")).toBeInTheDocument();
    });
  });

  it("shows Continue button when SSO enabled and email domain does not match", async () => {
    mockGetConfig.mockResolvedValue({
      registrationEnabled: true, passkeyEnabled: false, passkeyDefaultSignup: false,
      oidcEnabled: true,
      instanceName: "TestSpace",
    });
    mockDomains.mockResolvedValue({
      domains: [{ domain: "acme.com", orgSlug: "acme", orgName: "Acme" }],
    });

    renderLoginPage();
    await waitFor(() => expect(screen.getByPlaceholderText("Email")).toBeInTheDocument());

    // BYO email — domain doesn't match any claimed domain
    fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "alice@gmail.com" } });

    await waitFor(() => {
      expect(screen.getByText("Continue with email")).toBeInTheDocument();
    });
    // SSO button should NOT appear (no domain match)
    expect(screen.queryByText("Sign in with Acme")).not.toBeInTheDocument();
  });

  it("does not show Continue button when SSO is disabled", async () => {
    // oidcEnabled: false — no SSO configured, discovery is pointless
    renderLoginPage();
    await waitFor(() => expect(screen.getByPlaceholderText("Email")).toBeInTheDocument());

    fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "alice@gmail.com" } });

    expect(screen.queryByText("Continue with email")).not.toBeInTheDocument();
  });

  it("does not show Continue button for invalid email", async () => {
    mockGetConfig.mockResolvedValue({
      registrationEnabled: true, passkeyEnabled: false, passkeyDefaultSignup: false,
      oidcEnabled: true,
      instanceName: "TestSpace",
    });
    mockDomains.mockResolvedValue({
      domains: [{ domain: "acme.com", orgSlug: "acme", orgName: "Acme" }],
    });

    renderLoginPage();
    await waitFor(() => expect(screen.getByPlaceholderText("Email")).toBeInTheDocument());

    // No dot in domain part — invalid
    fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "alice@" } });

    expect(screen.queryByText("Continue with email")).not.toBeInTheDocument();
  });

  it("calls lookup and redirects on Continue click", async () => {
    mockGetConfig.mockResolvedValue({
      registrationEnabled: true, passkeyEnabled: false, passkeyDefaultSignup: false,
      oidcEnabled: true,
      instanceName: "TestSpace",
    });
    mockDomains.mockResolvedValue({
      domains: [{ domain: "acme.com", orgSlug: "acme", orgName: "Acme" }],
    });
    const redirectUrl = "/api/v1/auth/sso/acme/start";
    mockLookup.mockResolvedValue({ redirectUrl });

    // Spy on window.location.href setter — restores automatically via spyOn.
    const hrefSetter = vi.fn();
    const originalDescriptor = Object.getOwnPropertyDescriptor(window, "location");
    Object.defineProperty(window, "location", {
      value: { href: "https://localhost/login" },
      writable: true,
      configurable: true,
    });
    Object.defineProperty(window.location, "href", {
      get: () => "https://localhost/login",
      set: hrefSetter,
      configurable: true,
    });

    try {
      renderLoginPage();
      await waitFor(() => expect(screen.getByPlaceholderText("Email")).toBeInTheDocument());

      fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "alice@gmail.com" } });

      const continueBtn = await screen.findByText("Continue with email");
      fireEvent.click(continueBtn);

      await waitFor(() => {
        expect(mockLookup).toHaveBeenCalledWith("alice@gmail.com");
      });
      await waitFor(() => {
        expect(hrefSetter).toHaveBeenCalledWith(redirectUrl);
      });
    } finally {
      // Restore original window.location so subsequent tests aren't affected.
      if (originalDescriptor) {
        Object.defineProperty(window, "location", originalDescriptor);
      }
    }
  });

  it("shows not-found message when lookup returns not-found redirect", async () => {
    mockGetConfig.mockResolvedValue({
      registrationEnabled: true, passkeyEnabled: false, passkeyDefaultSignup: false,
      oidcEnabled: true,
      instanceName: "TestSpace",
    });
    mockDomains.mockResolvedValue({
      domains: [{ domain: "acme.com", orgSlug: "acme", orgName: "Acme" }],
    });
    // US-54.1 returns { redirectUrl: "/?lookup=not_found" } for unknown emails.
    // The frontend must handle this in-memory (SPA routing would lose the param).
    mockLookup.mockResolvedValue({ redirectUrl: "/?lookup=not_found" });

    renderLoginPage();
    await waitFor(() => expect(screen.getByPlaceholderText("Email")).toBeInTheDocument());

    fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "nobody@gmail.com" } });

    const continueBtn = await screen.findByText("Continue with email");
    fireEvent.click(continueBtn);

    await waitFor(() => {
      expect(screen.getByText(/couldn't find an account/i)).toBeInTheDocument();
    });
    // The lookup was called; no navigation happened.
    expect(mockLookup).toHaveBeenCalledWith("nobody@gmail.com");
  });

  it("shows rate-limited message when lookup returns 429", async () => {
    mockGetConfig.mockResolvedValue({
      registrationEnabled: true, passkeyEnabled: false, passkeyDefaultSignup: false,
      oidcEnabled: true,
      instanceName: "TestSpace",
    });
    mockDomains.mockResolvedValue({
      domains: [{ domain: "acme.com", orgSlug: "acme", orgName: "Acme" }],
    });

    const { ApiClientError } = await import("../api/client");
    mockLookup.mockRejectedValue(new ApiClientError(429, { error: "rate limited" }));

    renderLoginPage();
    await waitFor(() => expect(screen.getByPlaceholderText("Email")).toBeInTheDocument());

    fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "alice@gmail.com" } });

    const continueBtn = await screen.findByText("Continue with email");
    fireEvent.click(continueBtn);

    await waitFor(() => {
      expect(screen.getByText(/Too many attempts/i)).toBeInTheDocument();
    });
  });

  it("shows error message when lookup fails with network error", async () => {
    mockGetConfig.mockResolvedValue({
      registrationEnabled: true, passkeyEnabled: false, passkeyDefaultSignup: false,
      oidcEnabled: true,
      instanceName: "TestSpace",
    });
    mockDomains.mockResolvedValue({
      domains: [{ domain: "acme.com", orgSlug: "acme", orgName: "Acme" }],
    });

    mockLookup.mockRejectedValue(new Error("network error"));

    renderLoginPage();
    await waitFor(() => expect(screen.getByPlaceholderText("Email")).toBeInTheDocument());

    fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "alice@gmail.com" } });

    const continueBtn = await screen.findByText("Continue with email");
    fireEvent.click(continueBtn);

    await waitFor(() => {
      expect(screen.getByText(/Something went wrong/i)).toBeInTheDocument();
    });
  });

  it("shows SSO-not-configured message when ?sso=config_error", async () => {
    window.history.replaceState({}, "", "/login?sso=config_error");
    renderLoginPage();
    await waitFor(() => {
      expect(screen.getByText(/not configured on this instance/i)).toBeInTheDocument();
    });
  });

  it("shows org-not-configured message when ?sso=not_configured", async () => {
    window.history.replaceState({}, "", "/login?sso=not_configured");
    renderLoginPage();
    await waitFor(() => {
      expect(screen.getByText(/does not have single sign-on configured/i)).toBeInTheDocument();
    });
  });

  describe("return_to handling", () => {
    it("\"Create an account\" link preserves return_to param", async () => {
      window.history.replaceState({}, "", "/login?return_to=%2Fchat");
      renderLoginPage();
      await waitFor(() => expect(screen.getByText("Create an account")).toBeInTheDocument());
      const link = screen.getByText("Create an account").closest("a");
      expect(link!.getAttribute("href")).toContain("return_to=%2Fchat");
    });

    it("sanitises malicious return_to — link does not carry evil URL", async () => {
      window.history.replaceState({}, "", "/login?return_to=%2F%2Fevil.com");
      renderLoginPage();
      await waitFor(() => expect(screen.getByText("Create an account")).toBeInTheDocument());
      const link = screen.getByText("Create an account").closest("a");
      expect(link!.getAttribute("href")).not.toContain("evil");
    });

    it("\"create an account\" link after lookup not-found preserves return_to", async () => {
      window.history.replaceState({}, "", "/login?return_to=%2Finvitations%2Fabc123");
      mockGetConfig.mockResolvedValue({
        registrationEnabled: true, passkeyEnabled: false, passkeyDefaultSignup: false,
        oidcEnabled: true,
        instanceName: "TestSpace",
      });
      mockDomains.mockResolvedValue({
        domains: [{ domain: "acme.com", orgSlug: "acme", orgName: "Acme" }],
      });
      mockLookup.mockResolvedValue({ redirectUrl: "/?lookup=not_found" });

      renderLoginPage();
      await waitFor(() => expect(screen.getByPlaceholderText("Email")).toBeInTheDocument());

      fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "nobody@gmail.com" } });
      const continueBtn = await screen.findByText("Continue with email");
      fireEvent.click(continueBtn);

      await waitFor(() => {
        expect(screen.getByText(/couldn't find an account/i)).toBeInTheDocument();
      });

      const createLink = screen.getByText("create an account");
      expect(createLink).toBeInTheDocument();
      expect(createLink.closest("a")!.getAttribute("href")).toContain("return_to=%2Finvitations%2Fabc123");
    });

    it("navigates to return_to after successful login", async () => {
      window.history.replaceState({}, "", "/login?return_to=%2Fchat");
      mockLoginApi.mockResolvedValue({ user: { id: "u-1", role: "user" as const } });

      const hrefSetter = vi.fn();
      const originalDescriptor = Object.getOwnPropertyDescriptor(window, "location");

      renderLoginPage();
      await waitFor(() => expect(screen.getByPlaceholderText("Email")).toBeInTheDocument());

      // Mock window.location.href AFTER render so the useEffect could
      // read return_to from the real search params, but BEFORE the
      // login click triggers the redirect.
      Object.defineProperty(window, "location", {
        value: { href: "https://localhost/login" },
        writable: true,
        configurable: true,
      });
      Object.defineProperty(window.location, "href", {
        get: () => "https://localhost/login",
        set: hrefSetter,
        configurable: true,
      });

      fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "a@b.com" } });
      fireEvent.change(screen.getByPlaceholderText("Password"), { target: { value: "password123" } });

      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: /sign in/i }));
      });

      await waitFor(() => {
        expect(mockLoginApi).toHaveBeenCalledWith({
          email: "a@b.com",
          password: "password123",
          rememberMe: false,
        });
      });

      expect(hrefSetter).toHaveBeenCalledWith("/chat");

      if (originalDescriptor) {
        Object.defineProperty(window, "location", originalDescriptor);
      }
    });

    it("does NOT redirect to return_to when login fails", async () => {
      window.history.replaceState({}, "", "/login?return_to=%2Fchat");
      mockLoginApi.mockRejectedValue(new Error("401 invalid credentials"));

      const hrefSetter = vi.fn();
      const originalDescriptor = Object.getOwnPropertyDescriptor(window, "location");

      renderLoginPage();
      await waitFor(() => expect(screen.getByPlaceholderText("Email")).toBeInTheDocument());

      Object.defineProperty(window, "location", {
        value: { href: "https://localhost/login" },
        writable: true,
        configurable: true,
      });
      Object.defineProperty(window.location, "href", {
        get: () => "https://localhost/login",
        set: hrefSetter,
        configurable: true,
      });

      fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "a@b.com" } });
      fireEvent.change(screen.getByPlaceholderText("Password"), { target: { value: "wrong" } });

      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: /sign in/i }));
      });

      // The login promise rejects — the onSubmit handler throws before
      // reaching window.location.href. The user stays on /login.
      expect(hrefSetter).not.toHaveBeenCalled();

      if (originalDescriptor) {
        Object.defineProperty(window, "location", originalDescriptor);
      }
    });
  });

  // Design 0064 (#1669) phase 1 — org-scoped login surface. When
  // /auth/config returns orgContext (request arrived on <slug>.<base> for an
  // SSO-configured org), the login page offers the host org's SSO
  // first-class; on an org subdomain exactly one org SSO button renders (the
  // host org's): the email-matched verified-domain button is suppressed
  // whether it names a different org or the same one.
  describe("orgContext (org-scoped login surface)", () => {
    it("renders the host org's SSO button first-class when orgContext present", async () => {
      mockGetConfig.mockResolvedValue({
        registrationEnabled: true, passkeyEnabled: false, passkeyDefaultSignup: false,
        oidcEnabled: true,
        instanceName: "TestSpace",
        orgContext: { slug: "acme", name: "Acme Corp" },
      });

      renderLoginPage();
      const orgButton = await waitFor(() => {
        const btn = screen.getByText("Sign in with Acme Corp");
        expect(btn).toBeInTheDocument();
        return btn;
      });
      expect(screen.getByPlaceholderText("Password")).toBeInTheDocument();
      // 0064 §6.2 "first-class": the org button renders ABOVE the password
      // form in DOM order (DocumentPosition: the form FOLLOWS the button).
      const passwordForm = screen.getByPlaceholderText("Password").closest("form");
      expect(passwordForm).not.toBeNull();
      expect(orgButton.compareDocumentPosition(passwordForm!)).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
    });

    it("navigates to the org's SSO start URL on org-button click", async () => {
      mockGetConfig.mockResolvedValue({
        registrationEnabled: true, passkeyEnabled: false, passkeyDefaultSignup: false,
        oidcEnabled: true,
        instanceName: "TestSpace",
        orgContext: { slug: "acme", name: "Acme Corp" },
      });

      const hrefSetter = vi.fn();
      const originalDescriptor = Object.getOwnPropertyDescriptor(window, "location");
      Object.defineProperty(window, "location", {
        value: { href: "https://localhost/login" },
        writable: true,
        configurable: true,
      });
      Object.defineProperty(window.location, "href", {
        get: () => "https://localhost/login",
        set: hrefSetter,
        configurable: true,
      });

      try {
        renderLoginPage();
        const orgButton = await waitFor(() => screen.getByText("Sign in with Acme Corp"));
        await act(async () => {
          fireEvent.click(orgButton.closest("button")!);
        });
        // The mocked ssoRedirectURL renders /api/v1/auth/sso/<slug>/start —
        // pinning the SLUG (not just that navigation happened) so a wiring
        // regression (wrong org / hardcoded slug) fails here.
        expect(hrefSetter).toHaveBeenCalledWith("/api/v1/auth/sso/acme/start");
      } finally {
        if (originalDescriptor) {
          Object.defineProperty(window, "location", originalDescriptor);
        }
      }
    });

    it("falls back to a generic label when the org name is absent (C-strict shape)", async () => {
      mockGetConfig.mockResolvedValue({
        registrationEnabled: true, passkeyEnabled: false, passkeyDefaultSignup: false,
        oidcEnabled: true,
        instanceName: "TestSpace",
        orgContext: { slug: "acme" },
      });

      renderLoginPage();
      await waitFor(() => expect(screen.getByText("Sign in with your organisation")).toBeInTheDocument());
    });

    it("suppresses the verified-domain button for a different org when orgContext present", async () => {
      mockGetConfig.mockResolvedValue({
        registrationEnabled: true, passkeyEnabled: false, passkeyDefaultSignup: false,
        oidcEnabled: true,
        instanceName: "TestSpace",
        orgContext: { slug: "acme", name: "Acme Corp" },
      });
      mockDomains.mockResolvedValue({
        domains: [{ domain: "beta.com", orgSlug: "beta", orgName: "Beta Ltd" }],
      });

      renderLoginPage();
      await waitFor(() => expect(screen.getByText("Sign in with Acme Corp")).toBeInTheDocument());
      fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "alice@beta.com" } });
      await waitFor(() => {
        expect(screen.queryByText("Sign in with Beta Ltd")).not.toBeInTheDocument();
      });
    });

    it("renders exactly one org button when the email matches the host org's own verified domain", async () => {
      mockGetConfig.mockResolvedValue({
        registrationEnabled: true, passkeyEnabled: false, passkeyDefaultSignup: false,
        oidcEnabled: true,
        instanceName: "TestSpace",
        orgContext: { slug: "acme", name: "Acme Corp" },
      });
      mockDomains.mockResolvedValue({
        domains: [{ domain: "acme.com", orgSlug: "acme", orgName: "Acme Corp" }],
      });

      renderLoginPage();
      await waitFor(() => expect(screen.getByText("Sign in with Acme Corp")).toBeInTheDocument());
      fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "alice@acme.com" } });
      await waitFor(() => {
        expect(screen.getAllByText("Sign in with Acme Corp")).toHaveLength(1);
      });
    });

    it("renders no org button when orgContext is absent (root-page behavior unchanged)", async () => {
      mockGetConfig.mockResolvedValue({
        registrationEnabled: true, passkeyEnabled: false, passkeyDefaultSignup: false,
        oidcEnabled: true,
        instanceName: "TestSpace",
      });

      renderLoginPage();
      await waitFor(() => expect(screen.getByPlaceholderText("Password")).toBeInTheDocument());
      expect(screen.queryByText(/^Sign in with /)).not.toBeInTheDocument();
    });
  });
});
