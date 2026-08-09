import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Sidebar } from "./Sidebar";
import { AuthProvider } from "../../providers/AuthProvider";
import type { SessionListItem } from "../../api/types";

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
  },
}));

vi.mock("../../api/workflows", () => ({
  workspaceWorkflowApi: {
    activeRuns: vi.fn().mockResolvedValue([]),
    sessionOrigins: vi.fn().mockResolvedValue([]),
  },
}));

import { workspacesApi } from "../../api/workspaces";

describe("Sidebar — session title display", () => {
  let qc: QueryClient;

  beforeEach(() => {
    vi.clearAllMocks();
    qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  });

  function renderSidebar() {
    return render(
      <QueryClientProvider client={qc}>
        <AuthProvider>
          <MemoryRouter initialEntries={["/chat/ws-1/sess-1"]}>
            <Sidebar />
          </MemoryRouter>
        </AuthProvider>
      </QueryClientProvider>,
    );
  }

  it("displays session title from API response", async () => {
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "My Workspace", phase: "Active", userId: "u1", runtime: "base", storageSize: "5Gi", createdAt: "", updatedAt: "" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-1", title: "Clone lenaxia/llmsafespaces", messageCount: 3, status: "idle", hasUnread: false },
      { id: "sess-2", title: "Fix the bug", messageCount: 1, status: "active", hasUnread: false },
    ]);

    renderSidebar();

    await waitFor(() => {
      expect(screen.getByText("Clone lenaxia/llmsafespaces")).toBeInTheDocument();
      expect(screen.getByText("Fix the bug")).toBeInTheDocument();
    });
  });

  it("displays 'New chat' for sessions without title", async () => {
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "My Workspace", phase: "Active", userId: "u1", runtime: "base", storageSize: "5Gi", createdAt: "", updatedAt: "" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-1", messageCount: 3, status: "idle", hasUnread: false },
    ]);

    renderSidebar();

    await waitFor(() => {
      expect(screen.getByText("New chat")).toBeInTheDocument();
    });
  });

  it("updates session title when cache is mutated directly (simulates useSessionTitle fix)", async () => {
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "My Workspace", phase: "Active", userId: "u1", runtime: "base", storageSize: "5Gi", createdAt: "", updatedAt: "" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-1", messageCount: 3, status: "idle", hasUnread: false },
    ]);

    renderSidebar();

    // Initially shows "New chat"
    await waitFor(() => {
      expect(screen.getByText("New chat")).toBeInTheDocument();
    });

    // Simulate what useSessionTitle does: directly update the cache
    qc.setQueryData<SessionListItem[]>(["sessions", "ws-1"], (old) => {
      if (!old) return old;
      return old.map((s) =>
        s.id === "sess-1" ? { ...s, title: "Clone lenaxia/llmsafespaces" } : s,
      );
    });

    // Sidebar should now show the updated title
    await waitFor(() => {
      expect(screen.getByText("Clone lenaxia/llmsafespaces")).toBeInTheDocument();
      expect(screen.queryByText("New chat")).not.toBeInTheDocument();
    });
  });

  it("shows multiple sessions with mixed titled/untitled", async () => {
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "My Workspace", phase: "Active", userId: "u1", runtime: "base", storageSize: "5Gi", createdAt: "", updatedAt: "" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-1", title: "Has Title", messageCount: 5, status: "idle", hasUnread: false },
      { id: "sess-2", messageCount: 0, status: "idle", hasUnread: false },
      { id: "sess-3", title: "Another Title", messageCount: 2, status: "active", hasUnread: false },
    ]);

    renderSidebar();

    await waitFor(() => {
      expect(screen.getByText("Has Title")).toBeInTheDocument();
      expect(screen.getByText("Another Title")).toBeInTheDocument();
      expect(screen.getByText("New chat")).toBeInTheDocument();
    });
  });

  it("cache update only affects the targeted session", async () => {
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "My Workspace", phase: "Active", userId: "u1", runtime: "base", storageSize: "5Gi", createdAt: "", updatedAt: "" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-1", messageCount: 1, status: "idle", hasUnread: false },
      { id: "sess-2", title: "Keep This", messageCount: 2, status: "idle", hasUnread: false },
    ]);

    renderSidebar();

    await waitFor(() => {
      expect(screen.getByText("Keep This")).toBeInTheDocument();
    });

    // Update only sess-1
    qc.setQueryData<SessionListItem[]>(["sessions", "ws-1"], (old) => {
      if (!old) return old;
      return old.map((s) =>
        s.id === "sess-1" ? { ...s, title: "Updated Title" } : s,
      );
    });

    await waitFor(() => {
      expect(screen.getByText("Updated Title")).toBeInTheDocument();
      expect(screen.getByText("Keep This")).toBeInTheDocument();
    });
  });
});

describe("S36.5 — per-session context usage indicator in sidebar", () => {
  let qc: QueryClient;

  beforeEach(() => {
    vi.clearAllMocks();
    qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "My Workspace", phase: "Active", userId: "u1", runtime: "base", storageSize: "5Gi", createdAt: "", updatedAt: "" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-1", title: "Debug session", messageCount: 5, status: "idle" },
      { id: "sess-2", title: "Refactor session", messageCount: 2, status: "idle" },
    ]);
  });

  function renderSidebar() {
    return render(
      <QueryClientProvider client={qc}>
        <AuthProvider>
          <MemoryRouter initialEntries={["/chat/ws-1/sess-1"]}>
            <Sidebar />
          </MemoryRouter>
        </AuthProvider>
      </QueryClientProvider>,
    );
  }

  it("shows context token count next to a session when workspace status has contextUsed", async () => {
    // Pre-populate sessions cache with per-session context data.
    // context_used is now read from the sessions list (session_index via API),
    // not from the workspace-status sessions array.
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Debug session", messageCount: 3, status: "idle", hasUnread: false, contextUsed: 42000 },
      { id: "sess-2", title: "Refactor session", messageCount: 1, status: "idle", hasUnread: false },
    ]);

    renderSidebar();

    await waitFor(() => {
      expect(screen.getByText("Debug session")).toBeInTheDocument();
    });

    // sess-1 has contextUsed=42000 → should show "42K"
    expect(screen.getAllByText("42K").length).toBeGreaterThan(0);
    // sess-2 has contextUsed=0 → no indicator (zero is omitted)
    // "Refactor session" should be visible but no "0K" or "0" context indicator
    expect(screen.getByText("Refactor session")).toBeInTheDocument();
  });

  it("does NOT show context indicator when workspace status has no session data", async () => {
    // No workspace-status cache entry at all
    renderSidebar();

    await waitFor(() => {
      expect(screen.getByText("Debug session")).toBeInTheDocument();
    });

    // No context indicators (no "K" suffix numbers from context)
    expect(screen.queryByText(/42K/)).not.toBeInTheDocument();
  });

  it("updates context indicator when workspace status cache is updated", async () => {
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Debug session", messageCount: 2, status: "idle", hasUnread: false, contextUsed: 10000 },
    ]);

    renderSidebar();

    await waitFor(() => {
      expect(screen.getAllByText("10K").length).toBeGreaterThan(0);
    });

    // Update sessions cache — simulates a sessions-list poll refreshing context
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Debug session", messageCount: 3, status: "idle", hasUnread: false, contextUsed: 75000 },
    ]);

    await waitFor(() => {
      expect(screen.getAllByText("75K").length).toBeGreaterThan(0);
      expect(screen.queryByText("10K")).not.toBeInTheDocument();
    });
  });
});
