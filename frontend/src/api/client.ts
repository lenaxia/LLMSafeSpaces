import { getEnv } from "../env";
import type { ApiError } from "./types";

export class ApiClientError extends Error {
  status: number;
  body: ApiError;
  constructor(status: number, body: ApiError) {
    super(body.error);
    this.name = "ApiClientError";
    this.status = status;
    this.body = body;
  }
}

// noRedirectPaths lists request paths that MUST NOT trigger the global
// 401→/login redirect. Two categories:
//   * /auth/me — the app's own "am I logged in?" probe; a 401 here is
//     the normal not-logged-in signal, not an auth failure.
//   * /auth/register — 401 from Turnstile CAPTCHA. Users on /register
//     who fail the CAPTCHA must stay on /register to re-challenge;
//     redirecting them to /login mid-flow loses their form input and
//     hides the failure from the RegisterForm's own error handler.
const noRedirectPaths = new Set(["/auth/me", "/auth/register"]);

async function handleUnauthorized(status: number, path: string, body: ApiError): Promise<void> {
  // Fast-path: exclude paths that legitimately produce 401s during a
  // logged-in-user flow (see noRedirectPaths).
  if (noRedirectPaths.has(path)) return;
  // Also exclude turnstile_failed 401s from any path. Turnstile
  // middleware is a fail-closed gate, not an auth failure — losing
  // the current page + form input would degrade UX with no gain.
  if (body?.error === "turnstile_failed") return;
  if (status === 401 && !window.location.pathname.startsWith("/login")) {
    const current = window.location.pathname + window.location.search;
    const params = new URLSearchParams({ return_to: current });
    window.location.href = `/login?${params.toString()}`;
  }
}

async function request<T>(
  path: string,
  options: RequestInit = {},
): Promise<T> {
  const { apiBaseUrl } = getEnv();
  const url = `${apiBaseUrl}${path}`;
  // FormData bodies must NOT carry an explicit Content-Type — the browser
  // sets the multipart boundary. Every other body is JSON.
  const isFormData = options.body instanceof FormData;
  const res = await fetch(url, {
    ...options,
    credentials: "include",
    headers: {
      ...(isFormData ? {} : { "Content-Type": "application/json" }),
      ...options.headers as Record<string, string>,
    },
  });

  if (!res.ok) {
    const body = await res.json().catch(() => ({ error: res.statusText }));
    await handleUnauthorized(res.status, path, body);
    throw new ApiClientError(res.status, body);
  }

  if (res.status === 204) return undefined as T;
  // Guard against empty bodies on success statuses that are typed as
  // void (e.g., the prompt endpoint historically returned 202 with no
  // body). Without this, res.json() throws "Unexpected end of JSON
  // input". Matches the defensive pattern in sdks/typescript/src/client.ts.
  const text = await res.text();
  if (text === "") return undefined as T;
  return JSON.parse(text) as T;
}

export const api = {
  get: <T>(path: string, options?: RequestInit) => request<T>(path, options),
  post: <T>(path: string, body?: unknown, extraHeaders?: Record<string, string>) =>
    request<T>(path, {
      method: "POST",
      body: body !== undefined ? JSON.stringify(body) : undefined,
      headers: extraHeaders,
    }),
  put: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: "PUT", body: body !== undefined ? JSON.stringify(body) : undefined }),
  delete: <T>(path: string) => request<T>(path, { method: "DELETE" }),
  patch: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: "PATCH", body: body !== undefined ? JSON.stringify(body) : undefined }),
  // Multipart upload (Epic 67): the browser sets the Content-Type boundary.
  upload: <T>(path: string, form: FormData) =>
    request<T>(path, { method: "POST", body: form }),
};

export async function getRaw<T>(path: string): Promise<{ data: T; headers: Headers }> {
  const { apiBaseUrl } = getEnv();
  const url = `${apiBaseUrl}${path}`;
  const res = await fetch(url, {
    credentials: "include",
    headers: { "Content-Type": "application/json" },
  });

  if (!res.ok) {
    const body = await res.json().catch(() => ({ error: res.statusText }));
    await handleUnauthorized(res.status, path, body);
    throw new ApiClientError(res.status, body);
  }

  if (res.status === 204) return { data: undefined as T, headers: res.headers };
  const text = await res.text();
  if (text === "") return { data: undefined as T, headers: res.headers };
  return { data: JSON.parse(text) as T, headers: res.headers };
}

/**
 * Streaming fetch for chat messages. Returns the raw Response for
 * ReadableStream consumption.
 */
export async function streamRequest(
  path: string,
  body: unknown,
): Promise<Response> {
  const { apiBaseUrl } = getEnv();
  const url = `${apiBaseUrl}${path}`;
  const res = await fetch(url, {
    method: "POST",
    credentials: "include",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    await handleUnauthorized(res.status, path, err);
    throw new ApiClientError(res.status, err);
  }
  return res;
}
