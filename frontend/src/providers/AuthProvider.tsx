import { createContext, useCallback, useContext, useEffect, useState } from "react";
import type { ReactNode } from "react";
import { authApi } from "../api/auth";
import type { AuthResponse, User } from "../api/types";

interface AuthContextValue {
  user: User | null;
  loading: boolean;
  login: (username: string, password: string, rememberMe?: boolean) => Promise<void>;
  loginWithToken: (token: string) => Promise<void>;
  register: (
    username: string,
    email: string,
    password: string,
    turnstileToken?: string,
  ) => Promise<AuthResponse>;
  logout: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    authApi.me().then(setUser).catch(() => setUser(null)).finally(() => setLoading(false));
  }, []);

  const login = useCallback(async (username: string, password: string, rememberMe?: boolean) => {
    const res = await authApi.login({ email: username, password, rememberMe });
    setUser(res.user);
  }, []);

  const loginWithToken = useCallback(async (_token: string) => {
    // The passkey handlers now set the lsp_session cookie (HttpOnly + Secure),
    // matching /auth/login. No localStorage needed — the browser sends the
    // cookie automatically on subsequent requests.
    const user = await authApi.me();
    setUser(user);
  }, []);

  const register = useCallback(
    async (username: string, email: string, password: string, turnstileToken = "") => {
      const res = await authApi.register({ username, email, password }, turnstileToken);
      // #1651: an unverified registration is NOT a logged-in app user —
      // login is 403-gated until the emailed link is clicked. Leave user
      // null in that case so GuestOnly keeps the user on /register to see
      // the verification guidance instead of bouncing them to /chat.
      // (The register response also carries a session cookie; the user
      // can still enter the app via the card's continue path.)
      if (res.user?.emailVerified !== false) {
        setUser(res.user);
      }
      return res;
    },
    [],
  );

  const logout = useCallback(async () => {
    await authApi.logout();
    setUser(null);
  }, []);

  return (
    <AuthContext.Provider value={{ user, loading, login, loginWithToken, register, logout }}>
      {children}
    </AuthContext.Provider>
  );
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used within AuthProvider");
  return ctx;
}
