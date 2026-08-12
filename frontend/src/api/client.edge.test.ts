import { describe, expect, it, vi, beforeEach } from "vitest";
import { api, ApiClientError } from "./client";

describe("API client — auth error handling", () => {
  const mockFetch = vi.fn();

  beforeEach(() => {
    vi.stubGlobal("fetch", mockFetch);
  });

  it("401 response throws ApiClientError with status 401", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 401,
      statusText: "Unauthorized",
      json: () => Promise.resolve({ error: "token expired" }), text: () => Promise.resolve(JSON.stringify({ error: "token expired" })),
    });

    try {
      await api.get("/protected");
      expect.fail("should have thrown");
    } catch (e) {
      expect(e).toBeInstanceOf(ApiClientError);
      expect((e as ApiClientError).status).toBe(401);
      expect((e as ApiClientError).body.error).toBe("token expired");
    }
  });

  it("403 response throws with forbidden status", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 403,
      statusText: "Forbidden",
      json: () => Promise.resolve({ error: "not your resource" }), text: () => Promise.resolve(JSON.stringify({ error: "not your resource" })),
    });

    try {
      await api.get("/other-user/resource");
      expect.fail("should have thrown");
    } catch (e) {
      expect((e as ApiClientError).status).toBe(403);
    }
  });

  it("429 response includes rate limit info", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 429,
      statusText: "Too Many Requests",
      json: () => Promise.resolve({ error: "rate limited", retryAfter: 10 }), text: () => Promise.resolve(JSON.stringify({ error: "rate limited", retryAfter: 10 })),
    });

    try {
      await api.post("/workspaces/sb-1/sessions/s1/message", {});
      expect.fail("should have thrown");
    } catch (e) {
      expect((e as ApiClientError).status).toBe(429);
      expect((e as ApiClientError).body).toHaveProperty("retryAfter", 10);
    }
  });

  it("network failure throws with meaningful message", async () => {
    mockFetch.mockRejectedValue(new TypeError("Failed to fetch"));

    await expect(api.get("/anything")).rejects.toThrow("Failed to fetch");
  });

  it("server returns HTML instead of JSON on 500", async () => {
    mockFetch.mockResolvedValue({
      ok: false,
      status: 500,
      statusText: "Internal Server Error",
      json: () => Promise.reject(new Error("not json")),
    });

    try {
      await api.get("/broken");
      expect.fail("should have thrown");
    } catch (e) {
      expect((e as ApiClientError).status).toBe(500);
      expect((e as ApiClientError).body.error).toBe("Internal Server Error");
    }
  });

  it("credentials: include is always set (cookie auth)", async () => {
    mockFetch.mockResolvedValue({ ok: true, status: 200, json: () => Promise.resolve({}), text: () => Promise.resolve(JSON.stringify({})) });
    await api.get("/test");
    expect(mockFetch).toHaveBeenCalledWith(
      expect.any(String),
      expect.objectContaining({ credentials: "include" }),
    );
  });
});
