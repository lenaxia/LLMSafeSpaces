import { describe, expect, it, vi, beforeEach } from "vitest";
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
    vi.mocked(authApi.me).mockRejectedValue(new Error("401"));
  });

  it("fetches user after token issued (cookie-based auth)", async () => {
    // AuthProvider's useEffect calls me() first (rejects = not logged in).
    // loginWithToken calls me() again (resolves with user).
    vi.mocked(authApi.me).mockResolvedValueOnce(null as never);
    vi.mocked(authApi.me).mockResolvedValueOnce({
      id: "u1", username: "a", email: "a@b.com", role: "user", active: true, createdAt: "",
    });
    const { result } = useAuthHook();

    await act(async () => {
      await result.current.loginWithToken("cookie-set-by-server");
    });

    // No localStorage token — the server sets the HttpOnly cookie.
    expect(result.current.user?.id).toBe("u1");
  });

  it("clears user state on logout", async () => {
    vi.mocked(authApi.logout).mockResolvedValueOnce(undefined);
    const { result } = useAuthHook();

    await act(async () => {
      await result.current.logout();
    });

    expect(result.current.user).toBeNull();
  });
});
