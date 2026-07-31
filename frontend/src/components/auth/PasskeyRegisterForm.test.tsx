import { describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { PasskeyRegisterForm } from "./PasskeyRegisterForm";
import { passkeyApi } from "../../api/passkey";
import { ApiClientError } from "../../api/client";
import { browserSupportsWebAuthn } from "@simplewebauthn/browser";

vi.mock("../../api/passkey");
vi.mock("@simplewebauthn/browser", () => ({
  startRegistration: vi.fn(),
  browserSupportsWebAuthn: vi.fn(() => true),
}));

describe("PasskeyRegisterForm", () => {
  it("renders email + name + submit", () => {
    render(<PasskeyRegisterForm onSuccess={vi.fn()} />);
    expect(screen.getByPlaceholderText("Email")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Name (optional)")).toBeInTheDocument();
    expect(screen.getByText("Create account with passkey")).toBeInTheDocument();
  });

  it("shows unsupported-browser message", () => {
    vi.mocked(browserSupportsWebAuthn).mockReturnValueOnce(false);
    render(<PasskeyRegisterForm onSuccess={vi.fn()} />);
    expect(screen.getByText(/does not support passkeys/i)).toBeInTheDocument();
  });

  it("shows error on account already exists", async () => {
    vi.mocked(passkeyApi.registerBegin).mockRejectedValueOnce(
      new ApiClientError(409, { error: "account already exists" }),
    );

    render(<PasskeyRegisterForm onSuccess={vi.fn()} />);
    fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "dup@b.com" } });
    fireEvent.click(screen.getByText("Create account with passkey"));

    await waitFor(() => {
      expect(screen.getByText(/already exists/i)).toBeInTheDocument();
    });
  });

  it("shows error on NotAllowedError (cancelled)", async () => {
    const { startRegistration } = await import("@simplewebauthn/browser");
    vi.mocked(passkeyApi.registerBegin).mockResolvedValueOnce({ options: {}, sessionToken: "tok" });
    vi.mocked(startRegistration).mockRejectedValueOnce(
      Object.assign(new Error("cancelled"), { name: "NotAllowedError" }),
    );

    render(<PasskeyRegisterForm onSuccess={vi.fn()} />);
    fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "a@b.com" } });
    fireEvent.click(screen.getByText("Create account with passkey"));

    await waitFor(() => {
      expect(screen.getByText(/cancelled or timed out/i)).toBeInTheDocument();
    });
  });
});
