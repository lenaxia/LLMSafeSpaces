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

import { fireEvent, waitFor } from "@testing-library/react";
import { passkeyApi } from "../api/passkey";
import { startRegistration } from "@simplewebauthn/browser";

vi.mock("../api/passkey", () => ({
  passkeyApi: {
    registerBegin: vi.fn(),
    registerFinish: vi.fn(),
  },
}));

describe("RegisterPage passkey interactions", () => {
  it("transitions to recovery-codes display after registration", async () => {
    vi.mocked(passkeyApi.registerBegin).mockResolvedValueOnce({
      options: { publicKey: { challenge: "abc" } } as unknown as Record<string, unknown>,
      sessionToken: "tok",
    });
    vi.mocked(startRegistration).mockResolvedValueOnce({} as never);
    vi.mocked(passkeyApi.registerFinish).mockResolvedValueOnce({
      token: "jwt",
      recoveryCodes: ["CODE1", "CODE2"],
    });

    renderPage();

    await waitFor(() => expect(screen.getByText("Create account with passkey")).toBeInTheDocument());
    fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "a@b.com" } });
    fireEvent.click(screen.getByText("Create account with passkey"));

    await waitFor(() => {
      expect(screen.getByText("Save your recovery codes")).toBeInTheDocument();
    });
    expect(screen.getByText("CODE1")).toBeInTheDocument();
    expect(screen.getByText("CODE2")).toBeInTheDocument();
  });

  it("switches to password mode when 'Use password instead' clicked", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("Use password instead")).toBeInTheDocument());
    fireEvent.click(screen.getByText("Use password instead"));
    await waitFor(() => {
      expect(screen.getByPlaceholderText("Password")).toBeInTheDocument();
    });
    expect(screen.queryByText("Create account with passkey")).not.toBeInTheDocument();
  });
});
