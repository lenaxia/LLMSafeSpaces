// Design 0050 D5 (#896, review round 1): the live SSE path must thread
// the tool start time so the elapsed badge renders during a watched
// turn — the incident's motivating scenario. Both opencode wire shapes:
// 1.18.10+ flat (state.time.start, epoch millis) and ≤1.15.x nested
// (state.startedAt, ISO). The history path is covered in
// messages.test.ts; this file covers parseStreamEvent → streamParts.
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, waitFor, act } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";
import type { SessionContractEvent, ContractPart } from "../api/types";

let capturedSSEHandler: ((data: unknown) => void) | null = null;
let lastStreamParts: Array<Record<string, unknown>> = [];

vi.mock("../hooks/useEventStream", () => ({
  useEventStream: vi.fn((_workspaceId: string | undefined, handler: (data: unknown) => void) => {
    capturedSSEHandler = handler;
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
    requestInputSnapshot: vi.fn().mockResolvedValue(undefined),
    markSessionSeen: vi.fn().mockResolvedValue(undefined),
    renameWorkspace: vi.fn(),
    renameSession: vi.fn(),
  },
}));
vi.mock("../api/messages", () => ({
  messagesApi: {
    getHistory: vi.fn().mockResolvedValue([]),
    sendAsync: vi.fn().mockResolvedValue(undefined),
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
  useSessionStatus: () => "idle",
  resolveSessionStatus: () => "idle",
  SessionActivityProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

const WS = "ws-1";
const SES = "ses-badge";

// Contract SSE envelope (US-65.8): session.event whose data is a
// pkg/session Event — part.end carries the translated contract Part
// (mirrors the stream consumed at ChatPage.handleContractEvent).
function sendToolPart(part: Record<string, unknown>) {
  const contractTool = (raw: Record<string, unknown>) => ({
    type: "tool",
    tool: {
      name: raw.tool as string,
      callId: raw.callID as string | undefined,
      input: (raw.state as Record<string, unknown> | undefined)?.input,
      output: (raw.state as Record<string, unknown> | undefined)?.output,
      state: {
        status: (raw.state as Record<string, unknown> | undefined)?.status,
        startedAt: (raw.state as Record<string, unknown> | undefined)?.startedAt,
      },
    },
  });
  const rawState = part.state as Record<string, unknown> | undefined;
  const rawStart = rawState?.time ? (rawState.time as Record<string, number>).start : undefined;
  const startedAt = (rawState?.startedAt as string | undefined) ?? (typeof rawStart === "number" ? new Date(rawStart).toISOString() : undefined);
  const ev: SessionContractEvent = {
    type: "session.event",
    session_id: SES,
    data: {
      type: "part.end",
      sessionId: SES,
      messageId: "msg-badge",
      partId: typeof part.id === "string" ? part.id : undefined,
      part: contractTool({ ...part, state: { ...rawState, startedAt } }) as unknown as ContractPart,
    },
  };
  act(() => {
    capturedSSEHandler?.(ev);
  });
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
  capturedSSEHandler = null;
  lastStreamParts = [];
});

describe("SSE tool start time (#892 D5)", () => {
  it("threads flat-shape state.time.start (epoch millis) normalized to ISO", async () => {
    renderChat();
    await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

    const epochMs = Date.now() - 30_000;
    sendToolPart({ type: "tool", tool: "bash", callID: "call-1", state: { status: "running", time: { start: epochMs } } });

    await waitFor(() => expect(lastStreamParts.length).toBeGreaterThan(0));
    const tool = lastStreamParts.find((p) => p.type === "tool");
    expect(tool).toBeDefined();
    expect(tool!.toolStartedAt).toEqual(new Date(epochMs).toISOString());
  });

  it("threads legacy nested state.startedAt (ISO) verbatim", async () => {
    renderChat();
    await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

    const iso = "2026-08-15T21:35:25.000Z";
    sendToolPart({ type: "tool", tool: "bash", callID: "call-2", state: { status: "running", startedAt: iso } });

    await waitFor(() => expect(lastStreamParts.length).toBeGreaterThan(0));
    const tool = lastStreamParts.find((p) => p.type === "tool");
    expect(tool!.toolStartedAt).toEqual(iso);
  });

  it("preserves the original start time when a same-callID update omits it", async () => {
    renderChat();
    await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

    const epochMs = Date.now() - 60_000;
    sendToolPart({ type: "tool", tool: "bash", callID: "call-3", state: { status: "running", time: { start: epochMs } } });
    await waitFor(() => expect(lastStreamParts.length).toBeGreaterThan(0));
    const original = (lastStreamParts.find((p) => p.type === "tool")!.toolStartedAt as string);

    // Progress update: output grows, no time field on the wire.
    sendToolPart({ type: "tool", tool: "bash", callID: "call-3", state: { status: "running", output: "still working" } });
    await waitFor(() => {
      const tool = lastStreamParts.find((p) => p.type === "tool");
      expect(tool!.toolOutput).toBe("still working");
    });
    const after = lastStreamParts.find((p) => p.type === "tool")!.toolStartedAt;
    expect(after).toEqual(original);
  });

  it("no start time on the wire → no toolStartedAt (badge degrades to absent)", async () => {
    renderChat();
    await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

    sendToolPart({ type: "tool", tool: "bash", callID: "call-4", state: { status: "running" } });
    await waitFor(() => expect(lastStreamParts.length).toBeGreaterThan(0));
    expect(lastStreamParts.find((p) => p.type === "tool")!.toolStartedAt).toBeUndefined();
    expect(screen.getByTestId("chat-view")).toBeInTheDocument();
  });
});
