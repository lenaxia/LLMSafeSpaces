import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { MemoryRouter } from "react-router-dom";
import { ToastProvider } from "../../providers/ToastProvider";
import { OrgAdminSettingsTab } from "./OrgSettingsTab";
import type { OrgResponse } from "../../api/orgs";

const mockListOrgPolicies = vi.fn();
const mockSetOrgPolicy = vi.fn();
const mockOutletContext = vi.fn();

vi.mock("../../api/policies", () => ({
  policiesApi: {
    listOrg: (id: string) => mockListOrgPolicies(id),
    setOrg: (id: string, key: string, value: unknown) => mockSetOrgPolicy(id, key, value),
  },
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

function renderTab() {
  mockOutletContext.mockReturnValue({ org: ORG, isAdmin: true });
  return render(
    <ToastProvider>
      <MemoryRouter>
        <OrgAdminSettingsTab />
      </MemoryRouter>
    </ToastProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  mockListOrgPolicies.mockResolvedValue([]);
  mockSetOrgPolicy.mockResolvedValue(undefined);
});

describe("OrgAdminSettingsTab", () => {
  it("renders the three policy cards", async () => {
    renderTab();
    await waitFor(() => expect(screen.getByText("Workspace Limits")).toBeInTheDocument());
    expect(screen.getByText("Model & Provider Restrictions")).toBeInTheDocument();
    expect(screen.getByText("MCP & Image Defaults")).toBeInTheDocument();
  });

  it("loads existing policy values on mount", async () => {
    mockListOrgPolicies.mockResolvedValue([
      { key: "max_workspaces_per_member", value: 10, updatedAt: "2026-01-01T00:00:00Z" },
      { key: "max_active_workspaces_per_member", value: 5, updatedAt: "2026-01-01T00:00:00Z" },
      { key: "allowed_models", value: ["gpt-4o", "claude-sonnet-4"], updatedAt: "2026-01-01T00:00:00Z" },
      { key: "allowed_providers", value: ["openai"], updatedAt: "2026-01-01T00:00:00Z" },
      { key: "max_mcp_servers_per_workspace", value: 3, updatedAt: "2026-01-01T00:00:00Z" },
      { key: "default_runtime", value: "python-3.12", updatedAt: "2026-01-01T00:00:00Z" },
    ]);
    renderTab();
    await waitFor(() => expect(screen.getByDisplayValue("10")).toBeInTheDocument());
    expect(screen.getByDisplayValue("5")).toBeInTheDocument();
    expect(screen.getByDisplayValue("3")).toBeInTheDocument();
    expect(screen.getByDisplayValue("python-3.12")).toBeInTheDocument();
    // Model restriction toggle is ON because allowed_models is non-empty
    expect(screen.getByText("Only listed models are available to members")).toBeInTheDocument();
    // Provider restriction toggle is ON
    expect(screen.getByText("Only listed providers are available to members")).toBeInTheDocument();
  });

  it("saves workspace limits when Save Limits is clicked", async () => {
    renderTab();
    await waitFor(() => expect(screen.getByText("Save Limits")).toBeInTheDocument());

    // Find the max-workspaces input by its nearby label text
    const maxLabel = screen.getByText("Max workspaces per member");
    const container = maxLabel.closest("div")?.parentElement;
    const wsInput = container?.querySelector('input[type="number"]') as HTMLInputElement;
    fireEvent.change(wsInput, { target: { value: "15" } });

    fireEvent.click(screen.getByText("Save Limits"));

    await waitFor(() =>
      expect(mockSetOrgPolicy).toHaveBeenCalledWith("org-1", "max_workspaces_per_member", 15),
    );
  });

  it("saves model restrictions as an array when restrict is enabled", async () => {
    renderTab();
    await waitFor(() => expect(screen.getByText("Restrict allowed models")).toBeInTheDocument());

    // Enable restriction toggle
    const switches = screen.getAllByRole("switch");
    fireEvent.click(switches[0]!); // first switch = restrict models

    // Type model names
    await waitFor(() => expect(screen.getByPlaceholderText(/gpt-4o/)).toBeInTheDocument());
    fireEvent.change(screen.getByPlaceholderText(/gpt-4o/), {
      target: { value: "gpt-4o\nclaude-sonnet-4" },
    });

    fireEvent.click(screen.getByText("Save Models"));
    await waitFor(() =>
      expect(mockSetOrgPolicy).toHaveBeenCalledWith("org-1", "allowed_models", ["gpt-4o", "claude-sonnet-4"]),
    );
  });

  it("saves empty array when model restriction is disabled", async () => {
    // Start with models restricted
    mockListOrgPolicies.mockResolvedValue([
      { key: "allowed_models", value: ["gpt-4o"], updatedAt: "2026-01-01T00:00:00Z" },
    ]);
    renderTab();
    await waitFor(() => expect(screen.getByText("Only listed models are available to members")).toBeInTheDocument());

    // Disable restriction toggle
    const switches = screen.getAllByRole("switch");
    fireEvent.click(switches[0]!); // toggle off

    fireEvent.click(screen.getByText("Save Models"));
    await waitFor(() =>
      expect(mockSetOrgPolicy).toHaveBeenCalledWith("org-1", "allowed_models", []),
    );
  });

  it("saves MCP cap and default runtime together", async () => {
    renderTab();
    await waitFor(() => expect(screen.getByText("Save Settings")).toBeInTheDocument());

    // The MCP cap is the last number input (spinbutton)
    const inputs = screen.getAllByRole("spinbutton");
    fireEvent.change(inputs[inputs.length - 1]!, { target: { value: "5" } });

    // Change default runtime
    const runtimeInput = screen.getByPlaceholderText(/python-3.12-node-22/);
    fireEvent.change(runtimeInput, { target: { value: "my-image" } });

    fireEvent.click(screen.getByText("Save Settings"));
    await waitFor(() =>
      expect(mockSetOrgPolicy).toHaveBeenCalledWith("org-1", "max_mcp_servers_per_workspace", 5),
    );
    await waitFor(() =>
      expect(mockSetOrgPolicy).toHaveBeenCalledWith("org-1", "default_runtime", "my-image"),
    );
  });

  it("shows error toast on save failure", async () => {
    mockSetOrgPolicy.mockRejectedValue(new Error("feature gate rejected"));
    renderTab();
    await waitFor(() => expect(screen.getByText("Save Settings")).toBeInTheDocument());

    fireEvent.click(screen.getByText("Save Settings"));
    await waitFor(() => expect(screen.getByText("Failed to save")).toBeInTheDocument());
  });
});
