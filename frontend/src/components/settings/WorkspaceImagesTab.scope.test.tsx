import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, fireEvent, render as rawRender } from "@testing-library/react";
import { MemoryRouter, Routes, Route, Outlet } from "react-router-dom";
import { ToastProvider } from "../../providers/ToastProvider";
import { WorkspaceImagesTab } from "./WorkspaceImagesTab";
import type { OrgResponse } from "../../api/orgs";

const mockGetCatalog = vi.fn();
const mockListConfigs = vi.fn();
const mockCreateConfig = vi.fn();
const mockCreateOrgConfig = vi.fn();
const mockCreatePlatformConfig = vi.fn();
const mockDeleteConfig = vi.fn();
const mockRenameConfig = vi.fn();

vi.mock("../../api/imageFactory", () => ({
  imageFactoryApi: {
    getCatalog: () => mockGetCatalog(),
    listConfigs: () => mockListConfigs(),
    createConfig: (req: unknown) => mockCreateConfig(req),
    createOrgConfig: (orgId: string, req: unknown) => mockCreateOrgConfig(orgId, req),
    createPlatformConfig: (req: unknown) => mockCreatePlatformConfig(req),
    deleteConfig: (hash: string) => mockDeleteConfig(hash),
    renameConfig: (hash: string, name: string) => mockRenameConfig(hash, name),
  },
}));

const CATALOG = {
  architectures: ["linux/amd64"],
  bases: [{ name: "bookworm", version: "0.6.0", image: "img", tag: "0.6.0", isDefault: true }],
  extensions: [{ id: "ffmpeg", type: "apt", value: "ffmpeg", supportedBases: ["bookworm"] }],
  knownFailures: [],
};

const ORG: OrgResponse = {
  id: "org-1", name: "Acme", slug: "acme", createdBy: "u-1",
  createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-01-01T00:00:00Z",
  status: "active", planId: "team", subscriptionStatus: "active",
  userRole: "admin", memberCount: 2,
};

function renderWithOutlet(scope: "user" | "org" | "platform", org?: OrgResponse) {
  if (scope === "org" && org) {
    return rawRender(
      <ToastProvider>
        <MemoryRouter initialEntries={[`/orgs/${org.id}/images`]}>
          <Routes>
            <Route path="/orgs/:id" element={<Outlet context={{ org }} />}>
              <Route path="images" element={<WorkspaceImagesTab scope="org" />} />
            </Route>
          </Routes>
        </MemoryRouter>
      </ToastProvider>,
    );
  }
  if (scope === "platform") {
    return rawRender(
      <ToastProvider>
        <MemoryRouter>
          <WorkspaceImagesTab scope="platform" />
        </MemoryRouter>
      </ToastProvider>,
    );
  }
  return rawRender(
    <ToastProvider>
      <MemoryRouter>
        <WorkspaceImagesTab scope="user" />
      </MemoryRouter>
    </ToastProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  mockGetCatalog.mockResolvedValue(CATALOG);
  mockListConfigs.mockResolvedValue([]);
  mockCreateConfig.mockResolvedValue({ id: "c1", hash: "s-1", name: "test", status: "building", scope: "member" });
  mockCreateOrgConfig.mockResolvedValue({ id: "c2", hash: "s-2", name: "test", status: "building", scope: "org" });
  mockCreatePlatformConfig.mockResolvedValue({ id: "c3", hash: "s-3", name: "test", status: "building", scope: "platform" });
  mockDeleteConfig.mockResolvedValue(undefined);
  mockRenameConfig.mockResolvedValue({ id: "c1", hash: "s-a", name: "renamed", status: "ready", scope: "member" });
});

