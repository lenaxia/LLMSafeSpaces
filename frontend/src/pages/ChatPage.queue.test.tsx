/**
 * Integration tests for ChatPage message queue (backend-backed).
 *
 * Messages are enqueued via POST /queue (Redis-backed). The backend drains
 * the queue on session idle and publishes queue.update events on the
 * PLATFORM stream (workspace SSE); session lifecycle transitions arrive as
 * ABI contract events via useContractStream (US-69.10 hard cutover).
 *
 * A queued message's pill clears via queue.update "sent"/"delivering"
 * events (removeById + refreshQueue) — the queued-echo strip is deleted.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor, act, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { create } from "@bufbuild/protobuf";
import { ThemeProvider } from "../providers/ThemeProvider";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";
import type { Event } from "../abi/llmsafespaces/abi/v1/contract_pb";
import { EventSchema, EventType, MessageSchema, MessageType, PartSchema, PartType, SessionStatus } from "../abi/llmsafespaces/abi/v1/contract_pb";

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

// Busy state for the mocked provider (ChatPage's queue gate and the Stop
// button read it; contract SESSION_STATUS events no longer drive it here).
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

// Platform stream: queue.update / workspace.alert / workspace.phase live here.
let capturedPlatformHandler: ((data: unknown) => void) | null = null;
vi.mock("../hooks/useEventStream", () => ({
  useEventStream: vi.fn((_workspaceId: string | undefined, handler: (data: unknown) => void) => {
    capturedPlatformHandler = handler;
  }),
}));

// Contract stream: session lifecycle events (SESSION_STATUS IDLE etc.).
let capturedContractOptions: {
  onEvent: (event: Event, seq: bigint) => void;
  onSnapshot: (state: unknown) => void;
  onReconnect: () => void;
} | null = null;
let contractState: { seq: bigint; sessions: Map<string, unknown> };
vi.mock("../hooks/useContractStream", () => ({
  useContractStream: vi.fn((_workspaceId: string | undefined, options: Record<string, unknown>) => {
    capturedContractOptions = options as typeof capturedContractOptions;
    return contractState;
  }),
}));

import { workspacesApi } from "../api/workspaces";
import { messagesApi } from "../api/messages";

// --- Helpers ---

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

function sendPlatform(event: Record<string, unknown>) {
  act(() => { capturedPlatformHandler?.(event); });
}

function setBusy(v: boolean) {
  act(() => { mockBusyState.set(v); });
}

function abiEvent(partial: Partial<Event>): Event {
  return create(EventSchema, { sessionId: "ses_1", ...partial } as Parameters<typeof create>[1]);
}

function sendContractEvent(evt: Event, seq: bigint = 1n) {
  act(() => { capturedContractOptions?.onEvent(evt, seq); });
}

function sessionStatusEvent(sessionId: string, status: SessionStatus): Event {
  return abiEvent({ type: EventType.SESSION_STATUS, sessionId, status });
}

function messageStartUser(messageId: string): Event {
  return abiEvent({
    type: EventType.MESSAGE_START,
    messageId,
    message: create(MessageSchema, { id: messageId, sessionId: "ses_1", type: MessageType.USER }),
  });
}

function textPartEnd(partId: string, text: string, messageId?: string): Event {
  return abiEvent({
    type: EventType.PART_END,
    partId,
    messageId,
    part: create(PartSchema, { id: partId, type: PartType.TEXT, payload: { case: "text", value: text } }),
  });
}

function textDelta(partId: string, delta: string, messageId?: string): Event {
  return abiEvent({ type: EventType.PART_DELTA, partId, messageId, delta });
}

/** All agent-side bubble text currently rendered. */
function agentBubbleText() {
  return Array.from(document.querySelectorAll(".justify-start")).map((el) => el.textContent ?? "").join("\n");
}

async function renderReady(qc = makeQueryClient(), path = "/chat/ws-1/ses_1") {
  const utils = renderChat(qc, path);
  await waitFor(() => {
    expect(capturedPlatformHandler).not.toBeNull();
    expect(capturedContractOptions).not.toBeNull();
    expect(document.querySelector("textarea")).not.toBeDisabled();
  });
  return utils;
}

// --- Tests ---

