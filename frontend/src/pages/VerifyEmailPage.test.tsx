import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor, fireEvent, act } from "@testing-library/react";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { VerifyEmailPage } from "./VerifyEmailPage";
import { ApiClientError } from "../api/client";

const mockVerifyEmail = vi.fn();
const mockResend = vi.fn();

vi.mock("../api/auth", () => ({
  authApi: {
    verifyEmail: (token: string) => mockVerifyEmail(token),
    resendVerification: (email: string) => mockResend(email),
  },
}));

function renderPage() {
  return render(
    <MemoryRouter>
      <VerifyEmailPage />
    </MemoryRouter>,
  );
}

/** Terminal-state URL hygiene: ?token= must be stripped so a refresh cannot
 * re-POST a single-use token (the second POST would 410 "already used"). */
function tokenStrippedFromUrl() {
  return !new URLSearchParams(window.location.search).has("token");
}

describe("VerifyEmailPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    window.history.replaceState({}, "", "/verify-email");
  });

  it("shows resend-oriented state without calling the API when no token is present", async () => {
    renderPage();
    await waitFor(() =>
      expect(screen.getByText(/we'll send a verification link/i)).toBeInTheDocument(),
    );
    // No link was clicked — the page must NOT claim a link is invalid.
    expect(screen.queryByText(/invalid or incomplete/i)).not.toBeInTheDocument();
    expect(mockVerifyEmail).not.toHaveBeenCalled();
  });

  it("consumes the token on mount and shows success on 200", async () => {
    window.history.replaceState({}, "", "/verify-email?token=abc123");
    mockVerifyEmail.mockResolvedValue({ verified: true });

    renderPage();
    await waitFor(() => expect(mockVerifyEmail).toHaveBeenCalledWith("abc123"));
    await waitFor(() =>
      expect(screen.getByText(/email verified/i)).toBeInTheDocument(),
    );
    expect(screen.getByRole("link", { name: /continue to sign in/i })).toHaveAttribute(
      "href",
      "/login",
    );
    await waitFor(() => expect(tokenStrippedFromUrl()).toBe(true));
  });

  it("preserves return_to on the continue-to-sign-in link", async () => {
    window.history.replaceState({}, "", "/verify-email?token=abc&return_to=%2Fchat");
    mockVerifyEmail.mockResolvedValue({ verified: true });

    renderPage();
    await waitFor(() =>
      expect(screen.getByRole("link", { name: /continue to sign in/i })).toBeInTheDocument(),
    );
    expect(screen.getByRole("link", { name: /continue to sign in/i })).toHaveAttribute(
      "href",
      "/login?return_to=%2Fchat",
    );
  });

  it("shows expired state with resend form when the token is expired (410)", async () => {
    window.history.replaceState({}, "", "/verify-email?token=stale");
    mockVerifyEmail.mockRejectedValue(
      new ApiClientError(410, { error: "token expired" }),
    );

    renderPage();
    await waitFor(() =>
      expect(screen.getByText(/verification link has expired/i)).toBeInTheDocument(),
    );
    expect(screen.getByPlaceholderText("Email")).toBeInTheDocument();
  });

  it("prefills the resend email from ?email= context", async () => {
    window.history.replaceState({}, "", "/verify-email?token=stale&email=a%40b.com");
    mockVerifyEmail.mockRejectedValue(
      new ApiClientError(410, { error: "token expired" }),
    );

    renderPage();
    await waitFor(() =>
      expect(screen.getByPlaceholderText("Email")).toHaveValue("a@b.com"),
    );
  });

  it("shows already-verified guidance when the token was already used (410)", async () => {
    window.history.replaceState({}, "", "/verify-email?token=used");
    mockVerifyEmail.mockRejectedValue(
      new ApiClientError(410, { error: "token already used" }),
    );

    renderPage();
    await waitFor(() =>
      expect(screen.getByText(/already been used/i)).toBeInTheDocument(),
    );
    expect(screen.getByRole("link", { name: /sign in/i })).toHaveAttribute("href", "/login");
    // Already-used means the email IS verified — resend is not offered.
    expect(screen.queryByPlaceholderText("Email")).not.toBeInTheDocument();
  });

  it("shows invalid-link state with resend option on 404", async () => {
    window.history.replaceState({}, "", "/verify-email?token=ghost");
    mockVerifyEmail.mockRejectedValue(new ApiClientError(404, { error: "token not found" }));

    renderPage();
    await waitFor(() =>
      expect(screen.getByText(/verification link is invalid or incomplete/i)).toBeInTheDocument(),
    );
    expect(screen.getByPlaceholderText("Email")).toBeInTheDocument();
  });
  it("shows retry on network error", async () => {
    window.history.replaceState({}, "", "/verify-email?token=abc");
    mockVerifyEmail.mockRejectedValue(new Error("network down"));

    renderPage();
    await waitFor(() =>
      expect(screen.getByText(/something went wrong/i)).toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: /try again/i })).toBeInTheDocument();
  });

  describe("resend", () => {
    it("posts the email and shows a neutral confirmation on 202 (no enumeration)", async () => {
      window.history.replaceState({}, "", "/verify-email?token=stale&email=a%40b.com");
      mockVerifyEmail.mockRejectedValue(
        new ApiClientError(410, { error: "token expired" }),
      );
      mockResend.mockResolvedValue(undefined);

      renderPage();
      const emailInput = await screen.findByPlaceholderText("Email");
      expect(emailInput).toHaveValue("a@b.com");

      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: /send new link/i }));
      });

      await waitFor(() => expect(mockResend).toHaveBeenCalledWith("a@b.com"));
      await waitFor(() =>
        expect(
          screen.getByText(/if that email needs verification, a new link has been sent/i),
        ).toBeInTheDocument(),
      );
      // Enumeration pin: the confirmation must not imply account existence.
      expect(screen.queryByText(/not found/i)).not.toBeInTheDocument();
      expect(screen.queryByText(/no account/i)).not.toBeInTheDocument();
    });

    it("shows rate-limited message on 429", async () => {
      window.history.replaceState({}, "", "/verify-email?token=stale&email=a%40b.com");
      mockVerifyEmail.mockRejectedValue(
        new ApiClientError(410, { error: "token expired" }),
      );
      mockResend.mockRejectedValue(new ApiClientError(429, { error: "rate limited" }));

      renderPage();
      await screen.findByPlaceholderText("Email");
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: /send new link/i }));
      });

      await waitFor(() =>
        expect(screen.getByText(/too many attempts/i)).toBeInTheDocument(),
      );
    });
  });
});
