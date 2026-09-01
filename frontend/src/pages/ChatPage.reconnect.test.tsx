/**
 * Fold-driven stuck-session auto-abort tests (post US-69.10 hard cutover).
 *
 * The old D9/D10 evidence chain (the SSE busy/status dialect + the
 * the dialect. The new semantics (I12): the fold published by
 * useContractStream is projection-authoritative —
 *
 *   evidence = stuck question/permission tool as the last assistant part
 *              in history (toolState "running")
 *            + foldViewedSession.status === BUSY
 *            + foldViewedSession.pendingInputs.length === 0
 *            + no prompts in the provider store (pendingPromptCount 0)
 *            + a contract snapshot stamped AFTER this page started
 *              viewing the session (lastContractSnapshotAt >
 *              sessionMountedAt)
 *   dwell   = AUTO_ABORT_DWELL_MS (1500ms) → workspacesApi.abortSession
 *             → "Session was interrupted" banner + reconcile.
 *
 * All session state is driven through the mocked useContractStream: the
 * module-level `contractState` (the hook's return value) provides the
 * viewed session's fold snapshot, and `applySnapshot(...)` invokes the
 * captured onSnapshot callback to stamp lastContractSnapshotAt AFTER
 * mount. The platform stream (useEventStream) is captured but idle — it
 * carries no session state anymore.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, waitFor, act, screen, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";
import { create } from "@bufbuild/protobuf";
import {
  EventSchema,
  PartSchema,
  EventType,
  PartType,
  SessionStatus,
  InputKind,
} from "../abi/llmsafespaces/abi/v1/contract_pb";
import type { Event } from "../abi/llmsafespaces/abi/v1/contract_pb";
import type { SessionSnapshot } from "../abi/llmsafespaces/abi/v1/abi_pb";
import { SessionSnapshotSchema } from "../abi/llmsafespaces/abi/v1/abi_pb";

// --- Mocks (mirroring ChatPage.sse.test.tsx, the reference pattern) ---

// Provider prompt store so the pendingPromptCount guard reflects prompts
// delivered outside the fold (set directly by tests; read at render time
// by the usePendingQuestionsForSession mock).
const promptStore = vi.hoisted(() => ({ questions: [] as Array<Record<string, unknown>> }));

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
  usePendingQuestionsForSession: () => promptStore.questions,
  usePendingPermissionsForSession: () => [],
  useClearSessionPendingPrompts: () => () => {},
  useWorkspaceInputSnapshot: () => undefined,
  SessionActivityProvider: ({ children }: { children: any }) => <>{children}</>,
}));

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
      sendAsync: vi.fn(),
      queueMessage: vi.fn().mockResolvedValue({ messageID: "msg_q_mock" }),
      getQueue: vi.fn().mockResolvedValue({ messages: [] }),
      deleteQueueMessage: vi.fn().mockResolvedValue(undefined),
    },
  };
});

vi.mock("../api/sessions", () => ({ sessionsApi: { create: vi.fn() } }));

// Platform stream — mocked inert; it carries platform events only (no
// session state) after the cutover.
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

function applySnapshot(atSeq: bigint, sessions: SessionSnapshot[]) {
  contractState = { seq: atSeq, sessions: new Map(sessions.map((s) => [s.sessionId, s])) };
  act(() => { capturedContractOptions?.onSnapshot(contractState); });
}

function textPartEnd(partId: string, text: string, sessionId: string): Event {
  return create(EventSchema, {
    sessionId,
    type: EventType.PART_END,
    partId,
    part: create(PartSchema, { id: partId, type: PartType.TEXT, payload: { case: "text", value: text } }),
  } as Parameters<typeof create>[1]);
}

function makeSessionSnapshot(sessionId: string, init: Record<string, unknown> = {}): SessionSnapshot {
  return create(SessionSnapshotSchema, { sessionId, ...init } as Parameters<typeof create>[1]);
}

function busyFold(sessionId: string, pendingInputs: Array<Record<string, unknown>> = []): SessionSnapshot {
  return makeSessionSnapshot(sessionId, { status: SessionStatus.BUSY, pendingInputs });
}

/** Stuck-tool history fixture (old-suite shape): the last assistant
 * message ends with a question/permission tool_use part still "running". */
