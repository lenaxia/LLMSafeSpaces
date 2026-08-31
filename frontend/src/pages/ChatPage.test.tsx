import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../providers/ThemeProvider";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";
import { ApiClientError } from "../api/client";
import type { Event } from "../abi/llmsafespaces/abi/v1/contract_pb";
import type { SessionSnapshot } from "../abi/llmsafespaces/abi/v1/abi_pb";

vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getStatus: vi.fn(),
    activate: vi.fn(),
    list: vi.fn().mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } }),
    listModels: vi.fn().mockResolvedValue({ models: [], currentModel: "" }),
    setModel: vi.fn().mockResolvedValue({ model: "", applied: false }),
    renameWorkspace: vi.fn().mockResolvedValue({}),
    deleteWorkspace: vi.fn().mockResolvedValue({}),
    suspend: vi.fn().mockResolvedValue({}),
    deleteSession: vi.fn().mockResolvedValue(undefined),
    abortSession: vi.fn().mockResolvedValue(undefined),
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
vi.mock("../hooks/useEventStream", () => ({ useEventStream: vi.fn() }));

// Capture the contract-stream options ChatPage registers with
// useContractStream (ABI contract events), and expose a controllable fold
// state — same shape as ChatPage.sse.test.tsx.
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
import { messagesApi } from "../api/messages";
import { sessionsApi } from "../api/sessions";

function renderChatPage(path = "/chat") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return {
    qc,
    ...render(
      <QueryClientProvider client={qc}>
        <ThemeProvider>
          <TooltipProvider delayDuration={0}>
            <MemoryRouter initialEntries={[path]}>
              <Routes>
                <Route path="/chat" element={<ChatPage />} />
                <Route path="/chat/:workspaceId" element={<ChatPage />} />
                <Route path="/chat/:workspaceId/:sessionId" element={<ChatPage />} />
              </Routes>
            </MemoryRouter>
          </TooltipProvider>
        </ThemeProvider>
      </QueryClientProvider>,
    ),
  };
}

