/**
 * Regression tests for duplicate tool bubbles after an SSE reconnect
 * mid-turn (production symptom: chat.safespaces.dev session
 * ses_fe5364736ffe4E1HF3J3fUt2sf — one running bash call rendered as two
 * concurrent spinner bubbles with divergent elapsed clocks, "3m 22s" and
 * "47s", for a single in-flight call).
 *
 * Root cause chain (verified against live opencode SSE + history):
 *
 * 1. transformHistory (api/messages.ts) drops the contract part `id` and
 *    `tool.callId`, so ChatPage's `historyPartIds` set — the US-15.4
 *    reconnect boundary gate — is ALWAYS empty. The gate never drops
 *    events for parts already rendered from history.
 * 2. handleSSEReconnect → reconcileOnIdle runs while the session is still
 *    busy: the refetched history now INCLUDES the in-flight messages
 *    (opencode persists running parts), they render from `messages`, and
 *    `sseStreamParts` is cleared — but resumed `message.part.updated`
 *    events for those same parts re-append to `sseStreamParts` because the
 *    gate is a no-op (and reconcile disarmed `isReconnectMode`). The same
 *    tool renders twice: once from history, once from the stream.
 * 3. opencode rewrites `state.time.start` on every part snapshot (observed
 *    live: same part id, start moved 75.7s between two updates), so the
 *    live elapsed badge — which trusts the latest value — resets on every
 *    update instead of anchoring to the first-seen start.
 *
 * These tests pin the fix contract:
 * - after a mid-turn reconnect, a part already in refetched history must
 *   NOT re-enter sseStreamParts (no duplicate bubble);
 * - a part NOT in history (created after the refetch) must still stream;
 * - the elapsed anchor must be the FIRST-seen time.start per callID.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, waitFor, act, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";
import type { WorkspaceStreamEvent, SessionStatusEvent } from "../api/types";

// --- Mocks (mirroring ChatPage.optimistic-survival.test.tsx) ---

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

vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getStatus: vi.fn(),
    activate: vi.fn(),
    list: vi.fn().mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } }),
    renameWorkspace: vi.fn().mockResolvedValue(undefined),
    renameSession: vi.fn().mockResolvedValue(undefined),
    markSessionSeen: vi.fn().mockResolvedValue(undefined),
    getSessions: vi.fn().mockResolvedValue([]),
    abortSession: vi.fn(),
    requestInputSnapshot: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock("../api/messages", () => {
  const gh = vi.fn().mockResolvedValue([]);
  return {
    messagesApi: {
      getHistory: gh,
      getHistoryPage: vi.fn().mockImplementation(async () => {
        const msgs = await gh();
        return { messages: msgs, nextCursor: undefined };
      }),
      sendAsync: vi.fn().mockResolvedValue(undefined),
      queueMessage: vi.fn().mockResolvedValue({ messageID: "msg_q_mock" }),
      getQueue: vi.fn().mockResolvedValue({ messages: [] }),
      deleteQueueMessage: vi.fn().mockResolvedValue(undefined),
    },
  };
});

vi.mock("../api/sessions", () => ({ sessionsApi: { create: vi.fn() } }));

let capturedSSEHandler: ((data: unknown) => void) | null = null;
let capturedOnReconnect: (() => void) | null = null;
vi.mock("../hooks/useEventStream", () => ({
  useEventStream: vi.fn((_workspaceId: string | undefined, handler: (data: unknown) => void, options?: { onReconnect?: () => void }) => {
    capturedSSEHandler = handler;
    capturedOnReconnect = options?.onReconnect ?? null;
  }),
}));

// Mock ChatView to expose BOTH rendered sources — the history-driven
// `messages` array and the live `streamParts` array. The duplicate-bubble
// bug is precisely "the same tool present in both".
vi.mock("../components/chat/ChatView", () => ({
  ChatView: (props: Record<string, unknown>) => {
    return (
      <div
        data-testid="chat-view"
        data-streaming={String(props.streaming ?? false)}
        data-messages={JSON.stringify(props.messages ?? [])}
        data-stream-parts={JSON.stringify(props.streamParts ?? [])}
      >
        <textarea
          disabled={props.disabled as boolean}
          onChange={() => {}}
          onKeyDown={() => {}}
        />
      </div>
    );
  },
}));

import { messagesApi } from "../api/messages";

// --- Helpers ---

interface StreamPartLike {
  type: string;
  text?: string;
  toolState?: string;
  toolStartedAt?: string;
  toolCallID?: string;
  toolOutput?: string;
  messageID?: string;
}

function makeQueryClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: 0 } },
  });
}

function renderChat(qc: QueryClient, path: string) {
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[path]}>
        <TooltipProvider delayDuration={0}>
          <Routes>
            <Route path="/chat/:workspaceId/:sessionId" element={<ChatPage />} />
          </Routes>
        </TooltipProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function sendSSEEvent(event: WorkspaceStreamEvent) {
  if (event.type === "session.status") {
    mockBusyState.set(event.status === "busy");
  }
  act(() => { capturedSSEHandler?.(event); });
}

function triggerReconnect() {
  act(() => { capturedOnReconnect?.(); });
}

function getRenderedMessages(): Array<{ id: string; role: string; parts: Array<{ type: string; id?: string; toolCallId?: string; text?: string }> }> {
  const el = screen.getByTestId("chat-view");
  return JSON.parse(el.getAttribute("data-messages") || "[]");
}

function getRenderedStreamParts(): StreamPartLike[] {
  const el = screen.getByTestId("chat-view");
  return JSON.parse(el.getAttribute("data-stream-parts") || "[]");
}

function makeSessionStatusEvent(sessionId: string, status: "idle" | "busy"): SessionStatusEvent {
  return { type: "session.status", session_id: sessionId, status };
}

/**
 * Live-shaped contract part.end event for a running tool (US-65.8: the
 * SSE bridge translates agent events server-side; the frontend consumes
 * SessionContractEvent.data = ContractEvent):
 * {type:"agent.event", event_type, data:{type:"part.end", sessionId,
 *  messageId, part:{type:"tool", id, tool:{name, callId, input, output,
 *  state:{status, startedAt}}}}}
 * startedAt is ISO — the bridge forwards the agent's snapshot time,
 * which the agent rewrites per update (observed live 2026-08-20).
 */