describe("ChatPage message queue (backend-backed)", () => {
  beforeEach(() => {
    capturedPlatformHandler = null;
    capturedContractOptions = null;
    contractState = { seq: 0n, sessions: new Map() };
    mockBusyState.reset();
    vi.clearAllMocks();
    // Re-establish implementations: clearAllMocks keeps them, but tests
    // that override getQueue/queueMessage with mockImplementation must not
    // leak into later tests.
    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({ messages: [] });
    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValue({ messageID: "msg_q_test" });
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockResolvedValue({ messages: [], nextCursor: undefined });
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active", sessions: [{ id: "ses_1", status: "idle" }] });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } });
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([]);
  });

  it("sends immediately when not busy", async () => {
    const user = userEvent.setup();
    await renderReady();
    await user.type(document.querySelector("textarea")!, "hello");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    await waitFor(() => {
      expect(messagesApi.sendAsync).toHaveBeenCalledWith("ws-1", "ses_1", {
        parts: [{ type: "text", text: "hello" }],
        clientMessageID: expect.any(String),
      });
    });
  });

  it("textarea stays enabled while the session is busy", async () => {
    await renderReady();
    setBusy(true);
    expect(document.querySelector("textarea")).not.toBeDisabled();
  });

  it("holds message in queue when busy — calls queueMessage not sendAsync", async () => {
    const user = userEvent.setup();
    await renderReady();
    setBusy(true);
    await user.type(document.querySelector("textarea")!, "queued msg");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    await waitFor(() => {
      expect(messagesApi.queueMessage).toHaveBeenCalledWith("ws-1", "ses_1", "queued msg", [] as string[]);
    });
    expect(screen.getByText("queued msg")).toBeInTheDocument();
    expect(screen.getByText("1 message queued")).toBeInTheDocument();
    expect(messagesApi.sendAsync).not.toHaveBeenCalled();
  });

  it("holds message in queue when queue is non-empty, even after the session goes idle", async () => {
    // Regression: without checking queue.queuedMessages for pending
    // entries, a direct send races ahead of the draining queue when the
    // session transitions busy→idle (FIFO violation on reload).
    const user = userEvent.setup();
    // Stateful getQueue: returns empty until the user enqueues A, then
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
    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockImplementation(() => {
      userEnqueuedA = true;
      return Promise.resolve({ messageID: "msg_q_test" });
    });

    await renderReady();

    // 1. Session busy → enqueue message A
    setBusy(true);
    await user.type(document.querySelector("textarea")!, "message A");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    await waitFor(() => expect(screen.getByText("1 message queued")).toBeInTheDocument());

    // 2. Session goes idle (contract SESSION_STATUS) — the server-side
    //    drain starts but Redis still holds A; the refresh triggered by
    //    reconcileOnIdle keeps the pill visible.
    setBusy(false);
    sendContractEvent(sessionStatusEvent("ses_1", SessionStatus.IDLE));

    // 3. User sends message B during the drain window → queue path.
    await user.type(document.querySelector("textarea")!, "message B");
    await user.click(screen.getByRole("button", { name: "Send message" }));

    await waitFor(() => {
      expect(messagesApi.queueMessage).toHaveBeenCalledWith("ws-1", "ses_1", "message B", [] as string[]);
    });
    expect(messagesApi.sendAsync).not.toHaveBeenCalled();
  });

  it("a queued message's pill clears via queue.update sent (removeById + refresh)", async () => {
    // The sent announcement carries the messageID: the targeted removal is
    // instant and the follow-up refresh is authoritative catch-up (the GET
    // can transiently race the server-side LRem).
    const user = userEvent.setup();
    let enqueued = false;
    let sentAnnounced = false;
    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockImplementation(() => {
      return Promise.resolve({
        messages: enqueued && !sentAnnounced ? [{
          id: "msg_q_test",
          text: "queued msg",
          session_id: "ses_1",
          status: "pending",
        }] : [],
      });
    });
    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockImplementation(() => {
      enqueued = true;
      return Promise.resolve({ messageID: "msg_q_test" });
    });

    await renderReady();
    setBusy(true);
    await user.type(document.querySelector("textarea")!, "queued msg");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    await waitFor(() => expect(screen.getByText("1 message queued")).toBeInTheDocument());

    sentAnnounced = true;
    sendPlatform({ type: "queue.update", session_id: "ses_1", data: { event: "sent", messageID: "msg_q_test" } });

    await waitFor(() => {
      expect(screen.queryByText(/queued/)).not.toBeInTheDocument();
    });
    // ... and stays cleared once the refresh lands.
    await act(async () => { await new Promise((r) => setTimeout(r, 30)); });
    expect(screen.queryByText(/queued/)).not.toBeInTheDocument();
  });

  it("queue pill shows error on queue.update error event", async () => {
    const user = userEvent.setup();
    await renderReady();
    setBusy(true);
    await user.type(document.querySelector("textarea")!, "will fail");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    await waitFor(() => expect(screen.getByText("1 message queued")).toBeInTheDocument());

    sendPlatform({ type: "queue.update", session_id: "ses_1", data: { event: "error", messageID: "msg_q_test", error: "send failed" } });

    await waitFor(() => {
      expect(screen.getByLabelText("Retry")).toBeInTheDocument();
      expect(screen.getByLabelText("Dismiss")).toBeInTheDocument();
    });
  });

  it("stop button is shown while the session is busy", async () => {
    await renderReady();
    setBusy(true);
    await waitFor(() => expect(screen.getByLabelText("Stop generating")).toBeInTheDocument());
  });

  it("abort click calls abortSession", async () => {
    const user = userEvent.setup();
    await renderReady();
    setBusy(true);
    await user.type(document.querySelector("textarea")!, "first");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    await user.type(document.querySelector("textarea")!, "second");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    await waitFor(() => expect(screen.getByText("2 messages queued")).toBeInTheDocument());

    await user.click(screen.getByLabelText("Stop generating"));
    expect(workspacesApi.abortSession).toHaveBeenCalledWith("ws-1", "ses_1");
  });

  it("abort deletes queued messages from Redis via clearAll", async () => {
    const user = userEvent.setup();
    await renderReady();
    setBusy(true);
    await user.type(document.querySelector("textarea")!, "queued msg");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    await waitFor(() => expect(screen.getByText("1 message queued")).toBeInTheDocument());

    await user.click(screen.getByLabelText("Stop generating"));

    await waitFor(() => {
      expect(messagesApi.deleteQueueMessage).toHaveBeenCalledWith("ws-1", "ses_1", "msg_q_test");
    });
  });

  it("dismiss removes the error pill and deletes server-side", async () => {
    const user = userEvent.setup();
    await renderReady();
    setBusy(true);
    await user.type(document.querySelector("textarea")!, "msg");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    await waitFor(() => expect(screen.getByText("1 message queued")).toBeInTheDocument());

    sendPlatform({ type: "queue.update", session_id: "ses_1", data: { event: "error", messageID: "msg_q_test", error: "fail" } });

    await waitFor(() => expect(screen.getByLabelText("Dismiss")).toBeInTheDocument());
    await user.click(screen.getByLabelText("Dismiss"));

    await waitFor(() => {
      expect(screen.queryByLabelText("Dismiss")).not.toBeInTheDocument();
    });
    expect(messagesApi.deleteQueueMessage).toHaveBeenCalledWith("ws-1", "ses_1", "msg_q_test");
  });

  it("queue.update dismissed event removes the error pill via removeById", async () => {
    const user = userEvent.setup();
    await renderReady();
    setBusy(true);
    await user.type(document.querySelector("textarea")!, "will error");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    await waitFor(() => expect(screen.getByText("1 message queued")).toBeInTheDocument());

    sendPlatform({ type: "queue.update", session_id: "ses_1", data: { event: "error", messageID: "msg_q_test", error: "failed" } });
    await waitFor(() => expect(screen.getByLabelText("Dismiss")).toBeInTheDocument());

    sendPlatform({ type: "queue.update", session_id: "ses_1", data: { event: "dismissed", messageID: "msg_q_test" } });

    await waitFor(() => {
      expect(screen.queryByLabelText("Dismiss")).not.toBeInTheDocument();
    });
  });
});

