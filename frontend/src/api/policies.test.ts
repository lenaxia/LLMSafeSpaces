import { describe, it, expect, vi, beforeEach } from "vitest";
import { policiesApi } from "./policies";

const mockFetch = vi.fn();
global.fetch = mockFetch;

vi.mock("../env", () => ({
  getEnv: () => ({ apiBaseUrl: "http://localhost:8080/api/v1" }),
}));

beforeEach(() => {
  mockFetch.mockReset();
});

describe("policiesApi", () => {
  it("listOrg GETs /orgs/:id/policies", async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () => Promise.resolve([{ key: "allow_user_mcp_servers", value: true, updatedAt: "2026-01-01T00:00:00Z" }]), text: () => Promise.resolve(JSON.stringify([{ key: "allow_user_mcp_servers", value: true, updatedAt: "2026-01-01T00:00:00Z" }])),
    });
    const result = await policiesApi.listOrg("org-1");
    expect(result).toHaveLength(1);
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/orgs/org-1/policies",
      expect.objectContaining({ credentials: "include" }),
    );
  });

  it("setOrg PUTs a raw bool body (not an object wrapper)", async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ status: "ok" }), text: () => Promise.resolve(JSON.stringify({ status: "ok" })),
    });
    await policiesApi.setOrg("org-1", "allow_user_mcp_servers", true);
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/orgs/org-1/policies/allow_user_mcp_servers",
      expect.objectContaining({ method: "PUT", body: "true" }),
    );
  });

  // Regression for the api.put falsy-body bug: setting false must send a
  // body, not drop it (which would 400 on the backend's json.RawMessage bind).
  it("setOrg sends body 'false' (not undefined) when value is false", async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ status: "ok" }), text: () => Promise.resolve(JSON.stringify({ status: "ok" })),
    });
    await policiesApi.setOrg("org-1", "allow_user_mcp_servers", false);
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/orgs/org-1/policies/allow_user_mcp_servers",
      expect.objectContaining({ method: "PUT", body: "false" }),
    );
  });
});
