import { render, screen, waitFor } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { MemoryRouter, Routes, Route, Outlet } from "react-router-dom";
import { McpServersTab } from "./McpServersTab";
import type { OrgResponse } from "../../api/orgs";

// This integration test does NOT mock react-router-dom. It renders the
// component through a real <Outlet context={...}> → useOutletContext chain,
// proving the router→outlet→component wiring that Bug 1 broke. The API
// modules are mocked at the module level (network calls); the router hooks
// are real.

const mockOrgList = vi.fn();

vi.mock("../../api/mcpServers", () => ({
  adminMcpServersApi: { list: vi.fn().mockResolvedValue([]), create: vi.fn() },
  orgMcpServersApi: {
    list: (id: string) => mockOrgList(id),
    create: vi.fn().mockResolvedValue({}),
  },
  userMcpServersApi: { list: vi.fn().mockResolvedValue([]), create: vi.fn() },
}));

vi.mock("../../api/secrets", () => ({
  secretsApi: { list: vi.fn().mockResolvedValue({ secrets: [] }) },
}));

const ORG: OrgResponse = {
  id: "org-1",
  name: "Acme",
  slug: "acme",
  createdBy: "u-1",
  createdAt: "2026-01-01T00:00:00Z",
  updatedAt: "2026-01-01T00:00:00Z",
  status: "active",
  planId: "team",
  subscriptionStatus: "active",
  userRole: "admin",
  memberCount: 2,
};

beforeEach(() => {
  vi.clearAllMocks();
  mockOrgList.mockResolvedValue([]);
});

describe("McpServersTab real outlet-context integration", () => {
  it("receives orgId via real <Outlet context> and calls orgMcpServersApi", async () => {
    render(
      <MemoryRouter initialEntries={["/orgs/org-1/mcp-servers"]}>
        <Routes>
          <Route path="/orgs/:id" element={<Outlet context={{ org: ORG, isAdmin: true }} />}>
            <Route path="mcp-servers" element={<McpServersTab scope="org" />} />
          </Route>
        </Routes>
      </MemoryRouter>,
    );

    await waitFor(() => expect(mockOrgList).toHaveBeenCalledWith("org-1"));
    // Empty state renders — no crash, no error.
    expect(screen.getByText("MCP Servers")).toBeInTheDocument();
  });
});
