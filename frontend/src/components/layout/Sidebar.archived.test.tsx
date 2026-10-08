/**
 * #1627 Sidebar archived-group tests: the group renders only when ≥1
 * archived session exists, is ALWAYS collapsed by default (deliberate
 * friction — a human click expands it), archived sessions never render
 * in the live tree, and the kebab Archive/Unarchive actions call the
 * API with the right direction.
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
    renameWorkspace: vi.fn().mockResolvedValue(undefined),
    deleteWorkspace: vi.fn().mockResolvedValue(undefined),
    deleteSession: vi.fn().mockResolvedValue(undefined),
    abortSession: vi.fn().mockResolvedValue(undefined),
    renameSession: vi.fn().mockResolvedValue(undefined),
    setSessionArchived: vi.fn().mockResolvedValue(undefined),
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

function session(id: string, title: string, archived?: boolean) {
  return {
    id, title, messageCount: 3, status: "idle", hasUnread: false, lastMessageAt: "2026-10-01T00:00:00Z",
    ...(archived ? { archived: true } : {}),
  };
}

describe("Sidebar — archived sessions group (#1627)", () => {
  let qc: QueryClient;

  beforeEach(() => {
    vi.clearAllMocks();
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

  it("renders no Archived group when nothing is archived", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-live", "Live work"),
    ]);
    renderSidebar();
    await waitFor(() => expect(screen.getByText("Live work")).toBeTruthy());
    expect(screen.queryByTestId("archived-group")).toBeNull();
  });

  it("renders the group (collapsed, with count) and hides archived rows from the live tree", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-live", "Live work"),
      session("ses-old", "Old work", true),
    ]);
    renderSidebar();
    await waitFor(() => expect(screen.getByText("Live work")).toBeTruthy());

    const group = screen.getByTestId("archived-group");
    expect(group).toBeTruthy();
    expect(screen.getByText("1")).toBeTruthy();
    // Collapsed by default: the archived title is not rendered anywhere.
    expect(screen.queryByText("Old work")).toBeNull();
    // The expand control reports the collapsed state.
    expect(screen.getByLabelText("Expand archived sessions")).toBeTruthy();
  });

  it("expands on click and lists the archived session", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-live", "Live work"),
      session("ses-old", "Old work", true),
    ]);
    renderSidebar();
    await waitFor(() => expect(screen.getByTestId("archived-group")).toBeTruthy());

    fireEvent.click(screen.getByLabelText("Expand archived sessions"));
    expect(await screen.findByText("Old work")).toBeTruthy();
    expect(screen.getByLabelText("Collapse archived sessions")).toBeTruthy();
  });

  it("stays collapsed by default even when the archived session is selected", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-live", "Live work"),
      session("ses-old", "Old work", true),
    ]);
    renderSidebar("/chat/ws-1/ses-old");
    await waitFor(() => expect(screen.getByTestId("archived-group")).toBeTruthy());
    // Deliberate friction: selection NEVER auto-expands the group.
    expect(screen.queryByText("Old work")).toBeNull();
  });

  it("kebab Archive on a live session calls the API with archived:true", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-live", "Live work"),
    ]);
    renderSidebar("/chat/ws-1/ses-live");
    await waitFor(() => expect(screen.getByText("Live work")).toBeTruthy());

    // The workspace's own kebab renders first; the SESSION row's is last.
    const buttons = screen.getAllByLabelText("Actions");
    fireEvent.click(buttons[buttons.length - 1]!);
    fireEvent.click(await screen.findByText("Archive"));
    await waitFor(() =>
      expect(workspacesApi.setSessionArchived).toHaveBeenCalledWith("ws-1", "ses-live", true),
    );
  });

  it("kebab Unarchive on an archived row calls the API with archived:false", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-old", "Old work", true),
    ]);
    renderSidebar("/chat/ws-1/ses-old");
    await waitFor(() => expect(screen.getByTestId("archived-group")).toBeTruthy());

    fireEvent.click(screen.getByLabelText("Expand archived sessions"));
    await waitFor(() => expect(screen.getByText("Old work")).toBeTruthy());

    const buttons = screen.getAllByLabelText("Actions");
    fireEvent.click(buttons[buttons.length - 1]!);
    fireEvent.click(await screen.findByText("Unarchive"));
    await waitFor(() =>
      expect(workspacesApi.setSessionArchived).toHaveBeenCalledWith("ws-1", "ses-old", false),
    );
  });
});
