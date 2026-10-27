/**
 * #1966 Sidebar workers-folder tests: board/engine-spawned worker
 * sessions (origin-class 'worker') and parented Task-tool subagents
 * render inside a collapsed-by-default "Workers" folder with a count
 * badge — never in the main list — while user-facing sessions keep the
 * main list. Grouping is presentation-only: the list query still
 * fetches every session (search composes server-side over the same
 * listing), and worker rows stay fully usable when expanded.
 *
 * Born RED — the Workers folder did not exist.
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
import { workspaceWorkflowApi } from "../../api/workflows";

function session(id: string, title: string, overrides: Record<string, unknown> = {}) {
  return {
    id, title, messageCount: 3, status: "idle", hasUnread: false, lastMessageAt: "2026-10-01T00:00:00Z",
    ...overrides,
  };
}

describe("Sidebar — workers folder (#1966)", () => {
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

  it("renders no Workers folder when no session is worker-classified", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-user", "My chat"),
    ]);
    renderSidebar();
    await waitFor(() => expect(screen.getByText("My chat")).toBeTruthy());
    expect(screen.queryByTestId("workers-group")).toBeNull();
  });

  it("folders worker-origin sessions (collapsed, count badge) and keeps user sessions in the main list", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-user", "My chat"),
      session("ses-worker-1", "lane-1964 archived drawer"),
      session("ses-worker-2", "Reviewer verdict on triage"),
    ]);
    // The origins endpoint carries the #1915/#1966 worker marks.
    (workspaceWorkflowApi.sessionOrigins as ReturnType<typeof vi.fn>).mockResolvedValue([
      { sessionId: "ses-worker-1", origin: "worker" },
      { sessionId: "ses-worker-2", origin: "worker" },
    ]);
    renderSidebar();
    await waitFor(() => expect(screen.getByText("My chat")).toBeTruthy());

    const group = screen.getByTestId("workers-group");
    expect(group).toBeTruthy();
    // Count badge: 'Workers (2)'.
    expect(screen.getByText("2")).toBeTruthy();
    // Collapsed by default: worker titles render nowhere until clicked.
    expect(screen.queryByText("lane-1964 archived drawer")).toBeNull();
    expect(screen.queryByText("Reviewer verdict on triage")).toBeNull();
    expect(screen.getByLabelText("Expand worker sessions")).toBeTruthy();
  });

  it("folders parented Task-tool subagent sessions even without an origin mark", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-user", "My chat"),
      session("ses-sub", "Explore codebase", { parentId: "ses-user" }),
    ]);
    renderSidebar();
    await waitFor(() => expect(screen.getByTestId("workers-group")).toBeTruthy());
    expect(screen.getByText("1")).toBeTruthy();
    // The subagent left the main tree for the folder.
    expect(screen.queryByText("Explore codebase")).toBeNull();
  });

  it("keeps origin-manual/routine sessions in the main list (badged automation is user-facing)", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-routine", "Nightly digest", { origin: "routine" }),
      session("ses-manual", "Plain chat", { origin: "manual" }),
    ]);
    (workspaceWorkflowApi.sessionOrigins as ReturnType<typeof vi.fn>).mockResolvedValue([
      { sessionId: "ses-routine", origin: "routine" },
    ]);
    renderSidebar();
    await waitFor(() => expect(screen.getByText("Nightly digest")).toBeTruthy());
    expect(screen.getByText("Plain chat")).toBeTruthy();
    expect(screen.queryByTestId("workers-group")).toBeNull();
  });

  it("expands on click and lists the worker rows", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-user", "My chat"),
      session("ses-worker-1", "lane-1966 worker session folder"),
    ]);
    (workspaceWorkflowApi.sessionOrigins as ReturnType<typeof vi.fn>).mockResolvedValue([
      { sessionId: "ses-worker-1", origin: "worker" },
    ]);
    renderSidebar();
    await waitFor(() => expect(screen.getByTestId("workers-group")).toBeTruthy());

    fireEvent.click(screen.getByLabelText("Expand worker sessions"));
    expect(await screen.findByText("lane-1966 worker session folder")).toBeTruthy();
    expect(screen.getByLabelText("Collapse worker sessions")).toBeTruthy();
    // The user session still renders in the main list alongside.
    expect(screen.getByText("My chat")).toBeTruthy();
  });

  it("auto-expands the folder when the selected session is a worker (the active session stays visible)", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-user", "My chat"),
      session("ses-worker-1", "triage backfill G"),
    ]);
    (workspaceWorkflowApi.sessionOrigins as ReturnType<typeof vi.fn>).mockResolvedValue([
      { sessionId: "ses-worker-1", origin: "worker" },
    ]);
    renderSidebar("/chat/ws-1/ses-worker-1");
    // Tree precedent (auto-expand the ancestor chain of the active
    // session) — unlike the Archived group's deliberate friction, a
    // live worker CAN be the active session the user is reading.
    expect(await screen.findByText("triage backfill G")).toBeTruthy();
    expect(screen.getByLabelText("Collapse worker sessions")).toBeTruthy();
  });

  it("routes an archived worker to the Archived group (archive outranks the worker class)", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-user", "My chat"),
      session("ses-worker-1", "Old worker", { archived: true }),
    ]);
    (workspaceWorkflowApi.sessionOrigins as ReturnType<typeof vi.fn>).mockResolvedValue([
      { sessionId: "ses-worker-1", origin: "worker" },
    ]);
    renderSidebar();
    await waitFor(() => expect(screen.getByTestId("archived-group")).toBeTruthy());
    // Archived workers count only under Archived — not a second time
    // under Workers.
    expect(screen.queryByTestId("workers-group")).toBeNull();
  });

  it("keeps fetching the full list (presentation-only grouping; search composes server-side)", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-user", "My chat"),
      session("ses-worker-1", "lane worker"),
    ]);
    (workspaceWorkflowApi.sessionOrigins as ReturnType<typeof vi.fn>).mockResolvedValue([
      { sessionId: "ses-worker-1", origin: "worker" },
    ]);
    renderSidebar();
    await waitFor(() => expect(screen.getByTestId("workers-group")).toBeTruthy());
    // The grouping must never narrow the QUERY: the sidebar keeps the
    // #1735 includeArchived opt-in (and workers ride the same list) —
    // the API-side q filter still reaches worker sessions because the
    // listing itself is unchanged.
    expect(workspacesApi.getSessions).toHaveBeenCalledWith(
      "ws-1",
      { includeArchived: true },
    );
  });
});
