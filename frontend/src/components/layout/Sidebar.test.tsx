import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, render, act, waitFor, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Sidebar } from "./Sidebar";
import { AuthProvider } from "../../providers/AuthProvider";

vi.mock("../../api/auth", () => ({
  authApi: {
    me: vi.fn().mockResolvedValue({ id: "u1", username: "alice", email: "a@b.com", role: "user", active: true }),
  },
}));

let mockIsSessionBusy = (_sid: string) => false;
let mockIsSessionUnread = (_sid: string) => false;
let mockWorkspaceBusyCount = (_wsid: string) => 0;
let mockSessionPendingActions = (): Set<string> => new Set<string>();

vi.mock("../../providers/SessionActivityProvider", () => ({
  useIsSessionBusy: (sid: string) => mockIsSessionBusy(sid),
  useIsSessionUnread: (sid: string) => mockIsSessionUnread(sid),
  useWorkspaceBusyCount: (wsid: string) => mockWorkspaceBusyCount(wsid),
  useClearPendingUnread: () => () => {},
  useIsSessionPendingAction: () => false,
  useSessionPendingActions: () => mockSessionPendingActions(),
  useAddPendingAction: () => () => {},
  useRemovePendingAction: () => () => {},
  useAddPendingQuestion: () => () => {},
  useAddPendingPermission: () => () => {},
  usePendingQuestionsForSession: () => [],
  usePendingPermissionsForSession: () => [],
  useClearSessionPendingPrompts: () => () => {},
  useSessionStatus: (sid: string) => {
    if (mockSessionPendingActions().has(sid)) return "pending_input";
    if (mockIsSessionBusy(sid)) return "busy";
    if (mockIsSessionUnread(sid)) return "unread";
    return "idle";
  },
  SessionActivityProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

vi.mock("../../api/workspaces", () => ({
  workspacesApi: {
    list: vi.fn().mockResolvedValue({
      items: [
        { id: "ws-1", name: "alpha", phase: "Active", userId: "u1", runtime: "python", storageSize: "5Gi", createdAt: "", updatedAt: "" },
      ],
      pagination: { limit: 20, offset: 0, total: 1 },
    }),
    create: vi.fn().mockResolvedValue({ id: "ws-new", name: "new-ws" }),
    activate: vi.fn().mockResolvedValue({ resumed: "ws-1" }),
    suspend: vi.fn().mockResolvedValue(undefined),
    refreshCompute: vi.fn().mockResolvedValue({ restartGeneration: 1 }),
    ensureSession: vi.fn().mockResolvedValue({ sessionId: "sess-1", workspaceId: "ws-1" }),
    getSessions: vi.fn().mockResolvedValue([
      { id: "sess-1", title: "My session", messageCount: 3, status: "idle" },
    ]),
    renameWorkspace: vi.fn().mockResolvedValue(undefined),
    deleteWorkspace: vi.fn().mockResolvedValue(undefined),
    renameSession: vi.fn().mockResolvedValue(undefined),
    deleteSession: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock("../../api/workflows", () => ({
  workspaceWorkflowApi: {
    activeRuns: vi.fn().mockResolvedValue([]),
    sessionOrigins: vi.fn().mockResolvedValue([]),
  },
}));

vi.mock("../../api/orgs", () => ({
  orgsApi: {
    list: vi.fn().mockResolvedValue([]),
  },
}));

import { workspacesApi } from "../../api/workspaces";
import { ApiClientError } from "../../api/client";

function renderSidebar() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return {
    qc,
    ...render(
      <QueryClientProvider client={qc}>
        <AuthProvider>
          <MemoryRouter>
            <Sidebar />
          </MemoryRouter>
        </AuthProvider>
      </QueryClientProvider>,
    ),
  };
}

describe("Sidebar", () => {
  it("renders app title", async () => {
    renderSidebar();
    expect(await screen.findByText("Safe Space")).toBeInTheDocument();
  });

  it("renders username", async () => {
    renderSidebar();
    expect(await screen.findByText("alice")).toBeInTheDocument();
  });

  it("renders workspace list", async () => {
    renderSidebar();
    expect(await screen.findByText("alpha")).toBeInTheDocument();
  });

  it("renders settings button", async () => {
    renderSidebar();
    expect(await screen.findByLabelText("Settings")).toBeInTheDocument();
  });

  it("renders logout button", async () => {
    renderSidebar();
    expect(await screen.findByLabelText("Log out")).toBeInTheDocument();
  });

  it("renders kebab menu for workspace", async () => {
    renderSidebar();
    const kebabButtons = await screen.findAllByLabelText("Actions");
    expect(kebabButtons.length).toBeGreaterThanOrEqual(1);
  });

  it("groups refresh, suspend, and delete under a Lifecycle section", async () => {
    renderSidebar();
    await screen.findByText("alpha");

    const kebabButtons = await screen.findAllByLabelText("Actions");
    kebabButtons[0]!.click();

    expect(await screen.findByText("Lifecycle")).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Refresh compute" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Suspend" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Delete" })).toBeInTheDocument();
  });
});

describe("Sidebar — workspace refresh compute", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("shows a confirm dialog before refreshing and refreshes on confirm", async () => {
    renderSidebar();
    await screen.findByText("alpha");

    const kebabButtons = await screen.findAllByLabelText("Actions");
    kebabButtons[0]!.click();

    // Clicking the kebab item opens the confirm dialog (does not refresh yet).
    (await screen.findByRole("menuitem", { name: "Refresh compute" })).click();
    expect(await screen.findByText("Refresh compute?")).toBeInTheDocument();
    expect(workspacesApi.refreshCompute).not.toHaveBeenCalled();

    // Confirming in the dialog triggers the refresh.
    (await screen.findByRole("button", { name: "Refresh compute" })).click();
    await screen.findByText("alpha");
    expect(workspacesApi.refreshCompute).toHaveBeenCalledWith("ws-1");
  });

  it("does not refresh compute when the confirm dialog is cancelled", async () => {
    renderSidebar();
    await screen.findByText("alpha");

    const kebabButtons = await screen.findAllByLabelText("Actions");
    kebabButtons[0]!.click();

    (await screen.findByRole("menuitem", { name: "Refresh compute" })).click();
    expect(await screen.findByText("Refresh compute?")).toBeInTheDocument();

    (await screen.findByRole("button", { name: "Cancel" })).click();

    expect(workspacesApi.refreshCompute).not.toHaveBeenCalled();
  });

  it("new workspace button creates immediately without dialog", async () => {
    renderSidebar();
    const btn = await screen.findByLabelText("New workspace (default image)");
    expect(btn).toBeInTheDocument();
    // No dialog should be visible — the button triggers creation directly
    expect(screen.queryByText("New Workspace")).not.toBeInTheDocument();
  });

  it("does not render 'Sessions' subheading", async () => {
    renderSidebar();
    await screen.findByText("alpha");
    expect(screen.queryByText("Sessions")).not.toBeInTheDocument();
  });

  it("sidebar has resize-x class for resizability", async () => {
    renderSidebar();
    const aside = await screen.findByLabelText("Navigation");
    expect(aside.className).toContain("resize-x");
  });

  it("sidebar has overflow-x-hidden to prevent horizontal scroll", async () => {
    renderSidebar();
    const scrollContainer = (await screen.findByLabelText("Navigation")).querySelector(".overflow-x-hidden");
    expect(scrollContainer).toBeInTheDocument();
  });
});

describe("Sidebar — session delete", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("calls deleteSession when session kebab delete is confirmed (#814)", async () => {
    const { qc } = renderSidebar();

    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "My session", messageCount: 3, status: "idle" },
    ]);

    await screen.findByText("My session");

    const kebabButtons = await screen.findAllByLabelText("Actions");
    const sessionKebab = kebabButtons[kebabButtons.length - 1]!;
    sessionKebab.click();

    const deleteBtn = await screen.findByText("Delete");
    deleteBtn.click();

    // ConfirmDialog opens — click "Delete" in dialog
    await waitFor(() => screen.getByRole("button", { name: "Delete" }));
    fireEvent.click(screen.getAllByRole("button", { name: "Delete" }).pop()!);

    await waitFor(() => {
      expect(workspacesApi.deleteSession).toHaveBeenCalledWith("ws-1", "sess-1");
    });
  });

  it("does not call deleteSession when confirm is cancelled (#814)", async () => {
    const { qc } = renderSidebar();

    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Keep me", messageCount: 1, status: "idle" },
    ]);

    await screen.findByText("Keep me");

    const kebabButtons = await screen.findAllByLabelText("Actions");
    const sessionKebab = kebabButtons[kebabButtons.length - 1]!;
    sessionKebab.click();

    const deleteBtn = await screen.findByText("Delete");
    deleteBtn.click();

    // ConfirmDialog opens — click "Cancel"
    await waitFor(() => screen.getByRole("button", { name: "Cancel" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    expect(workspacesApi.deleteSession).not.toHaveBeenCalled();
  });

  it("treats 404 as success on delete (#814)", async () => {
    const { qc } = renderSidebar();

    const err404 = new ApiClientError(404, { error: "not found" });
    (workspacesApi.deleteSession as ReturnType<typeof vi.fn>).mockRejectedValueOnce(err404);

    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Will 404", messageCount: 1, status: "idle" },
    ]);

    await screen.findByText("Will 404");

    const kebabButtons = await screen.findAllByLabelText("Actions");
    const sessionKebab = kebabButtons[kebabButtons.length - 1]!;
    sessionKebab.click();

    const deleteBtn = await screen.findByText("Delete");
    deleteBtn.click();

    // ConfirmDialog opens — click "Delete" in dialog
    await waitFor(() => screen.getByRole("button", { name: "Delete" }));
    fireEvent.click(screen.getAllByRole("button", { name: "Delete" }).pop()!);

    await waitFor(() => {
      expect(workspacesApi.deleteSession).toHaveBeenCalledWith("ws-1", "sess-1");
    });
  });

  it("works in sandboxed iframe (ConfirmDialog renders in DOM) (#814)", async () => {
    const { qc } = renderSidebar();

    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Keep me", messageCount: 1, status: "idle" },
    ]);

    await screen.findByText("Keep me");

    // window.confirm blocked — ConfirmDialog should still work
    const confirmSpy = vi.spyOn(window, "confirm").mockImplementation(() => { throw new Error("Blocked"); });

    const kebabButtons = await screen.findAllByLabelText("Actions");
    const sessionKebab = kebabButtons[kebabButtons.length - 1]!;
    sessionKebab.click();

    const deleteBtn = await screen.findByText("Delete");
    deleteBtn.click();

    // Dialog must render despite window.confirm being blocked
    await waitFor(() => screen.getByRole("button", { name: "Delete" }));
    fireEvent.click(screen.getAllByRole("button", { name: "Delete" }).pop()!);

    await waitFor(() => {
      expect(workspacesApi.deleteSession).toHaveBeenCalledWith("ws-1", "sess-1");
    });
    confirmSpy.mockRestore();
  });
});

