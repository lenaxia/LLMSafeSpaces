/**
 * Integration tests for ChatPage message queue (backend-backed).
 *
 * Messages are enqueued via POST /queue (Redis-backed). The backend drains
 * the queue on session idle and publishes queue.update SSE events (sent/error).
 * The frontend manages display state (pills) locally, synced via SSE.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../providers/ThemeProvider";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";

vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getStatus: vi.fn(),
    activate: vi.fn(),
    abortSession: vi.fn(),
    list: vi.fn().mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } }),
    renameSession: vi.fn(),
    renameWorkspace: vi.fn().mockResolvedValue({}),
    markSessionSeen: vi.fn().mockResolvedValue(undefined),
    getSessions: vi.fn().mockResolvedValue([]),
  },
}));
const mockBusyState = vi.hoisted(() => {
  let val = false;
  const listeners = new Set<(v: boolean) => void>();
  return {
    get: () => val,
    set: (v: boolean) => { val = v; listeners.forEach((l) => l(v)); },
    subscribe: (l: (v: boolean) => void) => { listeners.add(l); },
    unsubscribe: (l: (v: boolean) => void) => { listeners.delete(l); },
    reset: () => { val = false; listeners.clear(); },
  };
});

vi.mock("../providers/SessionActivityProvider", async () => {
  const { useState, useEffect } = await vi.importActual<typeof import("react")>("react");
  return {
    useClearPendingUnread: () => () => {},
    useIsSessionBusy: () => {
      const [val, setVal] = useState(mockBusyState.get());
      useEffect(() => {
        mockBusyState.subscribe(setVal);
        return () => { mockBusyState.unsubscribe(setVal); };
      }, []);
      return val;
    },
    useIsSessionUnread: () => false,
    useWorkspaceBusyCount: () => 0,
    useIsSessionPendingAction: () => false,
    useSessionPendingActions: () => new Set<string>(),
    useAddPendingAction: () => () => {},
    useRemovePendingAction: () => () => {},
    useAddPendingQuestion: () => () => {},
    useAddPendingPermission: () => () => {},
    usePendingQuestionsForSession: () => [],
    usePendingPermissionsForSession: () => [],
    useClearSessionPendingPrompts: () => () => {},
    useSessionStatus: () => "idle",
    resolveSessionStatus: () => "idle",
    SessionActivityProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
  };
});
vi.mock("../api/messages", () => ({
  messagesApi: {
    getHistory: vi.fn().mockResolvedValue([]),
    getHistoryPage: vi.fn().mockResolvedValue({ messages: [], nextCursor: undefined }),
    sendAsync: vi.fn(),
    queueMessage: vi.fn().mockResolvedValue({ messageID: "msg_q_test" }),
    getQueue: vi.fn().mockResolvedValue({ messages: [] }),
    deleteQueueMessage: vi.fn().mockResolvedValue(undefined),
  },
}));
vi.mock("../api/sessions", () => ({ sessionsApi: { create: vi.fn() } }));

let capturedSSEHandler: ((data: unknown) => void) | null = null;
vi.mock("../hooks/useEventStream", () => ({
  useEventStream: vi.fn((_workspaceId: string | undefined, handler: (data: unknown) => void) => {
    capturedSSEHandler = handler;
  }),
}));

import { workspacesApi } from "../api/workspaces";
import { messagesApi } from "../api/messages";

function makeQueryClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
}

function renderChat(qc: QueryClient, path: string) {
  const wsId = path.split("/")[2];
  const sesId = path.split("/")[3];
  qc.setQueryData(["workspace-status", wsId], { phase: "Active", sessions: [{ id: sesId, status: "idle" }] });
  qc.setQueryData(["workspaces"], { items: [], pagination: { limit: 20, offset: 0, total: 0 } });
  qc.setQueryData(["messages", wsId, sesId], { pages: [{ messages: [], nextCursor: undefined }], pageParams: [undefined] });
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TooltipProvider delayDuration={0}>
          <MemoryRouter initialEntries={[path]}>
            <Routes>
              <Route path="/chat/:workspaceId/:sessionId" element={<ChatPage />} />
            </Routes>
          </MemoryRouter>
        </TooltipProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

function sendSSE(event: Record<string, unknown>) {
  if (event.type === "session.status") {
    mockBusyState.set(event.status === "busy");
  }
  act(() => { capturedSSEHandler?.(event); });
}

describe("ChatPage message queue (backend-backed)", () => {
  beforeEach(() => {
    capturedSSEHandler = null;
    mockBusyState.reset();
    vi.clearAllMocks();
    // resetAllMocks is needed because some tests use mockImplementation
    // (not mockReturnValue), which clearAllMocks doesn't reset.
    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({ messages: [] });
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active", sessions: [{ id: "ses_1", status: "idle" }] });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } });
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockResolvedValue({ messages: [], nextCursor: undefined });
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
  });

  it("sends immediately when not busy", async () => {
    const user = userEvent.setup();
    renderChat(makeQueryClient(), "/chat/ws-1/ses_1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    await user.type(document.querySelector("textarea")!, "hello");
    await user.click(screen.getByRole("button", { name: "Send message" }));

    await waitFor(() => {
      expect(messagesApi.sendAsync).toHaveBeenCalledWith("ws-1", "ses_1", {
        parts: [{ type: "text", text: "hello" }],
      });
    });
  });

  it("textarea stays enabled during streaming", async () => {
    renderChat(makeQueryClient(), "/chat/ws-1/ses_1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    sendSSE({ type: "session.status", session_id: "ses_1", status: "busy" });

    expect(document.querySelector("textarea")).not.toBeDisabled();
  });

  it("holds message in queue when busy — calls queueMessage not sendAsync", async () => {
    const user = userEvent.setup();
    renderChat(makeQueryClient(), "/chat/ws-1/ses_1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    sendSSE({ type: "session.status", session_id: "ses_1", status: "busy" });

    await user.type(document.querySelector("textarea")!, "queued msg");
    await user.click(screen.getByRole("button", { name: "Send message" }));

    await waitFor(() => {
      expect(messagesApi.queueMessage).toHaveBeenCalledWith("ws-1", "ses_1", "queued msg");
    });
    expect(screen.getByText("queued msg")).toBeInTheDocument();
    expect(screen.getByText("1 message queued")).toBeInTheDocument();
    expect(messagesApi.sendAsync).not.toHaveBeenCalled();
  });

  it("holds message in queue when queue is non-empty, even after session goes idle", async () => {
    // Regression: without checking queue.queuedMessages.length, a direct
    // send races ahead of the draining queue when the session transitions
    // busy→idle. opencode assigns the direct send an earlier
    // info.time.created than the still-draining queued message, so on
    // reload selectChronological places the queued message AFTER the
    // direct send — out of FIFO order.
    //
    // The race window is: idle event arrives → reconcileOnIdle calls
    // refreshQueue → GET /queue returns [A] (server hasn't drained yet)
    // → pill stays visible → user clicks send for B. In this window,
    // isSessionBusy=false and streaming=false, so without the fix,
    // handleSend would route to doSendNow (direct send).
    const user = userEvent.setup();
    // Stateful getQueue mock: returns empty until user enqueues A, then
    // returns [A] to simulate the drain window (Redis still holds A).
    let userEnqueuedA = false;
    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockImplementation(() => {
      return Promise.resolve({
        messages: userEnqueuedA ? [{
          id: "msg_q_test",
          text: "message A",
          session_id: "ses_1",
          workspace_id: "ws-1",
          enqueued_at: new Date().toISOString(),
          retry_count: 0,
        }] : [],
      });
    });
    // queueMessage flips the flag so subsequent refreshQueue calls return [A]
    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockImplementation(() => {
      userEnqueuedA = true;
      return Promise.resolve({ messageID: "msg_q_test" });
    });

    renderChat(makeQueryClient(), "/chat/ws-1/ses_1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    // 1. Session busy → enqueue message A
    sendSSE({ type: "session.status", session_id: "ses_1", status: "busy" });
    await user.type(document.querySelector("textarea")!, "message A");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    await waitFor(() => expect(screen.getByText("1 message queued")).toBeInTheDocument());

    // 2. Session goes idle — server-side drain starts but Redis still holds A.
    //    refreshQueue (triggered by reconcileOnIdle) keeps the pill visible
    //    because getQueue now returns [A].
    sendSSE({ type: "session.status", session_id: "ses_1", status: "idle" });

    // 3. User sends message B during the drain window.
    await user.type(document.querySelector("textarea")!, "message B");
    await user.click(screen.getByRole("button", { name: "Send message" }));

    await waitFor(() => {
      expect(messagesApi.queueMessage).toHaveBeenCalledWith("ws-1", "ses_1", "message B");
    });
    expect(messagesApi.sendAsync).not.toHaveBeenCalled();
  });

  it("queue pill is removed when backend sends (queue.update sent event)", async () => {
    const user = userEvent.setup();
    renderChat(makeQueryClient(), "/chat/ws-1/ses_1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    sendSSE({ type: "session.status", session_id: "ses_1", status: "busy" });

    await user.type(document.querySelector("textarea")!, "queued msg");
    await user.click(screen.getByRole("button", { name: "Send message" }));

    await waitFor(() => expect(screen.getByText("1 message queued")).toBeInTheDocument());

    sendSSE({ type: "queue.update", session_id: "ses_1", data: { event: "sent", messageID: "msg_q_test" } });

    await waitFor(() => {
      expect(screen.queryByText(/queued/)).not.toBeInTheDocument();
    });
  });

  it("queue pill shows error on queue.update error event", async () => {
    const user = userEvent.setup();
    renderChat(makeQueryClient(), "/chat/ws-1/ses_1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    sendSSE({ type: "session.status", session_id: "ses_1", status: "busy" });

    await user.type(document.querySelector("textarea")!, "will fail");
    await user.click(screen.getByRole("button", { name: "Send message" }));

    await waitFor(() => expect(screen.getByText("1 message queued")).toBeInTheDocument());

    sendSSE({ type: "queue.update", session_id: "ses_1", data: { event: "error", messageID: "msg_q_test", error: "send failed" } });

    await waitFor(() => {
      expect(screen.getByLabelText("Retry")).toBeInTheDocument();
      expect(screen.getByLabelText("Dismiss")).toBeInTheDocument();
    });
  });

  it("abort clears all queue pills", async () => {
    const user = userEvent.setup();
    renderChat(makeQueryClient(), "/chat/ws-1/ses_1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    sendSSE({ type: "session.status", session_id: "ses_1", status: "busy" });

    await user.type(document.querySelector("textarea")!, "first");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    await user.type(document.querySelector("textarea")!, "second");
    await user.click(screen.getByRole("button", { name: "Send message" }));

    await waitFor(() => {
      expect(screen.getByText("2 messages queued")).toBeInTheDocument();
    });

    await user.click(screen.getByLabelText("Stop generating"));

    expect(workspacesApi.abortSession).toHaveBeenCalledWith("ws-1", "ses_1");
  });

  it("stop button is shown during streaming", async () => {
    renderChat(makeQueryClient(), "/chat/ws-1/ses_1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    sendSSE({ type: "session.status", session_id: "ses_1", status: "busy" });

    await waitFor(() => expect(screen.getByLabelText("Stop generating")).toBeInTheDocument());
  });

  it("dismiss removes error pill", async () => {
    const user = userEvent.setup();
    renderChat(makeQueryClient(), "/chat/ws-1/ses_1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    sendSSE({ type: "session.status", session_id: "ses_1", status: "busy" });

    await user.type(document.querySelector("textarea")!, "msg");
    await user.click(screen.getByRole("button", { name: "Send message" }));

    await waitFor(() => expect(screen.getByText("1 message queued")).toBeInTheDocument());

    sendSSE({ type: "queue.update", session_id: "ses_1", data: { event: "error", messageID: "msg_q_test", error: "fail" } });

    await waitFor(() => expect(screen.getByLabelText("Dismiss")).toBeInTheDocument());
    await user.click(screen.getByLabelText("Dismiss"));

    await waitFor(() => {
      expect(screen.queryByLabelText("Dismiss")).not.toBeInTheDocument();
    });
  });

  it("abort deletes queued messages from Redis via clearAll", async () => {
    const user = userEvent.setup();
    renderChat(makeQueryClient(), "/chat/ws-1/ses_1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    sendSSE({ type: "session.status", session_id: "ses_1", status: "busy" });

    await user.type(document.querySelector("textarea")!, "queued msg");
    await user.click(screen.getByRole("button", { name: "Send message" }));

    await waitFor(() => expect(screen.getByText("1 message queued")).toBeInTheDocument());

    await user.click(screen.getByLabelText("Stop generating"));

    await waitFor(() => {
      expect(messagesApi.deleteQueueMessage).toHaveBeenCalledWith("ws-1", "ses_1", "msg_q_test");
    });
  });

  it("queue.update dismissed event removes error pill via removeById", async () => {
    const user = userEvent.setup();
    renderChat(makeQueryClient(), "/chat/ws-1/ses_1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    sendSSE({ type: "session.status", session_id: "ses_1", status: "busy" });

    await user.type(document.querySelector("textarea")!, "will error");
    await user.click(screen.getByRole("button", { name: "Send message" }));

    await waitFor(() => expect(screen.getByText("1 message queued")).toBeInTheDocument());

    sendSSE({ type: "queue.update", session_id: "ses_1", data: { event: "error", messageID: "msg_q_test", error: "failed" } });

    await waitFor(() => expect(screen.getByLabelText("Dismiss")).toBeInTheDocument());

    sendSSE({ type: "queue.update", session_id: "ses_1", data: { event: "dismissed", messageID: "msg_q_test" } });

    await waitFor(() => {
      expect(screen.queryByLabelText("Dismiss")).not.toBeInTheDocument();
    });
  });
});
