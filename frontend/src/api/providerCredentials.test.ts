// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach } from "vitest";
import { adminProviderCredentialsApi, userProviderCredentialsApi } from "./providerCredentials";

const mockFetch = vi.fn();
global.fetch = mockFetch;

vi.mock("../env", () => ({
  getEnv: () => ({ apiBaseUrl: "http://localhost:8080/api/v1" }),
}));

beforeEach(() => {
  mockFetch.mockReset();
});

// ─── adminProviderCredentialsApi ─────────────────────────────────────────────

describe("adminProviderCredentialsApi.deleteAutoApply", () => {
  it("sends sentinel '_' for 'all' target type (no targetId in rule)", async () => {
    mockFetch.mockResolvedValueOnce({ ok: true, status: 204, json: () => Promise.resolve(null), text: () => Promise.resolve("null") });

    await adminProviderCredentialsApi.deleteAutoApply("cred-1", "all", undefined);

    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/admin/provider-credentials/cred-1/auto-apply/all/_",
      expect.objectContaining({ method: "DELETE" }),
    );
  });

  it("passes provided targetId through for 'user' target type", async () => {
    mockFetch.mockResolvedValueOnce({ ok: true, status: 204, json: () => Promise.resolve(null), text: () => Promise.resolve("null") });

    await adminProviderCredentialsApi.deleteAutoApply("cred-1", "user", "user-xyz");

    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/admin/provider-credentials/cred-1/auto-apply/user/user-xyz",
      expect.objectContaining({ method: "DELETE" }),
    );
  });

  it("sends sentinel '_' for 'org' type with no targetId", async () => {
    mockFetch.mockResolvedValueOnce({ ok: true, status: 204, json: () => Promise.resolve(null), text: () => Promise.resolve("null") });

    await adminProviderCredentialsApi.deleteAutoApply("cred-1", "org", undefined);

    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/admin/provider-credentials/cred-1/auto-apply/org/_",
      expect.objectContaining({ method: "DELETE" }),
    );
  });
});

// ─── userProviderCredentialsApi ───────────────────────────────────────────────

describe("userProviderCredentialsApi.listBindings", () => {
  it("fetches from /provider-credentials/:id/bindings", async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ workspaceIds: ["ws-1", "ws-2"], bindings: [{ workspaceId: "ws-1", sourceType: "explicit" }, { workspaceId: "ws-2", sourceType: "auto" }] }), text: () => Promise.resolve(JSON.stringify({ workspaceIds: ["ws-1", "ws-2"], bindings: [{ workspaceId: "ws-1", sourceType: "explicit" }, { workspaceId: "ws-2", sourceType: "auto" }] })),
    });

    const result = await userProviderCredentialsApi.listBindings("cred-abc");

    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/provider-credentials/cred-abc/bindings",
      expect.objectContaining({ credentials: "include" }),
    );
    expect(result.workspaceIds).toEqual(["ws-1", "ws-2"]);
  });

  it("returns empty workspaceIds when none bound", async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ workspaceIds: [] }), text: () => Promise.resolve(JSON.stringify({ workspaceIds: [] })),
    });

    const result = await userProviderCredentialsApi.listBindings("cred-abc");
    expect(result.workspaceIds).toEqual([]);
  });
});
