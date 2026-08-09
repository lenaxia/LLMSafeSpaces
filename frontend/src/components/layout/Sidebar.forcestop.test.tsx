/**
 * Sidebar Force Stop tests: verify that the "Force Stop" kebab menu item
 * calls workspacesApi.abortSession with the correct workspace + session IDs.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor, render, fireEvent } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Sidebar } from "./Sidebar";
import { AuthProvider } from "../../providers/AuthProvider";

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
    ensureSession: vi.fn().mockResolvedValue({ sessionId: "sess-1", workspaceId: "ws-1" }),
    getSessions: vi.fn(),
    getStatus: vi.fn().mockResolvedValue(null),
    renameWorkspace: vi.fn().mockResolvedValue(undefined),
    deleteWorkspace: vi.fn().mockResolvedValue(undefined),
    renameSession: vi.fn().mockResolvedValue(undefined),
    suspend: vi.fn().mockResolvedValue(undefined),
    abortSession: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock("../../api/workflows", () => ({
  workspaceWorkflowApi: {
    activeRuns: vi.fn().mockResolvedValue([]),
    sessionOrigins: vi.fn().mockResolvedValue([]),
  },
}));

import { workspacesApi } from "../../api/workspaces";

function renderSidebar(initialPath = "/chat/ws-1") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={qc}>
      <AuthProvider>
        <MemoryRouter initialEntries={[initialPath]}>
          <Routes>
            <Route path="/chat/:workspaceId" element={<Sidebar />} />
            <Route path="/chat/:workspaceId/:sessionId" element={<Sidebar />} />
          </Routes>
        </MemoryRouter>
      </AuthProvider>
    </QueryClientProvider>,
  );
}

describe("Sidebar — Force Stop kebab menu item", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    (workspacesApi.abortSession as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "My Workspace", phase: "Active", userId: "u1", runtime: "base", storageSize: "5Gi", createdAt: "", updatedAt: "" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
  });

  it("calls abortSession with the correct workspace + session IDs", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-1", title: "First session", messageCount: 1, status: "idle" },
    ]);

    renderSidebar();

    await waitFor(() => expect(screen.getByText("First session")).toBeInTheDocument());

    // actionsButtons[0] = workspace kebab, [1] = session kebab.
    const actionsButtons = screen.getAllByLabelText("Actions");
    fireEvent.click(actionsButtons[1]!);

    fireEvent.click(screen.getByText("Force Stop"));

    expect(workspacesApi.abortSession).toHaveBeenCalledWith("ws-1", "sess-1");
  });

  it("Force Stop fires without a confirmation dialog", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-1", title: "First session", messageCount: 1, status: "idle" },
    ]);

    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);

    renderSidebar();

    await waitFor(() => expect(screen.getByText("First session")).toBeInTheDocument());

    const actionsButtons = screen.getAllByLabelText("Actions");
    fireEvent.click(actionsButtons[1]!);
    fireEvent.click(screen.getByText("Force Stop"));

    expect(workspacesApi.abortSession).toHaveBeenCalledWith("ws-1", "sess-1");
    expect(confirmSpy).not.toHaveBeenCalled();

    confirmSpy.mockRestore();
  });

  it("surfaces an alert when abortSession rejects", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-1", title: "First session", messageCount: 1, status: "idle" },
    ]);
    (workspacesApi.abortSession as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("boom"));

    const alertSpy = vi.spyOn(window, "alert").mockImplementation(() => {});

    renderSidebar();

    await waitFor(() => expect(screen.getByText("First session")).toBeInTheDocument());

    const actionsButtons = screen.getAllByLabelText("Actions");
    fireEvent.click(actionsButtons[1]!);
    fireEvent.click(screen.getByText("Force Stop"));

    await waitFor(() => {
      expect(alertSpy).toHaveBeenCalledWith("Failed to force stop session.");
    });

    alertSpy.mockRestore();
  });

  it("nested child session Force Stop uses the parent workspace ID", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "parent", title: "Parent task", messageCount: 1, status: "idle" },
      { id: "child", title: "Subtask child", parentId: "parent", messageCount: 1, status: "idle" },
    ]);

    renderSidebar();

    await waitFor(() => expect(screen.getByText("Parent task")).toBeInTheDocument());

    fireEvent.click(screen.getByLabelText("Expand subtasks"));
    expect(screen.getByText("Subtask child")).toBeInTheDocument();

    // [0] = workspace kebab, [1] = parent session kebab, [2] = child session kebab.
    const actionsButtons = screen.getAllByLabelText("Actions");
    expect(actionsButtons.length).toBeGreaterThanOrEqual(3);
    fireEvent.click(actionsButtons[2]!);

    fireEvent.click(screen.getByText("Force Stop"));

    expect(workspacesApi.abortSession).toHaveBeenCalledWith("ws-1", "child");
  });
});
