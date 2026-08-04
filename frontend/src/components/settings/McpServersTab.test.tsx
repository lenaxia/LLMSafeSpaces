import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { MemoryRouter } from "react-router-dom";
import { McpServersTab } from "./McpServersTab";
import type { OrgResponse } from "../../api/orgs";

const mockOrgList = vi.fn();
const mockOrgCreate = vi.fn();
const mockUserCreate = vi.fn();
const mockUserList = vi.fn();
const mockOutletContext = vi.fn();

vi.mock("../../api/mcpServers", () => ({
  adminMcpServersApi: { list: vi.fn(), create: vi.fn() },
  orgMcpServersApi: {
    list: (id: string) => mockOrgList(id),
    create: (id: string, req: unknown) => mockOrgCreate(id, req),
  },
  userMcpServersApi: {
    list: () => mockUserList(),
    create: (req: unknown) => mockUserCreate(req),
  },
}));

vi.mock("../../api/secrets", () => ({
  secretsApi: { list: vi.fn().mockResolvedValue({ secrets: [] }) },
}));

vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual<typeof import("react-router-dom")>("react-router-dom");
  return { ...actual, useOutletContext: () => mockOutletContext() };
});

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

function renderOrgTab() {
  mockOutletContext.mockReturnValue({ org: ORG, isAdmin: true });
  return render(
    <MemoryRouter>
      <McpServersTab scope="org" />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  mockOrgList.mockResolvedValue([]);
  mockUserList.mockResolvedValue([]);
});

// Regression (Bug 1): router.tsx mounted <OrgMcpServersTab scope="org" />
// without orgId, so every org-scope API call fell through to the user
// endpoint. This test asserts the org API is called with the org ID from
// outlet context — not the user API.
describe("McpServersTab org-scope routing", () => {
  it("lists via orgMcpServersApi with the org ID from outlet context", async () => {
    renderOrgTab();
    await waitFor(() => expect(mockOrgList).toHaveBeenCalledWith("org-1"));
    expect(mockUserList).not.toHaveBeenCalled();
  });

  it("creates via orgMcpServersApi, not userMcpServersApi", async () => {
    mockOrgCreate.mockResolvedValue({ id: "s1" });
    mockOrgList.mockResolvedValue([]);
    renderOrgTab();
    await waitFor(() => expect(screen.getByText("Add Server")).toBeInTheDocument());
    fireEvent.click(screen.getByText("Add Server"));
    // Fill minimal form fields and submit
    await waitFor(() => expect(screen.getByPlaceholderText("e.g. github-tools")).toBeInTheDocument());
    fireEvent.change(screen.getByPlaceholderText("e.g. github-tools"), { target: { value: "test-server" } });
    fireEvent.change(screen.getByPlaceholderText("https://example.com/mcp"), { target: { value: "https://example.com/mcp" } });
    const submitBtn = screen.getByRole("button", { name: /^create$/i });
    fireEvent.click(submitBtn);
    await waitFor(() => expect(mockOrgCreate).toHaveBeenCalledWith("org-1", expect.objectContaining({ name: "test-server" })));
    expect(mockUserCreate).not.toHaveBeenCalled();
  });
});
