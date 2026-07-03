/**
 * Tests for ChatPage's SSE event handler (handleSSEEvent).
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, waitFor, act, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";
import type { WorkspaceStreamEvent } from "../api/types";

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
  useIsSessionPendingAction: () => false,
  useSessionPendingActions: () => new Set<string>(),
  useAddPendingAction: () => () => {},
  useRemovePendingAction: () => () => {},
  useAddPendingQuestion: () => () => {},
  useAddPendingPermission: () => () => {},
  usePendingQuestionsForSession: () => [],
  usePendingPermissionsForSession: () => [],
  useClearSessionPendingPrompts: () => () => {},
  SessionActivityProvider: ({ children }: { children: any }) => <>{children}</>,
}));
vi.mock("../api/messages", () => {
  const gh = vi.fn().mockResolvedValue([]);
  return { messagesApi: { getHistory: gh, getHistoryPage: vi.fn().mockImplementation(async () => { const msgs = await gh(); return { messages: msgs, nextCursor: undefined }; }), sendAsync: vi.fn(), queueMessage: vi.fn().mockResolvedValue({ messageID: "msg_q_mock" }), getQueue: vi.fn().mockResolvedValue({ messages: [] }), deleteQueueMessage: vi.fn().mockResolvedValue(undefined) } };
});
vi.mock("../api/sessions", () => ({ sessionsApi: { create: vi.fn() } }));

// Capture the SSE handler ChatPage registers with useEventStream
let capturedSSEHandler: ((data: unknown) => void) | null = null;
vi.mock("../hooks/useEventStream", () => ({
  useEventStream: vi.fn((_workspaceId: string | undefined, handler: (data: unknown) => void) => {
    capturedSSEHandler = handler;
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

// renderChatNavigable renders the same tree as renderChat but also exposes a
// captured navigate() so a test can drive a real in-app route change against the
// SAME mounted ChatPage instance — required to exercise [sessionId] effects.
const navigateRef: { current: ((to: string) => void) | null } = { current: null };
function NavigateCapturer() {
  navigateRef.current = useNavigate();
  return null;
}
function renderChatNavigable(qc: QueryClient, path: string) {
  navigateRef.current = null;
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[path]}>
        <NavigateCapturer />
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

function sendSSEEvent(event: WorkspaceStreamEvent) {
  act(() => { capturedSSEHandler?.(event); });
}

function getStreamParts(): Array<{ type: string; text: string }> {
  const el = screen.getByTestId("chat-view");
  return JSON.parse(el.getAttribute("data-stream-parts") || "[]");
}

function makePartUpdatedEvent(sessionID: string, partType: string, text: string): WorkspaceStreamEvent {
  return {
    type: "opencode.event",
    event_type: "message.part.updated",
    data: {
      type: "message.part.updated",
      properties: { sessionID, part: { type: partType, text } },
    },
  } as unknown as WorkspaceStreamEvent;
}

function makePartDeltaEvent(sessionID: string, field: string, delta: string): WorkspaceStreamEvent {
  return {
    type: "opencode.event",
    event_type: "message.part.delta",
    data: {
      type: "message.part.delta",
      properties: { sessionID, field, delta },
    },
  } as unknown as WorkspaceStreamEvent;
}

function makePartUpdatedEventSnakeCase(session_id: string, text: string): WorkspaceStreamEvent {
  return {
    type: "opencode.event",
    event_type: "message.part.updated",
    data: {
      type: "message.part.updated",
      properties: { session_id, part: { type: "text", text } },
    },
  } as unknown as WorkspaceStreamEvent;
}

function makeSessionStatusEvent(session_id: string, status: "idle" | "busy"): WorkspaceStreamEvent {
  return { type: "session.status", session_id, status };
}

// --- Tests ---

describe("ChatPage SSE event handler", () => {
  beforeEach(() => {
    capturedSSEHandler = null;
    vi.clearAllMocks();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } });
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
  });

  describe("workspace.phase events (Epic 28: handled by useUserEventStream, not session stream)", () => {
    it("does NOT invalidate any queries — phase events handled by user stream", async () => {
      const qc = makeQueryClient();
      const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent({ type: "workspace.phase", phase: "Active" });
      // After Epic 28 hard cutover, session stream no longer handles workspace.phase
      const phaseCalls = invalidateSpy.mock.calls.filter((args) => {
        const key = (args[0] as { queryKey?: unknown })?.queryKey;
        return Array.isArray(key) && (key[0] === "workspace-status" || key[0] === "workspaces");
      });
      expect(phaseCalls).toHaveLength(0);
    });
  });

  describe("session.status events", () => {
    it("invalidates sessions query", async () => {
      const qc = makeQueryClient();
      const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent(makeSessionStatusEvent("sess-1", "idle"));
      expect(invalidateSpy).toHaveBeenCalledWith(expect.objectContaining({ queryKey: ["sessions", "ws-1"] }));
    });

    it("does NOT invalidate workspace-status query", async () => {
      const qc = makeQueryClient();
      const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent(makeSessionStatusEvent("sess-1", "busy"));
      const wsCalls = invalidateSpy.mock.calls.filter((args) => {
        const key = (args[0] as { queryKey?: unknown })?.queryKey;
        return Array.isArray(key) && key[0] === "workspace-status";
      });
      expect(wsCalls).toHaveLength(0);
    });

    it("REGRESSION: idle event triggers reconcile that does NOT cause duplicate localMessage rendering", async () => {
      // Prior bug: localMessages accumulated user+assistant messages on send,
      // and reconcileOnIdle refetched history (which now contained the same
      // messages), but localMessages was never cleared. The merge in
      // `allMessages = [...history, ...localMessages]` rendered every
      // message twice.
      //
      // Fix: clearing localMessages after the post-idle history refetch
      // succeeds. History is the single source of truth once idle.
      const user = userEvent.setup();
      const qc = makeQueryClient();

      // sendAsync resolves immediately; history starts empty then returns
      // the persisted message after idle reconcile triggers a refetch.
      let historyCallCount = 0;
      (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockImplementation(() => {
        historyCallCount++;
        if (historyCallCount === 1) return Promise.resolve([]);
        return Promise.resolve([
          { id: "msg-user-real", role: "user", parts: [{ type: "text", text: "ping" }] },
          { id: "msg-asst-real", role: "assistant", parts: [{ type: "text", text: "pong" }] },
        ]);
      });
      (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);

      const { container } = renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      await waitFor(() => expect(container.querySelector("textarea")).not.toBeDisabled());

      // User sends a message
      await user.click(container.querySelector("textarea")!);
      await user.type(container.querySelector("textarea")!, "ping");
      await user.keyboard("{Enter}");
      await waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());

      // Drive the idle SSE event — this triggers reconcileOnIdle which
      // refetches history and SHOULD clear localMessages so the merged
      // view (history + localMessages) does not duplicate.
      sendSSEEvent(makeSessionStatusEvent("sess-1", "idle"));

      // Wait for the reconcile refetch to land
      await waitFor(() => expect(historyCallCount).toBeGreaterThanOrEqual(2));

      // Wait for the merged messages render to update with deduped content.
      // Read the actual rendered messages from the ChatView mock's data attr.
      await waitFor(() => {
        const view = container.querySelector('[data-testid="chat-view"]');
        const messagesAttr = view?.getAttribute("data-messages") ?? "[]";
        const messages = JSON.parse(messagesAttr) as Array<{ id: string; role: string; parts: Array<{ text?: string }> }>;
        // EXACTLY 2 messages — no duplicates from localMessages+history merge
        expect(messages).toHaveLength(2);
        expect(messages.filter((m) => m.role === "user")).toHaveLength(1);
        expect(messages.filter((m) => m.role === "assistant")).toHaveLength(1);
      }, { timeout: 5_000 });
    });

    it("REGRESSION: assistant response is not duplicated when reconcileOnIdle's history fetch resolves BEFORE useChatStream's onComplete", async () => {
      // Validated against production via DevTools Network panel:
      // After session.status idle, two GET /message requests fire — one
      // from useChatStream.send (line 70 of useChatStream.ts) and one
      // from reconcileOnIdle's queryClient.refetchQueries.
      //
      // Race: if reconcileOnIdle's fetch resolves first, it clears
      // localMessages and populates history. Then useChatStream.send's
      // onComplete callback fires and re-adds the assistant message to
      // localMessages → assistant renders TWICE (history + localMessages).
      //
      // Fix: handleSend's onComplete must NOT add the assistant message
      // to localMessages. The streaming bubble shows it during streaming;
      // history (refetched by reconcileOnIdle) is authoritative after.
      const user = userEvent.setup();
      const qc = makeQueryClient();

      // history fetch resolution order:
      //   call 1 (initial mount) → empty
      //   call 2 (reconcileOnIdle's refetch) → [user, assistant]
      //   call 3 (useChatStream.send's await) → [user, assistant]
      // The race is the order of resolution between calls 2 and 3.
      //
      // Simulate the production race: reconcileOnIdle's fetch resolves
      // first (e.g., its Promise hits microtask queue earlier), then
      // useChatStream.send's fetch resolves second. We deliberately order
      // resolutions to expose the bug.
      let resolveCall3!: (history: unknown[]) => void;
      let historyCallCount = 0;
      (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockImplementation(() => {
        historyCallCount++;
        if (historyCallCount === 1) return Promise.resolve([]);
        if (historyCallCount === 2) {
          // reconcileOnIdle's refetch — resolve immediately
          return Promise.resolve([
            { id: "msg-user-real", role: "user", parts: [{ type: "text", text: "ping" }] },
            { id: "msg-asst-real", role: "assistant", parts: [{ type: "text", text: "pong" }] },
          ]);
        }
        // call 3: useChatStream.send's history fetch — defer resolution
        return new Promise<unknown[]>((res) => { resolveCall3 = res; });
      });
      (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);

      const { container } = renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      await waitFor(() => expect(container.querySelector("textarea")).not.toBeDisabled());

      await user.click(container.querySelector("textarea")!);
      await user.type(container.querySelector("textarea")!, "ping");
      await user.keyboard("{Enter}");
      await waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());

      // Idle SSE — drives BOTH paths concurrently:
      //   1. notifySessionIdle → useChatStream.send's await resolves → call 3 begins
      //   2. reconcileOnIdle → call 2 fires
      sendSSEEvent(makeSessionStatusEvent("sess-1", "idle"));

      // Wait for reconcileOnIdle's refetch (call 2) to land and update history
      await waitFor(() => expect(historyCallCount).toBeGreaterThanOrEqual(3));

      // Now resolve useChatStream.send's history fetch (call 3) AFTER
      // reconcileOnIdle has cleared localMessages. This causes onComplete
      // to fire and re-add the assistant message to the just-cleared
      // localMessages — exactly the production race.
      await act(async () => {
        resolveCall3([
          { id: "msg-user-real", role: "user", parts: [{ type: "text", text: "ping" }] },
          { id: "msg-asst-real", role: "assistant", parts: [{ type: "text", text: "pong" }] },
        ]);
        await new Promise((r) => setTimeout(r, 50));
      });

      // Critical assertion: assistant renders EXACTLY ONCE despite the race
      const view = container.querySelector('[data-testid="chat-view"]');
      const messagesAttr = view?.getAttribute("data-messages") ?? "[]";
      const messages = JSON.parse(messagesAttr) as Array<{ id: string; role: string; parts: Array<{ text?: string }> }>;
      expect(messages.filter((m) => m.role === "assistant")).toHaveLength(1);
      expect(messages.filter((m) => m.role === "user")).toHaveLength(1);
    });
  });

  describe("opencode.event with message.part.updated", () => {
    it("text part with matching session creates a text entry", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", "Hello streaming!"));
      await waitFor(() => {
        const parts = getStreamParts();
        expect(parts).toHaveLength(1);
        expect(parts[0]).toEqual({ type: "text", text: "Hello streaming!" });
      });
    });

    it("text part with snake_case session_id works", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent(makePartUpdatedEventSnakeCase("sess-1", "snake case works"));
      await waitFor(() => {
        const parts = getStreamParts();
        expect(parts[0]).toEqual({ type: "text", text: "snake case works" });
      });
    });

    it("ignores event with wrong session ID", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent(makePartUpdatedEvent("other-session", "text", "Should not appear"));
      await waitFor(() => {
        expect(getStreamParts()).toHaveLength(0);
      });
    });

    it("last text snapshot overwrites previous text in same part", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", "First"));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", "Final text"));
      await waitFor(() => {
        const parts = getStreamParts();
        // Second text part.updated with content updates the existing text part
        expect(parts[parts.length - 1]!.text).toBe("Final text");
      });
    });
  });

  describe("opencode.event with message.part.delta", () => {
    it("accumulates text deltas incrementally", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "Hello"));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", " world"));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "!"));
      await waitFor(() => {
        expect(getStreamParts()[0]!.text).toBe("Hello world!");
      });
    });

    it("discards deltas without preceding part.updated", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "orphan"));
      await waitFor(() => {
        expect(getStreamParts()).toHaveLength(0);
      });
    });

    it("ignores delta with wrong session ID", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", ""));
      sendSSEEvent(makePartDeltaEvent("other-session", "text", "should be ignored"));
      await waitFor(() => {
        expect(getStreamParts()[0]!.text).toBe("");
      });
    });
  });

  describe("opencode.event edge cases", () => {
    it("ignores event with missing payload", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent({ type: "opencode.event", event_type: "message.part.updated", data: { wrong: "structure" } } as unknown as WorkspaceStreamEvent);
      await waitFor(() => {
        expect(getStreamParts()).toHaveLength(0);
      });
    });

    it("ignores event with missing properties", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent({ type: "opencode.event", event_type: "message.part.updated", data: { payload: { type: "message.part.updated" } } } as unknown as WorkspaceStreamEvent);
      await waitFor(() => {
        expect(getStreamParts()).toHaveLength(0);
      });
    });

    it("ignores event with null data", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent({ type: "opencode.event", event_type: "message.part.updated", data: null } as unknown as WorkspaceStreamEvent);
      await waitFor(() => {
        expect(getStreamParts()).toHaveLength(0);
      });
    });
  });

  describe("nested SSE format unwrapping", () => {
    it("unwraps nested payload and processes message.part.updated", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent({
        type: "opencode.event",
        event_type: "message.part.updated",
        data: {
          directory: "ws-1",
          payload: {
            type: "message.part.updated",
            properties: { sessionID: "sess-1", part: { type: "text", text: "Nested format works!" } },
          },
        },
      } as unknown as WorkspaceStreamEvent);
      await waitFor(() => {
        expect(getStreamParts()[0]!.text).toBe("Nested format works!");
      });
    });

    it("unwraps nested payload and processes message.part.delta", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      // Activate text routing first
      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", ""));
      sendSSEEvent({
        type: "opencode.event",
        event_type: "message.part.delta",
        data: {
          directory: "ws-1",
          payload: {
            type: "message.part.delta",
            properties: { sessionID: "sess-1", field: "text", delta: "nested delta" },
          },
        },
      } as unknown as WorkspaceStreamEvent);
      await waitFor(() => {
        expect(getStreamParts()[0]!.text).toBe("nested delta");
      });
    });
  });

  describe("user echo filtering — sent-text tracking", () => {
    it("strips exact user echo from message.part.updated snapshot", async () => {
      const user = userEvent.setup();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);

      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      // Send a message to populate sentTextRef
      await waitFor(() => expect(document.querySelector("textarea")).not.toBeNull());
      await user.click(document.querySelector("textarea")!);
      await user.type(document.querySelector("textarea")!, "my question");
      await user.keyboard("{Enter}");

      // Simulate opencode echoing the user's message back as a part.updated
      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", "my question"));
      await waitFor(() => {
        expect(getStreamParts()).toHaveLength(0);
      });

      // Now the real assistant response arrives — should be accepted
      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", "Here is the answer!"));
      await waitFor(() => {
        expect(getStreamParts().find(p => p.type === "text")?.text).toBe("Here is the answer!");
      });
    });

    it("strips user echo prefix from message.part.updated snapshot", async () => {
      const user = userEvent.setup();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);

      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      await waitFor(() => expect(document.querySelector("textarea")).not.toBeNull());
      await user.click(document.querySelector("textarea")!);
      await user.type(document.querySelector("textarea")!, "hello");
      await user.keyboard("{Enter}");

      // Opencode echoes user text + assistant response in one snapshot
      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", "helloThe answer is 42"));
      await waitFor(() => {
        expect(getStreamParts().find(p => p.type === "text")?.text).toBe("The answer is 42");
      });
    });

    it("strips user echo prefix from accumulated deltas", async () => {
      const user = userEvent.setup();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);

      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      await waitFor(() => expect(document.querySelector("textarea")).not.toBeNull());
      await user.click(document.querySelector("textarea")!);
      await user.type(document.querySelector("textarea")!, "hi");
      await user.keyboard("{Enter}");

      // User echo arrives as part.updated — suppresses subsequent deltas
      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", "hi"));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "echo junk"));

      // Then reasoning starts, routing switches
      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "thinking"));

      // Then text response starts
      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "response text"));

      await waitFor(() => {
        expect(getStreamParts().find(p => p.type === "text")?.text).toBe("response text");
      });
    });
  });

  describe("thinking/reasoning streaming (Bug 2)", () => {
    it("accumulates thinking deltas with field=reasoning", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      // A reasoning part.updated must precede deltas to activate thinking routing
      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "reasoning", "Hmm "));
      sendSSEEvent(makePartDeltaEvent("sess-1", "reasoning", "let me think"));
      await waitFor(() => {
        expect(getStreamParts().find(p => p.type === "thinking")?.text).toBe("Hmm let me think");
      });
    });

    it("accumulates thinking deltas with field=thinking", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent(makePartUpdatedEvent("sess-1", "thinking", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "thinking", "I wonder..."));
      await waitFor(() => {
        expect(getStreamParts().find(p => p.type === "thinking")?.text).toBe("I wonder...");
      });
    });

    it("captures thinking part from message.part.updated", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent({
        type: "opencode.event",
        event_type: "message.part.updated",
        data: {
          type: "message.part.updated",
          properties: { sessionID: "sess-1", part: { type: "thinking", text: "Deep thoughts" } },
        },
      } as unknown as WorkspaceStreamEvent);
      await waitFor(() => {
        expect(getStreamParts().find(p => p.type === "thinking")?.text).toBe("Deep thoughts");
      });
    });

    it("captures reasoning part from message.part.updated", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent({
        type: "opencode.event",
        event_type: "message.part.updated",
        data: {
          type: "message.part.updated",
          properties: { sessionID: "sess-1", part: { type: "reasoning", text: "Chain of thought" } },
        },
      } as unknown as WorkspaceStreamEvent);
      await waitFor(() => {
        expect(getStreamParts().find(p => p.type === "thinking")?.text).toBe("Chain of thought");
      });
    });

    it("handleSend clears thinking text", async () => {
      const user = userEvent.setup();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);

      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent({
        type: "opencode.event",
        event_type: "message.part.updated",
        data: {
          type: "message.part.updated",
          properties: { sessionID: "sess-1", part: { type: "thinking", text: "Old thinking" } },
        },
      } as unknown as WorkspaceStreamEvent);
      await waitFor(() => {
        expect(getStreamParts().find(p => p.type === "thinking")?.text).toBe("Old thinking");
      });

      await waitFor(() => expect(document.querySelector("textarea")).not.toBeNull());
      await user.click(document.querySelector("textarea")!);
      await user.type(document.querySelector("textarea")!, "new message");
      await user.keyboard("{Enter}");

      await waitFor(() => {
        expect(getStreamParts().filter(p => p.type === "thinking")).toHaveLength(0);
      });
    });
  });

  describe("handleSend clears sseStreamText", () => {
    it("clears sseStreamText when user submits a new message", async () => {
      const user = userEvent.setup();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);

      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      // Set some streaming text via SSE
      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", "Old stream text"));
      await waitFor(() => {
        expect(getStreamParts().find(p => p.type === "text")?.text).toBe("Old stream text");
      });

      // Submit a new message — should clear sseStreamText
      await waitFor(() => expect(document.querySelector("textarea")).not.toBeNull());
      await user.click(document.querySelector("textarea")!);
      await user.type(document.querySelector("textarea")!, "new message");
      await user.keyboard("{Enter}");

      await waitFor(() => {
        expect(getStreamParts()).toHaveLength(0);
      });
    });
  });

  describe("streaming parts array (ordered accumulation)", () => {
    it("single thinking block followed by text produces two parts", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "thinking content"));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "response content"));

      await waitFor(() => {
        const parts = getStreamParts();
        expect(parts).toHaveLength(2);
        expect(parts[0]).toEqual({ type: "thinking", text: "thinking content" });
        expect(parts[1]).toEqual({ type: "text", text: "response content" });
      });
    });

    it("multiple thinking blocks produce separate entries (not overwritten)", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      // First thinking block
      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "thought 1"));

      // Tool interrupts
      sendSSEEvent(makePartUpdatedEvent("sess-1", "tool", ""));

      // Second thinking block
      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "thought 2"));

      // Response
      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "answer"));

      await waitFor(() => {
        const parts = getStreamParts();
        expect(parts).toHaveLength(4);
        expect(parts[0]).toEqual({ type: "thinking", text: "thought 1" });
        expect(parts[1]).toMatchObject({ type: "tool", text: "" });
        expect(parts[2]).toEqual({ type: "thinking", text: "thought 2" });
        expect(parts[3]).toEqual({ type: "text", text: "answer" });
      });
    });

    it("tool events produce tool entries in the array", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "let me search"));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "tool", ""));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "tool", ""));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "tool", ""));

      await waitFor(() => {
        const parts = getStreamParts();
        expect(parts[0]).toEqual({ type: "thinking", text: "let me search" });
        // Each tool event produces its own entry
        expect(parts.filter(p => p.type === "tool")).toHaveLength(3);
      });
    });

    it("full realistic sequence: echo → thinking → tools → thinking → tools → text", async () => {
      const user = userEvent.setup();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);

      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      // User sends
      await waitFor(() => expect(document.querySelector("textarea")).not.toBeNull());
      await user.click(document.querySelector("textarea")!);
      await user.type(document.querySelector("textarea")!, "fetch repo info");
      await user.keyboard("{Enter}");

      // Echo
      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", "fetch repo info"));
      // Step 1
      sendSSEEvent(makePartUpdatedEvent("sess-1", "step-start", ""));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "I'll use gh CLI"));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "tool", ""));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "tool", ""));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "step-finish", ""));
      // Step 2
      sendSSEEvent(makePartUpdatedEvent("sess-1", "step-start", ""));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "Let me try curl"));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "tool", ""));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "step-finish", ""));
      // Step 3: response
      sendSSEEvent(makePartUpdatedEvent("sess-1", "step-start", ""));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "Got the data"));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "Here is the repo info"));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "step-finish", ""));

      await waitFor(() => {
        const parts = getStreamParts();
        // Should have: thinking, tool(s), thinking, tool(s), thinking, text
        expect(parts.filter(p => p.type === "thinking")).toHaveLength(3);
        expect(parts.filter(p => p.type === "tool")).toHaveLength(3);
        expect(parts.filter(p => p.type === "text")).toHaveLength(1);
        expect(parts[parts.length - 1]).toEqual({ type: "text", text: "Here is the repo info" });
      });
    });

    it("deltas append to the last part in the array", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "Hello"));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", " world"));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "!"));

      await waitFor(() => {
        const parts = getStreamParts();
        expect(parts).toHaveLength(1);
        expect(parts[0]).toEqual({ type: "text", text: "Hello world!" });
      });
    });

    it("handleSend clears the parts array", async () => {
      const user = userEvent.setup();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);

      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "old content"));

      await waitFor(() => expect(getStreamParts()).toHaveLength(1));

      await waitFor(() => expect(document.querySelector("textarea")).not.toBeNull());
      await user.click(document.querySelector("textarea")!);
      await user.type(document.querySelector("textarea")!, "new msg");
      await user.keyboard("{Enter}");

      await waitFor(() => expect(getStreamParts()).toHaveLength(0));
    });

    it("user echo is suppressed — no parts created", async () => {
      const user = userEvent.setup();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);

      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      await waitFor(() => expect(document.querySelector("textarea")).not.toBeNull());
      await user.click(document.querySelector("textarea")!);
      await user.type(document.querySelector("textarea")!, "hello");
      await user.keyboard("{Enter}");

      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", "hello"));

      await waitFor(() => expect(getStreamParts()).toHaveLength(0));
    });

    it("reasoning snapshot updates existing thinking part text", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "partial"));
      // Snapshot arrives with full text
      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", "full thinking text"));

      await waitFor(() => {
        const parts = getStreamParts();
        expect(parts).toHaveLength(1);
        expect(parts[0]).toEqual({ type: "thinking", text: "full thinking text" });
      });
    });

    it("reasoning snapshot after tool events updates the correct thinking part", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      // Thinking block with deltas
      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "partial thought"));
      // Tools arrive
      sendSSEEvent(makePartUpdatedEvent("sess-1", "tool", ""));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "tool", ""));
      // Reasoning snapshot arrives (after tools, updates the tracked thinking part)
      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", "complete thought from snapshot"));

      await waitFor(() => {
        const parts = getStreamParts();
        expect(parts).toHaveLength(3); // thinking + 2 tools
        expect(parts[0]).toEqual({ type: "thinking", text: "complete thought from snapshot" });
        expect(parts[1]).toMatchObject({ type: "tool", text: "" });
        expect(parts[2]).toMatchObject({ type: "tool", text: "" });
      });
    });

    it("multiple thinking blocks are preserved across steps (snapshots don't overwrite other blocks)", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      // Step 1: thinking + tool
      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "step 1 thinking"));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "tool", ""));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", "step 1 complete"));
      // Step 2: thinking + tool
      sendSSEEvent(makePartUpdatedEvent("sess-1", "step-finish", ""));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "step-start", ""));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "step 2 thinking"));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "tool", ""));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", "step 2 complete"));
      // Step 3: text output
      sendSSEEvent(makePartUpdatedEvent("sess-1", "step-finish", ""));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "step-start", ""));
      sendSSEEvent(makePartUpdatedEvent("sess-1", "text", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "final answer"));

      await waitFor(() => {
        const parts = getStreamParts();
        const thinkingParts = parts.filter(p => p.type === "thinking");
        expect(thinkingParts).toHaveLength(2);
        expect(thinkingParts[0]).toEqual({ type: "thinking", text: "step 1 complete" });
        expect(thinkingParts[1]).toEqual({ type: "thinking", text: "step 2 complete" });
        expect(parts[parts.length - 1]).toEqual({ type: "text", text: "final answer" });
      });
    });

    it("deltas are discarded if last part type doesn't match active route", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      // Thinking block
      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", ""));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "thought"));
      // Tool arrives (last part is now tool)
      sendSSEEvent(makePartUpdatedEvent("sess-1", "tool", ""));
      // Reasoning snapshot sets activePartType back to "reasoning"
      sendSSEEvent(makePartUpdatedEvent("sess-1", "reasoning", "thought snapshot"));
      // A stray delta arrives — last part is tool, not thinking, so discard
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "SHOULD NOT APPEAR"));

      await waitFor(() => {
        const parts = getStreamParts();
        expect(parts[0]!.text).toBe("thought snapshot");
        expect(parts[1]).toMatchObject({ type: "tool", text: "" });
        // No part should contain the stray delta
        expect(parts.every(p => !p.text.includes("SHOULD NOT APPEAR"))).toBe(true);
      });
    });
  });

  describe("unknown events", () => {
    it("silently ignores unknown event types", async () => {
      const qc = makeQueryClient();
      const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent({ type: "unknown.event", foo: "bar" } as unknown as WorkspaceStreamEvent);
      expect(invalidateSpy).not.toHaveBeenCalled();
    });
  });

  it("old event.session shape does NOT trigger session invalidation (regression)", async () => {
    const qc = makeQueryClient();
    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");
    renderChat(qc, "/chat/ws-1/sess-1");
    await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
    sendSSEEvent({ session: { id: "s1", status: "active" } } as unknown as WorkspaceStreamEvent);
    expect(invalidateSpy).not.toHaveBeenCalled();
  });

  it("opencode.event without sessionId in URL is ignored", async () => {
    const qc = makeQueryClient();
    renderChat(qc, "/chat/ws-1");
    await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
    sendSSEEvent(makePartUpdatedEvent("any-session", "text", "no session"));
    await waitFor(() => {
      const chatView = screen.queryByTestId("chat-view");
      if (chatView) {
        expect(JSON.parse(chatView.getAttribute("data-stream-parts") || "[]")).toHaveLength(0);
      }
    });
  });

  describe("session.error lifecycle", () => {
    function makeSessionErrorEvent(sessionID: string, errName: string, errMessage: string): WorkspaceStreamEvent {
      return {
        type: "opencode.event",
        event_type: "session.error",
        data: {
          type: "session.error",
          properties: {
            sessionID,
            error: { name: errName, data: { message: errMessage } },
          },
        },
      } as unknown as WorkspaceStreamEvent;
    }

    function getMessagesFromView(): Array<{ id: string; role: string; parts: Array<{ type: string; text?: string }> }> {
      const view = screen.getByTestId("chat-view");
      return JSON.parse(view.getAttribute("data-messages") ?? "[]");
    }

    function getErrorParts(): Array<{ type: string; text?: string }> {
      return getMessagesFromView().flatMap((m) => m.parts).filter((p) => p.type === "error");
    }

    it("session.error message is visible until reconcileOnIdle clears it", async () => {
      // The error must be visible while the session is active, then cleared
      // when session.status=idle triggers reconcileOnIdle (history is now
      // authoritative). Previously errors persisted at the bottom of the
      // message list even after newer messages arrived above them.
      const qc = makeQueryClient();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeSessionErrorEvent("sess-1", "APIError", "Forbidden: Forbidden"));

      await waitFor(() => {
        const errors = getErrorParts();
        expect(errors).toHaveLength(1);
        expect(errors[0]?.text).toContain("Forbidden");
      });

      sendSSEEvent(makeSessionStatusEvent("sess-1", "idle"));

      await waitFor(() => {
        expect(getErrorParts()).toHaveLength(0);
      });
    });

    it("session.error messages are cleared when navigating to a new session", async () => {
      const qc = makeQueryClient();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

      const { unmount } = renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeSessionErrorEvent("sess-1", "APIError", "some error"));

      await waitFor(() => {
        expect(getErrorParts().length).toBeGreaterThan(0);
      });

      unmount();
      renderChat(qc, "/chat/ws-1/sess-2");

      await waitFor(() => {
        expect(getErrorParts()).toHaveLength(0);
      });
    });

    it("REGRESSION: multiple errors all cleared on idle", async () => {
      // If multiple session.error events fire before idle (e.g. two quick
      // provider failures), ALL of them must be cleared when reconcileOnIdle
      // runs. Previously only localMessages was cleared — sessionErrors
      // accumulated indefinitely.
      const qc = makeQueryClient();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeSessionErrorEvent("sess-1", "APIError", "first error"));
      sendSSEEvent(makeSessionErrorEvent("sess-1", "ContextOverflowError", "context full"));

      await waitFor(() => {
        expect(getErrorParts()).toHaveLength(2);
      });

      sendSSEEvent(makeSessionStatusEvent("sess-1", "idle"));

      await waitFor(() => {
        expect(getErrorParts()).toHaveLength(0);
      });
    });

    it("REGRESSION: error from a different session is ignored", async () => {
      // session.error events are filtered by sessionID — only errors for the
      // current session should appear. This prevents cross-session error leaks.
      const qc = makeQueryClient();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeSessionErrorEvent("sess-OTHER", "APIError", "wrong session error"));

      await new Promise((r) => setTimeout(r, 50));
      expect(getErrorParts()).toHaveLength(0);
    });

    it("REGRESSION: new error after reconcileOnIdle is visible (next turn)", async () => {
      // After reconcileOnIdle clears sessionErrors, a new session.error in
      // the next turn must still render. This guards against a hypothetical
      // bug where setSessionErrors([]) breaks subsequent accumulation.
      const qc = makeQueryClient();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      // Turn 1: error → idle → cleared
      sendSSEEvent(makeSessionErrorEvent("sess-1", "APIError", "turn 1 error"));
      await waitFor(() => expect(getErrorParts()).toHaveLength(1));

      sendSSEEvent(makeSessionStatusEvent("sess-1", "idle"));
      await waitFor(() => expect(getErrorParts()).toHaveLength(0));

      // Turn 2: new error must still render
      sendSSEEvent(makeSessionErrorEvent("sess-1", "APIError", "turn 2 error"));
      await waitFor(() => {
        const errors = getErrorParts();
        expect(errors).toHaveLength(1);
        expect(errors[0]?.text).toContain("turn 2 error");
      });
    });

    it("REGRESSION: error persists while session is busy, only clears on idle", async () => {
      // If the session stays busy after an error, the error must remain
      // visible. reconcileOnIdle only fires on session.status=idle.
      const qc = makeQueryClient();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeSessionStatusEvent("sess-1", "busy"));
      sendSSEEvent(makeSessionErrorEvent("sess-1", "APIError", "still busy error"));

      await waitFor(() => {
        expect(getErrorParts()).toHaveLength(1);
      });

      // Error still present — session hasn't gone idle
      await new Promise((r) => setTimeout(r, 50));
      expect(getErrorParts()).toHaveLength(1);

      // Now idle → cleared
      sendSSEEvent(makeSessionStatusEvent("sess-1", "idle"));
      await waitFor(() => expect(getErrorParts()).toHaveLength(0));
    });

    it("REGRESSION: error does not reappear after idle + new history", async () => {
      // The original bug: error stuck at bottom while history messages
      // populated above it. Simulate: error fires, history contains prior
      // messages, session goes idle → reconcileOnIdle clears errors and
      // refetches history. The final message list must NOT contain the error.
      const qc = makeQueryClient();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([
        { id: "msg-1", role: "user", parts: [{ type: "text", text: "earlier message" }] },
      ]);

      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      // Wait for initial history load
      await waitFor(() => {
        const msgs = getMessagesFromView();
        expect(msgs.some((m) => m.parts.some((p) => p.text === "earlier message"))).toBe(true);
      });

      // Error arrives during streaming
      sendSSEEvent(makeSessionErrorEvent("sess-1", "APIError", "Aborted"));

      await waitFor(() => expect(getErrorParts()).toHaveLength(1));

      // Session goes idle → reconcileOnIdle clears sessionErrors and refetches history
      sendSSEEvent(makeSessionStatusEvent("sess-1", "idle"));

      await waitFor(() => {
        const errors = getErrorParts();
        const msgs = getMessagesFromView();
        expect(errors).toHaveLength(0);
        // History still present
        expect(msgs.some((m) => m.parts.some((p) => p.text === "earlier message"))).toBe(true);
      });
    });

    it("REGRESSION: rapid error → idle → error → idle does not leak between turns", async () => {
      // Two quick turns: each produces an error that should be cleared by
      // its own idle. The second error must not be prematurely cleared by
      // the first idle, and must be cleared by the second idle.
      const qc = makeQueryClient();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      // Turn 1
      sendSSEEvent(makeSessionErrorEvent("sess-1", "APIError", "error A"));
      await waitFor(() => expect(getErrorParts()).toHaveLength(1));
      sendSSEEvent(makeSessionStatusEvent("sess-1", "idle"));
      await waitFor(() => expect(getErrorParts()).toHaveLength(0));

      // Turn 2
      sendSSEEvent(makeSessionStatusEvent("sess-1", "busy"));
      sendSSEEvent(makeSessionErrorEvent("sess-1", "APIError", "error B"));
      await waitFor(() => {
        const errors = getErrorParts();
        expect(errors).toHaveLength(1);
        expect(errors[0]?.text).toContain("error B");
      });
      sendSSEEvent(makeSessionStatusEvent("sess-1", "idle"));
      await waitFor(() => expect(getErrorParts()).toHaveLength(0));
    });

    it("REGRESSION: errors always rendered after history and localMessages in allMessages", async () => {
      // allMessages = [...history, ...localMessages, ...sessionErrors]
      // Errors must always be the last items so they appear at the bottom of
      // the chat. If history messages exist, they come first.
      const qc = makeQueryClient();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([
        { id: "hist-1", role: "user", parts: [{ type: "text", text: "from history" }] },
      ]);

      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeSessionErrorEvent("sess-1", "APIError", "pinned error"));

      await waitFor(() => {
        const msgs = getMessagesFromView();
        const hasHistory = msgs.some((m) => m.parts.some((p) => p.text === "from history"));
        const hasError = msgs.some((m) => m.parts.some((p) => p.type === "error"));
        expect(hasHistory).toBe(true);
        expect(hasError).toBe(true);

        // Error message must be the LAST message
        const lastMsg = msgs[msgs.length - 1];
        expect(lastMsg?.parts.some((p) => p.type === "error")).toBe(true);
      });
    });

    it("REGRESSION: sessionErrors are cleared even when reconcileOnIdle history refetch fails", async () => {
      // reconcileOnIdle calls await queryClient.refetchQueries(...) then
      // setSessionErrors([]). TanStack Query's refetchQueries does not throw
      // when individual query functions reject — it silently swallows errors
      // and resolves. So setSessionErrors([]) is always reached regardless of
      // fetch outcome.
      const qc = makeQueryClient();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("network fail"));

      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeSessionErrorEvent("sess-1", "APIError", "error before fail"));
      await waitFor(() => expect(getErrorParts()).toHaveLength(1));

      // Idle triggers reconcileOnIdle which calls refetchQueries → rejects
      sendSSEEvent(makeSessionStatusEvent("sess-1", "idle"));

      // reconcileOnIdle clears sessionErrors regardless of refetch outcome
      await waitFor(() => expect(getErrorParts()).toHaveLength(0));
    });
  });

  // ─────────────────────────────────────────────────────────────────────────
  // session.error name mapping
  // ─────────────────────────────────────────────────────────────────────────

  describe("session.error name mapping", () => {
    function makeSessionError(name: string, data?: Record<string, unknown>): WorkspaceStreamEvent {
      return {
        type: "opencode.event",
        event_type: "session.error",
        data: {
          type: "session.error",
          properties: {
            sessionID: "sess-1",
            error: { name, data: data ?? {} },
          },
        },
      } as unknown as WorkspaceStreamEvent;
    }

    function getErrorText(): string | undefined {
      const view = screen.getByTestId("chat-view");
      const msgs = JSON.parse(view.getAttribute("data-messages") ?? "[]") as Array<{ parts: Array<{ type: string; text?: string }> }>;
      return msgs.flatMap((m) => m.parts).find((p) => p.type === "error")?.text;
    }

    beforeEach(() => {
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    });

    it("ContextOverflowError shows /compact instruction", async () => {
      const qc = makeQueryClient();
      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeSessionError("ContextOverflowError"));

      await waitFor(() => {
        const text = getErrorText();
        expect(text).toBeDefined();
        expect(text).toContain("/compact");
        expect(text).toContain("Context limit reached");
      });
    });

    it("MessageOutputLengthError shows human-readable output limit message", async () => {
      const qc = makeQueryClient();
      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeSessionError("MessageOutputLengthError"));

      await waitFor(() => {
        const text = getErrorText();
        expect(text).toBeDefined();
        expect(text).toContain("too long");
        expect(text).not.toContain("MessageOutputLengthError");
      });
    });

    it("ProviderAuthError with providerID surfaces provider name", async () => {
      const qc = makeQueryClient();
      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeSessionError("ProviderAuthError", { providerID: "anthropic", message: "Unauthorized" }));

      await waitFor(() => {
        const text = getErrorText();
        expect(text).toBeDefined();
        expect(text).toContain("anthropic");
        expect(text).toContain("Authentication failed");
      });
    });

    it("ProviderAuthError without providerID falls back to raw message", async () => {
      const qc = makeQueryClient();
      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeSessionError("ProviderAuthError", { message: "invalid api key" }));

      await waitFor(() => {
        const text = getErrorText();
        expect(text).toBeDefined();
        expect(text).toContain("invalid api key");
      });
    });

    it("unknown error name falls back to data.message", async () => {
      const qc = makeQueryClient();
      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeSessionError("SomeUnknownError", { message: "something went wrong" }));

      await waitFor(() => {
        const text = getErrorText();
        expect(text).toBeDefined();
        expect(text).toContain("something went wrong");
      });
    });

    it("unknown error with no message falls back to error name", async () => {
      const qc = makeQueryClient();
      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeSessionError("WeirdNamedError"));

      await waitFor(() => {
        const text = getErrorText();
        expect(text).toBeDefined();
        expect(text).toContain("WeirdNamedError");
      });
    });
  });

  // ─────────────────────────────────────────────────────────────────────────
  // session.status=retry via opencode.event
  // ─────────────────────────────────────────────────────────────────────────

  describe("session.status=retry via opencode.event", () => {
    function makeRetryEvent(sessionID: string, attempt: number, message: string, nextOffsetMs = 5000): WorkspaceStreamEvent {
      return {
        type: "opencode.event",
        event_type: "session.status",
        data: {
          type: "session.status",
          properties: {
            sessionID,
            status: {
              type: "retry",
              attempt,
              message,
              next: Date.now() + nextOffsetMs,
            },
          },
        },
      } as unknown as WorkspaceStreamEvent;
    }

    beforeEach(() => {
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    });

    it("shows retry banner when retry event fires for current session", async () => {
      const qc = makeQueryClient();
      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeRetryEvent("sess-1", 1, "Rate limited"));

      await waitFor(() => {
        expect(screen.getByText(/rate limited/i)).toBeInTheDocument();
      });
    });

    it("shows attempt count when attempt > 1", async () => {
      const qc = makeQueryClient();
      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeRetryEvent("sess-1", 3, "Rate limited"));

      await waitFor(() => {
        expect(screen.getByText(/attempt 3/i)).toBeInTheDocument();
      });
    });

    it("ignores retry event for a different session", async () => {
      const qc = makeQueryClient();
      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeRetryEvent("sess-OTHER", 1, "Rate limited"));

      await new Promise((r) => setTimeout(r, 50));
      expect(screen.queryByText(/rate limited/i)).not.toBeInTheDocument();
    });

    it("clears retry banner when session.status=idle fires", async () => {
      const qc = makeQueryClient();
      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeRetryEvent("sess-1", 1, "Rate limited"));
      await waitFor(() => expect(screen.getByText(/rate limited/i)).toBeInTheDocument());

      sendSSEEvent({ type: "session.status", session_id: "sess-1", status: "idle" } as WorkspaceStreamEvent);

      await waitFor(() => {
        expect(screen.queryByText(/rate limited/i)).not.toBeInTheDocument();
      });
    });

    it("clears retry banner when session.status=busy fires (next attempt started)", async () => {
      const qc = makeQueryClient();
      renderChat(qc, "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeRetryEvent("sess-1", 1, "Rate limited"));
      await waitFor(() => expect(screen.getByText(/rate limited/i)).toBeInTheDocument());

      sendSSEEvent({ type: "session.status", session_id: "sess-1", status: "busy" } as WorkspaceStreamEvent);

      await waitFor(() => {
        expect(screen.queryByText(/rate limited/i)).not.toBeInTheDocument();
      });
    });
  });

  describe("agent_died events (US-44.1c)", () => {
    function makeAgentDiedEvent(workspaceId: string): WorkspaceStreamEvent {
      return {
        type: "agent_died",
        workspace_id: workspaceId,
        data: { reason: "unknown" },
      };
    }

    it("renders the dismissible agent_died banner on receipt of agent_died", async () => {
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      expect(screen.queryByText(/Agent is restarting/i)).not.toBeInTheDocument();

      sendSSEEvent(makeAgentDiedEvent("ws-1"));

      await waitFor(() => {
        const banner = screen.getByText(/Agent is restarting/i);
        expect(banner).toBeInTheDocument();
        expect(banner.closest("[role='alert']")).not.toBeNull();
      });
    });

    it("dismisses the agent_died banner when the Dismiss button is clicked", async () => {
      const user = userEvent.setup();
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeAgentDiedEvent("ws-1"));
      await waitFor(() => expect(screen.getByText(/Agent is restarting/i)).toBeInTheDocument());

      await user.click(screen.getByRole("button", { name: "Dismiss" }));

      await waitFor(() => {
        expect(screen.queryByText(/Agent is restarting/i)).not.toBeInTheDocument();
      });
    });

    it("clears the agent_died banner when the active session changes", async () => {
      (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
      renderChatNavigable(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      sendSSEEvent(makeAgentDiedEvent("ws-1"));
      await waitFor(() => expect(screen.getByText(/Agent is restarting/i)).toBeInTheDocument());

      await act(async () => {
        navigateRef.current?.("/chat/ws-1/sess-2");
      });

      await waitFor(() => {
        expect(screen.queryByText(/Agent is restarting/i)).not.toBeInTheDocument();
      });
    });
  });

  describe("messageID partitioning (part.messageID propagates to StreamPart)", () => {
    it("attaches part.messageID to text parts from message.part.updated", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent({
        type: "opencode.event",
        event_type: "message.part.updated",
        data: {
          type: "message.part.updated",
          properties: {
            sessionID: "sess-1",
            part: { type: "text", id: "prt_1", messageID: "msg_a", text: "hello" },
          },
        },
      } as unknown as WorkspaceStreamEvent);
      await waitFor(() => {
        const parts = getStreamParts() as Array<{ type: string; text: string; messageID?: string }>;
        expect(parts).toHaveLength(1);
        expect(parts[0]!.text).toBe("hello");
        expect(parts[0]!.messageID).toBe("msg_a");
      });
    });

    it("attaches part.messageID to tool parts from message.part.updated", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      sendSSEEvent({
        type: "opencode.event",
        event_type: "message.part.updated",
        data: {
          type: "message.part.updated",
          properties: {
            sessionID: "sess-1",
            part: {
              type: "tool",
              id: "prt_tool",
              messageID: "msg_a",
              tool: "bash",
              callID: "call_1",
              state: { status: "completed", title: "run tests" },
            },
          },
        },
      } as unknown as WorkspaceStreamEvent);
      await waitFor(() => {
        const parts = getStreamParts() as Array<{ type: string; text: string; messageID?: string }>;
        expect(parts).toHaveLength(1);
        expect(parts[0]!.type).toBe("tool");
        expect(parts[0]!.messageID).toBe("msg_a");
      });
    });

    it("partitions consecutive parts by messageID (text→tool→text→tool across two messages)", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());

      // First assistant message: text + tool call
      sendSSEEvent({
        type: "opencode.event",
        event_type: "message.part.updated",
        data: {
          type: "message.part.updated",
          properties: {
            sessionID: "sess-1",
            part: { type: "text", id: "prt_1", messageID: "msg_a", text: "first" },
          },
        },
      } as unknown as WorkspaceStreamEvent);
      sendSSEEvent({
        type: "opencode.event",
        event_type: "message.part.updated",
        data: {
          type: "message.part.updated",
          properties: {
            sessionID: "sess-1",
            part: {
              type: "tool",
              id: "prt_2",
              messageID: "msg_a",
              tool: "bash",
              callID: "call_1",
              state: { status: "completed" },
            },
          },
        },
      } as unknown as WorkspaceStreamEvent);
      // Second assistant message: text + tool call
      sendSSEEvent({
        type: "opencode.event",
        event_type: "message.part.updated",
        data: {
          type: "message.part.updated",
          properties: {
            sessionID: "sess-1",
            part: { type: "text", id: "prt_3", messageID: "msg_b", text: "second" },
          },
        },
      } as unknown as WorkspaceStreamEvent);
      sendSSEEvent({
        type: "opencode.event",
        event_type: "message.part.updated",
        data: {
          type: "message.part.updated",
          properties: {
            sessionID: "sess-1",
            part: {
              type: "tool",
              id: "prt_4",
              messageID: "msg_b",
              tool: "edit",
              callID: "call_2",
              state: { status: "completed" },
            },
          },
        },
      } as unknown as WorkspaceStreamEvent);

      await waitFor(() => {
        const parts = getStreamParts() as Array<{ type: string; text: string; messageID?: string }>;
        expect(parts).toHaveLength(4);
        expect(parts.map((p) => p.messageID)).toEqual(["msg_a", "msg_a", "msg_b", "msg_b"]);
      });
    });

    it("preserves messageID across delta accumulation", async () => {
      renderChat(makeQueryClient(), "/chat/ws-1/sess-1");
      await waitFor(() => expect(capturedSSEHandler).not.toBeNull());
      // Snapshot with empty text opens the text part; deltas accumulate onto it.
      sendSSEEvent({
        type: "opencode.event",
        event_type: "message.part.updated",
        data: {
          type: "message.part.updated",
          properties: {
            sessionID: "sess-1",
            part: { type: "text", id: "prt_1", messageID: "msg_a", text: "" },
          },
        },
      } as unknown as WorkspaceStreamEvent);
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", "Hello"));
      sendSSEEvent(makePartDeltaEvent("sess-1", "text", " world"));
      await waitFor(() => {
        const parts = getStreamParts() as Array<{ type: string; text: string; messageID?: string }>;
        expect(parts).toHaveLength(1);
        expect(parts[0]!.text).toBe("Hello world");
        expect(parts[0]!.messageID).toBe("msg_a");
      });
    });
  });
});
