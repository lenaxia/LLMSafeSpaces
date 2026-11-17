import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Sidebar } from "./Sidebar";
import { AuthProvider } from "../../providers/AuthProvider";
import type { WorkspaceListItem } from "../../api/types";

vi.mock("../../api/auth", () => ({
  authApi: {
    me: vi.fn().mockResolvedValue({ id: "u1", username: "alice", email: "a@b.com", role: "user", active: true }),
  },
}));

vi.mock("../../api/orgs", () => ({
  orgsApi: {
    list: vi.fn().mockResolvedValue([]),
  },
}));

vi.mock("../../api/workspaces", () => ({
  workspacesApi: {
    list: vi.fn(),
    create: vi.fn().mockResolvedValue({ id: "ws-new", name: "new-ws" }),
    activate: vi.fn().mockResolvedValue({ resumed: "ws-1" }),
    ensureSession: vi.fn().mockResolvedValue({ sessionId: "sess-1", workspaceId: "ws-1", resumed: false }),
    getSessions: vi.fn(),
    getStatus: vi.fn().mockResolvedValue(null),
    renameWorkspace: vi.fn().mockResolvedValue(undefined),
    deleteWorkspace: vi.fn().mockResolvedValue(undefined),
    renameSession: vi.fn().mockResolvedValue(undefined),
    suspend: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock("../../api/workflows", () => ({
  workspaceWorkflowApi: {
    activeRuns: vi.fn().mockResolvedValue([]),
    sessionOrigins: vi.fn().mockResolvedValue([]),
  },
}));

import { workspacesApi } from "../../api/workspaces";

type ListMock = ReturnType<typeof vi.fn>;

function makeWorkspace(over: Partial<WorkspaceListItem> = {}): WorkspaceListItem {
  return {
    id: "ws-1",
    name: "My Workspace",
    userId: "u1",
    runtime: "base",
    storageSize: "5Gi",
    createdAt: "",
    updatedAt: "",
    phase: "Active",
    ...over,
  };
}

function renderSidebar() {
  return render(
    <QueryClientProvider client={qc}>
      <AuthProvider>
        <MemoryRouter initialEntries={["/chat"]}>
          <Sidebar />
        </MemoryRouter>
      </AuthProvider>
    </QueryClientProvider>,
  );
}

let qc: QueryClient;

describe("Sidebar — first-session harness picker (bind-once)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  });

  it("picker opens on the FIRST session of an unbound multi-option workspace; submit binds the chosen harness", async () => {
    const user = userEvent.setup();
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ phase: "Creating", harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);

    renderSidebar();

    const newChat = await screen.findByRole("button", { name: "New chat" });
    await user.click(newChat);

    const dialog = screen.getByRole("dialog");
    expect(dialog).toBeInTheDocument();
    expect(screen.getByText(/cannot be changed later/i)).toBeInTheDocument();
    // The default is preselected inside the modal.
    expect(screen.getByRole("radio", { name: /opencode/i })).toHaveAttribute("aria-checked", "true");

    await user.click(screen.getByRole("radio", { name: /pi/i }));
    await user.click(screen.getByTestId("harness-picker-submit"));

    await waitFor(() => {
      expect(workspacesApi.ensureSession).toHaveBeenCalledWith("ws-1", { harness: "pi" });
    });
  });

  it("picker appears exactly once: a BOUND workspace never opens it — the call carries no harness", async () => {
    const user = userEvent.setup();
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ harness: "opencode", harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([
      { id: "sess-1", messageCount: 0, status: "idle", hasUnread: false },
    ]);

    renderSidebar();

    await user.click(await screen.findByRole("button", { name: "New chat" }));

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await waitFor(() => {
      expect(workspacesApi.ensureSession).toHaveBeenCalledWith("ws-1", {});
    });
  });

  it("single enabled harness: binds silently with NO modal", async () => {
    const user = userEvent.setup();
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ phase: "Creating", harnessOptions: ["opencode"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);

    renderSidebar();

    await user.click(await screen.findByRole("button", { name: "New chat" }));

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await waitFor(() => {
      expect(workspacesApi.ensureSession).toHaveBeenCalledWith("ws-1", { harness: "opencode" });
    });
  });

  it("unbound workspace with zero sessions renders the choose-a-harness hint; bound does not", async () => {
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [
        makeWorkspace({ phase: "Creating", harnessOptions: ["opencode", "pi"] }),
        makeWorkspace({ id: "ws-2", name: "Bound", harness: "pi", harnessOptions: ["opencode", "pi"] }),
      ],
      pagination: { limit: 20, offset: 0, total: 2 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);

    renderSidebar();

    const hint = await screen.findByTestId("harness-unbound-hint");
    expect(hint).toHaveTextContent(/choose a harness when starting your first session/i);
    expect(screen.getAllByTestId("harness-unbound-hint")).toHaveLength(1);
  });

  it("bound workspace renders the read-only harness badge in its header row", async () => {
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ name: "Pi Workspace", harness: "pi" })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);

    renderSidebar();

    const badge = await screen.findByTestId("harness-badge");
    expect(badge).toHaveTextContent("pi");
    expect(badge).toHaveAttribute("title", "Harness: pi — permanent for this workspace");
  });

  it("unbound workspace renders no badge", async () => {
    (workspacesApi.list as ListMock).mockResolvedValue({
      items: [makeWorkspace({ phase: "Creating", harnessOptions: ["opencode", "pi"] })],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ListMock).mockResolvedValue([]);

    renderSidebar();

    await screen.findByText("My Workspace");
    expect(screen.queryByTestId("harness-badge")).not.toBeInTheDocument();
  });
});
