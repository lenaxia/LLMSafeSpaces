/**
 * Regression tests for the "just-sent user message vanishes from chat"
 * bug (#447) — post US-69.10 hard cutover framing.
 *
 * Symptom (observed in production, chat.safespaces.dev session
 * ses_0ee4e1e45ffeKs0bu7oNQTnKLD): after the user sends a message, the
 * assistant receives it and starts responding, but the user's own bubble
 * never appears in the rendered chat list.
 *
 * Root cause: `reconcileOnIdle` (ChatPage.tsx) cleared ALL optimistic
 * localMessages whenever its refetched history had any messages. It
 * fires from two triggers — a contract SESSION_STATUS IDLE event and a
 * contract-stream reconnect (issue 440). If either fires after
 * doSendNow appended the optimistic user message but before opencode's
 * history returns that message back, the optimistic bubble was wiped
 * while history was still stale.
 *
 * The fix: reconcileOnIdle only drops optimistic messages whose
 * messageIdentityKey (role + manifest-stripped text) is present in the
 * refetched history — the server has demonstrably caught up with those.
 *
 * These tests assert the user's own outgoing bubble remains visible
 * across both triggers when the refetched history does not yet include
 * the just-sent message, and that it is safely de-duplicated when it
 * does. Session-idle is driven by contract SESSION_STATUS events through
 * the mocked useContractStream (the old SSE status dialect is deleted).
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, waitFor, act, screen, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";
import { create } from "@bufbuild/protobuf";
import {
  EventSchema,
  EventType,
  SessionStatus,
} from "../abi/llmsafespaces/abi/v1/contract_pb";
import type { Event } from "../abi/llmsafespaces/abi/v1/contract_pb";
import type { SessionSnapshot } from "../abi/llmsafespaces/abi/v1/abi_pb";

// --- Mocks (mirroring ChatPage.sse.test.tsx, the reference pattern) ---

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

// Platform stream — mocked inert; it carries platform events only after
// the cutover.
vi.mock("../hooks/useEventStream", () => ({
  useEventStream: vi.fn(),
}));

// Contract stream — captured options + controllable fold state.
let capturedContractOptions: {
  onEvent: (event: Event, seq: bigint) => void;
  onSnapshot: (state: { seq: bigint; sessions: ReadonlyMap<string, SessionSnapshot> }) => void;
  onReconnect: () => void;
} | null = null;
let contractState: { seq: bigint; sessions: Map<string, SessionSnapshot> };
vi.mock("../hooks/useContractStream", () => ({
  useContractStream: vi.fn((_workspaceId: string | undefined, options: Record<string, unknown>) => {
    capturedContractOptions = options as typeof capturedContractOptions;
    return contractState;
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

import { workspacesApi } from "../api/workspaces";
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

function sendContractEvent(evt: Event, seq: bigint = 1n) {
  act(() => { capturedContractOptions?.onEvent(evt, seq); });
}

function triggerContractReconnect() {
  act(() => { capturedContractOptions?.onReconnect(); });
}

/** Contract SESSION_STATUS event for the viewed session. */
function sessionStatusEvent(sessionId: string, status: SessionStatus): Event {
  return create(EventSchema, {
    sessionId,
    type: EventType.SESSION_STATUS,
    status,
  } as Parameters<typeof create>[1]);
}

function getRenderedMessages(): Array<{ id: string; role: string; parts: Array<{ type: string; text?: string }> }> {
  const el = screen.getByTestId("chat-view");
  return JSON.parse(el.getAttribute("data-messages") || "[]");
}

function userTexts(): string[] {
  return getRenderedMessages()
    .filter((m) => m.role === "user")
    .flatMap((m) => m.parts.map((p) => p.text))
    .filter((t): t is string => typeof t === "string");
}

const priorTurn = [
  { id: "msg-prior-user", role: "user", parts: [{ type: "text", text: "prior" }] },
  { id: "msg-prior-asst", role: "assistant", parts: [{ type: "text", text: "prior reply" }] },
];

async function sendText(text: string) {
  // Wait for ChatView to be mounted (the initial history load flips the
  // spinner → ChatView branch; a textarea grabbed before that is replaced
  // by a fresh uncontrolled node with an empty value).
  await screen.findByTestId("chat-view");
  const textarea = screen.getByRole("textbox");
  await waitFor(() => expect(textarea).not.toBeDisabled());
  await userEvent.type(textarea, text);
  // Re-query: async query resolution can re-render ChatView between the
  // type and the keypress, detaching the earlier node.
  fireEvent.keyDown(screen.getByRole("textbox"), { key: "Enter" });
  await waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());
}

