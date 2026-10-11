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
  /**
   * #1651: consume a single-use email-verification token (from the emailed
   * /verify-email?token= link). 200 = verified; 404 = unknown token;
   * 410 = expired or already used (see lib/tokenResult.classifyTokenError).
   */
  verifyEmail: (token: string) =>
    api.post<{ verified: boolean }>("/auth/verify-email", { token }),
  /**
   * #1651: resend the verification email. Enumeration-safe: the backend
   * always returns 202 — it never reveals whether the email has an account.
   */
  resendVerification: (email: string) =>
    api.post<void>("/auth/verify-email/resend", { email }),
  /**
   * #1651: request a password-reset email. Always 202 regardless of whether
   * the email exists (enumeration-safe; only existing verified mailboxes
   * actually receive a link).
   */
  requestPasswordReset: (email: string) =>
    api.post<void>("/auth/password-reset/request", { email }),
  /**
   * #1651: confirm a password reset with the single-use token from the
   * emailed /reset-password?token= link. 200 = done; 404 = unknown token;
   * 410 = expired or already used.
   */
  confirmPasswordReset: (token: string, newPassword: string) =>
    api.post<{ reset: boolean }>("/auth/password-reset/confirm", { token, newPassword }),
};
