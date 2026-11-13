/**
 * Integration test: per-session composer drafts through the REAL
 * ChatPage → ChatView → Composer chain (owner ask 2026-11-13).
 *
 * The bug: switching sessions unmounts the Composer (the
 * history-loading spinner branch replaces ChatView), destroying its
 * local text state — typed text was lost forever. These pins drive
 * REAL route navigation between two sessions of one workspace and
 * assert the drafts restore / consume semantics end to end.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../providers/ThemeProvider";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";

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
    abortSession: vi.fn(),
    markSessionSeen: vi.fn().mockResolvedValue(undefined),
    getSessions: vi.fn().mockResolvedValue([]),
  },
}));
vi.mock("../providers/SessionActivityProvider", () => ({
  useIsSessionDeleted: () => false,
  useWhileAwayStalenessSweep: () => {},
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

import { workspacesApi } from "../api/workspaces";
import { messagesApi } from "../api/messages";
import { loadSessionDraft } from "../lib/sessionDrafts";

// Force DESKTOP mode (the composer-history suite's pattern): with
// jsdom's default matchMedia the page renders the mobile shell and
// userEvent typing never reaches the textarea.
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

// In-page navigation: the sidebar's session click is a plain route
// push, so the harness mirrors it with useNavigate buttons.
function NavButtons() {
  const navigate = useNavigate();
  return (
    <>
      <button data-testid="goto-sess-1" onClick={() => navigate("/chat/ws-1/sess-1")}>go 1</button>
      <button data-testid="goto-sess-2" onClick={() => navigate("/chat/ws-1/sess-2")}>go 2</button>
    </>
  );
}

function renderChatPage(path: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ThemeProvider>
        <TooltipProvider delayDuration={0}>
          <MemoryRouter initialEntries={[path]}>
            <Routes>
              <Route path="/chat/:workspaceId/:sessionId" element={<><ChatPage /><NavButtons /></>} />
            </Routes>
          </MemoryRouter>
        </TooltipProvider>
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

async function goto(user: ReturnType<typeof userEvent.setup>, testid: string) {
  await user.click(screen.getByTestId(testid));
}

describe("ChatPage per-session composer drafts", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.clear();
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({
      items: [],
      pagination: { limit: 20, offset: 0, total: 0 },
    });
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({
      phase: "Active",
      sessions: [{ id: "sess-1", status: "idle" }, { id: "sess-2", status: "idle" }],
    });
    // Per-session history markers: waiting for the session's marker is
    // the house pattern (composer-history suite) for "the POST-history
    // composer is live" — a transient pre-isReady composer renders (and
    // is replaced) before history arrives, and typing into it would
    // land on a detached node.
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockImplementation(
      async (_ws: string, sid: string) => ({
        messages: [
          { id: `m-${sid}`, role: "user", parts: [{ type: "text", text: `marker ${sid}` }] },
        ],
        nextCursor: undefined,
      }),
    );
  });

  const composer = () => screen.getByPlaceholderText("Type a message...");
  // The stable post-history composer for a session: its marker has
  // rendered AND the box is enabled.
  const awaitSessionReady = async (sid: string) =>
    waitFor(() => {
      expect(screen.getByText(`marker ${sid}`)).toBeInTheDocument();
      expect(composer()).not.toBeDisabled();
    });

  it("PIN (a): type → switch away → switch back → the draft is restored", async () => {
    const user = userEvent.setup();
    renderChatPage("/chat/ws-1/sess-1");
    await awaitSessionReady("sess-1");
    await user.type(composer(), "draft in session one");

    // Switch to session 2 — the composer is empty for it.
    await goto(user, "goto-sess-2");
    await awaitSessionReady("sess-2");
    expect(composer()).toHaveValue("");

    // Switch back — session 1's draft is restored.
    await goto(user, "goto-sess-1");
    await awaitSessionReady("sess-1");
    expect(composer()).toHaveValue("draft in session one");
  });

  it("PIN (b): send → the draft is consumed (switch back shows empty)", async () => {
    const user = userEvent.setup();
    renderChatPage("/chat/ws-1/sess-1");
    await awaitSessionReady("sess-1");
    await user.type(composer(), "this one sends");
    await user.click(screen.getByRole("button", { name: "Send message" }));
    await waitFor(() => expect(composer()).toHaveValue(""));
    // The store itself is consumed, not just the box.
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("");

    await goto(user, "goto-sess-2");
    await awaitSessionReady("sess-2");
    await goto(user, "goto-sess-1");
    await waitFor(() => expect(screen.getByPlaceholderText("Type a message...")).not.toBeDisabled());
    await waitFor(() => expect(screen.getByPlaceholderText("Type a message...")).toHaveValue(""));
  });

  it("both sessions keep independent drafts across switches", async () => {
    const user = userEvent.setup();
    renderChatPage("/chat/ws-1/sess-1");
    await awaitSessionReady("sess-1");
    await user.type(composer(), "one");

    await goto(user, "goto-sess-2");
    await awaitSessionReady("sess-2");
    await user.type(composer(), "two");

    await goto(user, "goto-sess-1");
    await awaitSessionReady("sess-1");
    expect(composer()).toHaveValue("one");
    await goto(user, "goto-sess-2");
    await awaitSessionReady("sess-2");
    expect(composer()).toHaveValue("two");
    // Store truth, not just the box.
    expect(loadSessionDraft("ws-1", "sess-1")).toBe("one");
    expect(loadSessionDraft("ws-1", "sess-2")).toBe("two");
  });
});
