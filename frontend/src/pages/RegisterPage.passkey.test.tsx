import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AuthProvider } from "../providers/AuthProvider";
import { RegisterPage } from "./RegisterPage";

vi.mock("../api/auth", () => ({
  authApi: {
    me: vi.fn().mockRejectedValue(new Error("401")),
    register: vi.fn(),
    getConfig: vi.fn().mockResolvedValue({
      registrationEnabled: true,
      oidcEnabled: false,
      passkeyEnabled: true,
      passkeyDefaultSignup: true,
      instanceName: "Safe Space",
    }),
  },
}));

vi.mock("@simplewebauthn/browser", () => ({
  startRegistration: vi.fn(),
  browserSupportsWebAuthn: vi.fn(() => true),
}));

function renderPage() {
  return render(
    <AuthProvider>
      <MemoryRouter>
        <RegisterPage />
      </MemoryRouter>
    </AuthProvider>,
  );
}

describe("RegisterPage with passkey enabled", () => {
  it("defaults to passkey mode when passkeyDefaultSignup is true", async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Create account with passkey")).toBeInTheDocument();
    });
    expect(screen.queryByPlaceholderText("Password")).not.toBeInTheDocument();
  });

  it("shows 'Use password instead' button in passkey mode", async () => {
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Use password instead")).toBeInTheDocument();
    });
  });
});
