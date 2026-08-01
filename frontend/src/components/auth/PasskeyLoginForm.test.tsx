import { describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { PasskeyLoginForm } from "./PasskeyLoginForm";
import { passkeyApi } from "../../api/passkey";
import { ApiClientError } from "../../api/client";
import { browserSupportsWebAuthn } from "@simplewebauthn/browser";

vi.mock("../../api/passkey");
vi.mock("@simplewebauthn/browser", () => ({
  startAuthentication: vi.fn(),
  browserSupportsWebAuthn: vi.fn(() => true),
}));

describe("PasskeyLoginForm", () => {
  it("renders email input + submit button", () => {
    render(<PasskeyLoginForm onSuccess={vi.fn()} />);
    expect(screen.getByPlaceholderText("Email")).toBeInTheDocument();
    expect(screen.getByText("Sign in with passkey")).toBeInTheDocument();
  });

  it("shows unsupported-browser fallback with password switch", () => {
    vi.mocked(browserSupportsWebAuthn).mockReturnValueOnce(false);
    const onUsePassword = vi.fn();
    render(<PasskeyLoginForm onSuccess={vi.fn()} onUsePassword={onUsePassword} />);
    expect(screen.getByText(/does not support passkeys/i)).toBeInTheDocument();
    expect(screen.getByText("Use password instead")).toBeInTheDocument();
  });

  it("shows error on NotAllowedError (cancelled)", async () => {
    const { startAuthentication } = await import("@simplewebauthn/browser");
    vi.mocked(startAuthentication).mockRejectedValueOnce(
      Object.assign(new Error("cancelled"), { name: "NotAllowedError" }),
    );
    vi.mocked(passkeyApi.loginBegin).mockResolvedValueOnce({ options: { challenge: "abc" } as any, sessionToken: "tok" });

    render(<PasskeyLoginForm onSuccess={vi.fn()} />);
    fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "a@b.com" } });
    fireEvent.click(screen.getByText("Sign in with passkey"));

    await waitFor(() => {
      expect(screen.getByText(/cancelled or timed out/i)).toBeInTheDocument();
    });
  });

  it("shows 'no passkey registered' for that API error", async () => {
    vi.mocked(passkeyApi.loginBegin).mockRejectedValueOnce(
      new ApiClientError(400, { error: "no passkey registered for this account" }),
    );

    render(<PasskeyLoginForm onSuccess={vi.fn()} />);
    fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "a@b.com" } });
    fireEvent.click(screen.getByText("Sign in with passkey"));

    await waitFor(() => {
      expect(screen.getByText(/No passkey is registered/i)).toBeInTheDocument();
    });
  });

  it("calls onUsePassword when password link clicked", () => {
    const onUsePassword = vi.fn();
    render(<PasskeyLoginForm onSuccess={vi.fn()} onUsePassword={onUsePassword} />);
    fireEvent.click(screen.getByText("Use password instead"));
    expect(onUsePassword).toHaveBeenCalled();
  });

  it("calls onRecover when recovery link clicked", () => {
    const onRecover = vi.fn();
    render(<PasskeyLoginForm onSuccess={vi.fn()} onRecover={onRecover} />);
    fireEvent.click(screen.getByText(/recovery code/i));
    expect(onRecover).toHaveBeenCalled();
  });
});

it("shows session-expired message on expired challenge", async () => {
  vi.mocked(passkeyApi.loginBegin).mockRejectedValueOnce(
    new ApiClientError(400, { error: "passkey login failed" }),
  );
  render(<PasskeyLoginForm onSuccess={vi.fn()} />);
  fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "a@b.com" } });
  fireEvent.click(screen.getByText("Sign in with passkey"));
  await waitFor(() => {
    expect(screen.getByText(/session expired/i)).toBeInTheDocument();
  });
});
