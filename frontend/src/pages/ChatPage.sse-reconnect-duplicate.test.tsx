/**
 * Regression tests for duplicate streaming bubbles (post US-69.10 hard
 * cutover).
 *
 * Production symptom (chat.safespaces.dev session
 * ses_fe5364736ffe4E1HF3J3fUt2sf): one running bash call rendered as two
 * concurrent spinner bubbles with divergent elapsed clocks ("3m 22s" and
 * "47s") for a single in-flight call, after the stream re-delivered the
 * same part following a reconnect.
 *
 * The old fix contract (a historyPartIds gate fed by the SSE dialect) is
 * deleted with the dialect. The new stitch rule is entity-ID based (I12):
 * the live buffer `streamParts` upserts by part id (tool call id for tool
 * parts), so re-delivered snapshots of the same entity replace in place
 * and can never append a second bubble. When a contract-stream reconnect
 * reconciles history (opencode persists running parts), reconcileOnIdle
 * clears the live buffer entirely — history is authoritative — and any
 * post-reconnect re-emission of the same part re-materializes as exactly
 * ONE bubble.
 *
 * These tests pin:
 * - two PART_END contract events with the same part id → ONE bubble
 *   (upsert by entity id, never append);
 * - parts for different ids stay separate (no over-merging);
 * - the tool elapsed badge anchors to the FIRST-seen state.startedAt per
 *   call id (the agent rewrites startedAt on every snapshot — observed
 *   live: same call, start moved 75.7s between two updates);
 * - after a reconnect reconcile (history refetch), streamParts clear;
 *   a resumed re-emission of the same part re-enters as exactly one
 *   bubble.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, waitFor, act, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";
import { create } from "@bufbuild/protobuf";
import {
  EventSchema,
  PartSchema,
  EventType,
  PartType,
  ToolStatus,
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
      sendAsync: vi.fn(),
      queueMessage: vi.fn().mockResolvedValue({ messageID: "msg_q_mock" }),
      getQueue: vi.fn().mockResolvedValue({ messages: [] }),
      deleteQueueMessage: vi.fn().mockResolvedValue(undefined),
    },
  };
});

vi.mock("../api/sessions", () => ({ sessionsApi: { create: vi.fn() } }));

// Platform stream (useEventStream) — mocked inert; the workspace stream
// carries platform events only after the cutover and none are driven here.
vi.mock("../hooks/useEventStream", () => ({
  useEventStream: vi.fn(),
}));

// Contract stream (useContractStream) — captured options + controllable
// fold state; agent-derived events flow through onEvent.
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

// Mock ChatView to expose BOTH rendered sources — the history-driven
// `messages` array and the live `streamParts` array. The duplicate-bubble
// bug is precisely "the same entity rendered twice across the two".
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

import { workspacesApi } from "../api/workspaces";
import { messagesApi } from "../api/messages";

// --- Helpers ---

interface StreamPartLike {
  type: string;
  text?: string;
  partID?: string;
  toolState?: string;
  toolStartedAt?: string;
  toolCallID?: string;
  toolOutput?: string;
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

function sendContractEvent(evt: Event, seq: bigint = 1n) {
  act(() => { capturedContractOptions?.onEvent(evt, seq); });
}

function triggerContractReconnect() {
  act(() => { capturedContractOptions?.onReconnect(); });
}

function getRenderedMessages(): Array<{ id: string; role: string; parts: Array<{ type: string; id?: string; toolCallId?: string; text?: string }> }> {
  const el = screen.getByTestId("chat-view");
  return JSON.parse(el.getAttribute("data-messages") || "[]");
}

function getRenderedStreamParts(): StreamPartLike[] {
  const el = screen.getByTestId("chat-view");
  return JSON.parse(el.getAttribute("data-stream-parts") || "[]");
}

function abiEvent(partial: Partial<Event>): Event {
  return create(EventSchema, { sessionId: "sess-1", ...partial } as Parameters<typeof create>[1]);
}

function textPartEnd(partId: string, text: string): Event {
  return abiEvent({
    type: EventType.PART_END,
    partId,
    part: create(PartSchema, { id: partId, type: PartType.TEXT, payload: { case: "text", value: text } }),
  });
}

/** PART_END for a tool call with an explicit state.startedAt — the agent
 * rewrites startedAt on every part snapshot (observed live), which is
 * what made the old elapsed badge reset per update. */
function toolPartEndWithStart(partId: string, callId: string, status: ToolStatus, startedAtMs: number): Event {
  return abiEvent({
    type: EventType.PART_END,
    partId,
    part: create(PartSchema, {
      id: partId,
      type: PartType.TOOL,
      payload: {
        case: "tool",
        value: {
          callId,
          name: "bash",
          state: {
            status,
            startedAt: { seconds: BigInt(Math.floor(startedAtMs / 1000)), nanos: 0 },
          },
        },
      },
    }),
  });
}

