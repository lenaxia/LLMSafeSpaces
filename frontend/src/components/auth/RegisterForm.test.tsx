import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { RegisterForm } from "./RegisterForm";

// Mock the TurnstileWidget so we can drive it synchronously in the
// enabled-path tests below without hitting Cloudflare's real CDN. The
// mock exposes its callbacks via module-scoped refs so tests can fire
// the "user completed challenge" flow.
let mockTurnstileCallback: ((token: string) => void) | null = null;
vi.mock("./TurnstileWidget", () => ({
  TurnstileWidget: (props: {
    siteKey: string;
    onToken: (token: string) => void;
    onExpire?: () => void;
    onError?: (err: string) => void;
    className?: string;
  }) => {
    mockTurnstileCallback = props.onToken;
    return (
      <div data-testid="turnstile-widget" data-sitekey={props.siteKey} />
    );
  },
}));

// Mock the env module so we can flip turnstileSiteKey per test.
let mockSiteKey = "";
vi.mock("../../env", () => ({
  getEnv: () => ({ apiBaseUrl: "/api/v1", turnstileSiteKey: mockSiteKey }),
}));

describe("RegisterForm", () => {
  beforeEach(() => {
    mockSiteKey = "";
    mockTurnstileCallback = null;
  });

  afterEach(() => {
    mockSiteKey = "";
    mockTurnstileCallback = null;
  });

  it("renders username, email, and password fields", () => {
    render(<RegisterForm onSubmit={vi.fn()} />);
    expect(screen.getByPlaceholderText("Username")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Email")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Password")).toBeInTheDocument();
  });

  it("renders create account button", () => {
    render(<RegisterForm onSubmit={vi.fn()} />);
    expect(screen.getByRole("button", { name: "Create account" })).toBeInTheDocument();
  });

  it("calls onSubmit with all fields plus empty turnstile token when Turnstile disabled", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    render(<RegisterForm onSubmit={onSubmit} />);

    await user.type(screen.getByPlaceholderText("Username"), "bob");
    await user.type(screen.getByPlaceholderText("Email"), "bob@test.com");
    await user.type(screen.getByPlaceholderText("Password"), "password123");
    await user.click(screen.getByRole("button", { name: "Create account" }));

    // 4th arg is the Turnstile token; empty when disabled.
    expect(onSubmit).toHaveBeenCalledWith("bob", "bob@test.com", "password123", "");
  });

  it("shows loading state during submission", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn((): Promise<void> => new Promise(() => {}));
    render(<RegisterForm onSubmit={onSubmit} />);

    await user.type(screen.getByPlaceholderText("Username"), "bob");
    await user.type(screen.getByPlaceholderText("Email"), "bob@test.com");
    await user.type(screen.getByPlaceholderText("Password"), "password123");
    await user.click(screen.getByRole("button", { name: "Create account" }));

    expect(screen.getByRole("button", { name: "Creating account..." })).toBeDisabled();
  });

  it("shows error on failure", async () => {
    const user = userEvent.setup();
    const { ApiClientError } = await import("../../api/client");
    const onSubmit = vi.fn().mockRejectedValue(
      new ApiClientError(409, { error: "username taken" }),
    );
    render(<RegisterForm onSubmit={onSubmit} />);

    await user.type(screen.getByPlaceholderText("Username"), "bob");
    await user.type(screen.getByPlaceholderText("Email"), "bob@test.com");
    await user.type(screen.getByPlaceholderText("Password"), "password123");
    await user.click(screen.getByRole("button", { name: "Create account" }));

    await waitFor(() => {
      expect(screen.getByText("username taken")).toBeInTheDocument();
    });
  });

  it("password field has minLength 8", () => {
    render(<RegisterForm onSubmit={vi.fn()} />);
    expect(screen.getByPlaceholderText("Password")).toHaveAttribute("minlength", "8");
  });

  // --- Turnstile-enabled-path tests (from PR #501 review feedback). ---

  it("renders TurnstileWidget when turnstileSiteKey is non-empty", () => {
    mockSiteKey = "0x4AAAAAtestsitekey";
    render(<RegisterForm onSubmit={vi.fn()} />);
    const widget = screen.getByTestId("turnstile-widget");
    expect(widget).toBeInTheDocument();
    expect(widget.getAttribute("data-sitekey")).toBe("0x4AAAAAtestsitekey");
  });

  it("disables submit button until Turnstile issues a token", async () => {
    mockSiteKey = "0x4AAAAAtestsitekey";
    const user = userEvent.setup();
    render(<RegisterForm onSubmit={vi.fn()} />);

    await user.type(screen.getByPlaceholderText("Username"), "bob");
    await user.type(screen.getByPlaceholderText("Email"), "bob@test.com");
    await user.type(screen.getByPlaceholderText("Password"), "password123");

    // Even with all fields filled, submit is disabled — waiting on
    // Turnstile token.
    const button = screen.getByRole("button", { name: "Create account" });
    expect(button).toBeDisabled();
  });

  it("enables submit and forwards the Turnstile token as onSubmit's 4th arg", async () => {
    mockSiteKey = "0x4AAAAAtestsitekey";
    const user = userEvent.setup();
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    render(<RegisterForm onSubmit={onSubmit} />);

    await user.type(screen.getByPlaceholderText("Username"), "bob");
    await user.type(screen.getByPlaceholderText("Email"), "bob@test.com");
    await user.type(screen.getByPlaceholderText("Password"), "password123");

    // Simulate Cloudflare firing the challenge callback.
    expect(mockTurnstileCallback).not.toBeNull();
    mockTurnstileCallback?.("cf-turnstile-token-xyz");

    const button = screen.getByRole("button", { name: "Create account" });
    await waitFor(() => expect(button).not.toBeDisabled());
    await user.click(button);

    expect(onSubmit).toHaveBeenCalledWith(
      "bob",
      "bob@test.com",
      "password123",
      "cf-turnstile-token-xyz",
    );
  });

  it("clears the Turnstile token on turnstile_failed error, forcing re-challenge", async () => {
    mockSiteKey = "0x4AAAAAtestsitekey";
    const user = userEvent.setup();
    const { ApiClientError } = await import("../../api/client");
    // ApiError only has `error` + optional `code` fields per its type.
    // The full backend response includes `reason` + `detail` too, but
    // the frontend's ApiClientError only cares about `error` for the
    // turnstile_failed branch check.
    const onSubmit = vi.fn().mockRejectedValueOnce(
      new ApiClientError(401, { error: "turnstile_failed" }),
    );
    render(<RegisterForm onSubmit={onSubmit} />);

    await user.type(screen.getByPlaceholderText("Username"), "bob");
    await user.type(screen.getByPlaceholderText("Email"), "bob@test.com");
    await user.type(screen.getByPlaceholderText("Password"), "password123");

    mockTurnstileCallback?.("first-token");

    const button = screen.getByRole("button", { name: "Create account" });
    await waitFor(() => expect(button).not.toBeDisabled());
    await user.click(button);

    // After the turnstile_failed error, the button should re-disable
    // (token cleared, waiting for a new challenge).
    await waitFor(() => expect(button).toBeDisabled());
  });
});
