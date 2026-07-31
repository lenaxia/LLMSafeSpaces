import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AuthProvider } from "../providers/AuthProvider";
import { LoginPage } from "./LoginPage";

vi.mock("../api/auth", () => ({
  authApi: {
    me: vi.fn().mockRejectedValue(new Error("401")),
    login: vi.fn(),
    getConfig: vi.fn().mockResolvedValue({
      registrationEnabled: true,
      oidcEnabled: false,
      passkeyEnabled: true,
      passkeyDefaultSignup: true,
      instanceName: "TestSpace",
    }),
    lookup: vi.fn(),
  },
}));

vi.mock("../api/sso", () => ({
  ssoApi: { domains: vi.fn().mockResolvedValue({ domains: [] }) },
  ssoRedirectURL: vi.fn(),
}));

vi.mock("@simplewebauthn/browser", () => ({
  startAuthentication: vi.fn(),
  browserSupportsWebAuthn: vi.fn(() => true),
}));

function renderPage() {
  return render(
    <AuthProvider>
      <MemoryRouter>
        <LoginPage />
      </MemoryRouter>
    </AuthProvider>,
  );
}

describe("LoginPage with passkey enabled", () => {
  it("defaults to passkey login mode", async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Sign in with passkey")).toBeInTheDocument();
    });
    expect(screen.queryByPlaceholderText("Password")).not.toBeInTheDocument();
  });

  it("shows 'Use password instead' link", async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Use password instead")).toBeInTheDocument();
    });
  });

  it("shows recovery code link when passkey enabled", async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/recovery code/i)).toBeInTheDocument();
    });
  });
});

import { fireEvent } from "@testing-library/react";

describe("LoginPage passkey interactions", () => {
  it("switches to password mode when 'Use password instead' clicked", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("Use password instead")).toBeInTheDocument());
    fireEvent.click(screen.getByText("Use password instead"));
    await waitFor(() => {
      expect(screen.getByPlaceholderText("Password")).toBeInTheDocument();
    });
    // Passkey login form should be gone, but "Sign in with passkey" button
    // appears in password mode as the switch-back option.
    expect(screen.queryByText("Sign in with passkey")).toBeInTheDocument();
  });

  it("switches to recovery mode when 'Lost your passkey?' clicked", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText(/recovery code/i)).toBeInTheDocument());
    fireEvent.click(screen.getByText(/recovery code/i));
    await waitFor(() => {
      expect(screen.getByPlaceholderText("Recovery code")).toBeInTheDocument();
    });
  });
});
