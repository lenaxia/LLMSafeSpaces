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
    getSession: vi.fn().mockResolvedValue({ title: "" }),
    renameWorkspace: vi.fn().mockResolvedValue({}),
    deleteWorkspace: vi.fn().mockResolvedValue({}),
    suspend: vi.fn().mockResolvedValue({}),
    getSessions: vi.fn().mockResolvedValue([]),
    markSessionSeen: vi.fn().mockResolvedValue(undefined),
    ensureSession: vi.fn(),
    renameSession: vi.fn().mockResolvedValue(undefined),
    abortSession: vi.fn(),
    requestInputSnapshot: vi.fn().mockResolvedValue(undefined).mockResolvedValue({}),
    deleteSession: vi.fn().mockResolvedValue({}),
    listModels: vi.fn().mockResolvedValue({ models: [], currentModel: "" }),
    setModel: vi.fn().mockResolvedValue({ model: "", applied: false }),
  },
}));
vi.mock("../api/messages", () => {
  const gh = vi.fn().mockResolvedValue([]);
  return { messagesApi: { getHistory: gh, getHistoryPage: vi.fn().mockImplementation(async () => { const msgs = await gh(); return { messages: msgs, nextCursor: undefined }; }), sendAsync: vi.fn(), queueMessage: vi.fn().mockResolvedValue({ messageID: "msg_q_mock" }), getQueue: vi.fn().mockResolvedValue({ messages: [] }), deleteQueueMessage: vi.fn().mockResolvedValue(undefined) } };
});
vi.mock("../api/sessions", () => ({ sessionsApi: { create: vi.fn() } }));
vi.mock("../hooks/useEventStream", () => ({ useEventStream: vi.fn() }));
vi.mock("../hooks/useUserEventStream", () => ({ useUserEventStream: vi.fn() }));
vi.mock("../hooks/useChatStream", () => ({
  useChatStream: vi.fn(() => ({
    send: vi.fn(),
    abort: vi.fn(),
    streaming: false,
    localStreaming: false,
    notifySessionIdle: vi.fn(),
    error: null,
    clearError: vi.fn(),
    atCapRetryAfter: null,
    clearAtCap: vi.fn(),
    streamTimedOut: false,
    clearStreamTimedOut: vi.fn(),
  })),
}));
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

describe("ChatPage — subtask sessions are read-only", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "My Workspace", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
  });

  it("hides the composer and shows the view-only banner for a subtask (parentId set)", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-parent", title: "Parent task", messageCount: 1, status: "idle", hasUnread: false },
      { id: "sess-sub", parentId: "sess-parent", title: "Subtask", messageCount: 1, status: "idle", hasUnread: false },
    ]);

    renderChat("/chat/ws-1/sess-sub");

    await waitFor(() => {
      expect(screen.getByRole("status")).toBeInTheDocument();
    });
    expect(screen.getByText(/Subtasks are view-only/i)).toBeInTheDocument();
    expect(screen.queryByPlaceholderText("Type a message...")).not.toBeInTheDocument();
  });

  it("keeps the composer for a top-level session (no parentId)", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-parent", title: "Parent task", messageCount: 1, status: "idle", hasUnread: false },
    ]);

    renderChat("/chat/ws-1/sess-parent");

    await waitFor(() => {
      expect(screen.getByPlaceholderText("Type a message...")).toBeInTheDocument();
    });
    expect(screen.queryByText(/Subtasks are view-only/i)).not.toBeInTheDocument();
  });
});
