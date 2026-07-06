/**
 * Integration test: ChatPage → ChatView → Composer user-message history.
 *
 * Verifies that user messages in the loaded history are extracted and
 * threaded down to the Composer for Up/Arrow navigation. Uses the REAL
 * ChatView and Composer (not mocked) so the full prop chain is exercised.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../providers/ThemeProvider";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";

// Force DESKTOP mode: useIsMobile returns !matchMedia("(min-width: 768px)").matches.
// jsdom returns matches:false by default → useIsMobile=true (mobile), which
// disables history navigation. We mock matchMedia to return matches:true for
// the min-width query so history nav is active.
beforeEach(() => {
  vi.spyOn(window, "matchMedia").mockImplementation((query) => ({
    matches: query.includes("min-width"),
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  }) as unknown as MediaQueryList);
});

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

function renderChatPage(path = "/chat") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TooltipProvider delayDuration={0}>
          <MemoryRouter initialEntries={[path]}>
            <Routes>
              <Route path="/chat/:workspaceId/:sessionId" element={<ChatPage />} />
            </Routes>
          </MemoryRouter>
        </TooltipProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

describe("ChatPage user-message history wiring", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [],
      pagination: { limit: 20, offset: 0, total: 0 },
    });
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({
      phase: "Active",
      sessions: [{ id: "sess-1", status: "idle" }],
    });
  });

  it("Up on first line loads the most recent user message from history", async () => {
    const user = userEvent.setup();
    // History with two user turns. Chronological order (oldest first).
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "u1", role: "user", parts: [{ type: "text", text: "first question" }] },
      { id: "a1", role: "assistant", parts: [{ type: "text", text: "first answer" }] },
      { id: "u2", role: "user", parts: [{ type: "text", text: "second question" }] },
      { id: "a2", role: "assistant", parts: [{ type: "text", text: "second answer" }] },
    ]);

    renderChatPage("/chat/ws-1/sess-1");
    // Wait for history to load AND render before interacting — otherwise the
    // textarea re-mounts when history arrives and our click lands on a stale node.
    await waitFor(() => expect(screen.getByText("second question")).toBeInTheDocument());
    const textarea = screen.getByPlaceholderText("Type a message...");
    expect(textarea).not.toBeDisabled();

    await user.click(textarea); // focus, cursor at 0 (first line)
    await user.keyboard("{ArrowUp}");

    // Newest-first: the most recent user message is "second question".
    await waitFor(() => expect(textarea).toHaveValue("second question"));
  });

  it("repeated Up walks further back through user messages only", async () => {
    const user = userEvent.setup();
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "u1", role: "user", parts: [{ type: "text", text: "old question" }] },
      { id: "a1", role: "assistant", parts: [{ type: "text", text: "old answer" }] },
      { id: "u2", role: "user", parts: [{ type: "text", text: "new question" }] },
      { id: "a2", role: "assistant", parts: [{ type: "text", text: "new answer" }] },
    ]);

    renderChatPage("/chat/ws-1/sess-1");
    await waitFor(() => expect(screen.getByText("new question")).toBeInTheDocument());
    const textarea = screen.getByPlaceholderText("Type a message...");

    await user.click(textarea);
    await user.keyboard("{ArrowUp}"); // → new question
    await user.keyboard("{ArrowUp}"); // → old question
    await waitFor(() => expect(textarea).toHaveValue("old question"));
  });

  it("empty history: textarea remains editable (no history loaded)", async () => {
    const user = userEvent.setup();
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    renderChatPage("/chat/ws-1/sess-1");
    await screen.findByPlaceholderText("Type a message...");
    // Wait for the workspace to be Active and the composer enabled.
    await waitFor(() => expect(screen.getByPlaceholderText("Type a message...")).not.toBeDisabled());

    // With no history, Up/Down can't navigate. We verify the textarea is
    // usable — the no-op behavior itself is covered by the Composer unit
    // tests, which assert Up doesn't load anything when the history list
    // is empty.
    const textarea = screen.getByPlaceholderText("Type a message...");
    await user.type(textarea, "hello");
    expect(textarea).toHaveValue("hello");
  });
});