function makeToolPartUpdatedEvent(over: {
  partId: string;
  callID: string;
  messageID: string;
  status: "running" | "completed";
  output: string;
  timeStart: number;
}): WorkspaceStreamEvent {
  return {
    type: "session.event",
    data: {
      type: "part.end",
      sessionId: "ses_1",
      messageId: over.messageID,
      partId: over.partId,
      part: {
        type: "tool",
        id: over.partId,
        tool: {
          name: "bash",
          callId: over.callID,
          input: { command: "poll-loop" },
          output: over.output,
          state: {
            status: over.status,
            startedAt: new Date(over.timeStart).toISOString(),
          },
        },
      },
    },
  } as unknown as WorkspaceStreamEvent;
}

// Contract-shaped in-flight history message, as the adapter returns it:
// parts carry id; tool carries callId + state.startedAt.
function inFlightHistory(): Array<Record<string, unknown>> {
  return [
    { id: "msg-prior-user", type: "user", createdAt: "2026-08-20T18:50:00Z", parts: [{ type: "text", text: "run the poll" }] },
    {
      id: "msg-inflight",
      type: "assistant",
      createdAt: "2026-08-20T18:58:05Z",
      parts: [{
        type: "tool",
        id: "prt_running",
        tool: {
          name: "bash",
          callId: "call_running",
          input: { command: "poll-loop" },
          output: "18:58Z running=9",
          state: { status: "running", startedAt: "2026-08-20T18:58:05Z" },
        },
      }],
    },
  ];
}

const priorTurnOnly = [
  { id: "msg-prior-user", type: "user", createdAt: "2026-08-20T18:50:00Z", parts: [{ type: "text", text: "run the poll" }] },
];

// --- Tests ---