describe("ChatPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    capturedContractOptions = null;
    contractState = { seq: 0n, sessions: new Map() };
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } });
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    // clearAllMocks does not reset implementations set by earlier tests
    // (e.g. the cursor-keyed two-page mocks below) — re-establish the
    // default single-page wrapper every run.
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockImplementation(async () => ({
      messages: await (messagesApi.getHistory as ReturnType<typeof vi.fn>)(),
      nextCursor: undefined,
    }));
  });
  it("shows empty state when no workspace selected", () => {
    renderChatPage("/chat");
    expect(screen.getByText("Select a workspace to start chatting")).toBeInTheDocument();
  });

  it("registers ABI contract-stream handlers (US-69.10 cutover)", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => {
      expect(capturedContractOptions?.onEvent).toBeTypeOf("function");
      expect(capturedContractOptions?.onSnapshot).toBeTypeOf("function");
      expect(capturedContractOptions?.onReconnect).toBeTypeOf("function");
    });
  });

  it("shows workspace name in header", async () => {
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "My Workspace", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Suspended" });
    renderChatPage("/chat/ws-1");
    await waitFor(() => expect(screen.getByText("My Workspace")).toBeInTheDocument());
  });

  it("shows suspended banner for suspended workspace", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Suspended" });
    renderChatPage("/chat/ws-1");
    await waitFor(() => expect(screen.getByText(/is suspended/)).toBeInTheDocument());
  });

  it("shows transitioning state", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Resuming" });
    renderChatPage("/chat/ws-1");
    await waitFor(() => expect(screen.getByText(/resuming/i)).toBeInTheDocument());
  });

  it("disables composer when workspace is suspended", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Suspended" });
    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(document.querySelector("textarea")).toBeDisabled());
  });

  it("enables composer when workspace is running and session is selected", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());
  });

  it("shows kebab menu in header", async () => {
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "My Workspace", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    renderChatPage("/chat/ws-1");
    await waitFor(() => expect(screen.getByLabelText("Actions")).toBeInTheDocument());
  });

  it("renders messages in backend order regardless of createdAt values", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    // US-69.10 I12 stitch: transcript order is the backend's own order
    // (pages newest-first, reversed; within-page order preserved). createdAt
    // is deliberately scrambled to contradict the page order — timestamps
    // must never be consulted for ordering.
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "aa0000000001abcdef", role: "user", parts: [{ type: "text", text: "First" }], createdAt: "2026-01-03T00:00:00.000Z" },
      { id: "bb0000000002abcdef", role: "user", parts: [{ type: "text", text: "Second" }], createdAt: "2026-01-01T00:00:00.000Z" },
      { id: "cc0000000003abcdef", role: "assistant", parts: [{ type: "text", text: "Third" }], createdAt: "2026-01-02T00:00:00.000Z" },
    ]);
    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => {
      const bubbles = screen.getAllByText(/First|Second|Third/);
      expect(bubbles).toHaveLength(3);
      expect(bubbles[0]).toHaveTextContent("First");
      expect(bubbles[1]).toHaveTextContent("Second");
      expect(bubbles[2]).toHaveTextContent("Third");
    });
  });

  it("preserves the page's order when the API returns oldest-first", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    // Within-page order is preserved verbatim — an oldest-first page renders
    // oldest-first without any client-side reversal or sorting.
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "aa0000000001abcdef", role: "user", parts: [{ type: "text", text: "First" }], createdAt: "2026-01-01T00:00:00.000Z" },
      { id: "bb0000000002abcdef", role: "user", parts: [{ type: "text", text: "Second" }], createdAt: "2026-01-02T00:00:00.000Z" },
      { id: "cc0000000003abcdef", role: "assistant", parts: [{ type: "text", text: "Third" }], createdAt: "2026-01-03T00:00:00.000Z" },
    ]);
    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => {
      const bubbles = screen.getAllByText(/First|Second|Third/);
      expect(bubbles[0]).toHaveTextContent("First");
      expect(bubbles[1]).toHaveTextContent("Second");
      expect(bubbles[2]).toHaveTextContent("Third");
    });
  });

  it("dedupes messages whose ids repeat across pages, keeping the oldest-page occurrence", async () => {
    const user = userEvent.setup();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    // Page 1 (newest) carries a copy of msg_dup; the older page carries
    // another. selectByIdentity walks pages oldest-first, so the oldest-page
    // occurrence is the "first" seen and the newest-page copy is dropped.
    const pagesByCursor: Record<string, { messages: unknown[]; nextCursor?: string }> = {
      initial: {
        messages: [
          { id: "msg_dup", role: "user", parts: [{ type: "text", text: "dup from newest page" }], createdAt: "2026-01-03T00:00:00.000Z" },
          { id: "msg_new", role: "assistant", parts: [{ type: "text", text: "Latest" }], createdAt: "2026-01-04T00:00:00.000Z" },
        ],
        nextCursor: "msg_dup",
      },
      "msg_dup": {
        messages: [
          { id: "msg_old", role: "user", parts: [{ type: "text", text: "Oldest" }], createdAt: "2026-01-01T00:00:00.000Z" },
          { id: "msg_dup", role: "user", parts: [{ type: "text", text: "dup from oldest page" }], createdAt: "2026-01-02T00:00:00.000Z" },
        ],
        nextCursor: undefined,
      },
    };
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockImplementation(
      async (_ws: string, _ses: string, opts?: { before?: string }) =>
        pagesByCursor[opts?.before ?? "initial"]!,
    );

    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(screen.getByText("Latest")).toBeInTheDocument());

    // Load the older page through the real ChatView button.
    await user.click(screen.getByText("Load earlier messages"));

    await waitFor(() => {
      const bubbles = screen.getAllByText(/Oldest|dup from|Latest/);
      expect(bubbles).toHaveLength(3);
      expect(bubbles[0]).toHaveTextContent("Oldest");
      expect(bubbles[1]).toHaveTextContent("dup from oldest page");
      expect(bubbles[2]).toHaveTextContent("Latest");
    });
    expect(screen.queryByText("dup from newest page")).not.toBeInTheDocument();
  });

  it("newest messages stay at the bottom when an older page loads", async () => {
    const user = userEvent.setup();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    // Pages arrive newest-first; select reverses them so the transcript is
    // stable as older pages prepend at the top — the newest stays last.
    const pagesByCursor: Record<string, { messages: unknown[]; nextCursor?: string }> = {
      initial: {
        messages: [
          { id: "n-2", role: "user", parts: [{ type: "text", text: "Third" }], createdAt: "2026-01-03T00:00:00.000Z" },
          { id: "n-3", role: "assistant", parts: [{ type: "text", text: "Fourth (newest)" }], createdAt: "2026-01-04T00:00:00.000Z" },
        ],
        nextCursor: "n-2",
      },
      "n-2": {
        messages: [
          { id: "o-1", role: "user", parts: [{ type: "text", text: "First" }], createdAt: "2026-01-01T00:00:00.000Z" },
          { id: "o-2", role: "assistant", parts: [{ type: "text", text: "Second" }], createdAt: "2026-01-02T00:00:00.000Z" },
        ],
        nextCursor: undefined,
      },
    };
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockImplementation(
      async (_ws: string, _ses: string, opts?: { before?: string }) =>
        pagesByCursor[opts?.before ?? "initial"]!,
    );

    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => {
      const bubbles = screen.getAllByText(/Third|Fourth/);
      expect(bubbles[bubbles.length - 1]).toHaveTextContent("Fourth (newest)");
    });

    await user.click(screen.getByText("Load earlier messages"));

    await waitFor(() => {
      const bubbles = screen.getAllByText(/First|Second|Third|Fourth/);
      expect(bubbles).toHaveLength(4);
      // Older page prepended at the top; newest still last (bottom).
      expect(bubbles[0]).toHaveTextContent("First");
      expect(bubbles[bubbles.length - 1]).toHaveTextContent("Fourth (newest)");
    });
  });

  it("local optimistic message appears after history messages", async () => {
    const user = userEvent.setup();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    // History page arrives newest-first; within-page order is preserved
    // (Hi before Hello), and the optimistic local message appends after
    // all history messages.
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "bb0000000002abcdef", role: "assistant", parts: [{ type: "text", text: "Hi" }], createdAt: "2026-01-02T00:00:00.000Z" },
      { id: "aa0000000001abcdef", role: "user", parts: [{ type: "text", text: "Hello" }], createdAt: "2026-01-01T00:00:00.000Z" },
    ]);
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    renderChatPage("/chat/ws-1/sess-1");

    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());
    await user.click(document.querySelector("textarea")!);
    await user.type(document.querySelector("textarea")!, "New message");
    await user.click(screen.getByRole("button", { name: "Send message" }));

    await waitFor(() => {
      const allTexts = screen.getAllByText(/Hello|Hi|New message/);
      expect(allTexts[0]).toHaveTextContent("Hi");
      expect(allTexts[1]).toHaveTextContent("Hello");
      expect(allTexts[allTexts.length - 1]).toHaveTextContent("New message");
    });
  });

  it("auto-creates session when workspace Active and no sessionId", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (sessionsApi.create as ReturnType<typeof vi.fn>).mockResolvedValue({ sessionId: "new-sess" });
    renderChatPage("/chat/ws-1");
    await waitFor(() => expect(sessionsApi.create).toHaveBeenCalledWith("ws-1", "New chat"));
  });

  it("shows chatError banner when send fails", async () => {
    const user = userEvent.setup();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("LLM error"));

    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    await user.click(document.querySelector("textarea")!);
    await user.type(document.querySelector("textarea")!, "hello");
    await user.click(screen.getByRole("button", { name: "Send message" }));

    await waitFor(() => expect(screen.getByText("LLM error")).toBeInTheDocument());
  });

  it("Dismiss button clears chatError", async () => {
    const user = userEvent.setup();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("boom"));

    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    await user.click(document.querySelector("textarea")!);
    await user.type(document.querySelector("textarea")!, "hello");
    await user.click(screen.getByRole("button", { name: "Send message" }));

    await waitFor(() => expect(screen.getByText("boom")).toBeInTheDocument());
    await user.click(screen.getByRole("button", { name: "Dismiss" }));
    await waitFor(() => expect(screen.queryByText("boom")).not.toBeInTheDocument());
  });

  it("injects providerID/modelID from models list when currentModel has no slash", async () => {
    // Regression: currentModel is stored as a flat ID (e.g. "glm-5.1") with no
    // slash. The old code did indexOf('/') === -1 and silently dropped the model
    // from the prompt body, causing opencode to fall back to the session-level
    // default (opencode-relay/big-pickle) which returned 403 from the relay.
    const user = userEvent.setup();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (workspacesApi.listModels as ReturnType<typeof vi.fn>).mockResolvedValue({
      models: [
        { id: "glm-5.1", providerID: "thekao", name: "GLM 5.1", tier: "paid", freeTier: false, selected: true, enabled: true },
      ],
      currentModel: "glm-5.1",
      currentModelProviderID: "thekao",
    });
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);

    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    await user.click(document.querySelector("textarea")!);
    await user.type(document.querySelector("textarea")!, "hello");
    await user.click(screen.getByRole("button", { name: "Send message" }));

    await waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalledWith(
      "ws-1",
      "sess-1",
      expect.objectContaining({ model: { providerID: "thekao", modelID: "glm-5.1" } }),
    ));
  });

  it("falls back to find() when currentModelProviderID is absent (older API)", async () => {
    // Older API responses may omit currentModelProviderID. The find() fallback
    // must still resolve the correct providerID from the models array.
    const user = userEvent.setup();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (workspacesApi.listModels as ReturnType<typeof vi.fn>).mockResolvedValue({
      models: [
        { id: "glm-5.1", providerID: "thekao", name: "GLM 5.1", tier: "paid", freeTier: false, selected: true, enabled: true },
      ],
      currentModel: "glm-5.1",
      // no currentModelProviderID field
    });
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);

    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    await user.click(document.querySelector("textarea")!);
    await user.type(document.querySelector("textarea")!, "hello");
    await user.click(screen.getByRole("button", { name: "Send message" }));

    await waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalledWith(
      "ws-1",
      "sess-1",
      expect.objectContaining({ model: { providerID: "thekao", modelID: "glm-5.1" } }),
    ));
  });

  it("does not inject model when currentModel is empty", async () => {
    // No model selected: sendAsync must be called without a model field.
    const user = userEvent.setup();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (workspacesApi.listModels as ReturnType<typeof vi.fn>).mockResolvedValue({ models: [], currentModel: "" });
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);

    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());

    await user.click(document.querySelector("textarea")!);
    await user.type(document.querySelector("textarea")!, "hello");
    await user.click(screen.getByRole("button", { name: "Send message" }));

    await waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalledWith(
      "ws-1",
      "sess-1",
      expect.not.objectContaining({ model: expect.anything() }),
    ));
  });
});