describe("WorkspaceImagesTab scope routing", () => {
  it("user scope: create calls createConfig (member endpoint)", async () => {
    renderWithOutlet("user");
    await waitFor(() => expect(screen.getByPlaceholderText("e.g. ml-stack")).toBeInTheDocument());
    fireEvent.change(screen.getByPlaceholderText("e.g. ml-stack"), { target: { value: "test" } });
    fireEvent.click(screen.getByRole("checkbox"));
    fireEvent.click(screen.getByRole("button", { name: /Create Personal Image/ }));
    await waitFor(() => expect(mockCreateConfig).toHaveBeenCalled());
    expect(mockCreateOrgConfig).not.toHaveBeenCalled();
    expect(mockCreatePlatformConfig).not.toHaveBeenCalled();
  });

  it("org scope: create calls createOrgConfig with orgId from outlet context", async () => {
    renderWithOutlet("org", ORG);
    await waitFor(() => expect(screen.getByPlaceholderText("e.g. ml-stack")).toBeInTheDocument());
    fireEvent.change(screen.getByPlaceholderText("e.g. ml-stack"), { target: { value: "test" } });
    fireEvent.click(screen.getByRole("checkbox"));
    fireEvent.click(screen.getByRole("button", { name: /Create Org Image/ }));
    await waitFor(() => expect(mockCreateOrgConfig).toHaveBeenCalledWith("org-1", expect.objectContaining({ name: "test" })));
    expect(mockCreateConfig).not.toHaveBeenCalled();
  });

  it("platform scope: create calls createPlatformConfig", async () => {
    renderWithOutlet("platform");
    await waitFor(() => expect(screen.getByPlaceholderText("e.g. ml-stack")).toBeInTheDocument());
    fireEvent.change(screen.getByPlaceholderText("e.g. ml-stack"), { target: { value: "test" } });
    fireEvent.click(screen.getByRole("checkbox"));
    fireEvent.click(screen.getByRole("button", { name: /Create Platform Image/ }));
    await waitFor(() => expect(mockCreatePlatformConfig).toHaveBeenCalled());
    expect(mockCreateConfig).not.toHaveBeenCalled();
  });
});

describe("WorkspaceImagesTab scope grouping (Q3)", () => {
  it("org scope: shows member configs in separate section from org/platform", async () => {
    mockListConfigs.mockResolvedValue([
      { id: "c1", hash: "s-1", name: "Org Image", scope: "org", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
      { id: "c2", hash: "s-2", name: "Member Image", scope: "member", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
    ]);
    renderWithOutlet("org", ORG);
    await waitFor(() => expect(screen.getByText("Org Image")).toBeInTheDocument());
    // Both section headings render
    expect(screen.getByText("Org & Platform Images")).toBeInTheDocument();
    expect(screen.getByText("Member Images")).toBeInTheDocument();
    // Member config is in its own section
    expect(screen.getByText("Member Image")).toBeInTheDocument();
  });

  it("user scope: shows all configs in one flat list", async () => {
    mockListConfigs.mockResolvedValue([
      { id: "c1", hash: "s-1", name: "My Image", scope: "member", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
    ]);
    renderWithOutlet("user");
    await waitFor(() => expect(screen.getByText("My Image")).toBeInTheDocument());
    expect(screen.getByText("My Workspace Images")).toBeInTheDocument();
    expect(screen.queryByText("Member Images")).not.toBeInTheDocument();
    expect(screen.queryByText("Org & Platform Images")).not.toBeInTheDocument();
  });
});

describe("WorkspaceImagesTab edit permissions", () => {
  it("org scope: org configs are editable (rename/delete buttons visible)", async () => {
    mockListConfigs.mockResolvedValue([
      { id: "c1", hash: "s-1", name: "EditableOrgImg", scope: "org", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
    ]);
    renderWithOutlet("org", ORG);
    // Wait for config to render, then click to expand
    await waitFor(() => expect(screen.getByText("EditableOrgImg")).toBeInTheDocument());
    fireEvent.click(screen.getByText("EditableOrgImg"));
    // Rename/Delete buttons should appear after expansion
    await waitFor(() => expect(screen.getByText("Delete")).toBeInTheDocument(), { timeout: 3000 });
  });

  it("platform scope: all configs are editable", async () => {
    mockListConfigs.mockResolvedValue([
      { id: "c1", hash: "s-1", name: "Member Image", scope: "member", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
      { id: "c2", hash: "s-2", name: "Platform Image", scope: "platform", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
    ]);
    renderWithOutlet("platform");
    await waitFor(() => expect(screen.getByText("Member Image")).toBeInTheDocument());
    // Member configs in platform scope are editable
    fireEvent.click(screen.getByText("Member Image"));
    await waitFor(() => expect(screen.getByText("Rename")).toBeInTheDocument());
  });

  // Regression: when scope is org/platform and managed configs exist (no
  // member configs), the flat "My Workspace Images" list must NOT also
  // render them (duplicate display). Pre-fix: managed section + flat list
  // both rendered the same configs.
  it("org scope: managed configs render once, not duplicated in flat list", async () => {
    mockListConfigs.mockResolvedValue([
      { id: "c1", hash: "s-1", name: "OrgOnly", scope: "org", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
    ]);
    renderWithOutlet("org", ORG);
    await waitFor(() => expect(screen.getByText("OrgOnly")).toBeInTheDocument());
    // The flat "My Workspace Images" heading must NOT render for org scope
    // with managed configs — it would duplicate the managed section.
    expect(screen.queryByText("My Workspace Images")).not.toBeInTheDocument();
    // The managed section heading renders.
    expect(screen.getByText("Org & Platform Images")).toBeInTheDocument();
  });
});
