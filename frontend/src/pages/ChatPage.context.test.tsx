/**
 * S36.4 / US-69.10: ChatPage context usage — per-session contextUsed.
 *
 * 1. Cold start: DiskUsageBar receives contextUsed from the active
 *    session's sessions-list entry (persisted by proxy, returned by
 *    GET /workspaces/:id/sessions).
 * 2. Realtime: MESSAGE_END contract events derive the numerator from
 *    evt.message.cost — used = inputTokens + cacheReadTokens +
 *    cacheWriteTokens (bigint fields → Number()).
 * 3. Compaction indicator appears when contextUsed drops >50%, is
 *    dismissible, and does not false-positive across session switches.
 * 4. SESSION_UPDATED contract events refresh the sessions query cache.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { create } from "@bufbuild/protobuf";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";
import type { Event } from "../abi/llmsafespaces/abi/v1/contract_pb";
import { EventSchema, EventType, MessageSchema, MessageType, SessionSchema } from "../abi/llmsafespaces/abi/v1/contract_pb";
import type { SessionSnapshot } from "../abi/llmsafespaces/abi/v1/abi_pb";

vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getStatus: vi.fn(),
    getSessions: vi.fn().mockResolvedValue([]),
    activate: vi.fn(),
    list: vi.fn().mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } }),
    listModels: vi.fn().mockResolvedValue({ models: [], currentModel: "" }),
    setModel: vi.fn().mockResolvedValue({ model: "", applied: false }),
    renameWorkspace: vi.fn().mockResolvedValue({}),
    renameSession: vi.fn(),
    deleteWorkspace: vi.fn().mockResolvedValue({}),
    suspend: vi.fn().mockResolvedValue({}),
    abortSession: vi.fn(),
    markSessionSeen: vi.fn().mockResolvedValue(undefined),
    deleteSession: vi.fn().mockResolvedValue(undefined),
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
  SessionActivityProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
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

// Platform stream (workspace events only after the cutover) — captured,
// not driven here.
let capturedPlatformHandler: ((data: unknown) => void) | null = null;
vi.mock("../hooks/useEventStream", () => ({
  useEventStream: vi.fn((_workspaceId: string | undefined, handler: (data: unknown) => void) => {
    capturedPlatformHandler = handler;
  }),
}));

// Contract stream: capture the options ChatPage registers; the fold state
// stays empty (context usage derives from MESSAGE_END events, not the fold).
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

import { workspacesApi } from "../api/workspaces";

function makeQC() {
  return new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
}

function renderChat(qc: QueryClient, path: string) {
  const wsId = path.split("/")[2];
  const sesId = path.split("/")[3];
  qc.setQueryData(["workspace-status", wsId], { phase: "Active", contextTotal: 200000 });
  qc.setQueryData(["workspaces"], { items: [], pagination: { limit: 20, offset: 0, total: 0 } });
  if (sesId) {
    qc.setQueryData(["messages", wsId, sesId], { pages: [{ messages: [], nextCursor: undefined }], pageParams: [undefined] });
  }
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

/** Seed the sessions query cache with a session that has the given contextUsed. */
function seedSessionsCache(qc: QueryClient, workspaceId: string, sessionId: string, contextUsed?: number) {
  qc.setQueryData(["sessions", workspaceId], [
    { id: sessionId, title: "Test session", messageCount: 1, status: "idle", hasUnread: false, contextUsed },
  ]);
}

/** A MESSAGE_END contract event carrying the step's token cost. */
function messageEndEvent(sessionId: string, inputTokens: bigint, cacheRead: bigint, cacheWrite: bigint): Event {
  return create(EventSchema, {
    type: EventType.MESSAGE_END,
    sessionId,
    messageId: `msg_${sessionId}`,
    message: create(MessageSchema, {
      id: `msg_${sessionId}`,
      sessionId,
      type: MessageType.ASSISTANT,
      cost: { inputTokens, cacheReadTokens: cacheRead, cacheWriteTokens: cacheWrite },
    }),
  } as Parameters<typeof create>[1]);
}

/** Realtime context signal: one completed assistant step's token cost. */
function fireMessageEnd(sessionId: string, usedTokens: bigint, cacheRead = 0n, cacheWrite = 0n) {
  act(() => { capturedContractOptions?.onEvent(messageEndEvent(sessionId, usedTokens, cacheRead, cacheWrite), 1n); });
}

async function renderReady(qc: QueryClient, path = "/chat/ws-1/ses-1") {
  const utils = renderChat(qc, path);
  await waitFor(() => {
    expect(capturedContractOptions).not.toBeNull();
    expect(capturedPlatformHandler).not.toBeNull();
  });
  return utils;
}

// --- Tests ---