// D6 (#998): the hung-session alert banner. workspace.alert/session_hung
// (platform stream) renders the amber banner; a contract SESSION_STATUS
// idle for that session auto-clears it (the hang resolved); dismissal works.
describe("D6 hung-session alert banner (#998)", () => {
  beforeEach(() => {
    capturedPlatformHandler = null;
    capturedContractOptions = null;
    contractState = { seq: 0n, sessions: new Map() };
    mockBusyState.reset();
    vi.clearAllMocks();
    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({ messages: [] });
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active", sessions: [{ id: "ses_1", status: "idle" }] });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } });
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockResolvedValue({ messages: [], nextCursor: undefined });
  });

  it("renders the banner on workspace.alert and auto-clears on contract idle", async () => {
    await renderReady();

    sendPlatform({
      type: "workspace.alert",
      workspace_id: "ws-1",
      session_id: "ses_1",
      status: "session_hung",
      data: { alert: "session_hung", oldest_busy_seconds: 960, policy: "notify_only", guidance: "g" },
    });

    const banner = await screen.findByText(/busy for 16 min without/i);
    expect(banner).toBeInTheDocument();

    // The hang resolves: contract SESSION_STATUS idle for the same session.
    sendContractEvent(sessionStatusEvent("ses_1", SessionStatus.IDLE));
    await waitFor(() => {
      expect(screen.queryByText(/busy for 16 min without/i)).not.toBeInTheDocument();
    });
  });

  it("banner is dismissable", async () => {
    await renderReady();

    sendPlatform({
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
    capturedPlatformHandler = null;
    capturedContractOptions = null;
    contractState = { seq: 0n, sessions: new Map() };
    mockBusyState.reset();
    vi.clearAllMocks();
    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({ messages: [] });
    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValue({ messageID: "msg_q_test" });
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockResolvedValue({ messages: [], nextCursor: undefined });
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active", sessions: [{ id: "ses_1", status: "idle" }] });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } });
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([]);
  });

  it("parked ERROR entries do not force the queue path — direct send still used", async () => {
    // The suspend/resume casualty: entries parked as "delivery
    // unverifiable" stay in the queue forever until Retry/Dismiss.
    // Counting them as "queue busy" permanently reroutes every new send
    // through enqueue (which skips the direct-send flow).
    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({
      messages: [
        { id: "q-err-1", text: "old stranded message", status: "error", lastError: "delivery unverifiable: agent unreachable", session_id: "ses_1" },
      ],
    });
    const user = userEvent.setup();
    await renderReady();
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

  it("a delivering entry is invisible — the pill clears when the worker picks it up", async () => {
    // The outbox delivery send is synchronous turn-to-completion, so an
    // entry stays staged as delivering for the WHOLE multi-minute turn.
    // The pill must not render "queued" for that window (TUI parity: once
    // the agent owns the message it is in the conversation).
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
    await renderReady();
    setBusy(true);
    await user.type(document.querySelector("textarea")!, "long turn message");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    await waitFor(() => expect(screen.getByText("1 message queued")).toBeInTheDocument());

    // The worker stages the entry out (POST in flight) and the platform
    // stream announces it; the refresh that follows must drop the pill
    // even though GET /queue still REPORTS the delivering entry.
    sendPlatform({ type: "queue.update", session_id: "ses_1", data: { event: "delivering", messageID: "msg_q_test" } });

    await waitFor(() => {
      expect(screen.queryByText("1 message queued")).not.toBeInTheDocument();
    });
    expect(screen.queryByText("Sending…")).not.toBeInTheDocument();
  });

  it("queued send: user echo dropped by the MESSAGE_START gate; assistant deltas stream incrementally", async () => {
    // Regression for the #1110/#1112 live-streaming gap, driven through
    // ABI contract events while a message sits in the queue: the user
    // echo of the queued text must not render agent-side (MESSAGE_START
    // USER primes the gate), deltas render incrementally (not only at
    // block end), the final snapshot replaces without duplicating, and
    // the pill clears via queue.update (the queued-echo strip is gone).
    const user = userEvent.setup();
    await renderReady();
    setBusy(true);
    await user.type(document.querySelector("textarea")!, "queued hello");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    await waitFor(() => expect(messagesApi.queueMessage).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByText("1 message queued")).toBeInTheDocument());

    // The outbox delivers; the harness echoes the user's turn first...
    sendContractEvent(messageStartUser("msg_u"));
    sendContractEvent(textPartEnd("up_1", "queued hello", "msg_u"));
    // ...then the assistant streams: priming snapshot, deltas, final.
    sendContractEvent(textPartEnd("text-0", "", "msg_a"));
    sendContractEvent(textDelta("text-0", "Hello ", "msg_a"));
    await waitFor(() => expect(agentBubbleText()).toContain("Hello"));

    sendContractEvent(textDelta("text-0", "world", "msg_a"));
    await waitFor(() => expect(agentBubbleText()).toContain("Hello world"));

    // The final snapshot replaces the buffer without duplicating.
    sendContractEvent(textPartEnd("text-0", "Hello world", "msg_a"));
    await waitFor(() => expect(agentBubbleText().match(/Hello world/g)?.length).toBe(1));

    // The user echo never rendered agent-side.
    expect(agentBubbleText()).not.toContain("queued hello");

    // The pill clears via the platform queue.update announcement.
    sendPlatform({ type: "queue.update", session_id: "ses_1", data: { event: "sent", messageID: "msg_q_test" } });
    await waitFor(() => expect(screen.queryByText("1 message queued")).not.toBeInTheDocument());
  });
});

