/**
 * Sidebar hierarchy tests: verify that subagent (subtask) sessions render
 * nested under their parent, expand/collapse works, and orphaned sessions
 * appear in the synthetic group at the bottom.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor, render, fireEvent } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Sidebar } from "./Sidebar";
import { AuthProvider } from "../../providers/AuthProvider";

const STORAGE_KEY = "llmsafespaces_user_settings";

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

describe("Sidebar — session hierarchy", () => {
  let qc: QueryClient;

  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.removeItem(STORAGE_KEY);
    qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "My Workspace", phase: "Active", userId: "u1", runtime: "base", storageSize: "5Gi", createdAt: "", updatedAt: "" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
  });

  function renderSidebar(initialPath = "/chat/ws-1") {
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

  it("hides subtask children by default (collapsed)", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "parent", title: "Parent task", messageCount: 1, status: "idle" },
      { id: "child", title: "Subtask child", parentId: "parent", messageCount: 1, status: "idle" },
    ]);

    renderSidebar();

    await waitFor(() => {
      expect(screen.getByText("Parent task")).toBeInTheDocument();
    });
    // Child is collapsed-by-default and must not be visible.
    expect(screen.queryByText("Subtask child")).not.toBeInTheDocument();
  });

  it("expanding a parent reveals its subtask children", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "parent", title: "Parent task", messageCount: 1, status: "idle" },
      { id: "child", title: "Subtask child", parentId: "parent", messageCount: 1, status: "idle" },
    ]);

    renderSidebar();

    await waitFor(() => expect(screen.getByText("Parent task")).toBeInTheDocument());
    expect(screen.queryByText("Subtask child")).not.toBeInTheDocument();

    // The expand chevron is keyed off aria-label.
    const expandBtn = screen.getByLabelText("Expand subtasks");
    fireEvent.click(expandBtn);

    expect(screen.getByText("Subtask child")).toBeInTheDocument();
  });

  it("auto-expands ancestor chain when navigating directly to a subtask", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "parent", title: "Parent task", messageCount: 1, status: "idle" },
      { id: "child", title: "Subtask child", parentId: "parent", messageCount: 1, status: "idle" },
    ]);

    // URL points directly at the subtask — its parent must auto-expand
    // so the user can see where they are in the tree.
    renderSidebar("/chat/ws-1/child");

    await waitFor(() => {
      expect(screen.getByText("Parent task")).toBeInTheDocument();
      expect(screen.getByText("Subtask child")).toBeInTheDocument();
    });
  });

  it("nested subtasks (grandchildren) render with correct indentation chain", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "root", title: "Root task", messageCount: 1, status: "idle" },
      { id: "child", title: "Child task", parentId: "root", messageCount: 1, status: "idle" },
      { id: "grandchild", title: "Grandchild task", parentId: "child", messageCount: 1, status: "idle" },
    ]);

    renderSidebar("/chat/ws-1/grandchild");

    // Active = grandchild → both ancestors auto-expand
    await waitFor(() => {
      expect(screen.getByText("Root task")).toBeInTheDocument();
      expect(screen.getByText("Child task")).toBeInTheDocument();
      expect(screen.getByText("Grandchild task")).toBeInTheDocument();
    });
  });

  it("collapses subtasks when user clicks the chevron again", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "parent", title: "Parent task", messageCount: 1, status: "idle" },
      { id: "child", title: "Subtask child", parentId: "parent", messageCount: 1, status: "idle" },
    ]);

    renderSidebar("/chat/ws-1/child");

    await waitFor(() => expect(screen.getByText("Subtask child")).toBeInTheDocument());

    const collapseBtn = screen.getByLabelText("Collapse subtasks");
    fireEvent.click(collapseBtn);

    expect(screen.queryByText("Subtask child")).not.toBeInTheDocument();
  });

  it("orphaned subtasks appear in the 'Orphaned subtasks' group", async () => {
    // 'orphan' references a parent that's not in the list.
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "regular", title: "Regular session", messageCount: 1, status: "idle" },
      { id: "orphan", title: "Orphaned task", parentId: "deleted-parent", messageCount: 1, status: "idle" },
    ]);

    renderSidebar();

    await waitFor(() => {
      expect(screen.getByText("Regular session")).toBeInTheDocument();
      expect(screen.getByText("Orphaned subtasks")).toBeInTheDocument();
    });
    // Group is collapsed by default — content hidden until expanded.
    expect(screen.queryByText("Orphaned task")).not.toBeInTheDocument();
  });

  it("expanding the orphans group reveals orphaned sessions", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "orphan", title: "Lost session", parentId: "missing", messageCount: 1, status: "idle" },
    ]);

    renderSidebar();

    await waitFor(() => expect(screen.getByText("Orphaned subtasks")).toBeInTheDocument());

    const expandBtn = screen.getByLabelText("Expand orphaned subtasks");
    fireEvent.click(expandBtn);

    expect(screen.getByText("Lost session")).toBeInTheDocument();
  });

  it("orphans group is hidden entirely when there are no orphans", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "regular", title: "Just a session", messageCount: 1, status: "idle" },
    ]);

    renderSidebar();

    await waitFor(() => expect(screen.getByText("Just a session")).toBeInTheDocument());
    expect(screen.queryByText("Orphaned subtasks")).not.toBeInTheDocument();
  });

  it("sibling subtasks at same depth render as separate rows", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "parent", title: "Parent task", messageCount: 1, status: "idle" },
      { id: "child1", title: "First child", parentId: "parent", messageCount: 1, status: "idle" },
      { id: "child2", title: "Second child", parentId: "parent", messageCount: 1, status: "idle" },
    ]);

    renderSidebar("/chat/ws-1/child1");

    await waitFor(() => {
      expect(screen.getByText("First child")).toBeInTheDocument();
      expect(screen.getByText("Second child")).toBeInTheDocument();
    });
  });

  it("does not collapse parents the user has already expanded", async () => {
    // User expands a parent without navigating to its child; the auto-
    // expand logic must NOT clobber that state when sessions list updates.
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "parent", title: "Parent task", messageCount: 1, status: "idle" },
      { id: "child", title: "Manual expand", parentId: "parent", messageCount: 1, status: "idle" },
    ]);

    renderSidebar();
    await waitFor(() => expect(screen.getByText("Parent task")).toBeInTheDocument());

    fireEvent.click(screen.getByLabelText("Expand subtasks"));
    expect(screen.getByText("Manual expand")).toBeInTheDocument();

    // Simulate the query refetching with the same data — the
    // user's manual expand state must persist.
    qc.invalidateQueries({ queryKey: ["sessions", "ws-1"] });
    await waitFor(() => expect(screen.getByText("Manual expand")).toBeInTheDocument());
  });
});

describe("Sidebar — auto-expand/collapse setting", () => {
  let qc: QueryClient;

  beforeEach(async () => {
    vi.clearAllMocks();
    localStorage.removeItem(STORAGE_KEY);
    const { _resetStoreFromStorage } = await import("../../hooks/useUserSettings");
    _resetStoreFromStorage();
    qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "My Workspace", phase: "Active", userId: "u1", runtime: "base", storageSize: "5Gi", createdAt: "", updatedAt: "" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
  });

  function renderSidebarAtPath(path: string) {
    return render(
      <QueryClientProvider client={qc}>
        <AuthProvider>
          <MemoryRouter initialEntries={[path]}>
            <Routes>
              <Route path="/chat/:workspaceId" element={<Sidebar />} />
              <Route path="/chat/:workspaceId/:sessionId" element={<Sidebar />} />
            </Routes>
          </MemoryRouter>
        </AuthProvider>
      </QueryClientProvider>,
    );
  }

  it("auto-expands parent children when navigating to a parent session (setting on by default)", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "parent", title: "Parent task", messageCount: 1, status: "idle" },
      { id: "child", title: "Child session", parentId: "parent", messageCount: 1, status: "idle" },
      { id: "sibling", title: "Sibling session", messageCount: 1, status: "idle" },
    ]);

    // Navigate directly to the parent session — children should auto-expand.
    renderSidebarAtPath("/chat/ws-1/parent");

    await waitFor(() => {
      expect(screen.getByText("Parent task")).toBeInTheDocument();
      expect(screen.getByText("Child session")).toBeInTheDocument();
    });
  });

  it("does NOT auto-expand parent children when setting is off", async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ autoExpandChildren: false }));
    const { _resetStoreFromStorage } = await import("../../hooks/useUserSettings");
    _resetStoreFromStorage();

    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "parent", title: "Parent task", messageCount: 1, status: "idle" },
      { id: "child", title: "Child session", parentId: "parent", messageCount: 1, status: "idle" },
    ]);

    // Navigate to parent — children should stay collapsed when setting is off.
    renderSidebarAtPath("/chat/ws-1/parent");

    await waitFor(() => expect(screen.getByText("Parent task")).toBeInTheDocument());
    expect(screen.queryByText("Child session")).not.toBeInTheDocument();
  });

  it("auto-expands ancestor chain even when autoExpandChildren is off", async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ autoExpandChildren: false }));
    const { _resetStoreFromStorage } = await import("../../hooks/useUserSettings");
    _resetStoreFromStorage();

    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "parent", title: "Parent task", messageCount: 1, status: "idle" },
      { id: "child", title: "Child session", parentId: "parent", messageCount: 1, status: "idle" },
    ]);

    // Navigate to child — ancestor chain must still expand so child is visible.
    renderSidebarAtPath("/chat/ws-1/child");

    await waitFor(() => {
      expect(screen.getByText("Parent task")).toBeInTheDocument();
      expect(screen.getByText("Child session")).toBeInTheDocument();
    });
  });

  it("auto-collapses previous parent subtree when navigating to a sibling (setting on)", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "parent-a", title: "Parent A", messageCount: 1, status: "idle" },
      { id: "child-a", title: "Child of A", parentId: "parent-a", messageCount: 1, status: "idle" },
      { id: "parent-b", title: "Parent B", messageCount: 1, status: "idle" },
      { id: "child-b", title: "Child of B", parentId: "parent-b", messageCount: 1, status: "idle" },
    ]);

    // Start at parent-a — its children should auto-expand.
    renderSidebarAtPath("/chat/ws-1/parent-a");

    await waitFor(() => expect(screen.getByText("Child of A")).toBeInTheDocument());

    // Click parent-b to navigate away from parent-a's subtree.
    fireEvent.click(screen.getByText("Parent B"));

    // After navigating to parent-b, parent-a's children should collapse and
    // parent-b's children should expand.
    await waitFor(() => expect(screen.getByText("Child of B")).toBeInTheDocument());
    expect(screen.queryByText("Child of A")).not.toBeInTheDocument();
  });

  it("does NOT auto-collapse previous subtree when setting is off", async () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ autoExpandChildren: false }));
    const { _resetStoreFromStorage } = await import("../../hooks/useUserSettings");
    _resetStoreFromStorage();

    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "parent-a", title: "Parent A", messageCount: 1, status: "idle" },
      { id: "child-a", title: "Child of A", parentId: "parent-a", messageCount: 1, status: "idle" },
      { id: "parent-b", title: "Parent B", messageCount: 1, status: "idle" },
    ]);

    // Navigate to child-a — ancestor chain should expand even with setting off.
    renderSidebarAtPath("/chat/ws-1/child-a");

    await waitFor(() => expect(screen.getByText("Child of A")).toBeInTheDocument());

    // Click parent-b to navigate away — with setting off, child-a should NOT collapse.
    fireEvent.click(screen.getByText("Parent B"));

    await waitFor(() => expect(screen.getByText("Parent B")).toBeInTheDocument());
    // Child of A should still be visible (not auto-collapsed when setting is off).
    expect(screen.getByText("Child of A")).toBeInTheDocument();
  });
});
