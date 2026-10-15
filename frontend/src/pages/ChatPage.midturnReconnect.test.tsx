// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// RED-FIRST PIN (live-update reliability): a contract-stream reconnect
// MID-TURN must not destroy the live streaming buffer.
//
// The owner-observed repro this pins: while the agent is streaming a
// response, a reconnect happens (35s read timeout past a silent tool
// call behind a proxy, a broker resync, a laptop sleep) — and from that
// moment the response renders partial/garbled: the beginning is gone,
// later text arrives unformatted, and only the END-OF-TURN history
// reload restores the full message.
//
// Mechanism under test: ChatPage's contract-stream onReconnect handler
// calls reconcileOnIdle() UNCONDITIONALLY — including mid-turn.
// reconcileOnIdle refetches history and, whenever it is nonempty
// (always, for a session with any past), executes setSseStreamParts([]).
// The in-flight response's earlier parts live ONLY in that buffer (the
// server history does not contain the not-yet-finished message), so the
// wipe destroys them; subsequent PART_DELTAs then re-materialize as
// bare text parts with no container metadata (unformatted), and the
// transcript stays broken until the true end-of-turn reconcile.
//
// The pin's desired semantics: while the viewed session's fold says
// BUSY (mid-turn), a reconnect may refetch history (catching missed
// MESSAGE_ENDs is good) but must NOT clear the streaming buffer. The
// buffer still clears at the REAL end of turn (SESSION_STATUS IDLE →
// reconcileOnIdle) — pinned here as the complementary guard so the fix
// cannot simply stop reconciling.
//
// RED against current code: the mid-turn reconnect wipes the buffer
// (assertion on the streamed text fails after onReconnect).

import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, waitFor, act, screen } from "@testing-library/react";
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
import { SessionSnapshotSchema } from "../abi/llmsafespaces/abi/v1/abi_pb";

// --- Mocks (ChatPage.reconnect.test.tsx reference pattern) ---

const promptStore = vi.hoisted(() => ({ questions: [] as Array<Record<string, unknown>> }));

vi.mock("../providers/SessionActivityProvider", () => ({
  useIsSessionDeleted: () => false,
  useClearPendingUnread: () => () => {},
  useClearSessionBusy: () => () => {},
  useIsSessionBusy: () => false,
  useIsSessionUnread: () => false,
  useWorkspaceBusyCount: () => 0,
  useWorkspaceHung: () => false,
  useIsSessionPendingAction: () => false,
  useSessionPendingActions: () => new Set<string>(),
  useAddPendingAction: () => () => {},
  useRemovePendingAction: () => () => {},
  useDropPendingAction: () => () => {},
  useAddPendingQuestion: () => () => {},
  useAddPendingPermission: () => () => {},
  usePendingQuestionsForSession: () => promptStore.questions,
  usePendingPermissionsForSession: () => [],
  useClearSessionPendingPrompts: () => () => {},
  useWorkspaceInputSnapshot: () => undefined,
  SessionActivityProvider: ({ children }: { children: any }) => <>{children}</>,
  useWhileAwayStalenessSweep: () => {},
}));

vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getStatus: vi.fn().mockResolvedValue({ phase: "Active" }),
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
  const gh = vi.fn();
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

vi.mock("../hooks/useEventStream", () => ({
  useEventStream: vi.fn(),
}));

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

import { messagesApi } from "../api/messages";

// --- Helpers ---

function makeQueryClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 0 } } });
}

