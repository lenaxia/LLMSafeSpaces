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
vi.mock("../hooks/useEventStream", () => ({ useEventStream: vi.fn() }));

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
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } });
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
  });
  it("shows empty state when no workspace selected", () => {
    renderChatPage("/chat");
    expect(screen.getByText("Select a workspace to start chatting")).toBeInTheDocument();
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

  it("renders messages in chronological order regardless of API response order", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    // API returns newest-first (as opencode does with paginated queries)
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "cc0000000003abcdef", role: "assistant", parts: [{ type: "text", text: "Third" }] },
      { id: "bb0000000002abcdef", role: "user", parts: [{ type: "text", text: "Second" }] },
      { id: "aa0000000001abcdef", role: "user", parts: [{ type: "text", text: "First" }] },
    ]);
    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => {
      const bubbles = screen.getAllByText(/First|Second|Third/);
      expect(bubbles[0]).toHaveTextContent("First");
      expect(bubbles[1]).toHaveTextContent("Second");
      expect(bubbles[2]).toHaveTextContent("Third");
    });
  });

  it("renders messages in chronological order when API returns oldest-first", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    // API returns oldest-first (possible in some opencode versions)
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "aa0000000001abcdef", role: "user", parts: [{ type: "text", text: "First" }] },
      { id: "bb0000000002abcdef", role: "user", parts: [{ type: "text", text: "Second" }] },
      { id: "cc0000000003abcdef", role: "assistant", parts: [{ type: "text", text: "Third" }] },
    ]);
    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => {
      const bubbles = screen.getAllByText(/First|Second|Third/);
      expect(bubbles[0]).toHaveTextContent("First");
      expect(bubbles[1]).toHaveTextContent("Second");
      expect(bubbles[2]).toHaveTextContent("Third");
    });
  });

  it("renders messages in chronological order when API returns shuffled order", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    // Arbitrary/scrambled order
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "bb0000000002abcdef", role: "user", parts: [{ type: "text", text: "Second" }] },
      { id: "dd0000000004abcdef", role: "assistant", parts: [{ type: "text", text: "Fourth" }] },
      { id: "aa0000000001abcdef", role: "user", parts: [{ type: "text", text: "First" }] },
      { id: "cc0000000003abcdef", role: "assistant", parts: [{ type: "text", text: "Third" }] },
    ]);
    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => {
      const bubbles = screen.getAllByText(/First|Second|Third|Fourth/);
      expect(bubbles[0]).toHaveTextContent("First");
      expect(bubbles[1]).toHaveTextContent("Second");
      expect(bubbles[2]).toHaveTextContent("Third");
      expect(bubbles[3]).toHaveTextContent("Fourth");
    });
  });

  it("newest message always renders at the bottom after history refresh", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    // Simulate post-reconcile: newest message has highest ID
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "ff0000000006abcdef", role: "assistant", parts: [{ type: "text", text: "Latest response" }] },
      { id: "ee0000000005abcdef", role: "user", parts: [{ type: "text", text: "Latest question" }] },
      { id: "aa0000000001abcdef", role: "user", parts: [{ type: "text", text: "First message" }] },
    ]);
    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => {
      const bubbles = screen.getAllByText(/First message|Latest question|Latest response/);
      // Newest must be last (bottom)
      expect(bubbles[bubbles.length - 1]).toHaveTextContent("Latest response");
      // Oldest must be first (top)
      expect(bubbles[0]).toHaveTextContent("First message");
    });
  });

  it("local optimistic message appears after history messages", async () => {
    const user = userEvent.setup();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "bb0000000002abcdef", role: "assistant", parts: [{ type: "text", text: "Hi" }] },
      { id: "aa0000000001abcdef", role: "user", parts: [{ type: "text", text: "Hello" }] },
    ]);
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    renderChatPage("/chat/ws-1/sess-1");

    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());
    await user.click(document.querySelector("textarea")!);
    await user.type(document.querySelector("textarea")!, "New message");
    await user.click(screen.getByRole("button", { name: "Send message" }));

    await waitFor(() => {
      // The new local message should render after the history messages
      const allTexts = screen.getAllByText(/Hello|Hi|New message/);
      expect(allTexts[0]).toHaveTextContent("Hello");
      expect(allTexts[1]).toHaveTextContent("Hi");
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
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [{ id: "ws-1", name: "Test", phase: "Active" }],
      pagination: { limit: 20, offset: 0, total: 1 },
    });
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
  });

  it("calls deleteSession when kebab delete is confirmed", async () => {
    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(screen.getByLabelText("Actions")).toBeInTheDocument());

    vi.spyOn(window, "confirm").mockReturnValue(true);

    const kebab = screen.getByLabelText("Actions");
    await userEvent.click(kebab);

    const deleteBtn = await screen.findByText("Delete session");
    await userEvent.click(deleteBtn);

    expect(workspacesApi.deleteSession).toHaveBeenCalledWith("ws-1", "sess-1");
  });

  it("does not call deleteSession when confirm is cancelled", async () => {
    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(screen.getByLabelText("Actions")).toBeInTheDocument());

    vi.spyOn(window, "confirm").mockReturnValue(false);

    const kebab = screen.getByLabelText("Actions");
    await userEvent.click(kebab);

    const deleteBtn = await screen.findByText("Delete session");
    await userEvent.click(deleteBtn);

    expect(workspacesApi.deleteSession).not.toHaveBeenCalled();
  });

  it("treats 404 as success on delete", async () => {
    const err404 = new ApiClientError(404, { error: "not found" });
    (workspacesApi.deleteSession as ReturnType<typeof vi.fn>).mockRejectedValueOnce(err404);

    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(screen.getByLabelText("Actions")).toBeInTheDocument());

    vi.spyOn(window, "confirm").mockReturnValue(true);

    const kebab = screen.getByLabelText("Actions");
    await userEvent.click(kebab);

    const deleteBtn = await screen.findByText("Delete session");
    await userEvent.click(deleteBtn);

    expect(workspacesApi.deleteSession).toHaveBeenCalledWith("ws-1", "sess-1");
  });

  it("proceeds with deletion when window.confirm throws (sandboxed iframe)", async () => {
    (workspacesApi.deleteSession as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);

    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(screen.getByLabelText("Actions")).toBeInTheDocument());

    vi.spyOn(window, "confirm").mockImplementation(() => { throw new Error("Blocked"); });

    const kebab = screen.getByLabelText("Actions");
    await userEvent.click(kebab);

    const deleteBtn = await screen.findByText("Delete session");
    await userEvent.click(deleteBtn);

    expect(workspacesApi.deleteSession).toHaveBeenCalledWith("ws-1", "sess-1");
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
