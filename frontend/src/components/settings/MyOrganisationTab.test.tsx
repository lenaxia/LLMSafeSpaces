import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { MyOrganisationTab } from "./MyOrganisationTab";
import type { OrgResponse } from "../../api/orgs";

const mockList = vi.fn();

vi.mock("../../api/orgs", () => ({
  orgsApi: {
    list: () => mockList(),
  },
}));

const ORG: OrgResponse = {
  id: "org-1",
  name: "Acme Corp",
  slug: "acme-corp",
  createdBy: "user-1",
  createdAt: "2026-01-01T00:00:00Z",
  updatedAt: "2026-01-01T00:00:00Z",
  status: "active",
  planId: "enterprise",
  subscriptionStatus: "active",
  userRole: "admin",
  memberCount: 3,
};

function renderTab() {
  return render(
    <MemoryRouter>
      <MyOrganisationTab />
    </MemoryRouter>,
  );
}

describe("MyOrganisationTab", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("shows org info when the user has an org", async () => {
    mockList.mockResolvedValue([ORG]);
    renderTab();
    await waitFor(() => expect(screen.getByText("Acme Corp")).toBeInTheDocument());
    expect(screen.getByText(/3 members/)).toBeInTheDocument();
    expect(screen.getByText("enterprise")).toBeInTheDocument();
  });

  it("shows empty state when the user has no org", async () => {
    mockList.mockResolvedValue([]);
    renderTab();
    await waitFor(() =>
      expect(screen.getByText(/not a member of any organisation/i)).toBeInTheDocument(),
    );
  });

  it("shows invitation hint when the user has no org", async () => {
    mockList.mockResolvedValue([]);
    renderTab();
    await waitFor(() =>
      expect(screen.getByText(/check your email/i)).toBeInTheDocument(),
    );
    expect(screen.getByText(/invitation link/i)).toBeInTheDocument();
  });

  it("shows Manage link for admins", async () => {
    mockList.mockResolvedValue([ORG]);
    renderTab();
    await waitFor(() => expect(screen.getByText(/Manage organisation/)).toBeInTheDocument());
  });
});