describe("S36.4 — per-session contextUsed in DiskUsageBar", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    capturedPlatformHandler = null;
    capturedContractOptions = null;
    contractState = { seq: 0n, sessions: new Map() };
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({
      phase: "Active",
      contextTotal: 200000,
    });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "WS", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([]);
  });

  it("shows context bar with per-session contextUsed from sessions list (cold start)", async () => {
    // ChatPage invalidates the sessions query on mount (mark-seen flow);
    // the refetch must carry the same entry so the durable value sticks.
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "ses-1", title: "Test session", messageCount: 1, status: "idle", hasUnread: false, contextUsed: 45000 },
    ]);
    const qc = makeQC();
    seedSessionsCache(qc, "ws-1", "ses-1", 45000);
    await renderReady(qc);
    await waitFor(() => {
      expect(screen.getAllByText(/45K/).length).toBeGreaterThan(0);
    });
  });

  it("shows context bar with 0 when session has no contextUsed", async () => {
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "ses-1", title: "Test session", messageCount: 1, status: "idle", hasUnread: false, contextUsed: undefined },
    ]);
    const qc = makeQC();
    seedSessionsCache(qc, "ws-1", "ses-1", undefined);
    await renderReady(qc);
    await waitFor(() => {
      expect(screen.getAllByText(/Context/).length).toBeGreaterThan(0);
    });
  });

  it("MESSAGE_END cost (input + cacheRead + cacheWrite, bigint → Number) updates the context numerator", async () => {
    const qc = makeQC();
    await renderReady(qc);
    // 90K input + 5K cache read + 5K cache write = 100K used.
    fireMessageEnd("ses-1", 90_000n, 5_000n, 5_000n);
    await waitFor(() => {
      expect(screen.getAllByText(/100K/).length).toBeGreaterThan(0);
    });
  });

  it("MESSAGE_END for a different session does not move the viewed session's bar", async () => {
    const qc = makeQC();
    await renderReady(qc);
    fireMessageEnd("ses-other", 111_000n);
    await act(async () => { await new Promise((r) => setTimeout(r, 30)); });
    expect(screen.queryByText(/111K/)).not.toBeInTheDocument();
  });
});

describe("S36.4 — compaction indicator (MESSAGE_END-derived)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    capturedPlatformHandler = null;
    capturedContractOptions = null;
    contractState = { seq: 0n, sessions: new Map() };
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({
      phase: "Active",
      contextTotal: 200000,
    });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "WS", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([]);
  });

  it("shows compaction banner when contextUsed drops >50% via MESSAGE_END events", async () => {
    const qc = makeQC();
    await renderReady(qc);

    // First step: 100K prompt tokens
    fireMessageEnd("ses-1", 100_000n);
    await waitFor(() => expect(screen.getAllByText(/100K/).length).toBeGreaterThan(0));

    // Second step: 40K — drops >50%, triggers the compaction banner
    fireMessageEnd("ses-1", 40_000n);
    await waitFor(() => {
      expect(screen.getByText(/context compacted/i)).toBeInTheDocument();
    });
  });

  it("does NOT show compaction banner when contextUsed drops less than 50%", async () => {
    const qc = makeQC();
    await renderReady(qc);

    fireMessageEnd("ses-1", 100_000n);
    await waitFor(() => expect(screen.getAllByText(/100K/).length).toBeGreaterThan(0));

    // Drop by only 30%: 70K — no compaction
    fireMessageEnd("ses-1", 70_000n);
    await waitFor(() => expect(screen.getAllByText(/70K/).length).toBeGreaterThan(0));

    expect(screen.queryByText(/context compacted/i)).not.toBeInTheDocument();
  });

  it("compaction banner can be dismissed", async () => {
    const user = userEvent.setup();
    const qc = makeQC();
    await renderReady(qc);

    fireMessageEnd("ses-1", 100_000n);
    await waitFor(() => expect(screen.getAllByText(/100K/).length).toBeGreaterThan(0));

    fireMessageEnd("ses-1", 20_000n);
    await waitFor(() => expect(screen.getByText(/context compacted/i)).toBeInTheDocument());

    await user.click(screen.getByRole("button", { name: /dismiss/i }));
    expect(screen.queryByText(/context compacted/i)).not.toBeInTheDocument();
  });

  it("no false compaction banner when switching to a session with lower contextUsed", async () => {
    // S36.4 regression: prevContextUsedRef resets on session switch, so a
    // lower first reading on the new session must not read as compaction.
    const qc = makeQC();
    const { unmount } = await renderReady(qc, "/chat/ws-1/ses-1");

    fireMessageEnd("ses-1", 150_000n);
    await waitFor(() => expect(screen.getAllByText(/150K/).length).toBeGreaterThan(0));

    unmount();
    await renderReady(qc, "/chat/ws-1/ses-2");

    // ses-2's first reading is 20K — no previous value → no compaction
    fireMessageEnd("ses-2", 20_000n);
    await waitFor(() => expect(screen.getAllByText(/20K/).length).toBeGreaterThan(0));
    expect(screen.queryByText(/context compacted/i)).not.toBeInTheDocument();
  });
});

describe("SESSION_UPDATED contract events", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    capturedPlatformHandler = null;
    capturedContractOptions = null;
    contractState = { seq: 0n, sessions: new Map() };
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({
      phase: "Active",
      contextTotal: 200000,
    });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "WS", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([]);
  });

  it("updates the sessions query cache with the new title", async () => {
    // The mount-time invalidation refetches the sessions query; the mock
    // supplies the old entry so the cache settles before the event lands.
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "ses-1", title: "old" },
    ]);
    const qc = makeQC();
    await renderReady(qc);
    await waitFor(() => expect(qc.getQueryData(["sessions", "ws-1"])).toEqual([{ id: "ses-1", title: "old" }]));
    act(() => {
      capturedContractOptions?.onEvent(create(EventSchema, {
        type: EventType.SESSION_UPDATED,
        sessionId: "ses-1",
        session: create(SessionSchema, { id: "ses-1", title: "fresh title" }),
      } as Parameters<typeof create>[1]), 1n);
    });
    await waitFor(() => {
      expect(qc.getQueryData(["sessions", "ws-1"])).toEqual([
        { id: "ses-1", title: "fresh title" },
      ]);
    });
  });
});

