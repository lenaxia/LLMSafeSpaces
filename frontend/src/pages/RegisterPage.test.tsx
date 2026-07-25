import { describe, expect, it, vi } from "vitest";
import { screen, waitFor, fireEvent, act } from "@testing-library/react";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AuthProvider } from "../providers/AuthProvider";
import { RegisterPage } from "./RegisterPage";

const mockRegister = vi.fn();

vi.mock("../api/auth", () => ({
  authApi: {
    me: vi.fn().mockRejectedValue(new Error("401")),
    register: (...args: unknown[]) => mockRegister(...args),
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

    it("navigates to return_to after successful register", async () => {
      window.history.replaceState({}, "", "/register?return_to=%2Fchat");
      mockRegister.mockResolvedValue({ user: { id: "u-1", role: "user" as const } });

      const hrefSetter = vi.fn();
      const originalDescriptor = Object.getOwnPropertyDescriptor(window, "location");

      renderRegisterPage();
      await waitFor(() => expect(screen.getByPlaceholderText("Username")).toBeInTheDocument());

      Object.defineProperty(window, "location", {
        value: { href: "https://localhost/register" },
        writable: true,
        configurable: true,
      });
      Object.defineProperty(window.location, "href", {
        get: () => "https://localhost/register",
        set: hrefSetter,
        configurable: true,
      });

      fireEvent.change(screen.getByPlaceholderText("Username"), { target: { value: "alice" } });
      fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "a@b.com" } });
      fireEvent.change(screen.getByPlaceholderText("Password"), { target: { value: "password123" } });

      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: /create account/i }));
      });

      await waitFor(() => {
        expect(hrefSetter).toHaveBeenCalledWith("/chat");
      });

      if (originalDescriptor) {
        Object.defineProperty(window, "location", originalDescriptor);
      }
    });
  });
});
