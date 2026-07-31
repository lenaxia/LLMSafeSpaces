import { createContext, useCallback, useContext, useEffect, useState } from "react";
import type { ReactNode } from "react";
import { authApi } from "../api/auth";
import type { User } from "../api/types";

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
  ) => Promise<void>;
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

  const loginWithToken = useCallback(async (token: string) => {
    // The passkey ceremony endpoints return the token in the JSON body but
    // do NOT set a cookie (unlike /auth/login). Store it in localStorage and
    // inject it into subsequent requests as a Bearer header.
    localStorage.setItem("lsp_token", token);
    const user = await authApi.me();
    setUser(user);
  }, []);

  const register = useCallback(
    async (username: string, email: string, password: string, turnstileToken = "") => {
      const res = await authApi.register({ username, email, password }, turnstileToken);
      setUser(res.user);
    },
    [],
  );

  const logout = useCallback(async () => {
    await authApi.logout();
    localStorage.removeItem("lsp_token");
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
