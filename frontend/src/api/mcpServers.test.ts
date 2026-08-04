import { describe, it, expect, vi, beforeEach } from "vitest";
import { adminMcpServersApi, orgMcpServersApi, userMcpServersApi } from "./mcpServers";

const mockFetch = vi.fn();
global.fetch = mockFetch;

vi.mock("../env", () => ({
  getEnv: () => ({ apiBaseUrl: "http://localhost:8080/api/v1" }),
}));

beforeEach(() => {
  mockFetch.mockReset();
});

// Regression: the backend list handler returns gin.H{"servers": out} (an
// envelope), but the API client typed the response as McpServerResponse[]
// (bare array). Without unwrapping, setServers stored the envelope object
// and servers.map threw "n.map is not a function" on render.
describe("mcpServers list envelope unwrap", () => {
  const server = { id: "s1", name: "test", transport: "http", url: "https://x", enabled: true };

  it("adminMcpServersApi.list unwraps {servers:[...]} into a bare array", async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ servers: [server] }),
    });
    const result = await adminMcpServersApi.list();
    expect(Array.isArray(result)).toBe(true);
    expect(result).toHaveLength(1);
    expect(result[0]!.id).toBe("s1");
  });

  it("orgMcpServersApi.list unwraps {servers:[...]} into a bare array", async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ servers: [server] }),
    });
    const result = await orgMcpServersApi.list("org-1");
    expect(Array.isArray(result)).toBe(true);
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/orgs/org-1/mcp-servers",
      expect.objectContaining({ credentials: "include" }),
    );
  });

  it("userMcpServersApi.list unwraps {servers:[...]} into a bare array", async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ servers: [server] }),
    });
    const result = await userMcpServersApi.list();
    expect(Array.isArray(result)).toBe(true);
  });

  it("handles empty envelope {servers:[]}", async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ servers: [] }),
    });
    const result = await adminMcpServersApi.list();
    expect(Array.isArray(result)).toBe(true);
    expect(result).toHaveLength(0);
  });

  it("passes through a bare array if backend ever returns one", async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () => Promise.resolve([server]),
    });
    const result = await adminMcpServersApi.list();
    expect(Array.isArray(result)).toBe(true);
    expect(result).toHaveLength(1);
  });
});
