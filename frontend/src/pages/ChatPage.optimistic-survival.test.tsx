/**
 * Regression tests for the "just-sent user message vanishes from chat" bug.
 *
 * Symptom (observed in production, chat.safespaces.dev session
 * ses_0ee4e1e45ffeKs0bu7oNQTnKLD): after the user sends a message, the
 * assistant receives it and starts responding, but the user's own bubble
 * never appears in the rendered chat list (it is missing from the DOM, not
 * merely below the scroll fold — the rendered `role="log"` element had no
 * trailing user bubble).
 *
 * Suspected root cause: `reconcileOnIdle` (ChatPage.tsx) clears
 * `localMessages` whenever its refetched history has any messages
 * (`if (msgs.length > 0) setLocalMessages([])`). This fires from two
 * triggers — `session.status=idle` SSE and `handleSSEReconnect` (issue 440).
 * If either fires after `doSendNow` appended the optimistic user message
 * but before opencode's GET /message returns that message back, the
 * optimistic bubble is wiped while history is still stale.
 *
 * These tests assert the user's own outgoing bubble must remain visible
 * across both triggers, even when the refetched history does not yet
 * include the just-sent message.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, waitFor, act, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";
import type { WorkspaceStreamEvent, SessionStatusEvent } from "../api/types";

// --- Mocks (mirroring ChatPage.reconnect.test.tsx for consistency) ---

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
    abortSession: vi.fn().mockResolvedValue(undefined),
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

// Capture SSE handler + onReconnect from useEventStream
let capturedSSEHandler: ((data: unknown) => void) | null = null;
let capturedOnReconnect: (() => void) | null = null;
vi.mock("../hooks/useEventStream", () => ({
  useEventStream: vi.fn((_workspaceId: string | undefined, handler: (data: unknown) => void, options?: { onReconnect?: () => void }) => {
    capturedSSEHandler = handler;
    capturedOnReconnect = options?.onReconnect ?? null;
  }),
}));

// Mock ChatView to expose the rendered `messages` array — this is the
// authoritative contract: whatever is in data-messages is what reaches
// the chat list.
vi.mock("../components/chat/ChatView", () => ({
  ChatView: (props: Record<string, unknown>) => {
    return (
      <div
        data-testid="chat-view"
        data-streaming={String(props.streaming ?? false)}
        data-messages={JSON.stringify(props.messages ?? [])}
      >
        <textarea
          disabled={props.disabled as boolean}
          onChange={() => {}}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              (props.onSend as (t: string) => void)((e.target as HTMLTextAreaElement).value);
            }
          }}
        />
      </div>
    );
  },
}));

import { messagesApi } from "../api/messages";

// --- Helpers ---

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

function getRenderedMessages(): Array<{ id: string; role: string; parts: Array<{ type: string; text?: string }> }> {
  const el = screen.getByTestId("chat-view");
  return JSON.parse(el.getAttribute("data-messages") || "[]");
}

function makeSessionStatusEvent(sessionId: string, status: "idle" | "busy"): SessionStatusEvent {
  return { type: "session.status", session_id: sessionId, status };
}

// --- Tests ---

describe("Optimistic user message survival across reconcile", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockBusyState.reset();
    capturedSSEHandler = null;
    capturedOnReconnect = null;
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
  });

  it("BUG REPRO — SSE reconnect mid-send wipes the just-sent user bubble before history catches up", async () => {
    // Scenario: a prior turn exists in history (msg-prior-user/asst). The user
    // sends a NEW message ("hello"). Network blips → handleSSEReconnect fires
    // → reconcileOnIdle refetches. Opencode hasn't persisted the new user
    // message yet, so history returns the prior turn only. reconcileOnIdle
    // sees msgs.length > 0 and clears localMessages. The just-sent "hello"
    // bubble vanishes from the rendered chat list — exactly the production
    // symptom.
    const user = userEvent.setup();
    const qc = makeQueryClient();
    qc.setQueryData(
      ["workspace-status", "ws-1"],
      { phase: "Active", sessions: [{ id: "ses_1", status: "idle" }] },
    );

    // First call: initial load returns one prior turn (so msgs.length>0 will
    // hold when reconcile re-fetches). Subsequent calls (the reconcile
    // refetch) return the SAME prior turn (the new send hasn't been
    // persisted by opencode yet).
    const priorTurn = [
      { id: "msg-prior-user", role: "user", parts: [{ type: "text", text: "prior" }] },
      { id: "msg-prior-asst", role: "assistant", parts: [{ type: "text", text: "prior reply" }] },
    ];
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue(priorTurn);

    renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
    await waitFor(() => expect(capturedOnReconnect).not.toBeNull());
    await waitFor(() => {
      const msgs = getRenderedMessages();
      expect(msgs).toHaveLength(2);
    });

    // User sends a new message
    const textarea = screen.getByRole("textbox") as HTMLTextAreaElement;
    await user.click(textarea);
    await user.type(textarea, "hello");
    await user.keyboard("{Enter}");

    await waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());

    // Sanity check: optimistic bubble was appended (prior turn + new user msg).
    await waitFor(() => {
      const msgs = getRenderedMessages();
      const userMsgs = msgs.filter((m) => m.role === "user");
      expect(userMsgs.some((m) => m.parts.some((p) => p.text === "hello"))).toBe(true);
    });

    // Now SSE reconnects (issue 440 path: in-place opencode restart, brief
    // network drop, etc.). Server-side history hasn't been updated yet to
    // include the just-sent "hello".
    triggerReconnect();

    // Wait for the reconcile refetch to land. After this the bug currently
    // wipes localMessages.
    await waitFor(() => {
      // getHistoryPage is called by reconcileOnIdle's queryClient.refetchQueries
      expect((messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mock.calls.length).toBeGreaterThanOrEqual(2);
    });

    // ASSERTION — the just-sent user bubble MUST still be visible in the
    // chat list. Production bug: it vanishes here because reconcileOnIdle
    // clears localMessages even though the new message isn't in history yet.
    await waitFor(() => {
      const msgs = getRenderedMessages();
      const userTexts = msgs
        .filter((m) => m.role === "user")
        .flatMap((m) => m.parts.map((p) => p.text));
      expect(userTexts).toContain("hello");
    });
  });

  it("BUG REPRO — premature session.status=idle SSE wipes the just-sent user bubble before history catches up", async () => {
    // Variant: instead of SSE reconnect, a stale or premature `session.status=idle`
    // event arrives (e.g. opencode flushed a sub-task or interrupt). reconcileOnIdle
    // still fires, with the same wipe effect.
    const user = userEvent.setup();
    const qc = makeQueryClient();
    qc.setQueryData(
      ["workspace-status", "ws-1"],
      { phase: "Active", sessions: [{ id: "ses_1", status: "idle" }] },
    );

    const priorTurn = [
      { id: "msg-prior-user", role: "user", parts: [{ type: "text", text: "prior" }] },
      { id: "msg-prior-asst", role: "assistant", parts: [{ type: "text", text: "prior reply" }] },
    ];
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue(priorTurn);

    renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
    await waitFor(() => {
      const msgs = getRenderedMessages();
      expect(msgs).toHaveLength(2);
    });

    const textarea = screen.getByRole("textbox") as HTMLTextAreaElement;
    await user.click(textarea);
    await user.type(textarea, "world");
    await user.keyboard("{Enter}");
    await waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());

    await waitFor(() => {
      const msgs = getRenderedMessages();
      const userMsgs = msgs.filter((m) => m.role === "user");
      expect(userMsgs.some((m) => m.parts.some((p) => p.text === "world"))).toBe(true);
    });

    // A spurious / premature idle fires. History still doesn't have "world".
    sendSSEEvent(makeSessionStatusEvent("ses_1", "idle"));

    // Reconcile refetches.
    await waitFor(() => {
      expect((messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mock.calls.length).toBeGreaterThanOrEqual(2);
    });

    // The bubble MUST still be visible.
    await waitFor(() => {
      const msgs = getRenderedMessages();
      const userTexts = msgs
        .filter((m) => m.role === "user")
        .flatMap((m) => m.parts.map((p) => p.text));
      expect(userTexts).toContain("world");
    });
  });

  it("CONTROL — when history refetch DOES contain the just-sent message, the optimistic bubble may be cleared (no duplicate)", async () => {
    // This is the happy path: opencode has persisted the user message by
    // the time reconcileOnIdle runs. The refetched history includes the
    // message, so it's safe to clear localMessages. Rendered list still
    // shows the user message — sourced from history rather than
    // localMessages — and there is NO duplicate.
    //
    // This test guards against an over-aggressive fix that drops the
    // existing "clear-when-history-is-authoritative" behavior (the fix
    // documented in ChatPage.sse.test.tsx:226).
    const user = userEvent.setup();
    const qc = makeQueryClient();
    qc.setQueryData(
      ["workspace-status", "ws-1"],
      { phase: "Active", sessions: [{ id: "ses_1", status: "idle" }] },
    );

    let historyCallCount = 0;
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockImplementation(() => {
      historyCallCount++;
      if (historyCallCount === 1) return Promise.resolve([]);
      // Subsequent fetches (reconcileOnIdle's refetch) DO include the
      // just-sent message.
      return Promise.resolve([
        { id: "msg-user-real", role: "user", parts: [{ type: "text", text: "ping" }] },
        { id: "msg-asst-real", role: "assistant", parts: [{ type: "text", text: "pong" }] },
      ]);
    });

    renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

    const textarea = screen.getByRole("textbox") as HTMLTextAreaElement;
    await user.click(textarea);
    await user.type(textarea, "ping");
    await user.keyboard("{Enter}");
    await waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());

    sendSSEEvent(makeSessionStatusEvent("ses_1", "idle"));

    await waitFor(() => {
      const msgs = getRenderedMessages();
      // EXACTLY 2 (no duplicate from localMessages + history) and the user
      // bubble for "ping" is still present.
      expect(msgs).toHaveLength(2);
      const userTexts = msgs
        .filter((m) => m.role === "user")
        .flatMap((m) => m.parts.map((p) => p.text));
      expect(userTexts).toContain("ping");
    });
  });
});
