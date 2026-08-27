import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { render } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";

vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getStatus: vi.fn(),
    activate: vi.fn(),
    list: vi.fn(),
    getSession: vi.fn(),
    renameWorkspace: vi.fn().mockResolvedValue({}),
    deleteWorkspace: vi.fn().mockResolvedValue({}),
    suspend: vi.fn().mockResolvedValue({}),
    getSessions: vi.fn().mockResolvedValue([]),
    markSessionSeen: vi.fn().mockResolvedValue(undefined),
    ensureSession: vi.fn(),
    renameSession: vi.fn().mockResolvedValue(undefined),
  },
}));
vi.mock("../api/messages", () => {
  const gh = vi.fn().mockResolvedValue([]);
  return { messagesApi: { getHistory: gh, getHistoryPage: vi.fn().mockImplementation(async () => { const msgs = await gh(); return { messages: msgs, nextCursor: undefined }; }), sendAsync: vi.fn(), queueMessage: vi.fn().mockResolvedValue({ messageID: "msg_q_mock" }), getQueue: vi.fn().mockResolvedValue({ messages: [] }), deleteQueueMessage: vi.fn().mockResolvedValue(undefined) } };
});
vi.mock("../api/sessions", () => ({ sessionsApi: { create: vi.fn() } }));
vi.mock("../hooks/useEventStream", () => ({ useEventStream: vi.fn() }));
vi.mock("../providers/SessionActivityProvider", () => ({
  useClearPendingUnread: () => () => {},
  useIsSessionBusy: () => false,
  useIsSessionUnread: () => false,
  useWorkspaceBusyCount: () => 0,
    useWorkspaceHung: () => false,
  useIsSessionPendingAction: () => false,
  useSessionPendingActions: () => new Set<string>(),
  useAddPendingAction: () => () => {},
  useRemovePendingAction: () => () => {},
  useAddPendingQuestion: () => () => {},
  useAddPendingPermission: () => () => {},
  usePendingQuestionsForSession: () => [],
  usePendingPermissionsForSession: () => [],
  useClearSessionPendingPrompts: () => () => {},
    useWorkspaceInputSnapshot: () => undefined,
  SessionActivityProvider: ({ children }: { children: any }) => <>{children}</>,
}));

import { workspacesApi } from "../api/workspaces";

function renderChat(path: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return {
    qc,
    ...render(
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={[path]}>
          <TooltipProvider delayDuration={0}>
            <Routes>
              <Route path="/chat/:workspaceId" element={<ChatPage />} />
              <Route path="/chat/:workspaceId/:sessionId" element={<ChatPage />} />
            </Routes>
          </TooltipProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    ),
  };
}