function stuckHistory(toolText: string): Array<Record<string, unknown>> {
  return [
    { id: "msg-user", role: "user", parts: [{ type: "text", text: "push to github" }] },
    {
      id: "msg-asst",
      role: "assistant",
      parts: [{ type: "tool_use", text: toolText, toolState: "running" }],
    },
  ];
}

const STUCK_QUESTION = "question: GitHub auth required";
const STUCK_PERMISSION = "permission: shell";

function getRenderedMessages(): Array<{ id: string; role: string; parts: Array<{ type: string; text?: string }> }> {
  const el = screen.getByTestId("chat-view");
  return JSON.parse(el.getAttribute("data-messages") || "[]");
}

function abortMock() {
  return workspacesApi.abortSession as ReturnType<typeof vi.fn>;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

// --- Tests ---

describe("Fold-driven auto-abort of sessions stuck on an input tool", () => {
  beforeEach(() => {
    capturedContractOptions = null;
    contractState = { seq: 0n, sessions: new Map() };
    promptStore.questions = [];
    vi.clearAllMocks();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    abortMock().mockResolvedValue(undefined);
  });

  async function renderStuck(qc: QueryClient, sessionId: string, history: Array<Record<string, unknown>>) {
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue(history);
    renderChat(qc, `/chat/ws-1/${sessionId}`);
    await waitFor(() => expect(capturedContractOptions).not.toBeNull());
    // Deterministic: the stuck tool is rendered from history (history
    // loaded → the abort effect has run once with an empty fold).
    await waitFor(
      () => expect(getRenderedMessages().some((m) => m.parts.some((p) => p.type === "tool_use"))).toBe(true),
      { timeout: 5000 },
    );
  }

  /** Completes the evidence: a fold snapshot (BUSY, no pendingInputs)
   * stamped after mount, plus a live part event to force the re-render
   * that re-reads the mutated contractState (the mocked hook is a plain
   * vi.fn — only a ChatPage re-render picks up the new fold). */
  function armEvidence(sessionId: string, fold?: SessionSnapshot) {
    applySnapshot(1n, [fold ?? busyFold(sessionId)]);
    sendContractEvent(textPartEnd("p-ev", "streaming", sessionId));
  }

  it("busy fold + stuck question tool + fresh snapshot without pendingInputs → abortSession after the dwell + interrupted banner", async () => {
    const qc = makeQueryClient();
    await renderStuck(qc, "sess-stuck", stuckHistory(STUCK_QUESTION));

    armEvidence("sess-stuck");

    await waitFor(
      () => expect(abortMock()).toHaveBeenCalledWith("ws-1", "sess-stuck"),
      { timeout: 5000 },
    );
    await waitFor(() =>
      expect(screen.getByText(/session was interrupted/i)).toBeInTheDocument(),
    );
  }, 10000);

  it("auto-aborts when the stuck tool is a permission (not just a question)", async () => {
    const qc = makeQueryClient();
    await renderStuck(qc, "sess-perm", stuckHistory(STUCK_PERMISSION));

    armEvidence("sess-perm");

    await waitFor(
      () => expect(abortMock()).toHaveBeenCalledWith("ws-1", "sess-perm"),
      { timeout: 5000 },
    );
    await waitFor(() =>
      expect(screen.getByText(/session was interrupted/i)).toBeInTheDocument(),
    );
  }, 10000);

  it("abort failure still shows the interrupted banner and reconciles history", async () => {
    abortMock().mockRejectedValue(new Error("network error"));
    const qc = makeQueryClient();
    await renderStuck(qc, "sess-abort-fail", stuckHistory(STUCK_QUESTION));

    armEvidence("sess-abort-fail");

    await waitFor(
      () => expect(abortMock()).toHaveBeenCalledWith("ws-1", "sess-abort-fail"),
      { timeout: 5000 },
    );
    // Even when abort fails, the banner must still appear.
    await waitFor(() =>
      expect(screen.getByText(/session was interrupted/i)).toBeInTheDocument(),
    );
  }, 10000);

  it("does NOT abort when the fold carries the pending input (question is live in the projection)", async () => {
    const qc = makeQueryClient();
    await renderStuck(qc, "sess-live-fold", stuckHistory(STUCK_QUESTION));

    // The fold is projection-authoritative: pendingInputs present means
    // the question is still live — the tool is not stuck.
    armEvidence("sess-live-fold", busyFold("sess-live-fold", [{
      id: "q1",
      sessionId: "sess-live-fold",
      rootSessionId: "sess-live-fold",
      kind: InputKind.QUESTION,
      question: "Continue?",
      header: "GitHub auth",
      options: [{ label: "yes", description: "" }],
      multiple: false,
      custom: false,
    }]));

    // Longer than the 1500ms dwell — must not fire.
    await sleep(2500);
    expect(abortMock()).not.toHaveBeenCalled();
    expect(screen.queryByText(/session was interrupted/i)).toBeNull();
  }, 10000);

  it("does NOT abort when a provider prompt is present (pendingPromptCount guard)", async () => {
    promptStore.questions = [{
      id: "q-live",
      session_id: "sess-live-q",
      questions: [{ question: "How to proceed?", header: "GitHub auth", options: [] }],
    }];
    const qc = makeQueryClient();
    await renderStuck(qc, "sess-live-q", stuckHistory(STUCK_QUESTION));

    // Fold says BUSY with no pendingInputs, but the provider store has a
    // live question (delivered outside this view's fold) — no abort.
    armEvidence("sess-live-q");

    await sleep(2500);
    expect(abortMock()).not.toHaveBeenCalled();
    expect(screen.queryByText(/session was interrupted/i)).toBeNull();
  }, 10000);

  it("B — a user send during the dwell resets the anchor: the original deadline passes without an abort", async () => {
    const qc = makeQueryClient();
    await renderStuck(qc, "sess-send", stuckHistory(STUCK_QUESTION));

    // Evidence holds; the dwell anchor is set at t0 with a 1500ms timer.
    armEvidence("sess-send");

    // Well inside the dwell, the user actively sends a message.
    await sleep(1000);
    const textarea = screen.getByRole("textbox");
    await userEvent.type(textarea, "hello there");
    // Re-query: async query resolution can re-render ChatView between the
    // type and the keypress, detaching the earlier node.
    fireEvent.keyDown(screen.getByRole("textbox"), { key: "Enter" });
    await waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());

    // Past the ORIGINAL dwell deadline: doSendNow dropped the abort
    // anchor, so the armed timer was discarded (a fresh dwell would only
    // re-anchor while evidence still holds — active use resets it).
    await sleep(800);
    expect(abortMock()).not.toHaveBeenCalled();

    // The fold then reports the session idle (the turn the user started
    // completes) — evidence broken permanently, anchor never re-arms.
    applySnapshot(2n, [makeSessionSnapshot("sess-send", { status: SessionStatus.IDLE })]);
    sendContractEvent(textPartEnd("p-ev2", "done", "sess-send"));

    await sleep(1700);
    expect(abortMock()).not.toHaveBeenCalled();
    expect(screen.queryByText(/session was interrupted/i)).toBeNull();
  }, 15000);

  it("snapshot stamped BEFORE this page started viewing the session is not evidence (busy→busy session switch)", async () => {
    // N4/e staleness: the snapshot was received while viewing session A
    // (or by an earlier view) — session B's mount time postdates it, so
    // it cannot prove anything about B's pending inputs. B must NOT be
    // aborted even though B's history shows a stuck tool and the fold
    // says BUSY.
    const healthyA = [
      { id: "a1", role: "user", parts: [{ type: "text", text: "hello a" }] },
    ];
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockImplementation(
      async (_ws: string, sid: string) => ({
        messages: sid === "sess-b" ? stuckHistory(STUCK_QUESTION) : healthyA,
        nextCursor: undefined,
      }),
    );
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockImplementation(
      async (_ws: string, sid: string) => (sid === "sess-b" ? stuckHistory(STUCK_QUESTION) : healthyA),
    );

    function SwitchButton() {
      const navigate = useNavigate();
      return <button data-testid="switch-btn" onClick={() => navigate("/chat/ws-1/sess-b")}>switch</button>;
    }

    const qc = makeQueryClient();
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={["/chat/ws-1/sess-a"]}>
          <TooltipProvider delayDuration={0}>
            <SwitchButton />
            <Routes>
              <Route path="/chat/:workspaceId/:sessionId" element={<ChatPage />} />
            </Routes>
          </TooltipProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    await waitFor(() => expect(capturedContractOptions).not.toBeNull());
    await waitFor(() => expect(getRenderedMessages()).toHaveLength(1));

    // Snapshot (with B BUSY in the fold) stamped while still viewing A.
    applySnapshot(1n, [busyFold("sess-b")]);

    // Navigate within the SPA to busy session B — its mount stamp now
    // postdates the snapshot.
    act(() => { screen.getByTestId("switch-btn").click(); });

    // B's stuck history loads; everything holds EXCEPT snapshot freshness.
    await waitFor(
      () => expect(getRenderedMessages().some((m) => m.parts.some((p) => p.type === "tool_use"))).toBe(true),
      { timeout: 5000 },
    );

    await sleep(2500);
    expect(abortMock()).not.toHaveBeenCalled();
    expect(screen.queryByText(/session was interrupted/i)).toBeNull();
  }, 15000);

  it("R1 — busy→busy switch with a FRESH post-switch snapshot still aborts the newly-viewed stuck session", async () => {
    // The staleness guard must not permanently disarm recovery: after
    // switching to B, a snapshot stamped after the switch is valid
    // evidence and the abort fires for B.
    const healthyA = [
      { id: "a1", role: "user", parts: [{ type: "text", text: "hello a" }] },
    ];
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockImplementation(
      async (_ws: string, sid: string) => ({
        messages: sid === "sess-b2" ? stuckHistory(STUCK_PERMISSION) : healthyA,
        nextCursor: undefined,
      }),
    );
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockImplementation(
      async (_ws: string, sid: string) => (sid === "sess-b2" ? stuckHistory(STUCK_PERMISSION) : healthyA),
    );

    function SwitchButton() {
      const navigate = useNavigate();
      return <button data-testid="switch-btn" onClick={() => navigate("/chat/ws-1/sess-b2")}>switch</button>;
    }

    const qc = makeQueryClient();
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={["/chat/ws-1/sess-a"]}>
          <TooltipProvider delayDuration={0}>
            <SwitchButton />
            <Routes>
              <Route path="/chat/:workspaceId/:sessionId" element={<ChatPage />} />
            </Routes>
          </TooltipProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    await waitFor(() => expect(capturedContractOptions).not.toBeNull());
    await waitFor(() => expect(getRenderedMessages()).toHaveLength(1));

    act(() => { screen.getByTestId("switch-btn").click(); });

    // B's stuck history loads while the fold is still empty — no abort.
    await waitFor(
      () => expect(getRenderedMessages().some((m) => m.parts.some((p) => p.type === "tool_use"))).toBe(true),
      { timeout: 5000 },
    );

    // A FRESH snapshot arrives after the switch — valid evidence for B.
    applySnapshot(1n, [busyFold("sess-b2")]);
    sendContractEvent(textPartEnd("p-ev", "streaming", "sess-b2"));

    await waitFor(
      () => expect(abortMock()).toHaveBeenCalledWith("ws-1", "sess-b2"),
      { timeout: 5000 },
    );
    await waitFor(() =>
      expect(screen.getByText(/session was interrupted/i)).toBeInTheDocument(),
    );
  }, 15000);
});

