/**
 * Tests for ChatPage's agent input request handling (US-16.11, US-16.12).
 * Tests the SSE event → state → prompt render → resolve lifecycle.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, act, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";
import { SessionActivityProvider } from "../providers/SessionActivityProvider";
import type { WorkspaceStreamEvent } from "../api/types";

// --- Mocks ---

vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getStatus: vi.fn(),
    activate: vi.fn(),
    list: vi.fn().mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } }),
    renameSession: vi.fn(),
    markSessionSeen: vi.fn().mockResolvedValue(undefined),
    getSessions: vi.fn().mockResolvedValue([]),
  },
}));
// Use the REAL SessionActivityProvider here: these tests verify the SSE →
// provider → prompt-render chain end-to-end, which a stub mock cannot drive
// (no state update → no re-render). Only the user-wide event stream is stubbed.
vi.mock("../hooks/useUserEventStream", () => ({
  useUserEventStream: () => {},
}));
vi.mock("../api/messages", () => ({ messagesApi: { getHistory: vi.fn().mockResolvedValue([]), getHistoryPage: vi.fn().mockResolvedValue({ messages: [], nextCursor: undefined }), sendAsync: vi.fn(), queueMessage: vi.fn().mockResolvedValue({ messageID: "msg_q_mock" }), getQueue: vi.fn().mockResolvedValue({ messages: [] }), deleteQueueMessage: vi.fn().mockResolvedValue(undefined) } }));
vi.mock("../api/sessions", () => ({ sessionsApi: { create: vi.fn() } }));
vi.mock("../api/input", () => ({
  inputApi: {
    questionReply: vi.fn().mockResolvedValue(true),
    questionReject: vi.fn().mockResolvedValue(true),
    permissionReply: vi.fn().mockResolvedValue(true),
    listQuestions: vi.fn().mockResolvedValue([]),
    listPermissions: vi.fn().mockResolvedValue([]),
  },
}));

// Capture the SSE handler
let capturedSSEHandler: ((data: unknown) => void) | null = null;
vi.mock("../hooks/useEventStream", () => ({
  useEventStream: vi.fn((_workspaceId: string | undefined, handler: (data: unknown) => void) => {
    capturedSSEHandler = handler;
  }),
}));

// Mock ChatView to render prompts prop (so we can test the full integration)
vi.mock("../components/chat/ChatView", () => ({
  ChatView: (props: Record<string, unknown>) => (
    <div data-testid="chat-view">
      {props.prompts as React.ReactNode}
    </div>
  ),
}));

import { workspacesApi } from "../api/workspaces";
import { messagesApi } from "../api/messages";

// --- Helpers ---

function makeQueryClient() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  return qc;
}

function renderChat(qc: QueryClient, path: string) {
  // Pre-seed queries so the component renders ChatView immediately
  const wsId = path.split("/")[2]; // e.g. "ws-1"
  const sesId = path.split("/")[3]; // e.g. "ses_1"
  qc.setQueryData(["workspace-status", wsId], { phase: "Active" });
  qc.setQueryData(["workspaces"], { items: [], pagination: { limit: 20, offset: 0, total: 0 } });
  qc.setQueryData(["messages", wsId, sesId], { pages: [{ messages: [], nextCursor: undefined }], pageParams: [undefined] });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[path]}>
        <SessionActivityProvider>
          <TooltipProvider delayDuration={0}>
            <Routes>
              <Route path="/chat/:workspaceId/:sessionId" element={<ChatPage />} />
            </Routes>
          </TooltipProvider>
        </SessionActivityProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function sendSSE(event: WorkspaceStreamEvent) {
  act(() => { capturedSSEHandler?.(event); });
}

const questionEvent: WorkspaceStreamEvent = {
  type: "agent.question",
  data: {
    id: "que_abc",
    session_id: "ses_1",
    questions: [{ header: "Language", question: "Pick one", options: [{ label: "Go", description: "fast" }] }],
  },
} as unknown as WorkspaceStreamEvent;

const questionResolvedEvent: WorkspaceStreamEvent = {
  type: "agent.question.resolved",
  data: { request_id: "que_abc", session_id: "ses_1" },
} as unknown as WorkspaceStreamEvent;

const permissionEvent: WorkspaceStreamEvent = {
  type: "agent.permission",
  data: {
    id: "per_xyz",
    session_id: "ses_1",
    permission: "shell",
    patterns: ["rm -rf /tmp"],
  },
} as unknown as WorkspaceStreamEvent;

const permissionResolvedEvent: WorkspaceStreamEvent = {
  type: "agent.permission.resolved",
  data: { request_id: "per_xyz", session_id: "ses_1", reply: "once" },
} as unknown as WorkspaceStreamEvent;

// --- Tests ---

describe("ChatPage agent input requests (US-16.11, US-16.12)", () => {
  beforeEach(() => {
    capturedSSEHandler = null;
    vi.clearAllMocks();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } });
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockResolvedValue({ messages: [], nextCursor: undefined });
  });

  it("agent.question event renders QuestionPrompt", async () => {
    const qc = makeQueryClient();
    renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());
    sendSSE(questionEvent);
    expect(screen.getByText("Pick one")).toBeInTheDocument();
  });

  it("agent.question.resolved removes QuestionPrompt", async () => {
    const qc = makeQueryClient();
    renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());
    sendSSE(questionEvent);
    expect(screen.getByText("Pick one")).toBeInTheDocument();
    sendSSE(questionResolvedEvent);
    expect(screen.queryByText("Pick one")).not.toBeInTheDocument();
  });

  it("agent.permission event renders PermissionPrompt", async () => {
    const qc = makeQueryClient();
    renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());
    sendSSE(permissionEvent);
    expect(screen.getByText("Run shell command")).toBeInTheDocument();
  });

  it("agent.permission.resolved removes PermissionPrompt", async () => {
    const qc = makeQueryClient();
    renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());
    sendSSE(permissionEvent);
    expect(screen.getByText("Run shell command")).toBeInTheDocument();
    sendSSE(permissionResolvedEvent);
    expect(screen.queryByText("Run shell command")).not.toBeInTheDocument();
  });

  it("event for different session is ignored", async () => {
    const qc = makeQueryClient();
    renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());
    const wrongSession = { ...questionEvent, data: { ...(questionEvent as any).data, session_id: "ses_other" } } as unknown as WorkspaceStreamEvent;
    sendSSE(wrongSession);
    expect(screen.queryByText("Pick one")).not.toBeInTheDocument();
  });

  it("duplicate event (same id) does not double render", async () => {
    const qc = makeQueryClient();
    renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());
    sendSSE(questionEvent);
    sendSSE(questionEvent);
    expect(screen.getAllByText("Pick one")).toHaveLength(1);
  });

  it("session idle clears all pending prompts (US-16.12)", async () => {
    const qc = makeQueryClient();
    renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());
    sendSSE(questionEvent);
    sendSSE(permissionEvent);
    expect(screen.getByText("Pick one")).toBeInTheDocument();
    expect(screen.getByText("Run shell command")).toBeInTheDocument();
    sendSSE({ type: "session.status", session_id: "ses_1", status: "idle" } as unknown as WorkspaceStreamEvent);
    expect(screen.queryByText("Pick one")).not.toBeInTheDocument();
    expect(screen.queryByText("Run shell command")).not.toBeInTheDocument();
  });

  it("session error clears pending prompts (US-16.12)", async () => {
    const qc = makeQueryClient();
    renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());
    sendSSE(questionEvent);
    expect(screen.getByText("Pick one")).toBeInTheDocument();
    sendSSE({
      type: "session.event",
      session_id: "ses_1",
      data: { type: "error", sessionId: "ses_1", error: { code: "timeout", message: "timed out" } },
    } as unknown as WorkspaceStreamEvent);
    expect(screen.queryByText("Pick one")).not.toBeInTheDocument();
  });

  it("session idle for different session does NOT clear prompts", async () => {
    const qc = makeQueryClient();
    renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());
    sendSSE(questionEvent);
    sendSSE({ type: "session.status", session_id: "ses_other", status: "idle" } as unknown as WorkspaceStreamEvent);
    expect(screen.getByText("Pick one")).toBeInTheDocument();
  });

  it("no pending prompts + idle is a no-op", async () => {
    const qc = makeQueryClient();
    renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());
    sendSSE({ type: "session.status", session_id: "ses_1", status: "idle" } as unknown as WorkspaceStreamEvent);
    expect(screen.getByTestId("chat-view")).toBeInTheDocument();
  });

  it("user answers question → onResolved removes prompt", async () => {
    const qc = makeQueryClient();
    renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());
    sendSSE(questionEvent);
    const goBtn = screen.getByRole("button", { name: "Go" });
    act(() => { goBtn.click(); });
    const submitBtn = screen.getByText("Submit answers");
    act(() => { submitBtn.click(); });
    await waitFor(() => expect(screen.queryByText("Pick one")).not.toBeInTheDocument());
  });

  it("session change clears pending prompts", async () => {
    const qc = makeQueryClient();
    const { unmount } = renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());
    sendSSE(questionEvent);
    expect(screen.getByText("Pick one")).toBeInTheDocument();
    // Simulate navigation to different session by unmounting and re-rendering
    unmount();
    qc.setQueryData(["workspace-status", "ws-1"], { phase: "Active" });
    qc.setQueryData(["messages", "ws-1", "ses_2"], { pages: [{ messages: [], nextCursor: undefined }], pageParams: [undefined] });
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={["/chat/ws-1/ses_2"]}>
          <TooltipProvider delayDuration={0}>
            <Routes>
              <Route path="/chat/:workspaceId/:sessionId" element={<ChatPage />} />
            </Routes>
          </TooltipProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());
    // Prompt from ses_1 should not be visible
    expect(screen.queryByText("Pick one")).not.toBeInTheDocument();
  });

  // Subtask/subagent prompts (e.g. opencode `task` tool spawning a child
  // session) emit events with the SUBTASK's session_id. The backend
  // populates root_session_id with the user-visible parent session so the
  // chat UI can bubble the prompt into the parent view rather than dropping
  // it. See worklog (subtask permission bubbling).
  it("agent.permission from a subtask bubbles to parent session via root_session_id", async () => {
    const qc = makeQueryClient();
    renderChat(qc, "/chat/ws-1/ses_parent");
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());

    const subtaskPermission: WorkspaceStreamEvent = {
      type: "agent.permission",
      data: {
        id: "per_subtask",
        session_id: "ses_child",       // subagent's own session
        root_session_id: "ses_parent", // user-visible parent
        permission: "shell",
        patterns: ["ls"],
      },
    } as unknown as WorkspaceStreamEvent;

    sendSSE(subtaskPermission);
    expect(screen.getByText("Run shell command")).toBeInTheDocument();
  });

  it("agent.question from a subtask bubbles to parent session via root_session_id", async () => {
    const qc = makeQueryClient();
    renderChat(qc, "/chat/ws-1/ses_parent");
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());

    const subtaskQuestion: WorkspaceStreamEvent = {
      type: "agent.question",
      data: {
        id: "que_subtask",
        session_id: "ses_child",
        root_session_id: "ses_parent",
        questions: [{ header: "Language", question: "Pick one", options: [{ label: "Go", description: "fast" }] }],
      },
    } as unknown as WorkspaceStreamEvent;

    sendSSE(subtaskQuestion);
    expect(screen.getByText("Pick one")).toBeInTheDocument();
  });

  it("agent.permission with root_session_id pointing at a different parent is ignored", async () => {
    const qc = makeQueryClient();
    renderChat(qc, "/chat/ws-1/ses_parent");
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());

    const otherTreePermission: WorkspaceStreamEvent = {
      type: "agent.permission",
      data: {
        id: "per_other",
        session_id: "ses_other_child",
        root_session_id: "ses_other_parent", // different tree
        permission: "shell",
        patterns: ["ls"],
      },
    } as unknown as WorkspaceStreamEvent;

    sendSSE(otherTreePermission);
    expect(screen.queryByText("Run shell command")).not.toBeInTheDocument();
  });

  it("backward compat: event without root_session_id falls back to session_id match", async () => {
    // Older API replicas without root resolution still emit events with only
    // session_id. The frontend must match those when session_id === URL session.
    const qc = makeQueryClient();
    renderChat(qc, "/chat/ws-1/ses_1");
    await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());

    const legacyEvent: WorkspaceStreamEvent = {
      type: "agent.permission",
      data: {
        id: "per_legacy",
        session_id: "ses_1",
        // no root_session_id
        permission: "shell",
        patterns: ["ls"],
      },
    } as unknown as WorkspaceStreamEvent;

    sendSSE(legacyEvent);
    expect(screen.getByText("Run shell command")).toBeInTheDocument();
  });
});
