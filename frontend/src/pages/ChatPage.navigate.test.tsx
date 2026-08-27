import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { waitFor, act } from "@testing-library/react";
import { render } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";

vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getStatus: vi.fn(),
    activate: vi.fn(),
    list: vi.fn().mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } }),
    listModels: vi.fn().mockResolvedValue({ models: [], currentModel: "" }),
    setModel: vi.fn().mockResolvedValue({ model: "", applied: false }),
    renameWorkspace: vi.fn().mockResolvedValue({}),
    deleteWorkspace: vi.fn().mockResolvedValue({}),
    suspend: vi.fn().mockResolvedValue({}),
    deleteSession: vi.fn().mockResolvedValue(undefined),
    markSessionSeen: vi.fn().mockResolvedValue(undefined),
    getSessions: vi.fn().mockResolvedValue([]),
  },
}));
vi.mock("../api/messages", () => {
  const gh = vi.fn().mockResolvedValue([]);
  return { messagesApi: { getHistory: gh, getHistoryPage: vi.fn().mockImplementation(async () => { const msgs = await gh(); return { messages: msgs, nextCursor: undefined }; }), sendAsync: vi.fn(), queueMessage: vi.fn().mockResolvedValue({ messageID: "msg_q_mock" }), getQueue: vi.fn().mockResolvedValue({ messages: [] }), deleteQueueMessage: vi.fn().mockResolvedValue(undefined) } };
});
vi.mock("../api/sessions", () => ({ sessionsApi: { create: vi.fn().mockResolvedValue({ sessionId: "sess-auto" }) } }));
vi.mock("../hooks/useEventStream", () => ({ useEventStream: vi.fn() }));
vi.mock("../providers/SessionActivityProvider", () => ({
  useClearPendingUnread: () => vi.fn(),
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
  SessionActivityProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
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
              <Route path="/chat/:workspaceId/:sessionId" element={<ChatPage />} />
              <Route path="/chat/:workspaceId" element={<ChatPage />} />
            </Routes>
          </TooltipProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    ),
  };
}

describe("Mark-seen on navigate (US-37.8)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers({ shouldAdvanceTime: true });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "Test WS", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "sess-1", title: "Session 1", messageCount: 1, status: "idle", lastSeenAt: "2026-06-09T00:00:00Z", hasUnread: false },
    ]);
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("calls markSessionSeen immediately on navigate-to session", async () => {
    renderChat("/chat/ws-1/sess-1");

    await waitFor(() => {
      expect(workspacesApi.markSessionSeen).toHaveBeenCalledWith("ws-1", "sess-1");
    });
  });

  it("calls markSessionSeen debounced on navigate-away", async () => {
    const { qc } = renderChat("/chat/ws-1/sess-1");

    await waitFor(() => {
      expect(workspacesApi.markSessionSeen).toHaveBeenCalled();
    });

    vi.clearAllMocks();
    (workspacesApi.markSessionSeen as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });

    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Session 1", messageCount: 1, status: "idle" },
      { id: "sess-2", title: "Session 2", messageCount: 1, status: "idle" },
    ]);

    await act(async () => {
      vi.advanceTimersByTime(1500);
    });

    expect(workspacesApi.markSessionSeen).not.toHaveBeenCalledWith("ws-1", "sess-1");
  });

  it("does not call markSessionSeen when workspace is not Active", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Suspended" });

    renderChat("/chat/ws-1/sess-1");

    await act(async () => {
      vi.advanceTimersByTime(2000);
    });

    expect(workspacesApi.markSessionSeen).not.toHaveBeenCalled();
  });

  it("silently ignores failed markSessionSeen calls", async () => {
    (workspacesApi.markSessionSeen as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("network error"));

    const { container } = renderChat("/chat/ws-1/sess-1");

    await act(async () => {
      vi.advanceTimersByTime(1000);
    });

    expect(container.querySelector("[role='alert']")).not.toBeInTheDocument();
  });

  it("invalidates sessions query after mark-seen", async () => {
    const { qc } = renderChat("/chat/ws-1/sess-1");
    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");

    await waitFor(() => {
      expect(workspacesApi.markSessionSeen).toHaveBeenCalledWith("ws-1", "sess-1");
    });

    expect(invalidateSpy).toHaveBeenCalledWith(expect.objectContaining({ queryKey: ["sessions", "ws-1"] }));
  });
});
