import { describe, expect, it, vi, afterEach, beforeEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { AuthProvider, useAuth } from "./AuthProvider";

vi.mock("../api/auth", () => ({
  authApi: {
    me: vi.fn(),
    login: vi.fn(),
    register: vi.fn(),
    logout: vi.fn(),
    getConfig: vi.fn(),
  },
}));

import { authApi } from "../api/auth";

function useAuthHook() {
  return renderHook(() => useAuth(), { wrapper: AuthProvider });
}

describe("loginWithToken", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.mocked(authApi.me).mockRejectedValue(new Error("401"));
  });

  afterEach(() => {
    localStorage.clear();
  });

  it("stores token in localStorage and fetches user", async () => {
    // First me() call is from AuthProvider's useEffect (rejects = not logged in).
    // Second me() call is from loginWithToken (resolves with user).
    vi.mocked(authApi.me).mockResolvedValueOnce(null as never);
    vi.mocked(authApi.me).mockResolvedValueOnce({ id: "u1", username: "a", email: "a@b.com", role: "user", active: true, createdAt: "" });
    const { result } = useAuthHook();

    await act(async () => {
      await result.current.loginWithToken("jwt-token-123");
    });

    expect(localStorage.getItem("lsp_token")).toBe("jwt-token-123");
    expect(result.current.user?.id).toBe("u1");
  });

  it("clears token on logout", async () => {
    localStorage.setItem("lsp_token", "old-token");
    vi.mocked(authApi.logout).mockResolvedValueOnce(undefined);
    const { result } = useAuthHook();

    await act(async () => {
      await result.current.logout();
    });

    expect(localStorage.getItem("lsp_token")).toBeNull();
  });
});
