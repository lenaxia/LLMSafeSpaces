// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// RED-FIRST PINS (live-update reliability): the contract stream's
// SESSION_STATUS IDLE must reach the busy-indicator state, and a turn
// that has visibly ended must not gate the next send into the queue.
//
// The owner-observed repro this pins: a turn finishes — the transcript
// reconciles (the contract stream's SESSION_STATUS IDLE drives
// reconcileOnIdle — ChatPage demonstrably receives it) — but the busy
// badge stays on until the user hits stop, and messages sent meanwhile
// vanish into the queue instead of the transcript.
//
// Root cause under test: the provider's busySessions map is cleared
// only by the USER stream's session.status=idle frame. ChatPage's
// contract-stream SESSION_STATUS IDLE handler calls
// notifySessionIdle/clearStreamTimedOut/reconcileOnIdle/refreshQueue/
// clearSessionPendingPrompts — NONE of which clear the provider's busy
// map (and REST refetches cannot: seedBusy skips already-seeded
// workspaces by design). One stream demonstrably knows the turn ended;
// the other one's badge never finds out.
//
// Harness: the REAL SessionActivityProvider (no provider mock) with
// useUserEventStream mocked to CAPTURE the provider's stream callbacks
// (so the user stream is controllable), useContractStream mocked to
// capture the contract callbacks (the ChatPage reference pattern).
//
// Both pins are RED against current code:
//   1. bridge pin: contract idle arrives → busy must clear. Today it
//      stays latched (the missing bridge).
//   2. compound pin: after contract idle (+ history reconcile), a new
//      send must go DIRECT (sendAsync + optimistic bubble). Today the
//      stale busy holds the send-gate (ChatPage handleSend:
//      isSessionBusy || streaming → queue.enqueue) — the message is
//      invisible in the transcript until some later flush.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, waitFor, act, screen, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ChatPage } from "./ChatPage";
import { SessionActivityProvider, useIsSessionBusy } from "../providers/SessionActivityProvider";
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

// --- Mocks ---

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

// Platform stream — mocked inert (platform events only post-cutover).
vi.mock("../hooks/useEventStream", () => ({
  useEventStream: vi.fn(),
}));

// User stream — the REAL provider subscribes through this mock; the
// captured options let the test drive user-stream events into the
// provider exactly as the live socket would.
let capturedUserStreamOptions: {
  onEvent?: (event: unknown) => void;
  onReconnect?: () => void;
} | null = null;
vi.mock("../hooks/useUserEventStream", () => ({
  useUserEventStream: vi.fn((opts: Record<string, unknown>) => {
    capturedUserStreamOptions = opts as typeof capturedUserStreamOptions;
  }),
}));

// Contract stream — captured options + controllable fold state
// (ChatPage reference pattern from ChatPage.reconnect.test.tsx).
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

function BusyProbe() {
  const busy = useIsSessionBusy("sess-1");
  return <span data-testid="busy-probe">{busy ? "yes" : "no"}</span>;
}

function renderChat(qc: QueryClient) {
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/chat/ws-1/sess-1"]}>
        <SessionActivityProvider>
          <TooltipProvider delayDuration={0}>
            <BusyProbe />
            <Routes>
              <Route path="/chat/:workspaceId/:sessionId" element={<ChatPage />} />
            </Routes>
          </TooltipProvider>
        </SessionActivityProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

/** The user stream delivers the busy transition for sess-1 (the frame
 * that latches the badge — exactly what the live socket does). */
function deliverUserStreamBusy() {
  act(() => {
    capturedUserStreamOptions?.onEvent?.({
      type: "session.status",
      workspace_id: "ws-1",
      session_id: "sess-1",
      status: "busy",
    });
  });
}

/** The contract stream delivers SESSION_STATUS IDLE for sess-1 (the
 * frame that demonstrably reaches ChatPage and drives the end-of-turn
 * history reconcile). */
function deliverContractIdle() {
  const evt = create(EventSchema, {
    sessionId: "sess-1",
    type: EventType.SESSION_STATUS,
    status: SessionStatus.IDLE,
  } as Parameters<typeof create>[1]);
  act(() => { capturedContractOptions?.onEvent(evt, 1n); });
}

function getRenderedMessages(): Array<{ id: string; role: string; parts: Array<{ type: string; text?: string }> }> {
  const el = screen.getByTestId("chat-view");
  return JSON.parse(el.getAttribute("data-messages") || "[]");
}

// --- Tests ---

describe("Contract-stream SESSION_STATUS IDLE must clear the busy badge (bridge pin)", () => {
  beforeEach(() => {
    capturedUserStreamOptions = null;
    capturedContractOptions = null;
    contractState = {
      seq: 1n,
      sessions: new Map([
        [ "sess-1", create(SessionSnapshotSchema, { sessionId: "sess-1", status: SessionStatus.BUSY } as Parameters<typeof create>[1]) ],
      ]),
    };
    vi.clearAllMocks();
  });

  it("user-stream busy latches, then contract idle (turn visibly ended) must clear it", async () => {
    const qc = makeQueryClient();
    renderChat(qc);
    await waitFor(() => expect(capturedContractOptions).not.toBeNull());
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());

    // Baseline: not busy.
    expect(screen.getByTestId("busy-probe").textContent).toBe("no");

    // The turn starts (user stream busy frame) — badge latches.
    deliverUserStreamBusy();
    expect(screen.getByTestId("busy-probe").textContent).toBe("yes");

    // The turn ends. The contract stream delivers IDLE — ChatPage
    // receives it (this very handler drives reconcileOnIdle), but the
    // user stream's idle frame was lost (broker drop / reconnect gap —
    // server-verified loss paths). The badge must still clear: one
    // stream reporting the turn's end is enough.
    deliverContractIdle();

    // Reconcile microtasks land (the IDLE handler also refetches
    // history — the observable "turn ended" signal).
    await waitFor(() => expect(screen.getByTestId("busy-probe").textContent).toBe("no"), {
      timeout: 3000,
    });
  }, 10000);

  it("compound: after contract idle + history reconcile, a new send goes DIRECT (not queue-held)", async () => {
    const qc = makeQueryClient();
    renderChat(qc);
    await waitFor(() => expect(capturedContractOptions).not.toBeNull());
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());

    deliverUserStreamBusy();
    expect(screen.getByTestId("busy-probe").textContent).toBe("yes");

    deliverContractIdle();
    // History reconcile is the visible end-of-turn signal; give the
    // refetch a beat to land even under the current (stuck) badge.
    await waitFor(
      () => expect((messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mock.calls.length).toBeGreaterThan(0),
      { timeout: 3000 },
    );

    // The user sends the next message. The turn has visibly ended —
    // this must be a DIRECT send with an optimistic transcript bubble,
    // not a queue hold (queue renders as a pill, invisible in the
    // transcript until a much later flush).
    const textarea = screen.getByRole("textbox");
    await userEvent.type(textarea, "second message");
    fireEvent.keyDown(screen.getByRole("textbox"), { key: "Enter" });

    await waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled(), { timeout: 3000 });
    expect(messagesApi.queueMessage).not.toHaveBeenCalled();
    await waitFor(
      () =>
        expect(
          getRenderedMessages().some((m) =>
            m.parts.some((p) => p.text === "second message"),
          ),
        ).toBe(true),
      { timeout: 3000 },
    );
  }, 15000);
});