describe("ChatPage — session delete", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    capturedContractOptions = null;
    contractState = { seq: 0n, sessions: new Map() };
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockImplementation(async () => ({
      messages: await (messagesApi.getHistory as ReturnType<typeof vi.fn>)(),
      nextCursor: undefined,
    }));
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "Test", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
  });

  it("calls deleteSession when kebab delete is confirmed (#814 ConfirmDialog)", async () => {
    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(screen.getByLabelText("Actions")).toBeInTheDocument());

    const kebab = screen.getByLabelText("Actions");
    await userEvent.click(kebab);

    const deleteBtn = await screen.findByText("Delete session");
    await userEvent.click(deleteBtn);

    // ConfirmDialog opens — click the "Delete" button in the dialog
    const dialogConfirm = await screen.findByRole("button", { name: "Delete" });
    await userEvent.click(dialogConfirm);

    await waitFor(() => {
      expect(workspacesApi.deleteSession).toHaveBeenCalledWith("ws-1", "sess-1");
    });
  });

  it("does not call deleteSession when confirm is cancelled (#814)", async () => {
    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(screen.getByLabelText("Actions")).toBeInTheDocument());

    const kebab = screen.getByLabelText("Actions");
    await userEvent.click(kebab);

    const deleteBtn = await screen.findByText("Delete session");
    await userEvent.click(deleteBtn);

    // ConfirmDialog opens — click "Cancel"
    const cancelBtn = screen.getByRole("button", { name: "Cancel" });
    await userEvent.click(cancelBtn);

    expect(workspacesApi.deleteSession).not.toHaveBeenCalled();
  });

  it("treats 404 as success on delete (#814 ConfirmDialog)", async () => {
    const err404 = new ApiClientError(404, { error: "not found" });
    (workspacesApi.deleteSession as ReturnType<typeof vi.fn>).mockRejectedValueOnce(err404);

    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(screen.getByLabelText("Actions")).toBeInTheDocument());

    const kebab = screen.getByLabelText("Actions");
    await userEvent.click(kebab);

    const deleteBtn = await screen.findByText("Delete session");
    await userEvent.click(deleteBtn);

    const dialogConfirm = await screen.findByRole("button", { name: "Delete" });
    await userEvent.click(dialogConfirm);

    await waitFor(() => {
      expect(workspacesApi.deleteSession).toHaveBeenCalledWith("ws-1", "sess-1");
    });
  });

  it("works in sandboxed iframe (ConfirmDialog renders in DOM, no window.confirm) (#814)", async () => {
    // ConfirmDialog uses Radix DOM portal, so window.confirm is never called.
    // This test verifies the action is available even when window.confirm is blocked.
    (workspacesApi.deleteSession as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    const confirmSpy = vi.spyOn(window, "confirm").mockImplementation(() => { throw new Error("Blocked"); });

    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(screen.getByLabelText("Actions")).toBeInTheDocument());

    const kebab = screen.getByLabelText("Actions");
    await userEvent.click(kebab);

    const deleteBtn = await screen.findByText("Delete session");
    await userEvent.click(deleteBtn);

    // Dialog must render despite window.confirm being blocked
    const dialogConfirm = await screen.findByRole("button", { name: "Delete" });
    await userEvent.click(dialogConfirm);

    await waitFor(() => {
      expect(workspacesApi.deleteSession).toHaveBeenCalledWith("ws-1", "sess-1");
    });
    confirmSpy.mockRestore();
  });

  it("header kebab Force Stop calls abortSession with correct IDs", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });

    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(screen.getByLabelText("Actions")).toBeInTheDocument());

    const kebab = screen.getByLabelText("Actions");
    await userEvent.click(kebab);

    const forceStopBtn = await screen.findByText("Force Stop");
    await userEvent.click(forceStopBtn);

    expect(workspacesApi.abortSession).toHaveBeenCalledWith("ws-1", "sess-1");
  });

  it("header kebab Force Stop fires without confirmation", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });

    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);

    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(screen.getByLabelText("Actions")).toBeInTheDocument());

    const kebab = screen.getByLabelText("Actions");
    await userEvent.click(kebab);

    const forceStopBtn = await screen.findByText("Force Stop");
    await userEvent.click(forceStopBtn);

    expect(workspacesApi.abortSession).toHaveBeenCalledWith("ws-1", "sess-1");
    expect(confirmSpy).not.toHaveBeenCalled();

    confirmSpy.mockRestore();
  });

  it("header kebab Force Stop surfaces alert on failure", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (workspacesApi.abortSession as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("boom"));

    const alertSpy = vi.spyOn(window, "alert").mockImplementation(() => {});

    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(screen.getByLabelText("Actions")).toBeInTheDocument());

    const kebab = screen.getByLabelText("Actions");
    await userEvent.click(kebab);

    const forceStopBtn = await screen.findByText("Force Stop");
    await userEvent.click(forceStopBtn);

    await waitFor(() => {
      expect(alertSpy).toHaveBeenCalledWith("Failed to force stop session.");
    });

    alertSpy.mockRestore();
  });
});
