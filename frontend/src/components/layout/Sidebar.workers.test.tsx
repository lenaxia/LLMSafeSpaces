/**
 * #1966/ruling 6 (2026-10-28 owner batch) Sidebar Board-workers-group
 * tests: board/engine-spawned worker sessions (origin-class 'worker'
 * spawn provenance — current AND past, idle included) render inside a
 * collapsed-by-default "Board workers" group with a count badge —
 * never as flat roots of the main list — while user-facing sessions
 * keep the main list. Membership is provenance, NEVER parentage: a
 * worker's genuine subagent children nest BENEATH it inside the group,
 * and subagents of user sessions stay in the main tree under their
 * parents. Grouping is presentation-only: the list query still fetches
 * every session (search composes server-side over the same listing),
 * and worker rows stay fully usable when expanded. The group hides
 * entirely when the workspace has no board workers.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor, render, fireEvent } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Sidebar } from "./Sidebar";
import { AuthProvider } from "../../providers/AuthProvider";

const busySessions = new Set<string>();

vi.mock("../../providers/SessionActivityProvider", () => ({
  useIsSessionAborted: () => false,
  useIsSessionBusy: (sid: string) => busySessions.has(sid),
  useIsSessionUnread: () => false,
  useWorkspaceBusyCount: () => busySessions.size,
  useWorkspaceHung: () => false,
  useClearPendingUnread: () => () => {},
  useIsSessionPendingAction: () => false,
  useSessionPendingActions: () => new Set<string>(),
  useAddPendingAction: () => () => {},
  useAddPendingQuestion: () => () => {},
  useAddPendingPermission: () => () => {},
  usePendingQuestionsForSession: () => [],
  usePendingPermissionsForSession: () => [],
  useClearSessionPendingPrompts: () => () => {},
  useSessionStatus: (sid: string) => (busySessions.has(sid) ? "busy" : "idle"),
  SessionActivityProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

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

describe("Sidebar — Board workers group (#1966/ruling 6)", () => {
  let qc: QueryClient;

  beforeEach(() => {
    vi.clearAllMocks();
    busySessions.clear();
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

  it("hides the group entirely when the workspace has no board workers — even with subagents present", async () => {
    // A user session with a genuine Task-tool subagent: parentage does
    // NOT folder anything anymore (ruling 6 — the worker subtree is
    // for board workers); the subagent nests under its user parent.
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-user", "My chat"),
      session("ses-sub", "Explore codebase", { parentId: "ses-user" }),
    ]);
    renderSidebar();
    await waitFor(() => expect(screen.getByText("My chat")).toBeTruthy());
    expect(screen.queryByTestId("workers-group")).toBeNull();
    // The subagent nests beneath its user parent in the main tree
    // (subtrees collapse by default — expand the parent to see it).
    fireEvent.click(screen.getByLabelText("Expand subtasks"));
    expect(await screen.findByText("Explore codebase")).toBeTruthy();
  });

  it("groups worker-provenance sessions (collapsed, count badge) and leaves no worker a flat main-list root", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-user", "My chat"),
      session("ses-worker-1", "lane-1964 archived drawer"),
      session("ses-worker-2", "Reviewer verdict on triage"),
    ]);
    // The origins endpoint carries the #1915/#1966 worker marks (the
    // server-side listing join is the primary carrier; this merge is
    // the compatibility fallback).
    (workspaceWorkflowApi.sessionOrigins as ReturnType<typeof vi.fn>).mockResolvedValue([
      { sessionId: "ses-worker-1", origin: "worker" },
      { sessionId: "ses-worker-2", origin: "worker" },
    ]);
    renderSidebar();
    await waitFor(() => expect(screen.getByText("My chat")).toBeTruthy());

    const group = screen.getByTestId("workers-group");
    expect(group).toBeTruthy();
    expect(screen.getByText("Board workers")).toBeTruthy();
    // Count badge: 2 workers.
    expect(screen.getByText("2")).toBeTruthy();
    // Collapsed by default: worker titles render nowhere until clicked.
    expect(screen.queryByText("lane-1964 archived drawer")).toBeNull();
    expect(screen.queryByText("Reviewer verdict on triage")).toBeNull();
    expect(screen.getByLabelText("Expand board workers")).toBeTruthy();
  });

  it("classifies by provenance ON the listing payload too (no /session-origins fallback needed)", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-user", "My chat"),
      session("ses-worker-1", "join-carried worker", { origin: "worker" }),
    ]);
    renderSidebar();
    await waitFor(() => expect(screen.getByTestId("workers-group")).toBeTruthy());
    expect(screen.getByText("1")).toBeTruthy();
    fireEvent.click(screen.getByLabelText("Expand board workers"));
    expect(await screen.findByText("join-carried worker")).toBeTruthy();
  });

  it("sorts the worker rows by recent activity (newest first) and shows a busy badge on busy workers", async () => {
    busySessions.add("ses-worker-busy");
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-user", "My chat"),
      session("ses-worker-idle", "Idle worker", { origin: "worker", lastMessageAt: "2026-10-26T10:00:00Z" }),
      session("ses-worker-busy", "Busy worker", { origin: "worker", lastMessageAt: "2026-10-28T12:00:00Z" }),
    ]);
    renderSidebar();
    await waitFor(() => expect(screen.getByTestId("workers-group")).toBeTruthy());
    fireEvent.click(screen.getByLabelText("Expand board workers"));

    const busyRow = await screen.findByText("Busy worker");
    expect(busyRow).toBeTruthy();
    expect(screen.getByText("Idle worker")).toBeTruthy();
    // Busy badge: the busy worker's row renders the spinner inside the
    // group (idle shows no indicator — the row's default state).
    expect(busyRow.closest("button")?.querySelector('[data-testid="busy-indicator"], .animate-spin')).toBeTruthy();
    // Activity order: newest (busy, 10-28) before older (idle, 10-26).
    const rows = document.querySelectorAll("[data-testid='workers-group'] ~ *");
    expect(rows.length).toBeGreaterThan(0);
    const titles = Array.from(document.querySelectorAll("*"))
      .filter((el) => /^(Busy|Idle) worker$/.test(el.textContent ?? "") && el.children.length === 0)
      .map((el) => el.textContent);
    expect(titles.indexOf("Busy worker")).toBeLessThan(titles.indexOf("Idle worker"));
  });

  it("nests a worker's genuine subagent children beneath it inside the group", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-user", "My chat"),
      session("ses-worker-1", "Board worker", { origin: "worker" }),
      session("ses-sub", "Worker's explorer subagent", { parentId: "ses-worker-1" }),
    ]);
    renderSidebar();
    await waitFor(() => expect(screen.getByTestId("workers-group")).toBeTruthy());
    // Collapsed: neither the worker nor its child renders.
    expect(screen.queryByText("Board worker")).toBeNull();

    fireEvent.click(screen.getByLabelText("Expand board workers"));
    expect(await screen.findByText("Board worker")).toBeTruthy();
    // The worker's child rides its subtree INTO the group — it is NOT
    // promoted to a main-list root (it never renders outside the
    // group), and stays hidden until the worker's subtree expands.
    expect(screen.queryByText("Worker's explorer subagent")).toBeNull();
    expect(screen.getByLabelText("Expand subtasks")).toBeTruthy();
    fireEvent.click(screen.getByLabelText("Expand subtasks"));
    expect(await screen.findByText("Worker's explorer subagent")).toBeTruthy();
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

  it("auto-expands the group when the selected session is a worker (the active session stays visible)", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-user", "My chat"),
      session("ses-worker-1", "triage backfill G", { origin: "worker" }),
    ]);
    renderSidebar("/chat/ws-1/ses-worker-1");
    // Tree precedent (auto-expand the ancestor chain of the active
    // session) — unlike the Archived group's deliberate friction, a
    // live worker CAN be the active session the user is reading.
    expect(await screen.findByText("triage backfill G")).toBeTruthy();
    expect(screen.getByLabelText("Collapse board workers")).toBeTruthy();
  });

  it("auto-expands the group when the selected session is a worker's nested subagent", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-user", "My chat"),
      session("ses-worker-1", "Board worker", { origin: "worker" }),
      session("ses-sub", "Nested explorer", { parentId: "ses-worker-1" }),
    ]);
    renderSidebar("/chat/ws-1/ses-sub");
    expect(await screen.findByText("Nested explorer")).toBeTruthy();
    expect(screen.getByLabelText("Collapse board workers")).toBeTruthy();
  });

  it("routes an archived worker to the Archived group (archive outranks the worker class)", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-user", "My chat"),
      session("ses-worker-1", "Old worker", { origin: "worker", archived: true }),
    ]);
    (workspaceWorkflowApi.sessionOrigins as ReturnType<typeof vi.fn>).mockResolvedValue([
      { sessionId: "ses-worker-1", origin: "worker" },
    ]);
    renderSidebar();
    await waitFor(() => expect(screen.getByTestId("archived-group")).toBeTruthy());
    // Archived workers count only under Archived — not a second time
    // under Board workers.
    expect(screen.queryByTestId("workers-group")).toBeNull();
  });

  it("keeps fetching the full list (presentation-only grouping; search composes server-side)", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      session("ses-user", "My chat"),
      session("ses-worker-1", "lane worker", { origin: "worker" }),
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
