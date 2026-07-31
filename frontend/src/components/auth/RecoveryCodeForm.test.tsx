import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { RecoveryCodeForm } from "./RecoveryCodeForm";
import { passkeyApi } from "../../api/passkey";
import { ApiClientError } from "../../api/client";

vi.mock("../../api/passkey");

describe("RecoveryCodeForm", () => {
  it("renders email + code inputs", () => {
    render(<RecoveryCodeForm onSuccess={vi.fn()} />);
    expect(screen.getByPlaceholderText("Email")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Recovery code")).toBeInTheDocument();
  });

  it("shows error on invalid code", async () => {
    vi.mocked(passkeyApi.recover).mockRejectedValueOnce(
      new ApiClientError(401, { error: "invalid" }),
    );
    const onSuccess = vi.fn();
    render(<RecoveryCodeForm onSuccess={onSuccess} />);

    fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "a@test.com" } });
    fireEvent.change(screen.getByPlaceholderText("Recovery code"), { target: { value: "BADCODE" } });
    fireEvent.click(screen.getByText("Recover account"));

    await waitFor(() => {
      expect(screen.getByText(/Invalid recovery code/i)).toBeInTheDocument();
    });
    expect(onSuccess).not.toHaveBeenCalled();
  });

  it("calls onSuccess with token on valid code", async () => {
    vi.mocked(passkeyApi.recover).mockResolvedValueOnce({
      token: "jwt-token",
      mustEnrollPasskey: true,
    });
    const onSuccess = vi.fn();
    render(<RecoveryCodeForm onSuccess={onSuccess} />);

    fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "a@test.com" } });
    fireEvent.change(screen.getByPlaceholderText("Recovery code"), { target: { value: "VALIDCODE12" } });
    fireEvent.click(screen.getByText("Recover account"));

    await waitFor(() => expect(onSuccess).toHaveBeenCalledWith("jwt-token", true));
  });

  it("calls onCancel when back button clicked", () => {
    const onCancel = vi.fn();
    render(<RecoveryCodeForm onSuccess={vi.fn()} onCancel={onCancel} />);
    fireEvent.click(screen.getByText("Back to sign in"));
    expect(onCancel).toHaveBeenCalled();
  });
});