describe("ChatPage — workspace auto-rename", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
  });

  // --- Happy path: auto-generated name gets renamed ---

  it("renames workspace when name matches adjective-noun-number pattern", async () => {
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "happy-cloud-42", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSession as ReturnType<typeof vi.fn>).mockResolvedValue({
      id: "sess-1",
      title: "Clone lenaxia/llmsafespaces",
    });

    renderChat("/chat/ws-1/sess-1");

    await waitFor(() => {
      expect(workspacesApi.renameWorkspace).toHaveBeenCalledWith(
        "ws-1",
        "Clone lenaxia/llmsafespaces",
      );
    });
  });

  it("renames workspace when name matches 'New session - <timestamp>' pattern", async () => {
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "New session - 2026-05-27T23:03:56.256Z", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSession as ReturnType<typeof vi.fn>).mockResolvedValue({
      id: "sess-1",
      title: "Clone lenaxia/llmsafespaces",
    });

    renderChat("/chat/ws-1/sess-1");

    await waitFor(() => {
      expect(workspacesApi.renameWorkspace).toHaveBeenCalledWith(
        "ws-1",
        "Clone lenaxia/llmsafespaces",
      );
    });
  });

  // --- Skips temporary titles ---

  it("does NOT rename workspace when session title is a temporary opencode title", async () => {
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "happy-cloud-42", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSession as ReturnType<typeof vi.fn>).mockResolvedValue({
      id: "sess-1",
      title: "New session - 2026-05-28T01:00:00.000Z",
    });

    renderChat("/chat/ws-1/sess-1");

    // Wait for the hook to process
    await waitFor(() => expect(workspacesApi.getSession).toHaveBeenCalled());
    // Give time for the effect to fire
    await new Promise((r) => setTimeout(r, 50));

    expect(workspacesApi.renameWorkspace).not.toHaveBeenCalled();
  });

  it("does NOT rename workspace when session title starts with 'New session -' followed by date", async () => {
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "cool-fox-7", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSession as ReturnType<typeof vi.fn>).mockResolvedValue({
      id: "sess-1",
      title: "New session - 2026-01-01T00:00:00Z",
    });

    renderChat("/chat/ws-1/sess-1");

    await waitFor(() => expect(workspacesApi.getSession).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 50));

    expect(workspacesApi.renameWorkspace).not.toHaveBeenCalled();
  });

  // --- Does NOT rename user-chosen names ---

  it("does NOT rename workspace when name is user-chosen (not auto-generated)", async () => {
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "My Project", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSession as ReturnType<typeof vi.fn>).mockResolvedValue({
      id: "sess-1",
      title: "Clone lenaxia/llmsafespaces",
    });

    renderChat("/chat/ws-1/sess-1");

    await waitFor(() => expect(workspacesApi.getSession).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 50));

    expect(workspacesApi.renameWorkspace).not.toHaveBeenCalled();
  });

  it("does NOT rename workspace when name contains uppercase (user-set)", async () => {
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "Clone lenaxia/llmsafespaces", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSession as ReturnType<typeof vi.fn>).mockResolvedValue({
      id: "sess-1",
      title: "Different title",
    });

    renderChat("/chat/ws-1/sess-1");

    await waitFor(() => expect(workspacesApi.getSession).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 50));

    expect(workspacesApi.renameWorkspace).not.toHaveBeenCalled();
  });

  // --- Only renames once ---

  it("only renames workspace once even if title changes", async () => {
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "fast-cat-99", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSession as ReturnType<typeof vi.fn>).mockResolvedValue({
      id: "sess-1",
      title: "First Title",
    });

    renderChat("/chat/ws-1/sess-1");

    await waitFor(() => {
      expect(workspacesApi.renameWorkspace).toHaveBeenCalledWith("ws-1", "First Title");
    });

    // Should not be called a second time
    expect(workspacesApi.renameWorkspace).toHaveBeenCalledTimes(1);
  });

  // --- Does not rename when session title is empty ---

  it("does NOT rename workspace when session title is undefined", async () => {
    vi.clearAllMocks();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "blue-dog-5", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSession as ReturnType<typeof vi.fn>).mockResolvedValue({
      id: "sess-1",
      title: undefined,
    });

    renderChat("/chat/ws-1/sess-1");

    await waitFor(() => expect(workspacesApi.getSession).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 50));

    expect(workspacesApi.renameWorkspace).not.toHaveBeenCalled();
  });

  // --- Chat header display ---

  it("displays workspace name and session title in header", async () => {
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "Clone lenaxia/llmsafespaces", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSession as ReturnType<typeof vi.fn>).mockResolvedValue({
      id: "sess-1",
      title: "Fix the bug",
    });

    renderChat("/chat/ws-1/sess-1");

    await waitFor(() => {
      expect(screen.getByText("Clone lenaxia/llmsafespaces")).toBeInTheDocument();
      expect(screen.getByText("Fix the bug")).toBeInTheDocument();
    });
  });

  it("displays 'New chat' when session has no title", async () => {
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "My Workspace", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSession as ReturnType<typeof vi.fn>).mockResolvedValue({
      id: "sess-1",
      title: undefined,
    });

    renderChat("/chat/ws-1/sess-1");

    await waitFor(() => {
      expect(screen.getByText("My Workspace")).toBeInTheDocument();
      expect(screen.getByText("New chat")).toBeInTheDocument();
    });
  });
});
