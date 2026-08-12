import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";

describe("env", () => {
  beforeEach(() => {
    vi.resetModules();
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("getEnv returns default when not loaded", async () => {
    const { getEnv } = await import("./env");
    expect(getEnv().apiBaseUrl).toBe("/api/v1");
  });

  it("loadEnv fetches and caches env.json", async () => {
    const mockFetch = vi.fn().mockResolvedValue({
      json: () => Promise.resolve({ apiBaseUrl: "http://custom:8080/api/v1" }), text: () => Promise.resolve(JSON.stringify({ apiBaseUrl: "http://custom:8080/api/v1" })),
    });
    vi.stubGlobal("fetch", mockFetch);

    const { loadEnv, getEnv } = await import("./env");
    const env = await loadEnv();
    expect(env.apiBaseUrl).toBe("http://custom:8080/api/v1");
    expect(getEnv().apiBaseUrl).toBe("http://custom:8080/api/v1");
    expect(mockFetch).toHaveBeenCalledWith("/env.json");
  });

  it("loadEnv falls back to defaults on fetch failure", async () => {
    const mockFetch = vi.fn().mockRejectedValue(new Error("network"));
    vi.stubGlobal("fetch", mockFetch);

    const { loadEnv } = await import("./env");
    const env = await loadEnv();
    expect(env.apiBaseUrl).toBe("/api/v1");
  });
});
