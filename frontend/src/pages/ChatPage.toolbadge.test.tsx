// Design 0050 D5 (#896, review round 1): the live contract-stream path must
// thread the tool start time so the elapsed badge renders during a watched
// turn — the incident's motivating scenario. After the US-69.10 hard cutover
// tool parts arrive as ABI Part payloads on PART_START/PART_END contract
// events (useContractStream); ToolState.startedAt is a protobuf Timestamp
// {seconds: bigint, nanos: number}. The history path is covered in
// messages.test.ts; this file covers contract events → streamParts.
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, waitFor, act } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { create } from "@bufbuild/protobuf";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";
import { EventSchema, PartSchema } from "../abi/llmsafespaces/abi/v1/contract_pb";
import type { Event } from "../abi/llmsafespaces/abi/v1/contract_pb";
import type { SessionSnapshot } from "../abi/llmsafespaces/abi/v1/abi_pb";

let capturedContractOptions: {
  onEvent: (event: Event, seq: bigint) => void;
} | null = null;
let contractState: { seq: bigint; sessions: Map<string, SessionSnapshot> };
let lastStreamParts: Array<Record<string, unknown>> = [];
let nextSeq = 0n;

vi.mock("../hooks/useEventStream", () => ({ useEventStream: vi.fn() }));

// Capture the contract-stream options ChatPage registers with
// useContractStream, and expose a controllable fold state.
vi.mock("../hooks/useContractStream", () => ({
  useContractStream: vi.fn((_workspaceId: string | undefined, options: Record<string, unknown>) => {
    capturedContractOptions = options as typeof capturedContractOptions;
    return contractState;
  }),
}));

vi.mock("../components/chat/ChatView", () => ({
  ChatView: (props: Record<string, unknown>) => {
    lastStreamParts = (props.streamParts ?? []) as Array<Record<string, unknown>>;
    return <div data-testid="chat-view" data-streaming={String(props.streaming ?? false)} />;
  },
}));

vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getStatus: vi.fn(),
    activate: vi.fn(),
    list: vi.fn().mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } }),
    getSessions: vi.fn().mockResolvedValue([]),
    markSessionSeen: vi.fn().mockResolvedValue(undefined),
  },
}));
vi.mock("../api/messages", () => {
  const gh = vi.fn().mockResolvedValue([]);
  return { messagesApi: { getHistory: gh, getHistoryPage: vi.fn().mockImplementation(async () => { const msgs = await gh(); return { messages: msgs, nextCursor: undefined }; }), sendAsync: vi.fn(), queueMessage: vi.fn().mockResolvedValue({ messageID: "msg_q_mock" }), getQueue: vi.fn().mockResolvedValue({ messages: [] }), deleteQueueMessage: vi.fn().mockResolvedValue(undefined) } };
});
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
  SessionActivityProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

const WS = "ws-1";
const SES = "ses-badge";

function abiEvent(partial: Partial<Event>): Event {
  return create(EventSchema, { sessionId: SES, ...partial } as Parameters<typeof create>[1]);
}

/** One PART_END contract event carrying a tool Part whose ToolState carries
 * the given startedAt (protobuf Timestamp: bigint seconds + nanos). NB:
 * a Date is NOT a valid Timestamp MessageInit — create() accepts it
 * silently but produces the epoch-zero Timestamp (seconds 0n). */
function toolPartEnd(partId: string, tool: {
  callId: string;
  name?: string;
  status?: number;
  startedAt?: { seconds: bigint; nanos: number };
}, messageId = "msg-badge"): Event {
  return abiEvent({
    type: 7, // PART_END
    partId,
    messageId,
    part: create(PartSchema, {
      id: partId,
      type: 3,
      payload: {
        case: "tool",
        value: {
          callId: tool.callId,
          name: tool.name ?? "bash",
          ...(tool.status !== undefined || tool.startedAt !== undefined
            ? {
              state: {
                ...(tool.status !== undefined ? { status: tool.status } : {}),
                ...(tool.startedAt !== undefined ? { startedAt: tool.startedAt } : {}),
              },
            }
            : {}),
        },
      },
    }),
  });
}

function sendContractEvent(evt: Event) {
  act(() => { capturedContractOptions?.onEvent(evt, ++nextSeq); });
}

function renderChat() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 0 } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[`/chat/${WS}/${SES}`]}>
        <TooltipProvider delayDuration={0}>
          <Routes>
            <Route path="/chat/:workspaceId/:sessionId" element={<ChatPage />} />
          </Routes>
        </TooltipProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  capturedContractOptions = null;
  contractState = { seq: 0n, sessions: new Map() };
  lastStreamParts = [];
  nextSeq = 0n;
});