describe("Contract-stream reconnect reconciliation", () => {
  beforeEach(() => {
    capturedContractOptions = null;
    contractState = { seq: 0n, sessions: new Map() };
    promptStore.questions = [];
    vi.clearAllMocks();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockImplementation(async () => {
      const msgs = await (messagesApi.getHistory as ReturnType<typeof vi.fn>)();
      return { messages: msgs, nextCursor: undefined };
    });
  });

  it("contract-stream reconnect refreshes the queue and refetches message history (issue 440 — transcript resync)", async () => {
    const qc = makeQueryClient();
    renderChat(qc, "/chat/ws-1/sess-1");
    await waitFor(() => expect(capturedContractOptions).not.toBeNull());
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());

    const historyBefore = (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mock.calls.length;
    const queueBefore = (messagesApi.getQueue as ReturnType<typeof vi.fn>).mock.calls.length;

    act(() => { capturedContractOptions?.onReconnect(); });

    // A stream gap may have missed persisted messages — the reconnect
    // path must resync the transcript and the queue.
    await waitFor(() => {
      expect((messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mock.calls.length).toBeGreaterThan(historyBefore);
      expect((messagesApi.getQueue as ReturnType<typeof vi.fn>).mock.calls.length).toBeGreaterThan(queueBefore);
    });
  });
});
