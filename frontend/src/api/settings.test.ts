import { describe, it, expect, vi, beforeEach } from "vitest";
import { settingsApi } from "./settings";

// Mock the fetch function
const mockFetch = vi.fn();
global.fetch = mockFetch;

// Mock env
vi.mock("../env", () => ({
  getEnv: () => ({ apiBaseUrl: "http://localhost:8080/api/v1" }),
}));

beforeEach(() => {
  mockFetch.mockReset();
});

describe("settingsApi", () => {
  describe("getUserSettings", () => {
    it("fetches user settings", async () => {
      mockFetch.mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: () => Promise.resolve({ settings: { theme: "dark" }, schemaVersion: 1 }), text: () => Promise.resolve(JSON.stringify({ settings: { theme: "dark" }, schemaVersion: 1 })),
      });

      const result = await settingsApi.getUserSettings();
      expect(result.settings.theme).toBe("dark");
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/users/me/settings",
        expect.objectContaining({ credentials: "include" }),
      );
    });
  });

  describe("getUserSchema", () => {
    it("fetches user schema", async () => {
      mockFetch.mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: () => Promise.resolve({ settings: [{ key: "theme", type: "enum" }], schemaVersion: 1 }), text: () => Promise.resolve(JSON.stringify({ settings: [{ key: "theme", type: "enum" }], schemaVersion: 1 })),
      });

      const result = await settingsApi.getUserSchema();
      expect(result.settings).toHaveLength(1);
      expect(result.settings[0]!.key).toBe("theme");
    });
  });

  describe("setUserSetting", () => {
    it("sends PUT with value", async () => {
      mockFetch.mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: () => Promise.resolve({ key: "theme", value: "dark" }), text: () => Promise.resolve(JSON.stringify({ key: "theme", value: "dark" })),
      });

      await settingsApi.setUserSetting("theme", "dark");
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/users/me/settings/theme",
        expect.objectContaining({
          method: "PUT",
          body: JSON.stringify({ value: "dark" }),
        }),
      );
    });
  });

  describe("getAdminSettings", () => {
    it("fetches admin settings", async () => {
      mockFetch.mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: () => Promise.resolve({ settings: { "auth.registrationEnabled": true }, schemaVersion: 1 }), text: () => Promise.resolve(JSON.stringify({ settings: { "auth.registrationEnabled": true }, schemaVersion: 1 })),
      });

      const result = await settingsApi.getAdminSettings();
      expect(result.settings["auth.registrationEnabled"]).toBe(true);
    });

    it("throws on 404 (non-admin)", async () => {
      mockFetch.mockResolvedValueOnce({
        ok: false,
        status: 404,
        statusText: "Not Found",
        json: () => Promise.resolve({ error: "Not Found" }), text: () => Promise.resolve(JSON.stringify({ error: "Not Found" })),
      });

      await expect(settingsApi.getAdminSettings()).rejects.toThrow();
    });
  });

  describe("setAdminSetting", () => {
    it("sends PUT with value", async () => {
      mockFetch.mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: () => Promise.resolve({ key: "auth.lockoutAttempts", value: 10 }), text: () => Promise.resolve(JSON.stringify({ key: "auth.lockoutAttempts", value: 10 })),
      });

      await settingsApi.setAdminSetting("auth.lockoutAttempts", 10);
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/admin/settings/auth.lockoutAttempts",
        expect.objectContaining({
          method: "PUT",
          body: JSON.stringify({ value: 10 }),
        }),
      );
    });

    it("throws on validation error", async () => {
      mockFetch.mockResolvedValueOnce({
        ok: false,
        status: 400,
        statusText: "Bad Request",
        json: () => Promise.resolve({ error: "value 999 above maximum 100" }), text: () => Promise.resolve(JSON.stringify({ error: "value 999 above maximum 100" })),
      });

      await expect(settingsApi.setAdminSetting("auth.lockoutAttempts", 999)).rejects.toThrow();
    });
  });
});
