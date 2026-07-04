import { describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { RegisterForm } from "./RegisterForm";

describe("RegisterForm", () => {
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

  it("calls onSubmit with all fields", async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn().mockResolvedValue(undefined);
    render(<RegisterForm onSubmit={onSubmit} />);

    await user.type(screen.getByPlaceholderText("Username"), "bob");
    await user.type(screen.getByPlaceholderText("Email"), "bob@test.com");
    await user.type(screen.getByPlaceholderText("Password"), "password123");
    await user.click(screen.getByRole("button", { name: "Create account" }));

    expect(onSubmit).toHaveBeenCalledWith("bob", "bob@test.com", "password123");
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
});
