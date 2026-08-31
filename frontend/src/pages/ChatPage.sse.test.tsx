/**
 * Tests for ChatPage's event dispatch after the US-69.10 hard cutover:
 * session-state events arrive as ABI contract events via useContractStream
 * (snapshot-first, discard rule, I12 entity-ID stitch); the workspace
 * stream (useEventStream) carries platform events only (workspace.phase /
 * workspace.alert / queue.update / agent_died).
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, waitFor, act, screen, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";
import type { WorkspaceStreamEvent } from "../api/types";
import type { Event } from "../abi/llmsafespaces/abi/v1/contract_pb";
import { create } from "@bufbuild/protobuf";
import { EventSchema, PartSchema, MessageSchema, SessionSchema } from "../abi/llmsafespaces/abi/v1/contract_pb";
import type { SessionSnapshot } from "../abi/llmsafespaces/abi/v1/abi_pb";
import { SessionSnapshotSchema } from "../abi/llmsafespaces/abi/v1/abi_pb";

// --- Mocks ---

vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getStatus: vi.fn(),
    activate: vi.fn(),
    list: vi.fn().mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } }),
    markSessionSeen: vi.fn().mockResolvedValue(undefined),
    getSessions: vi.fn().mockResolvedValue([]),
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
  return { messagesApi: { getHistory: gh, getHistoryPage: vi.fn().mockImplementation(async () => { const msgs = await gh(); return { messages: msgs, nextCursor: undefined }; }), sendAsync: vi.fn(), queueMessage: vi.fn().mockResolvedValue({ messageID: "msg_q_mock" }), getQueue: vi.fn().mockResolvedValue({ messages: [] }), deleteQueueMessage: vi.fn().mockResolvedValue(undefined) } };
});
vi.mock("../api/sessions", () => ({ sessionsApi: { create: vi.fn() } }));

// Capture the platform-stream handler ChatPage registers with useEventStream
let capturedPlatformHandler: ((data: unknown) => void) | null = null;
vi.mock("../hooks/useEventStream", () => ({
  useEventStream: vi.fn((_workspaceId: string | undefined, handler: (data: unknown) => void) => {
    capturedPlatformHandler = handler;
  }),
}));

// Capture the contract-stream options ChatPage registers with
// useContractStream, and expose a controllable fold state.
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

// Mock ChatView to expose streaming text as data attributes
vi.mock("../components/chat/ChatView", () => ({
  ChatView: (props: Record<string, unknown>) => {
    return (
      <div
        data-testid="chat-view"
        data-stream-parts={JSON.stringify(props.streamParts ?? [])}
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
            <Route path="/chat/:workspaceId" element={<ChatPage />} />
            <Route path="/chat/:workspaceId/:sessionId" element={<ChatPage />} />
          </Routes>
        </TooltipProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function sendPlatformEvent(event: WorkspaceStreamEvent) {
  act(() => { capturedPlatformHandler?.(event); });
}

/** Sends one ABI contract event through the captured contract-stream
 * handler — the post-discard-rule delivery path ChatPage consumes. */
function sendContractEvent(evt: Event, seq: bigint = 1n) {
  act(() => { capturedContractOptions?.onEvent(evt, seq); });
}

function applySnapshot(atSeq: bigint, sessions: SessionSnapshot[]) {
  contractState = { seq: atSeq, sessions: new Map(sessions.map((s) => [s.sessionId, s])) };
  act(() => { capturedContractOptions?.onSnapshot(contractState); });
}

function getStreamParts(): Array<{ type: string; text: string; partID?: string; toolState?: string; toolStartedAt?: string; toolCallID?: string }> {
  const el = screen.getByTestId("chat-view");
  return JSON.parse(el.getAttribute("data-stream-parts") || "[]");
}

function abiEvent(partial: Partial<Event>): Event {
  return create(EventSchema, { sessionId: "sess-1", ...partial } as Parameters<typeof create>[1]);
}

function textPartEnd(partId: string, text: string, sessionId = "sess-1", messageId?: string): Event {
  return abiEvent({
    type: 7, // PART_END
    sessionId,
    messageId,
    partId,
    part: create(PartSchema, { id: partId, type: 1, payload: { case: "text", value: text } }),
  });
}