describe("Sidebar — activity spinner and unread pulsation (US-37.5/37.6)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockIsSessionBusy = (_sid: string) => false;
    mockIsSessionUnread = (_sid: string) => false;
    mockWorkspaceBusyCount = (_wsid: string) => 0;
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [
        { id: "ws-1", name: "alpha", phase: "Active", userId: "u1", runtime: "python", storageSize: "5Gi", createdAt: "", updatedAt: "" },
      ],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-1", title: "Busy session", messageCount: 1, status: "idle" },
      { id: "sess-2", title: "Idle session", messageCount: 2, status: "idle" },
    ]);
  });

  it("shows spinner for busy session", async () => {
    mockIsSessionBusy = (sid: string) => sid === "sess-1";
    mockWorkspaceBusyCount = (_wsid: string) => 1;

    const { qc } = renderSidebar();

    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Busy session", messageCount: 1, status: "idle" },
      { id: "sess-2", title: "Idle session", messageCount: 2, status: "idle" },
    ]);

    await screen.findByText("Busy session");
    const spinners = document.querySelectorAll(".animate-spin");
    expect(spinners.length).toBeGreaterThanOrEqual(1);
  });

  it("applies unread pulse class to unread session", async () => {
    mockIsSessionUnread = (sid: string) => sid === "sess-2";

    const { qc } = renderSidebar();

    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Selected", messageCount: 1, status: "idle" },
      { id: "sess-2", title: "Unread session", messageCount: 2, status: "idle" },
    ]);

    await screen.findByText("Unread session");
    const pulsing = document.querySelectorAll(".animate-unread-pulse");
    expect(pulsing.length).toBeGreaterThanOrEqual(1);
  });

  it("shows spinner on collapsed workspace when sessions are busy (#34)", async () => {
    mockWorkspaceBusyCount = (wsid: string) => wsid === "ws-1" ? 2 : 0;

    const { qc } = renderSidebar();

    await screen.findByText("alpha");

    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Busy session", messageCount: 1, status: "idle" },
    ]);

    await screen.findByText("Busy session");

    const collapseButton = screen.getByText("alpha").closest("button")!;
    await collapseButton.click();

    const workspaceButton = screen.getByText("alpha").closest("button")!;
    const blueSpinners = workspaceButton.querySelectorAll("[data-busy]");
    expect(blueSpinners.length).toBe(1);
  });

  // Only top-level (parent) sessions pulsate when they have an unread response.
  // Subtasks (children) stay quiet when done — pulsating every completed
  // subtask is noise. Children still show the blue spinner while busy.
  it("only top-level sessions pulse; subtasks do not pulse when unread", async () => {
    mockIsSessionUnread = (sid: string) => sid === "sess-parent" || sid === "sess-child";
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-parent", title: "Parent session", messageCount: 2, status: "idle", hasUnread: true },
      { id: "sess-child", parentId: "sess-parent", title: "Child task", messageCount: 1, status: "idle", hasUnread: true },
    ]);

    renderSidebar();

    await screen.findByText("Parent session");

    // Expand the parent so the child row is rendered.
    await act(async () => {
      screen.getByLabelText("Expand subtasks").click();
    });

    await screen.findByText("Child task");

    // The parent (depth 0) title pulses; the child (depth 1) title does not.
    const parentTitle = screen.getByText("Parent session");
    const childTitle = screen.getByText("Child task");
    expect(parentTitle.className).toContain("animate-unread-pulse");
    expect(childTitle.className).not.toContain("animate-unread-pulse");
  });

  it("subtask still shows blue spinner when busy (depth does not affect busy)", async () => {
    mockIsSessionBusy = (sid: string) => sid === "sess-child";
    mockWorkspaceBusyCount = (wsid: string) => wsid === "ws-1" ? 1 : 0;
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-parent", title: "Parent session", messageCount: 2, status: "idle" },
      { id: "sess-child", parentId: "sess-parent", title: "Child task", messageCount: 1, status: "idle" },
    ]);

    renderSidebar();

    await screen.findByText("Parent session");

    await act(async () => {
      screen.getByLabelText("Expand subtasks").click();
    });

    await screen.findByText("Child task");

    // The child row should contain a blue spinner.
    const childRow = screen.getByText("Child task").closest("div")!;
    const blueSpinners = childRow.querySelectorAll("[data-busy]");
    expect(blueSpinners.length).toBe(1);
  });
});