describe("contract-stream tool start time (#892 D5)", () => {
  it("ToolState.startedAt (protobuf Timestamp) renders as toolStartedAt ISO", async () => {
    renderChat();
    await waitFor(() => expect(capturedContractOptions).not.toBeNull());

    sendContractEvent(toolPartEnd("tp1", {
      callId: "call-1",
      status: 2, // RUNNING
      startedAt: { seconds: 1788177600n, nanos: 0 },
    }));

    await waitFor(() => expect(lastStreamParts.length).toBeGreaterThan(0));
    const tool = lastStreamParts.find((p) => p.type === "tool");
    expect(tool).toBeDefined();
    expect(tool!.toolStartedAt).toEqual(new Date(1788177600_000).toISOString());
    expect(tool!.toolState).toBe("running");
    expect(tool!.toolCallID).toBe("call-1");
  });

  it("nanos floor to milliseconds in the ISO conversion", async () => {
    renderChat();
    await waitFor(() => expect(capturedContractOptions).not.toBeNull());

    sendContractEvent(toolPartEnd("tp2", {
      callId: "call-2",
      status: 2,
      startedAt: { seconds: 1788177600n, nanos: 250_000_000 }, // exact millis
    }));
    sendContractEvent(toolPartEnd("tp3", {
      callId: "call-3",
      status: 2,
      startedAt: { seconds: 1788177600n, nanos: 500_999_999 }, // sub-milli nanos floor to 500ms
    }));

    await waitFor(() => expect(lastStreamParts.length).toBe(2));
    const byCall = new Map(lastStreamParts.map((p) => [p.toolCallID, p.toolStartedAt as string | undefined]));
    expect(byCall.get("call-2")).toEqual(new Date(1788177600_250).toISOString());
    expect(byCall.get("call-3")).toEqual(new Date(1788177600_500).toISOString());
  });

  it("repeat PART_END for the same callId keeps the FIRST-seen startedAt (anchor)", async () => {
    renderChat();
    await waitFor(() => expect(capturedContractOptions).not.toBeNull());

    sendContractEvent(toolPartEnd("tp4", {
      callId: "call-4",
      status: 2,
      startedAt: { seconds: 1788177600n, nanos: 0 },
    }));
    await waitFor(() => expect(lastStreamParts.length).toBeGreaterThan(0));
    const first = lastStreamParts.find((p) => p.type === "tool")!.toolStartedAt as string;

    // The agent rewrites the start time on the later snapshot (new part id,
    // same call id) — the elapsed badge must not reset to the later value.
    sendContractEvent(toolPartEnd("tp4b", {
      callId: "call-4",
      status: 3, // COMPLETED
      startedAt: { seconds: 1788177999n, nanos: 0 },
    }));
    await waitFor(() => {
      const tool = lastStreamParts.find((p) => p.type === "tool");
      expect(tool!.toolState).toBe("completed");
    });
    expect(lastStreamParts.find((p) => p.type === "tool")!.toolStartedAt).toEqual(first);
    expect(lastStreamParts.filter((p) => p.type === "tool")).toHaveLength(1);
  });

  it("preserves the original startedAt when a same-callId update omits it", async () => {
    renderChat();
    await waitFor(() => expect(capturedContractOptions).not.toBeNull());

    sendContractEvent(toolPartEnd("tp5", {
      callId: "call-5",
      status: 2,
      startedAt: { seconds: 1788177600n, nanos: 0 },
    }));
    await waitFor(() => expect(lastStreamParts.length).toBeGreaterThan(0));
    const original = lastStreamParts.find((p) => p.type === "tool")!.toolStartedAt as string;

    // Progress update: state transitions, no startedAt on the wire.
    sendContractEvent(toolPartEnd("tp5b", { callId: "call-5", status: 3 }));
    await waitFor(() => {
      const tool = lastStreamParts.find((p) => p.type === "tool");
      expect(tool!.toolState).toBe("completed");
    });
    expect(lastStreamParts.find((p) => p.type === "tool")!.toolStartedAt).toEqual(original);
  });

  it("absent startedAt → toolStartedAt undefined (badge degrades to absent)", async () => {
    renderChat();
    await waitFor(() => expect(capturedContractOptions).not.toBeNull());

    sendContractEvent(toolPartEnd("tp6", { callId: "call-6", status: 2 }));

    await waitFor(() => expect(lastStreamParts.length).toBeGreaterThan(0));
    expect(lastStreamParts.find((p) => p.type === "tool")!.toolStartedAt).toBeUndefined();
  });
});
