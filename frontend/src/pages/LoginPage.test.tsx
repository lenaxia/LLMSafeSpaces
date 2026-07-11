import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor, fireEvent, act } from "@testing-library/react";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import * as ReactRouter from "react-router-dom";
import { AuthProvider } from "../providers/AuthProvider";
import { LoginPage } from "./LoginPage";

// Default mocks — individual tests override via mockConfig/mockDomains.
const mockGetConfig = vi.fn();
const mockDomains = vi.fn();
const mockLookup = vi.fn();
const mockLoginApi = vi.fn();
const mockNavigate = vi.fn();

vi.mock("react-router-dom", async (importOriginal) => {
  const actual = (await importOriginal()) as typeof ReactRouter;
  return {
    ...actual,
    useNavigate: () => mockNavigate,
  };
});

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
      registrationEnabled: true,
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
      registrationEnabled: true,
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
      registrationEnabled: true,
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
      registrationEnabled: true,
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
      registrationEnabled: true,
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
      registrationEnabled: true,
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
      registrationEnabled: true,
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
      registrationEnabled: true,
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
        registrationEnabled: true,
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
      mockNavigate.mockClear();

      renderLoginPage();
      await waitFor(() => expect(screen.getByPlaceholderText("Email")).toBeInTheDocument());

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

      expect(mockNavigate).toHaveBeenCalledWith("/chat");
    });
  });
});