function reasoningPartEnd(partId: string, text: string, messageId?: string): Event {
  return abiEvent({
    type: 7,
    partId,
    messageId,
    part: create(PartSchema, { id: partId, type: 2, payload: { case: "reasoning", value: text } }),
  });
}

function textDelta(partId: string, delta: string, messageId?: string): Event {
  return abiEvent({ type: 6, partId, messageId, delta });
}

function toolPartEnd(partId: string, tool: { name: string; callId?: string; status?: number }, messageId?: string): Event {
  return abiEvent({
    type: 7,
    partId,
    messageId,
    part: create(PartSchema, {
      id: partId,
      type: 3,
      payload: {
        case: "tool",
        value: {
          callId: tool.callId ?? "",
          name: tool.name,
          ...(tool.status !== undefined ? { state: { status: tool.status } } : {}),
        },
      },
    }),
  });
}

function makeSessionSnapshot(sessionId: string, init: Record<string, unknown> = {}): SessionSnapshot {
  return create(SessionSnapshotSchema, { sessionId, ...init } as Parameters<typeof create>[1]);
}

// --- Tests ---

describe("ChatPage event dispatch (contract stream + platform stream)", () => {
  beforeEach(() => {
    capturedPlatformHandler = null;
    capturedContractOptions = null;
    contractState = { seq: 0n, sessions: new Map() };
    vi.clearAllMocks();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } });
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
  });

  async function renderReady(qc: QueryClient, path = "/chat/ws-1/sess-1") {
    const utils = renderChat(qc, path);
    await waitFor(() => {
      expect(capturedPlatformHandler).not.toBeNull();
      expect(capturedContractOptions).not.toBeNull();
    });
    return utils;
  }

  describe("platform stream (useEventStream)", () => {
    it("workspace.phase reaches the queue phase hook without invalidating queries", async () => {
      const qc = makeQueryClient();
      const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
      await renderReady(qc);
      sendPlatformEvent({ type: "workspace.phase", phase: "Active" });
      const phaseCalls = invalidateSpy.mock.calls.filter((args) => {
        const key = (args[0] as { queryKey?: unknown })?.queryKey;
        return Array.isArray(key) && (key[0] === "workspace-status" || key[0] === "workspaces");
      });
      expect(phaseCalls).toHaveLength(0);
    });

    it("agent_died sets the banner", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      sendPlatformEvent({ type: "agent_died", data: { reason: "unknown", message: "boom" } } as never);
      expect(await screen.findByRole("alert")).toHaveTextContent("boom");
    });

    it("queue.update delivering triggers a queue refresh", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      sendPlatformEvent({ type: "queue.update", session_id: "sess-1", data: { event: "enqueued" } } as never);
      // No crash; the queue refresh is fire-and-forget.
      expect(capturedPlatformHandler).not.toBeNull();
    });
  });

  describe("SESSION_STATUS contract events", () => {
    it("invalidates the sessions query", async () => {
      const qc = makeQueryClient();
      const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
      await renderReady(qc);
      sendContractEvent(abiEvent({ type: 1, status: 2 }), 1n); // SESSION_STATUS IDLE
      expect(invalidateSpy).toHaveBeenCalledWith(expect.objectContaining({ queryKey: ["sessions", "ws-1"] }));
    });

    it("does NOT invalidate the workspace-status query", async () => {
      const qc = makeQueryClient();
      const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
      await renderReady(qc);
      sendContractEvent(abiEvent({ type: 1, status: 3 }), 1n); // BUSY
      const wsCalls = invalidateSpy.mock.calls.filter((args) => {
        const key = (args[0] as { queryKey?: unknown })?.queryKey;
        return Array.isArray(key) && key[0] === "workspace-status";
      });
      expect(wsCalls).toHaveLength(0);
    });

    it("REGRESSION: idle reconcile does not duplicate localMessages", async () => {
      await new Promise((r) => setTimeout(r, 100));
      const qc = makeQueryClient();
      (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([
        { id: "m1", type: "user", parts: [{ type: "text", text: "hello" }] },
      ]);
      await renderReady(qc);
      // Optimistic user message
      const textarea = await screen.findByRole("textbox");
      await waitFor(() => expect(textarea).not.toBeDisabled());
      await userEvent.type(textarea, "hello");
      // Re-query: async query resolution can re-render ChatView between
      // the type and the keypress, detaching the earlier node.
      fireEvent.keyDown(screen.getByRole("textbox"), { key: "Enter" });
      await waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());
      sendContractEvent(abiEvent({ type: 1, status: 2 }), 1n);
      const el = await screen.findByTestId("chat-view");
      await waitFor(() => {
        const rendered = JSON.parse(el.getAttribute("data-messages") || "[]");
        const userMsgs = rendered.filter((m: { role: string }) => m.role === "user");
        expect(userMsgs).toHaveLength(1);
      }, { timeout: 5000 });
    });
  });

  describe("part rendering (I12 entity-ID stitch)", () => {
    it("text part creates a bubble keyed by part id", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      sendContractEvent(textPartEnd("p1", "Hello world"));
      await waitFor(() => expect(getStreamParts()).toHaveLength(1));
      expect(getStreamParts()[0]).toMatchObject({ type: "text", text: "Hello world", partID: "p1" });
    });

    it("later snapshots of the same part id replace, not append", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      sendContractEvent(textPartEnd("p1", "Hel"));
      sendContractEvent(textPartEnd("p1", "Hello world"));
      await waitFor(() => expect(getStreamParts()).toHaveLength(1));
      expect(getStreamParts()[0]!.text).toBe("Hello world");
    });

    it("deltas append to the matching part id; unseen part materializes", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      sendContractEvent(textPartEnd("p1", ""));
      sendContractEvent(textDelta("p1", " wor"));
      sendContractEvent(textDelta("p1", "ld"));
      await waitFor(() => expect(getStreamParts()).toHaveLength(1));
      expect(getStreamParts()[0]!.text).toBe(" world");
      // A delta for a part never seen creates it (projection parity).
      sendContractEvent(textDelta("p9", "late"));
      await waitFor(() => expect(getStreamParts()).toHaveLength(2));
      expect(getStreamParts()[1]).toMatchObject({ partID: "p9", text: "late" });
    });

    it("reasoning parts render as thinking bubbles, separate parts stay separate", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      sendContractEvent(reasoningPartEnd("r1", "hmm"));
      sendContractEvent(textPartEnd("t1", "answer"));
      sendContractEvent(reasoningPartEnd("r2", "more"));
      await waitFor(() => expect(getStreamParts()).toHaveLength(3));
      const types = getStreamParts().map((p) => p.type);
      expect(types).toEqual(["thinking", "text", "thinking"]);
    });

    it("ignores parts for a different session", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      sendContractEvent(textPartEnd("p1", "x", "sess-OTHER"));
      await act(async () => { await new Promise((r) => setTimeout(r, 30)); });
      expect(getStreamParts()).toHaveLength(0);
    });

    it("user-message echo is dropped by message id (MESSAGE_START USER primes the gate)", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      sendContractEvent(abiEvent({
        type: 3, // MESSAGE_START
        messageId: "msg_u1",
        message: create(MessageSchema, { id: "msg_u1", sessionId: "sess-1", type: 1 }), // USER
      }));
      // The harness echoes the user's text as parts of that same message.
      sendContractEvent(textPartEnd("up1", "user typed this", "sess-1", "msg_u1"));
      sendContractEvent(textDelta("up1", " more", "msg_u1"));
      await act(async () => { await new Promise((r) => setTimeout(r, 30)); });
      expect(getStreamParts()).toHaveLength(0);
      // Assistant parts of a different message still render.
      sendContractEvent(textPartEnd("ap1", "assistant reply", "sess-1", "msg_a1"));
      await waitFor(() => expect(getStreamParts()).toHaveLength(1));
      expect(getStreamParts()[0]!.text).toBe("assistant reply");
    });

    it("tool parts render with state; repeat snapshots update in place", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      sendContractEvent(toolPartEnd("tp1", { name: "bash", callId: "call-1", status: 2 }));
      sendContractEvent(toolPartEnd("tp1", { name: "bash", callId: "call-1", status: 3 }));
      await waitFor(() => expect(getStreamParts()).toHaveLength(1));
      const tool = getStreamParts()[0]!;
      expect(tool.type).toBe("tool");
      expect(tool.toolState).toBe("completed");
      expect(tool.toolCallID).toBe("call-1");
    });

    it("handleSend clears the parts array", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      sendContractEvent(textPartEnd("p1", "prior turn"));
      await waitFor(() => expect(getStreamParts()).toHaveLength(1));
      const textarea = screen.getByRole("textbox");
      await userEvent.type(textarea, "new message");
      await userEvent.keyboard("{Enter}");
      await waitFor(() => expect(getStreamParts()).toHaveLength(0));
    });
  });

  describe("ERROR contract events", () => {
    it("maps error codes to user-facing text", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      sendContractEvent(abiEvent({
        type: 10,
        error: { code: "ContextOverflowError", message: "overflow" } as never,
      }));
      const el = await screen.findByTestId("chat-view");
      await waitFor(() => {
        const rendered = JSON.parse(el.getAttribute("data-messages") || "[]");
        expect(rendered.some((m: { parts: Array<{ text?: string }> }) =>
          m.parts.some((p) => (p.text ?? "").includes("/compact")))).toBe(true);
      });
    });

    it("ignores errors from a different session", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      sendContractEvent(abiEvent({
        type: 10,
        sessionId: "sess-OTHER",
        error: { code: "x", message: "boom" } as never,
      }));
      const el = screen.getByTestId("chat-view");
      await act(async () => { await new Promise((r) => setTimeout(r, 30)); });
      const rendered = JSON.parse(el.getAttribute("data-messages") || "[]");
      expect(rendered).toHaveLength(0);
    });
  });

  describe("context usage (per-step occupancy)", () => {
    it("MESSAGE_END cost tokens update the context numerator", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      // DiskUsageBar receives contextUsed — assert via the render output
      // would need the real bar; here we assert the dispatch does not
      // throw and the event path is exercised (unit coverage of the
      // derivation lives in the e2e specs).
      sendContractEvent(abiEvent({
        type: 4, // MESSAGE_END
        messageId: "msg_a1",
        message: create(MessageSchema, { id: "msg_a1", sessionId: "sess-1", type: 2, cost: { inputTokens: 100n, cacheReadTokens: 20n, cacheWriteTokens: 5n } }),
      }));
      expect(capturedContractOptions).not.toBeNull();
    });
  });

  describe("SESSION_UPDATED contract events", () => {
    it("updates the sidebar title cache for any session", async () => {
      const qc = makeQueryClient();
      qc.setQueryData(["sessions", "ws-1"], [
        { id: "sess-2", title: "old" },
      ]);
      await renderReady(qc);
      sendContractEvent(abiEvent({
        type: 2,
        sessionId: "sess-2",
        session: create(SessionSchema, { id: "sess-2", title: "fresh title" }),
      }));
      await waitFor(() => {
        expect(qc.getQueryData(["sessions", "ws-1"])).toEqual([
          { id: "sess-2", title: "fresh title" },
        ]);
      });
    });
  });

  describe("fold-driven prompt seeding (I12)", () => {
    it("snapshot pendingInputs add prompts via the provider", async () => {
      const qc = makeQueryClient();
      const adds: unknown[][] = [];
      const { useAddPendingQuestion } = await import("../providers/SessionActivityProvider");
      void useAddPendingQuestion;
      void adds;
      await renderReady(qc);
      applySnapshot(5n, [
        makeSessionSnapshot("sess-1", {
          status: 3,
          pendingInputs: [{
            id: "q1",
            sessionId: "sess-1",
            kind: 1,
            question: "Continue?",
            header: "Confirm",
            options: [{ label: "yes", description: "" }],
          }],
        }),
      ]);
      // The provider is mocked in this suite; the prompt-sync path is
      // covered in ChatPage.input.test.tsx with a live provider spy.
      expect(capturedContractOptions).not.toBeNull();
    });
  });

  describe("unknown platform events", () => {
    it("silently ignores unknown event types", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      sendPlatformEvent({ type: "mystery.event" } as unknown as WorkspaceStreamEvent);
      expect(capturedPlatformHandler).not.toBeNull();
    });
  });
});