describe("SSE reconnect mid-turn must not duplicate in-flight parts", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockBusyState.reset();
    capturedSSEHandler = null;
    capturedOnReconnect = null;
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
  });

  it("BUG REPRO — part already in refetched history must not re-enter sseStreamParts after reconnect", async () => {
    const qc = makeQueryClient();
    qc.setQueryData(
      ["workspace-status", "ws-1"],
      { phase: "Active", sessions: [{ id: "ses_1", status: "idle" }] },
    );

    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue(priorTurnOnly);

    renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
    await waitFor(() => {
      expect(getRenderedMessages()).toHaveLength(1);
    });

    // Session goes busy; the live turn streams a running bash tool.
    sendSSEEvent(makeSessionStatusEvent("ses_1", "busy"));
    sendSSEEvent(makeToolPartUpdatedEvent({
      partId: "prt_running", callID: "call_running", messageID: "msg-inflight",
      status: "running", output: "18:58Z running=9", timeStart: Date.parse("2026-08-20T18:58:05Z"),
    }));

    await waitFor(() => {
      const parts = getRenderedStreamParts();
      expect(parts.filter((p) => p.toolCallID === "call_running")).toHaveLength(1);
    });

    // SSE drops + reconnects. The reconcile refetch now returns the
    // in-flight message (opencode persisted the running part).
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue(inFlightHistory());
    triggerReconnect();

    await waitFor(() => {
      expect((messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mock.calls.length).toBeGreaterThanOrEqual(2);
    });
    await waitFor(() => {
      // History now renders the in-flight tool (id preserved through the
      // contract transform).
      const msgs = getRenderedMessages();
      expect(msgs.some((m) => m.parts.some((p) => p.toolCallId === "call_running" || p.id === "prt_running"))).toBe(true);
    });

    // The stream resumes: opencode re-emits part.updated for the SAME
    // part (output grows; time.start rewritten to the snapshot time).
    sendSSEEvent(makeToolPartUpdatedEvent({
      partId: "prt_running", callID: "call_running", messageID: "msg-inflight",
      status: "running", output: "18:58Z running=9\n19:00Z running=6", timeStart: Date.parse("2026-08-20T19:00:00Z"),
    }));

    // ASSERT — exactly ONE rendering of the tool across both sources.
    // Production bug: it lives in messages (history) AND re-enters
    // sseStreamParts → two spinner bubbles for one bash run.
    await waitFor(() => {
      const streamDupes = getRenderedStreamParts().filter((p) => p.toolCallID === "call_running");
      expect(streamDupes).toHaveLength(0);
    });
  });

  it("elapsed badge anchors to the FIRST-seen time.start per callID (opencode rewrites it per snapshot)", async () => {
    const qc = makeQueryClient();
    qc.setQueryData(
      ["workspace-status", "ws-1"],
      { phase: "Active", sessions: [{ id: "ses_1", status: "idle" }] },
    );
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

    sendSSEEvent(makeSessionStatusEvent("ses_1", "busy"));

    const t1 = Date.parse("2026-08-20T18:58:05Z");
    const t2 = t1 + 75_744; // next snapshot rewrites time.start (observed live)
    sendSSEEvent(makeToolPartUpdatedEvent({
      partId: "prt_x", callID: "call_x", messageID: "msg_x",
      status: "running", output: "line1", timeStart: t1,
    }));
    sendSSEEvent(makeToolPartUpdatedEvent({
      partId: "prt_x", callID: "call_x", messageID: "msg_x",
      status: "running", output: "line1\nline2", timeStart: t2,
    }));

    await waitFor(() => {
      const parts = getRenderedStreamParts().filter((p) => p.toolCallID === "call_x");
      expect(parts).toHaveLength(1);
      expect(parts[0]!.toolStartedAt).toBe(new Date(t1).toISOString());
    });
  });

  it("CONTROL — a part NOT in history (created after the refetch) still streams after reconnect", async () => {
    const qc = makeQueryClient();
    qc.setQueryData(
      ["workspace-status", "ws-1"],
      { phase: "Active", sessions: [{ id: "ses_1", status: "idle" }] },
    );

    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue(priorTurnOnly);

    renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
    await waitFor(() => expect(getRenderedMessages()).toHaveLength(1));

    sendSSEEvent(makeSessionStatusEvent("ses_1", "busy"));
    sendSSEEvent(makeToolPartUpdatedEvent({
      partId: "prt_running", callID: "call_running", messageID: "msg-inflight",
      status: "running", output: "18:58Z running=9", timeStart: Date.parse("2026-08-20T18:58:05Z"),
    }));

    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue(inFlightHistory());
    triggerReconnect();

    await waitFor(() => {
      expect((messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mock.calls.length).toBeGreaterThanOrEqual(2);
    });

    // A NEW message + part arrives after the refetch — it is not in
    // history yet and MUST stream (guards against an over-aggressive gate).
    sendSSEEvent(makeToolPartUpdatedEvent({
      partId: "prt_new", callID: "call_new", messageID: "msg_new",
      status: "running", output: "new tool", timeStart: Date.parse("2026-08-20T19:05:00Z"),
    }));

    await waitFor(() => {
      const parts = getRenderedStreamParts().filter((p) => p.toolCallID === "call_new");
      expect(parts).toHaveLength(1);
    });
    // And the reconnect-known part still does not duplicate.
    await waitFor(() => {
      expect(getRenderedStreamParts().filter((p) => p.toolCallID === "call_running")).toHaveLength(0);
    });
  });
});
