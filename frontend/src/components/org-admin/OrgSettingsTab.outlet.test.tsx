import { render, screen, waitFor } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { MemoryRouter, Routes, Route, Outlet } from "react-router-dom";
import { ToastProvider } from "../../providers/ToastProvider";
import { OrgAdminSettingsTab } from "./OrgSettingsTab";
import type { OrgResponse } from "../../api/orgs";

// Integration test: does NOT mock react-router-dom. Renders through a real
// <Outlet context={...}> → useOutletContext chain, proving the router→outlet
// →component wiring works end-to-end. The API modules are mocked at the
// module level (network calls); the router hooks are real.

const mockListOrgPolicies = vi.fn();

vi.mock("../../api/policies", () => ({
  policiesApi: {
    listOrg: (id: string) => mockListOrgPolicies(id),
    setOrg: vi.fn().mockResolvedValue(undefined),
  },
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
  mockListOrgPolicies.mockResolvedValue([]);
});

describe("OrgAdminSettingsTab real outlet-context integration", () => {
  it("receives orgId via real <Outlet context> and loads policies", async () => {
    mockListOrgPolicies.mockResolvedValue([
      { key: "max_workspaces_per_member", value: 20, updatedAt: "2026-01-01T00:00:00Z" },
    ]);

    render(
      <ToastProvider>
        <MemoryRouter initialEntries={["/orgs/org-1/settings"]}>
          <Routes>
            <Route path="/orgs/:id" element={<Outlet context={{ org: ORG, isAdmin: true }} />}>
              <Route path="settings" element={<OrgAdminSettingsTab />} />
            </Route>
          </Routes>
        </MemoryRouter>
      </ToastProvider>,
    );

    // The component loaded policies via the real outlet context (org.id =
    // "org-1"), proving the router→outlet→component wiring works.
    await waitFor(() => expect(mockListOrgPolicies).toHaveBeenCalledWith("org-1"));

    // The loaded value renders (20 = the policy value).
    expect(screen.getByText("Workspace Limits")).toBeInTheDocument();
    expect(screen.getByDisplayValue("20")).toBeInTheDocument();
  });
});
