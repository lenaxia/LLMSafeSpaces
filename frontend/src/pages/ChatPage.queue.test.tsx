/**
 * Integration tests for ChatPage message queue (backend-backed).
 *
 * Messages are enqueued via POST /queue (Redis-backed). The backend drains
 * the queue on session idle and publishes queue.update SSE events (sent/error).
 * The frontend manages display state (pills) locally, synced via SSE.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor, act , fireEvent } from "@testing-library/react";
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
    requestInputSnapshot: vi.fn().mockResolvedValue(undefined),
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
    useWorkspaceInputSnapshot: () => undefined,
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
        clientMessageID: expect.any(String),
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
      expect(messagesApi.queueMessage).toHaveBeenCalledWith("ws-1", "ses_1", "queued msg", [] as string[]);
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
      expect(messagesApi.queueMessage).toHaveBeenCalledWith("ws-1", "ses_1", "message B", [] as string[]);
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

// D6 (#998): the hung-session alert banner. workspace.alert/session_hung
// renders the amber banner; session.status=idle for that session
// auto-clears it (the hang resolved); dismissal works.
describe("D6 hung-session alert banner (#998)", () => {
  it("renders the banner on workspace.alert and auto-clears on idle", async () => {
    renderChat(makeQueryClient(), "/chat/ws-1/ses_1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    sendSSE({
      type: "workspace.alert",
      workspace_id: "ws-1",
      session_id: "ses_1",
      status: "session_hung",
      data: { alert: "session_hung", oldest_busy_seconds: 960, policy: "notify_only", guidance: "g" },
    });

    const banner = await screen.findByText(/busy for 16 min without/i);
    expect(banner).toBeInTheDocument();

    // The hang resolves: idle for the same session clears it.
    sendSSE({ type: "session.status", session_id: "ses_1", status: "idle" });
    await waitFor(() => {
      expect(screen.queryByText(/busy for 16 min without/i)).not.toBeInTheDocument();
    });
  });

  it("banner is dismissable", async () => {
    renderChat(makeQueryClient(), "/chat/ws-1/ses_1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    sendSSE({
      type: "workspace.alert",
      workspace_id: "ws-1",
      session_id: "ses_1",
      status: "session_hung",
      data: { alert: "session_hung", oldest_busy_seconds: 960, policy: "notify_only", guidance: "g" },
    });

    const dismiss = await screen.findByRole("button", { name: "Dismiss" });
    fireEvent.click(dismiss);
    await waitFor(() => {
      expect(screen.queryByText(/busy for 16 min without/i)).not.toBeInTheDocument();
    });
  });
});

describe("queue-path regressions (suspend/resume casualties)", () => {
  beforeEach(() => {
    capturedSSEHandler = null;
    mockBusyState.reset();
    vi.clearAllMocks();
    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({ messages: [] });
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active", sessions: [{ id: "ses_1", status: "idle" }] });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } });
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockResolvedValue({ messages: [], nextCursor: undefined });
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
  });

  it("parked ERROR entries do not force the queue path — direct send still used", async () => {
    // The suspend/resume casualty: entries parked as "delivery
    // unverifiable" stay in the queue forever until Retry/Dismiss.
    // Counting them as "queue busy" permanently reroutes every new send
    // through enqueue (which skips the user-echo strip and mis-renders).
    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({
      messages: [
        { id: "q-err-1", text: "old stranded message", status: "error", lastError: "delivery unverifiable: agent unreachable", session_id: "ses_1" },
      ],
    });
    const user = userEvent.setup();
    renderChat(makeQueryClient(), "/chat/ws-1/ses_1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());
    // Wait for the queue refresh to land the error entry in state.
    await waitFor(() => expect(screen.getByText("old stranded message")).toBeInTheDocument());

    await user.type(document.querySelector("textarea")!, "fresh direct message");
    await user.click(screen.getByRole("button", { name: "Send message" }));

    await waitFor(() => {
      expect(messagesApi.sendAsync).toHaveBeenCalledWith("ws-1", "ses_1", {
        parts: [{ type: "text", text: "fresh direct message" }],
        clientMessageID: expect.any(String),
      });
    });
    expect(messagesApi.queueMessage).not.toHaveBeenCalled();
  });

  it("user echo of a QUEUED message is NOT rendered as an assistant bubble", async () => {
    // Busy → send → enqueue. Then the outbox delivers later: the agent
    // echoes the user turn as a part.end text event. Without echo
    // tracking on the queue path, that text lands in the assistant
    // streaming buffer (ChatView hardcodes role=assistant).
    const user = userEvent.setup();
    renderChat(makeQueryClient(), "/chat/ws-1/ses_1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    sendSSE({ type: "session.status", session_id: "ses_1", status: "busy" });
    await user.type(document.querySelector("textarea")!, "queued hello");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    await waitFor(() => expect(messagesApi.queueMessage).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByText("1 message queued")).toBeInTheDocument());

    // Agent reachable again; the outbox delivers the queued message and
    // a NEW turn starts. The turn echoes the user text first, then the
    // assistant answer streams — both while the stream is live.
    sendSSE({ type: "session.status", session_id: "ses_1", status: "busy" });
    sendSSE({
      type: "session.event",
      session_id: "ses_1",
      data: {
        type: "part.end",
        sessionId: "ses_1",
        messageId: "msg_user_echo",
        part: { id: "pt_1", type: "text", text: "queued hello" },
      },
    });
    sendSSE({
      type: "session.event",
      session_id: "ses_1",
      data: {
        type: "part.end",
        sessionId: "ses_1",
        messageId: "msg_reply",
        part: { id: "pt_2", type: "text", text: "the answer" },
      },
    });

    // Sanity: the assistant stream IS rendering (the answer shows on
    // the agent side) — proving the echo check below is not vacuous.
    await waitFor(() => {
      const bubbles = Array.from(document.querySelectorAll(".justify-start"));
      expect(bubbles.some((el) => el.textContent?.includes("the answer"))).toBe(true);
    });
    // The user echo must NOT appear in any agent-side bubble.
    const agentBubbles = Array.from(document.querySelectorAll(".justify-start"));
    const leak = agentBubbles.find((el) => el.textContent?.includes("queued hello"));
    expect(leak).toBeUndefined();
    // The echo also clears the pending pill: the agent owns the message
    // now, it is in the conversation, not the queue.
    await waitFor(() => {
      expect(screen.queryByText("1 message queued")).not.toBeInTheDocument();
    });
  });

  it("a delivering entry is invisible — the pill clears when the worker picks it up", async () => {
    // The outbox delivery send is synchronous turn-to-completion, so an
    // entry stays staged as delivering for the WHOLE multi-minute turn.
    // The pill must not render "Sending…" for that window (TUI parity:
    // once the agent owns the message it is in the conversation).
    let enqueued = false;
    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockImplementation(() => {
      return Promise.resolve({
        messages: enqueued
          ? [{ id: "msg_q_test", text: "long turn message", session_id: "ses_1", status: "delivering" }]
          : [],
      });
    });
    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockImplementation(() => {
      enqueued = true;
      return Promise.resolve({ messageID: "msg_q_test" });
    });

    const user = userEvent.setup();
    renderChat(makeQueryClient(), "/chat/ws-1/ses_1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    sendSSE({ type: "session.status", session_id: "ses_1", status: "busy" });
    await user.type(document.querySelector("textarea")!, "long turn message");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    await waitFor(() => expect(screen.getByText("1 message queued")).toBeInTheDocument());

    // The worker stages the entry out (POST in flight) and the bridge
    // announces it; the refresh that follows must drop the pill even
    // though GET /queue still REPORTS the delivering entry.
    sendSSE({ type: "queue.update", session_id: "ses_1", data: { event: "delivering", messageID: "msg_q_test" } });

    await waitFor(() => {
      expect(screen.queryByText("1 message queued")).not.toBeInTheDocument();
    });
    expect(screen.queryByText("Sending…")).not.toBeInTheDocument();
  });
});

describe("V2 delivery: live streaming through the session.next sequence (#1112)", () => {
  beforeEach(() => {
    capturedSSEHandler = null;
    mockBusyState.reset();
    vi.clearAllMocks();
    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({ messages: [] });
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active", sessions: [{ id: "ses_1", status: "idle" }] });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } });
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockResolvedValue({ messages: [], nextCursor: undefined });
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
  });

  it("streams assistant text incrementally — not only at block end", async () => {
    // Regression for the #1110 post-merge gap: the prompted user echo
    // leaves the delta buffer in "user-echo" (discard) mode; without
    // the text.started priming part.end, deltas were dropped and the
    // text appeared only at text.ended.
    const user = userEvent.setup();
    renderChat(makeQueryClient(), "/chat/ws-1/ses_1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    sendSSE({ type: "session.status", session_id: "ses_1", status: "busy" });
    await user.type(document.querySelector("textarea")!, "stream me live");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    await waitFor(() => expect(messagesApi.queueMessage).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByText("1 message queued")).toBeInTheDocument());

    const agentBubbleText = () =>
      Array.from(document.querySelectorAll(".justify-start")).map((el) => el.textContent ?? "").join("\n");

    // The translated V2 sequence: user echo → priming (empty part.end)
    // → deltas → final snapshot. All as session.event contract events.
    const ce = (data: unknown) =>
      sendSSE({ type: "session.event", session_id: "ses_1", data });

    ce({ type: "part.end", sessionId: "ses_1", messageId: "msg_u", part: { type: "text", text: "stream me live" } });
    ce({ type: "part.end", sessionId: "ses_1", messageId: "msg_a", partId: "text-0", part: { id: "text-0", type: "text", text: "" } });

    ce({ type: "part.delta", sessionId: "ses_1", messageId: "msg_a", partId: "text-0", delta: "Hello " });
    await waitFor(() => expect(agentBubbleText()).toContain("Hello"));

    ce({ type: "part.delta", sessionId: "ses_1", messageId: "msg_a", partId: "text-0", delta: "world" });
    await waitFor(() => expect(agentBubbleText()).toContain("Hello world"));

    // The final snapshot replaces the buffer without duplicating.
    ce({ type: "part.end", sessionId: "ses_1", messageId: "msg_a", partId: "text-0", part: { id: "text-0", type: "text", text: "Hello world" } });
    await waitFor(() => expect(agentBubbleText().match(/Hello world/g)?.length).toBe(1));

    // The user echo never renders agent-side, and the pill cleared.
    expect(agentBubbleText()).not.toContain("stream me live");
    await waitFor(() => expect(screen.queryByText("1 message queued")).not.toBeInTheDocument());
  });
});