// --- Tests ---

describe("Optimistic user message survival across reconcile", () => {
  beforeEach(() => {
    capturedContractOptions = null;
    contractState = { seq: 0n, sessions: new Map() };
    vi.clearAllMocks();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
  });

  it("BUG REPRO — contract-stream reconnect mid-send must not wipe the just-sent user bubble before history catches up", async () => {
    const qc = makeQueryClient();

    // Initial load and the reconcile refetch both return the prior turn
    // only — opencode has not persisted the just-sent message yet.
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue(priorTurn);

    renderChat(qc, "/chat/ws-1/sess-1");
    await waitFor(() => expect(capturedContractOptions).not.toBeNull());
    await waitFor(() => {
      expect(getRenderedMessages()).toHaveLength(2);
    });

    // User sends a new message; the optimistic bubble appends.
    await sendText("hello");
    await waitFor(() => {
      expect(userTexts()).toContain("hello");
    });

    // The contract stream reconnects (issue 440 path: in-place opencode
    // restart, brief network drop). Server-side history still does not
    // include the just-sent "hello".
    triggerContractReconnect();

    // Wait for the reconcile refetch to land.
    await waitFor(() => {
      expect((messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mock.calls.length).toBeGreaterThanOrEqual(2);
    });

    // ASSERTION — the just-sent user bubble MUST still be visible.
    await waitFor(() => {
      expect(userTexts()).toContain("hello");
    });
  });

  it("BUG REPRO — a premature contract SESSION_STATUS IDLE event must not wipe the just-sent user bubble", async () => {
    // Variant: instead of a reconnect, a stale/premature IDLE contract
    // event arrives (e.g. opencode flushed a sub-task or interrupt).
    // reconcileOnIdle still fires, with the same wipe risk.
    const qc = makeQueryClient();
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue(priorTurn);

    renderChat(qc, "/chat/ws-1/sess-1");
    await waitFor(() => expect(capturedContractOptions).not.toBeNull());
    await waitFor(() => {
      expect(getRenderedMessages()).toHaveLength(2);
    });

    await sendText("world");
    await waitFor(() => {
      expect(userTexts()).toContain("world");
    });

    // A spurious/premature idle fires. History still doesn't have "world".
    sendContractEvent(sessionStatusEvent("sess-1", SessionStatus.IDLE));

    await waitFor(() => {
      expect((messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mock.calls.length).toBeGreaterThanOrEqual(2);
    });

    // The bubble MUST still be visible.
    await waitFor(() => {
      expect(userTexts()).toContain("world");
    });
  });

  it("CONTROL — when the history refetch DOES contain the just-sent message, the optimistic bubble is dropped (no duplicate)", async () => {
    // Happy path: opencode persisted the user message by the time
    // reconcileOnIdle runs. The refetched history includes the message,
    // so the identity-key filter drops the optimistic copy. The rendered
    // list still shows the user message — sourced from history — and
    // there is NO duplicate.
    const qc = makeQueryClient();

    let historyCallCount = 0;
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockImplementation(() => {
      historyCallCount++;
      if (historyCallCount === 1) return Promise.resolve([]);
      // Subsequent fetches (the reconcile refetch) DO include the
      // just-sent message.
      return Promise.resolve([
        { id: "msg-user-real", role: "user", parts: [{ type: "text", text: "ping" }] },
        { id: "msg-asst-real", role: "assistant", parts: [{ type: "text", text: "pong" }] },
      ]);
    });

    renderChat(qc, "/chat/ws-1/sess-1");
    await waitFor(() => expect(capturedContractOptions).not.toBeNull());

    await sendText("ping");

    sendContractEvent(sessionStatusEvent("sess-1", SessionStatus.IDLE));

    await waitFor(() => {
      const msgs = getRenderedMessages();
      // EXACTLY 2 (no duplicate from localMessages + history) and the
      // user bubble for "ping" is still present.
      expect(msgs).toHaveLength(2);
      expect(userTexts().filter((t) => t === "ping")).toHaveLength(1);
    });
  });
});
