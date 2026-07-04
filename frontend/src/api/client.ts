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

async function handleUnauthorized(status: number): Promise<void> {
  if (status === 401 && !window.location.pathname.startsWith("/login")) {
    window.location.href = "/login";
  }
}

async function request<T>(
  path: string,
  options: RequestInit = {},
): Promise<T> {
  const { apiBaseUrl } = getEnv();
  const url = `${apiBaseUrl}${path}`;
  const res = await fetch(url, {
    ...options,
    credentials: "include",
    headers: {
      "Content-Type": "application/json",
      ...options.headers,
    },
  });

  if (!res.ok) {
    const body = await res.json().catch(() => ({ error: res.statusText }));
    // Don't redirect on /auth/me 401 — that's the normal "not logged in" check
    if (path !== "/auth/me") {
      await handleUnauthorized(res.status);
    }
    throw new ApiClientError(res.status, body);
  }

  if (res.status === 204) return undefined as T;
  return res.json();
}

export const api = {
  get: <T>(path: string) => request<T>(path),
  post: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: "POST", body: body ? JSON.stringify(body) : undefined }),
  put: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: "PUT", body: body ? JSON.stringify(body) : undefined }),
  delete: <T>(path: string) => request<T>(path, { method: "DELETE" }),
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
    if (path !== "/auth/me") {
      await handleUnauthorized(res.status);
    }
    throw new ApiClientError(res.status, body);
  }

  if (res.status === 204) return { data: undefined as T, headers: res.headers };
  return { data: await res.json(), headers: res.headers };
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
    await handleUnauthorized(res.status);
    throw new ApiClientError(res.status, err);
  }
  return res;
}