// Contract-shaped in-flight history message, as the adapter returns it:
// parts carry id; tool carries callId + state.startedAt.
function inFlightHistory(): Array<Record<string, unknown>> {
  return [
    { id: "msg-prior-user", role: "user", createdAt: "2026-08-20T18:50:00Z", parts: [{ type: "text", text: "run the poll" }] },
    {
      id: "msg-inflight",
      role: "assistant",
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
  { id: "msg-prior-user", role: "user", createdAt: "2026-08-20T18:50:00Z", parts: [{ type: "text", text: "run the poll" }] },
];

// --- Tests ---

describe("Entity-ID stitch: reconnect re-delivery must not duplicate in-flight parts", () => {
  beforeEach(() => {
    capturedContractOptions = null;
    contractState = { seq: 0n, sessions: new Map() };
    vi.clearAllMocks();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockImplementation(async () => {
      const msgs = await (messagesApi.getHistory as ReturnType<typeof vi.fn>)();
      return { messages: msgs, nextCursor: undefined };
    });
  });

  async function renderReady(qc: QueryClient, path = "/chat/ws-1/sess-1") {
    const utils = renderChat(qc, path);
    await waitFor(() => {
      expect(capturedContractOptions).not.toBeNull();
    });
    return utils;
  }

  it("REGRESSION — two PART_END events with the same part id render as ONE bubble (upsert, not append)", async () => {
    const qc = makeQueryClient();
    await renderReady(qc);

    // The stream re-delivers the same part after a reconnect (opencode
    // re-emits part snapshots it already sent). The entity-ID stitch must
    // replace the existing bubble keyed by part id — never append.
    sendContractEvent(textPartEnd("p1", "Hel"));
    sendContractEvent(textPartEnd("p1", "Hello world"));

    await waitFor(() => {
      const parts = getRenderedStreamParts().filter((p) => p.partID === "p1");
      expect(parts).toHaveLength(1);
    });
    expect(getRenderedStreamParts()[0]!.text).toBe("Hello world");
  });

  it("parts for DIFFERENT ids stay separate bubbles (no over-merging)", async () => {
    const qc = makeQueryClient();
    await renderReady(qc);

    sendContractEvent(textPartEnd("p1", "first"));
    sendContractEvent(textPartEnd("p2", "second"));
    sendContractEvent(toolPartEndWithStart("tp1", "call-a", ToolStatus.RUNNING, Date.parse("2026-08-20T18:58:05Z")));
    sendContractEvent(toolPartEndWithStart("tp2", "call-b", ToolStatus.RUNNING, Date.parse("2026-08-20T19:05:00Z")));

    await waitFor(() => expect(getRenderedStreamParts()).toHaveLength(4));
    const parts = getRenderedStreamParts();
    expect(parts.map((p) => p.partID)).toEqual(["p1", "p2", "tp1", "tp2"]);
    expect(parts.map((p) => p.type)).toEqual(["text", "text", "tool", "tool"]);
    expect(parts[2]!.toolCallID).toBe("call-a");
    expect(parts[3]!.toolCallID).toBe("call-b");
  });

  it("elapsed badge anchors to the FIRST-seen state.startedAt per call id (the agent rewrites it per snapshot)", async () => {
    const qc = makeQueryClient();
    await renderReady(qc);

    const t1 = Date.parse("2026-08-20T18:58:05Z");
    const t2 = t1 + 75_744; // next snapshot rewrites startedAt (observed live)
    sendContractEvent(toolPartEndWithStart("prt_x", "call_x", ToolStatus.RUNNING, t1));
    sendContractEvent(toolPartEndWithStart("prt_x", "call_x", ToolStatus.COMPLETED, t2));

    await waitFor(() => {
      const parts = getRenderedStreamParts().filter((p) => p.toolCallID === "call_x");
      expect(parts).toHaveLength(1);
      // Anchored to the first-seen start — the divergent-elapsed-clocks
      // symptom was two bubbles trusting two different starts.
      expect(parts[0]!.toolStartedAt).toBe(new Date(t1).toISOString());
      // The later snapshot's state landed via replace-in-place.
      expect(parts[0]!.toolState).toBe("completed");
    });
  });

  it("REGRESSION — contract-stream reconnect reconcile clears streamParts once history owns the turn; a re-emission re-materializes exactly ONE bubble", async () => {
    const qc = makeQueryClient();
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue(priorTurnOnly);

    await renderReady(qc);
    await waitFor(() => {
      expect(getRenderedMessages()).toHaveLength(1);
    });

    // The live turn streams a running bash tool.
    sendContractEvent(toolPartEndWithStart("prt_running", "call_running", ToolStatus.RUNNING, Date.parse("2026-08-20T18:58:05Z")));
    await waitFor(() => {
      expect(getRenderedStreamParts().filter((p) => p.toolCallID === "call_running")).toHaveLength(1);
    });

    // The contract stream drops + reconnects. The reconcile refetch now
    // returns the in-flight message (opencode persisted the running part).
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue(inFlightHistory());
    triggerContractReconnect();

    await waitFor(() => {
      expect((messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mock.calls.length).toBeGreaterThanOrEqual(2);
    });
    await waitFor(() => {
      // History now renders the in-flight tool (part id preserved)...
      expect(
        getRenderedMessages().some((m) => m.parts.some((p) => p.toolCallId === "call_running" || p.id === "prt_running")),
      ).toBe(true);
      // ...and the live buffer is cleared: the same entity must not render
      // from both sources at once.
      expect(getRenderedStreamParts()).toHaveLength(0);
    });

    // The stream resumes: the agent re-emits part snapshots for the SAME
    // call (startedAt rewritten to the later snapshot time). The entity-ID
    // stitch must re-materialize it as exactly ONE live bubble, never a
    // duplicate.
    sendContractEvent(toolPartEndWithStart("prt_running", "call_running", ToolStatus.RUNNING, Date.parse("2026-08-20T19:00:00Z")));

    await waitFor(() => {
      const dupes = getRenderedStreamParts().filter((p) => p.toolCallID === "call_running");
      expect(dupes).toHaveLength(1);
    });
  });
});