describe("Sidebar — suspended workspace does not auto-resume", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockIsSessionBusy = (_sid: string) => false;
    mockIsSessionUnread = (_sid: string) => false;
    mockWorkspaceBusyCount = (_wsid: string) => 0;
  });

  it("clicking suspended workspace name does not call activate", async () => {
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [
        { id: "ws-sus", name: "suspended", phase: "Suspended", userId: "u1", runtime: "python", storageSize: "5Gi", createdAt: "", updatedAt: "" },
      ],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    renderSidebar();

    await screen.findByText("suspended");
    screen.getByText("suspended").closest("button")!.click();

    expect(workspacesApi.activate).not.toHaveBeenCalled();
  });

  it("clicking the resume (Play) button calls activate", async () => {
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [
        { id: "ws-sus", name: "suspended", phase: "Suspended", userId: "u1", runtime: "python", storageSize: "5Gi", createdAt: "", updatedAt: "" },
      ],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    renderSidebar();

    await screen.findByText("suspended");
    await act(async () => {
      screen.getByLabelText("Resume workspace").click();
    });

    expect(workspacesApi.activate).toHaveBeenCalledWith("ws-sus");
  });
});

describe("Sidebar — pending action indicator", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockIsSessionBusy = (_sid: string) => false;
    mockIsSessionUnread = (_sid: string) => false;
    mockWorkspaceBusyCount = (_wsid: string) => 0;
    mockSessionPendingActions = () => new Set<string>();
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [
        { id: "ws-1", name: "alpha", phase: "Active", userId: "u1", runtime: "python", storageSize: "5Gi", createdAt: "", updatedAt: "" },
      ],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
  });

  it("shows HelpCircle with pulse when session has pending action", async () => {
    mockSessionPendingActions = () => new Set(["sess-pending"]);
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-pending", title: "Needs action", messageCount: 1, status: "idle", hasUnread: false },
    ]);

    renderSidebar();

    await screen.findByText("Needs action");
    // The title span should pulse when a pending action exists
    const titleSpan = screen.getByText("Needs action");
    expect(titleSpan.className).toContain("animate-unread-pulse");
  });

  it("shows HelpCircle when session is busy AND has pending action (F7 — the bug)", async () => {
    mockIsSessionBusy = (sid: string) => sid === "sess-busy-pending";
    mockWorkspaceBusyCount = (wsid: string) => wsid === "ws-1" ? 1 : 0;
    mockSessionPendingActions = () => new Set(["sess-busy-pending"]);
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-busy-pending", title: "Busy Pending", messageCount: 1, status: "active", hasUnread: false },
    ]);

    renderSidebar();

    await screen.findByText("Busy Pending");
    const row = screen.getByText("Busy Pending").closest("button")!;
    // HelpCircle (amber) should be present
    expect(row.querySelector(".text-amber-500")).toBeTruthy();
    // BusyIndicator (blue spinner) should NOT be present
    expect(row.querySelector("[data-busy]")).toBeFalsy();
  });

  it("parent shows indicator when child has pending action (bubble-up)", async () => {
    mockSessionPendingActions = () => new Set(["sess-child"]);
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-parent", title: "Parent", messageCount: 1, status: "idle", hasUnread: false },
      { id: "sess-child", parentId: "sess-parent", title: "Child with prompt", messageCount: 0, status: "idle", hasUnread: false },
    ]);

    renderSidebar();

    await screen.findByText("Parent");
    // Expand parent to verify child exists
    await act(async () => { screen.getByLabelText("Expand subtasks").click(); });
    await screen.findByText("Child with prompt");

    // The parent (depth 0) should show the indicator — bubble-up from child
    const parentTitle = screen.getByText("Parent");
    const parentRowTitle = parentTitle.closest("button")?.querySelector("span");
    expect(parentRowTitle?.className).toContain("animate-unread-pulse");

    // The child (depth 1) should NOT show the indicator (only depth 0)
    const childTitle = screen.getByText("Child with prompt");
    expect(childTitle.className).not.toContain("animate-unread-pulse");
  });

  it("pending indicator shows even when session is unread", async () => {
    mockSessionPendingActions = () => new Set(["sess-urgent"]);
    mockIsSessionUnread = (sid: string) => sid === "sess-urgent";
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-urgent", title: "Urgent", messageCount: 2, status: "idle", hasUnread: true },
    ]);

    renderSidebar();

    await screen.findByText("Urgent");
    const titleSpan = screen.getByText("Urgent");
    expect(titleSpan.className).toContain("animate-unread-pulse");
  });
});
