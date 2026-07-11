import { describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AuthProvider } from "../providers/AuthProvider";
import { RegisterPage } from "./RegisterPage";

vi.mock("../api/auth", () => ({
  authApi: {
    me: vi.fn().mockRejectedValue(new Error("401")),
    register: vi.fn(),
  },
}));

function renderRegisterPage() {
  return render(
    <AuthProvider>
      <MemoryRouter>
        <RegisterPage />
      </MemoryRouter>
    </AuthProvider>,
  );
}

describe("RegisterPage", () => {
  it("renders create account form", async () => {
    renderRegisterPage();
    await waitFor(() => expect(screen.getByRole("heading", { name: "Create account" })).toBeInTheDocument());
    expect(screen.getByPlaceholderText("Username")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Email")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Password")).toBeInTheDocument();
  });

  it("shows sign in link", async () => {
    renderRegisterPage();
    await waitFor(() => expect(screen.getByText(/already have an account/i)).toBeInTheDocument());
  });

  describe("return_to handling", () => {
    it("\"Already have an account?\" link preserves return_to", async () => {
      window.history.replaceState({}, "", "/register?return_to=%2Fchat");
      renderRegisterPage();
      await waitFor(() => expect(screen.getByText(/already have an account/i)).toBeInTheDocument());
      const link = screen.getByText(/already have an account/i).closest("a");
      expect(link).toBeTruthy();
      expect(link!.getAttribute("href")).toContain("return_to=%2Fchat");
    });

    it("sanitises malicious return_to — sign in link does not carry evil URL", async () => {
      window.history.replaceState({}, "", "/register?return_to=%2F%2Fevil.com");
      renderRegisterPage();
      await waitFor(() => expect(screen.getByText(/already have an account/i)).toBeInTheDocument());
      const link = screen.getByText(/already have an account/i).closest("a");
      expect(link!.getAttribute("href")).not.toContain("evil");
    });
  });
});
