import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor, fireEvent, act } from "@testing-library/react";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ResetPasswordPage } from "./ResetPasswordPage";
import { ApiClientError } from "../api/client";

const mockRequest = vi.fn();
const mockConfirm = vi.fn();

vi.mock("../api/auth", () => ({
  authApi: {
    requestPasswordReset: (email: string) => mockRequest(email),
    confirmPasswordReset: (token: string, newPassword: string) =>
      mockConfirm(token, newPassword),
  },
}));

function renderPage() {
  return render(
    <MemoryRouter>
      <ResetPasswordPage />
    </MemoryRouter>,
  );
}

describe("ResetPasswordPage — request mode (no token)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    window.history.replaceState({}, "", "/reset-password");
  });

  it("renders the email request form", async () => {
    renderPage();
    expect(await screen.findByPlaceholderText("Email")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /send reset link/i })).toBeInTheDocument();
  });

  it("posts the email and shows the neutral always-202 confirmation", async () => {
    mockRequest.mockResolvedValue(undefined);
    renderPage();
    fireEvent.change(await screen.findByPlaceholderText("Email"), {
      target: { value: "a@b.com" },
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /send reset link/i }));
    });
    await waitFor(() => expect(mockRequest).toHaveBeenCalledWith("a@b.com"));
    await waitFor(() =>
      expect(
        screen.getByText(/if an account exists for that email, a password reset link has been sent/i),
      ).toBeInTheDocument(),
    );
    // Enumeration pin: nothing that implies the account exists or not.
    expect(screen.queryByText(/email not found/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/no account/i)).not.toBeInTheDocument();
  });

  it("shows the SAME confirmation copy when the backend 202s an unknown email (always-202 contract)", async () => {
    // The backend cannot tell us — it always 202s. This pins that the client
    // renders one fixed message regardless; only transport errors differ.
    mockRequest.mockResolvedValue(undefined);
    renderPage();
    fireEvent.change(await screen.findByPlaceholderText("Email"), {
      target: { value: "nobody@nowhere.test" },
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /send reset link/i }));
    });
    await waitFor(() =>
      expect(
        screen.getByText(/if an account exists for that email, a password reset link has been sent/i),
      ).toBeInTheDocument(),
    );
  });

  it("shows rate-limited message on 429", async () => {
    mockRequest.mockRejectedValue(new ApiClientError(429, { error: "rate limited" }));
    renderPage();
    fireEvent.change(await screen.findByPlaceholderText("Email"), {
      target: { value: "a@b.com" },
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /send reset link/i }));
    });
    await waitFor(() =>
      expect(screen.getByText(/too many attempts/i)).toBeInTheDocument(),
    );
  });

  it("shows generic error on network failure and allows retry", async () => {
    mockRequest.mockRejectedValueOnce(new Error("network down")).mockResolvedValue(undefined);
    renderPage();
    fireEvent.change(await screen.findByPlaceholderText("Email"), {
      target: { value: "a@b.com" },
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /send reset link/i }));
    });
    await waitFor(() =>
      expect(screen.getByText(/something went wrong/i)).toBeInTheDocument(),
    );
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /send reset link/i }));
    });
    await waitFor(() =>
      expect(
        screen.getByText(/if an account exists for that email, a password reset link has been sent/i),
      ).toBeInTheDocument(),
    );
  });
});

describe("ResetPasswordPage — confirm mode (?token=)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    window.history.replaceState({}, "", "/reset-password?token=tok123");
  });

  async function fillAndSubmit(password: string, confirm: string) {
    renderPage();
    fireEvent.change(await screen.findByPlaceholderText("New password"), {
      target: { value: password },
    });
    fireEvent.change(screen.getByPlaceholderText("Confirm new password"), {
      target: { value: confirm },
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /reset password/i }));
    });
  }

  it("renders the new-password form and never calls request in confirm mode", async () => {
    renderPage();
    expect(await screen.findByPlaceholderText("New password")).toBeInTheDocument();
    expect(screen.queryByPlaceholderText("Email")).not.toBeInTheDocument();
  });

  it("rejects short passwords client-side without calling the API", async () => {
    await fillAndSubmit("short", "short");
    expect(await screen.findByText(/at least 8 characters/i)).toBeInTheDocument();
    expect(mockConfirm).not.toHaveBeenCalled();
  });

  it("rejects mismatched confirmation client-side without calling the API", async () => {
    await fillAndSubmit("password123", "password456");
    expect(await screen.findByText(/passwords do not match/i)).toBeInTheDocument();
    expect(mockConfirm).not.toHaveBeenCalled();
  });

  it("confirms and continues to /login?reset=success on 200", async () => {
    mockConfirm.mockResolvedValue({ reset: true });
    renderPage();
    fireEvent.change(await screen.findByPlaceholderText("New password"), {
      target: { value: "newpassword1" },
    });
    fireEvent.change(screen.getByPlaceholderText("Confirm new password"), {
      target: { value: "newpassword1" },
    });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /reset password/i }));
    });
    await waitFor(() => expect(mockConfirm).toHaveBeenCalledWith("tok123", "newpassword1"));
    await waitFor(() =>
      expect(screen.getByText(/password has been reset/i)).toBeInTheDocument(),
    );
    // Redirect chain pin: the sign-in CTA targets /login?reset=success
    // (the login page renders the password-updated notice there).
    expect(
      screen.getByRole("link", { name: /sign in with your new password/i }),
    ).toHaveAttribute("href", "/login?reset=success");
  });

  it("shows expired state with request-a-new-link on 410 expired", async () => {
    mockConfirm.mockRejectedValue(new ApiClientError(410, { error: "token expired" }));
    await fillAndSubmit("newpassword1", "newpassword1");
    await waitFor(() =>
      expect(screen.getByText(/reset link has expired/i)).toBeInTheDocument(),
    );
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /request a new link/i }));
    });
    // Back in request mode.
    expect(await screen.findByPlaceholderText("Email")).toBeInTheDocument();
  });

  it("shows already-used guidance on 410 already used", async () => {
    mockConfirm.mockRejectedValue(
      new ApiClientError(410, { error: "token already used" }),
    );
    await fillAndSubmit("newpassword1", "newpassword1");
    await waitFor(() =>
      expect(screen.getByText(/already been used/i)).toBeInTheDocument(),
    );
    expect(screen.getByRole("link", { name: /sign in/i })).toBeInTheDocument();
  });

  it("shows invalid-link state on 404", async () => {
    mockConfirm.mockRejectedValue(new ApiClientError(404, { error: "token not found" }));
    await fillAndSubmit("newpassword1", "newpassword1");
    await waitFor(() =>
      expect(screen.getByText(/reset link is invalid or incomplete/i)).toBeInTheDocument(),
    );
  });

  it("shows generic error with retry on network failure", async () => {
    mockConfirm
      .mockRejectedValueOnce(new Error("network down"))
      .mockResolvedValue({ reset: true });
    await fillAndSubmit("newpassword1", "newpassword1");
    await waitFor(() =>
      expect(screen.getByText(/something went wrong/i)).toBeInTheDocument(),
    );
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /try again/i }));
    });
    await waitFor(() =>
      expect(screen.getByText(/password has been reset/i)).toBeInTheDocument(),
    );
    expect(mockConfirm).toHaveBeenCalledTimes(2);
  });
});