function renderChat(qc: QueryClient) {
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/chat/ws-1/sess-1"]}>
        <TooltipProvider delayDuration={0}>
          <Routes>
            <Route path="/chat/:workspaceId/:sessionId" element={<ChatPage />} />
          </Routes>
        </TooltipProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function sendContractEvent(evt: Event, seq: bigint) {
  act(() => { capturedContractOptions?.onEvent(evt, seq); });
}

function partDelta(partId: string, delta: string): Event {
  return create(EventSchema, {
    sessionId: "sess-1",
    type: EventType.PART_DELTA,
    partId,
    delta,
  } as Parameters<typeof create>[1]);
}

function sessionStatusEvent(status: SessionStatus): Event {
  return create(EventSchema, {
    sessionId: "sess-1",
    type: EventType.SESSION_STATUS,
    status,
  } as Parameters<typeof create>[1]);
}

function foldSnapshot(sessionId: string, status: SessionStatus): SessionSnapshot {
  return create(SessionSnapshotSchema, { sessionId, status } as Parameters<typeof create>[1]);
}

function getStreamPartsText(): string {
  const el = screen.getByTestId("chat-view");
  const parts = JSON.parse(el.getAttribute("data-stream-parts") || "[]") as Array<{ text?: string }>;
  return parts.map((p) => p.text ?? "").join("");
}

// Nonempty history (any session with a past): the precondition for
// reconcileOnIdle's destructive branch.
const ONE_MESSAGE_HISTORY = [
  { id: "msg-user-1", role: "user", parts: [{ type: "text", text: "previous turn" }] },
];

// --- Tests ---

describe("Contract-stream reconnect mid-turn must not wipe the streaming buffer", () => {
  beforeEach(() => {
    capturedContractOptions = null;
    contractState = { seq: 0n, sessions: new Map() };
    promptStore.questions = [];
    vi.clearAllMocks();
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([...ONE_MESSAGE_HISTORY]);
  });

  it("keeps streamed text across a reconnect while the fold says BUSY (mid-turn)", async () => {
    // Mid-turn: the fold reports the session busy.
    contractState = { seq: 1n, sessions: new Map([["sess-1", foldSnapshot("sess-1", SessionStatus.BUSY)]]) };

    const qc = makeQueryClient();
    renderChat(qc);
    await waitFor(() => expect(capturedContractOptions).not.toBeNull());
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());
    await waitFor(() =>
      expect((messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mock.calls.length).toBeGreaterThan(0),
    );

    // The agent streams: two deltas build the live buffer.
    sendContractEvent(partDelta("p-1", "Hello "), 1n);
    sendContractEvent(partDelta("p-1", "world — the beginning of the answer"), 2n);
    await waitFor(() => expect(getStreamPartsText()).toContain("Hello world — the beginning of the answer"));

    // The stream reconnects MID-TURN (read timeout / resync / sleep).
    const historyCalls = (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mock.calls.length;
    act(() => { capturedContractOptions?.onReconnect(); });

    // The reconnect's history refetch lands (reconcile ran — the
    // destructive branch's precondition is satisfied).
    await waitFor(() =>
      expect((messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mock.calls.length).toBeGreaterThan(historyCalls),
    );

    // Desired: the live streaming buffer SURVIVES the mid-turn
    // reconnect — the in-flight message exists nowhere else.
    expect(getStreamPartsText()).toContain("Hello world — the beginning of the answer");
  }, 10000);

  it("guard: the buffer still clears at the REAL end of turn (SESSION_STATUS IDLE)", async () => {
    contractState = { seq: 1n, sessions: new Map([["sess-1", foldSnapshot("sess-1", SessionStatus.BUSY)]]) };

    const qc = makeQueryClient();
    renderChat(qc);
    await waitFor(() => expect(capturedContractOptions).not.toBeNull());
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());

    sendContractEvent(partDelta("p-1", "streamed text"), 1n);
    await waitFor(() => expect(getStreamPartsText()).toContain("streamed text"));

    // The turn genuinely ends: fold flips IDLE and the IDLE event
    // arrives — the end-of-turn reconcile MUST clear the buffer (the
    // authoritative history replaces it).
    contractState = { seq: 2n, sessions: new Map([["sess-1", foldSnapshot("sess-1", SessionStatus.IDLE)]]) };
    sendContractEvent(sessionStatusEvent(SessionStatus.IDLE), 3n);

    await waitFor(() => expect(getStreamPartsText()).toBe(""));
  }, 10000);
});
