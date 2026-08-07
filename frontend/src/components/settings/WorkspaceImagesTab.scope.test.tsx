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
  it("org scope: shows member configs in separate section from org", async () => {
    mockListConfigs.mockResolvedValue([
      { id: "c1", hash: "s-1", name: "Org Image", scope: "org", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
      { id: "c2", hash: "s-2", name: "Member Image", scope: "member", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
    ]);
    renderWithOutlet("org", ORG);
    await waitFor(() => expect(screen.getByText("Org Image")).toBeInTheDocument());
    // Managed section heading
    expect(screen.getByText("Org Images")).toBeInTheDocument();
    // Member configs in separate section
    expect(screen.getByText("Member Images")).toBeInTheDocument();
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
    expect(screen.queryByText("Org Images")).not.toBeInTheDocument();
    expect(screen.queryByText("Platform Images")).not.toBeInTheDocument();
  });
});

describe("WorkspaceImagesTab edit permissions", () => {
  it("org scope: org configs are editable (rename/delete buttons visible)", async () => {
    mockListConfigs.mockResolvedValue([
      { id: "c1", hash: "s-1", name: "EditableOrgImg", scope: "org", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
    ]);
    renderWithOutlet("org", ORG);
    await waitFor(() => expect(screen.getByText("EditableOrgImg")).toBeInTheDocument());
    fireEvent.click(screen.getByText("EditableOrgImg"));
    await waitFor(() => expect(screen.getByText("Delete")).toBeInTheDocument(), { timeout: 3000 });
  });

  it("platform scope: platform configs are editable, member configs are read-only", async () => {
    mockListConfigs.mockResolvedValue([
      { id: "c1", hash: "s-1", name: "Member Image", scope: "member", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
      { id: "c2", hash: "s-2", name: "Platform Image", scope: "platform", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
    ]);
    renderWithOutlet("platform");
    await waitFor(() => expect(screen.getByText("Platform Image")).toBeInTheDocument());
    // Platform configs are editable
    fireEvent.click(screen.getByText("Platform Image"));
    await waitFor(() => expect(screen.getByText("Rename")).toBeInTheDocument());
  });

  // Regression: member configs must NOT be editable from platform/org tabs.
  // Pre-fix: canEdit returned true for all configs on platform scope, so
  // member configs showed Rename/Delete buttons. This test expands a member
  // config on platform scope and asserts NO edit buttons appear.
  it("platform scope: member configs are read-only (no rename/delete)", async () => {
    mockListConfigs.mockResolvedValue([
      { id: "c1", hash: "s-1", name: "Member Image", scope: "member", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
    ]);
    renderWithOutlet("platform");
    await waitFor(() => expect(screen.getByText("Member Image")).toBeInTheDocument());
    fireEvent.click(screen.getByText("Member Image"));
    // Wait a moment for any potential edit buttons to render
    await new Promise((r) => setTimeout(r, 100));
    expect(screen.queryByText("Rename")).not.toBeInTheDocument();
    expect(screen.queryByText("Delete")).not.toBeInTheDocument();
  });

  // Regression: org tab must also show member configs as read-only.
  it("org scope: member configs are read-only (no rename/delete)", async () => {
    mockListConfigs.mockResolvedValue([
      { id: "c1", hash: "s-1", name: "OrgImage", scope: "org", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
      { id: "c2", hash: "s-2", name: "MemberImage", scope: "member", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
    ]);
    renderWithOutlet("org", ORG);
    await waitFor(() => expect(screen.getByText("MemberImage")).toBeInTheDocument());
    fireEvent.click(screen.getByText("MemberImage"));
    await new Promise((r) => setTimeout(r, 100));
    expect(screen.queryByText("Rename")).not.toBeInTheDocument();
    expect(screen.queryByText("Delete")).not.toBeInTheDocument();
  });

  // Cross-scope visibility: org tab shows platform configs in a separate
  // read-only section.
  it("org scope: shows platform configs in a separate section", async () => {
    mockListConfigs.mockResolvedValue([
      { id: "c1", hash: "s-1", name: "OrgImg", scope: "org", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
      { id: "c2", hash: "s-2", name: "PlatformImg", scope: "platform", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
    ]);
    renderWithOutlet("org", ORG);
    await waitFor(() => expect(screen.getByText("PlatformImg")).toBeInTheDocument());
    expect(screen.getByText("Org Images")).toBeInTheDocument();
    expect(screen.getByText("Platform Images")).toBeInTheDocument();
    // Platform config is read-only on org tab
    fireEvent.click(screen.getByText("PlatformImg"));
    await new Promise((r) => setTimeout(r, 100));
    expect(screen.queryByText("Rename")).not.toBeInTheDocument();
  });

  // Cross-scope visibility: platform tab shows org configs in a separate
  // read-only section.
  it("platform scope: shows org configs in a separate section", async () => {
    mockListConfigs.mockResolvedValue([
      { id: "c1", hash: "s-1", name: "PlatImg", scope: "platform", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
      { id: "c2", hash: "s-2", name: "OrgImg", scope: "org", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
    ]);
    renderWithOutlet("platform");
    await waitFor(() => expect(screen.getByText("OrgImg")).toBeInTheDocument());
    expect(screen.getByText("Platform Images")).toBeInTheDocument();
    expect(screen.getByText("Org Images")).toBeInTheDocument();
    // Org config is read-only on platform tab
    fireEvent.click(screen.getByText("OrgImg"));
    await new Promise((r) => setTimeout(r, 100));
    expect(screen.queryByText("Rename")).not.toBeInTheDocument();
  });
});

describe("WorkspaceImagesTab delete confirm + base version", () => {
  // Regression: delete confirmation showed config hash, not friendly name.
  it("delete confirm shows the config name, not the hash", async () => {
    mockListConfigs.mockResolvedValue([
      { id: "c1", hash: "s-cryptic", name: "My ML Stack", scope: "member", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
    ]);
    renderWithOutlet("user");
    await waitFor(() => expect(screen.getByText("My ML Stack")).toBeInTheDocument());
    fireEvent.click(screen.getByText("My ML Stack"));
    await waitFor(() => expect(screen.getByText("Delete")).toBeInTheDocument());
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);
    fireEvent.click(screen.getByText("Delete"));
    expect(confirmSpy).toHaveBeenCalledWith(expect.stringContaining("My ML Stack"));
    expect(confirmSpy).not.toHaveBeenCalledWith(expect.stringContaining("s-cryptic"));
    confirmSpy.mockRestore();
  });

  // Regression: base selector now tracks version and sends it to the API.
  it("create sends baseVersion from the selected base", async () => {
    renderWithOutlet("user");
    await waitFor(() => expect(screen.getByPlaceholderText("e.g. ml-stack")).toBeInTheDocument());
    fireEvent.change(screen.getByPlaceholderText("e.g. ml-stack"), { target: { value: "test" } });
    fireEvent.click(screen.getByRole("checkbox"));
    fireEvent.click(screen.getByRole("button", { name: /Create Personal Image/ }));
    await waitFor(() => expect(mockCreateConfig).toHaveBeenCalled());
    expect(mockCreateConfig).toHaveBeenCalledWith(expect.objectContaining({
      baseVersion: "0.6.0",
    }));
  });

  it("org scope: managed configs render once, not duplicated in flat list", async () => {
    mockListConfigs.mockResolvedValue([
      { id: "c1", hash: "s-1", name: "OrgOnly", scope: "org", status: "ready", selection: [], baseName: "bookworm", baseVersion: "0.6.0" },
    ]);
    renderWithOutlet("org", ORG);
    await waitFor(() => expect(screen.getByText("OrgOnly")).toBeInTheDocument());
    expect(screen.queryByText("My Workspace Images")).not.toBeInTheDocument();
    expect(screen.getByText("Org Images")).toBeInTheDocument();
  });
});
