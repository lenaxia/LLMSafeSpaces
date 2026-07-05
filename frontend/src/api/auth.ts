import { api } from "./client";
import type {
  AuthConfig,
  AuthResponse,
  LoginRequest,
  RegisterRequest,
  User,
} from "./types";

export const authApi = {
  getConfig: () => api.get<AuthConfig>("/auth/config"),
  login: (req: LoginRequest) => api.post<AuthResponse>("/auth/login", req),
  /**
   * POST /auth/register.
   *
   * When the API's Turnstile middleware is enabled (chart:
   * turnstile.enabled=true), callers MUST supply a valid token from
   * the Cloudflare Turnstile widget in the second argument. When
   * Turnstile is disabled server-side, the token is ignored — the
   * frontend can always pass an empty string.
   */
  register: (req: RegisterRequest, turnstileToken = "") =>
    api.post<AuthResponse>(
      "/auth/register",
      req,
      turnstileToken ? { "cf-turnstile-response": turnstileToken } : undefined,
    ),
  logout: () => api.post<void>("/auth/logout"),
  me: () => api.get<User>("/auth/me"),
  /**
   * Epic 54, US-54.1: Email-led login discovery. Resolves an email to a single
   * redirectUrl pointing at the user's org (subdomain when configured, direct
   * SSO start URL otherwise). Enumeration-safe: always 200 with {redirectUrl}.
   */
  lookup: (email: string) =>
    api.post<{ redirectUrl: string }>("/auth/lookup", { email }),
};
