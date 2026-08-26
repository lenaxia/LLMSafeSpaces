import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";
import { ApiClientError } from "../api/client";

// LLMSafeSpaces#490: chat page silently renders an empty state when
// the message-history query returns 5xx. Users can't tell "backend
// broken" from "no messages yet." This test asserts the integration:
// when `useMessageHistory` fails with an ApiClientError, the ChatPage
// renders `ChatHistoryErrorBanner` above the message list with the
// user-facing message + a Retry action.

// ── mocks: minimal wiring to render ChatPage ────────────────────────────
vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getStatus: vi.fn().mockResolvedValue({ phase: "Active", sessions: [] }),
    activate: vi.fn(),
    list: vi.fn().mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } }),
    listModels: vi.fn().mockResolvedValue({ models: [], currentModel: "" }),
    setModel: vi.fn().mockResolvedValue({ model: "", applied: false }),
    renameWorkspace: vi.fn().mockResolvedValue({}),
    renameSession: vi.fn().mockResolvedValue({}),
    abortSession: vi.fn(),
    requestInputSnapshot: vi.fn().mockResolvedValue(undefined).mockResolvedValue({}),
    getSession: vi.fn().mockResolvedValue({ title: "" }),
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

// The mock at the top of the file gets replaced inside individual tests
// via `getHistoryPage.mockImplementation(...)`. Default: happy path.
const getHistoryPageMock = vi.fn();
vi.mock("../api/messages", () => ({
  messagesApi: {
    getHistory: vi.fn().mockResolvedValue([]),
    getHistoryPage: (workspaceId: string, sessionId: string, opts: unknown) =>
      getHistoryPageMock(workspaceId, sessionId, opts),
    sendAsync: vi.fn(),
    queueMessage: vi.fn().mockResolvedValue({ messageID: "msg_q_mock" }),
    getQueue: vi.fn().mockResolvedValue({ messages: [] }),
  },
}));
vi.mock("../api/sessions", () => ({ sessionsApi: { create: vi.fn() } }));
vi.mock("../hooks/useEventStream", () => ({ useEventStream: vi.fn() }));

function renderChat(path: string) {
  const qc = new QueryClient({
    // retry:false prevents react-query from re-invoking the mock 3x and
    // muddying the test — we assert exactly one failure surfaced.
    defaultOptions: { queries: { retry: false, refetchInterval: false } },
  });
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

describe("ChatPage — message history error banner (#490)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    getHistoryPageMock.mockReset();
  });

  it("renders the diagnostic banner when message history returns 5xx", async () => {
    // The exact shape #486 hit: opencode's raw envelope pass-through
    // via proxy_handlers.go:154-157. `body.error` DOES NOT exist at
    // the top level — the message is at `body.data.message`, the ref
    // at `body.data.ref`. useInfiniteQuery.error captures the thrown
    // ApiClientError. This test asserts on the RAW production shape,
    // NOT a synthetic top-level `error`-augmented one; a synthetic
    // fixture would let banner-message-extraction bugs pass silently
    // (see the initial #491 review finding).
    const body = {
      name: "UnknownError",
      data: {
        message: "Unexpected server error. Check server logs for details.",
        ref: "err_b8d02ae9",
      },
    } as unknown as ConstructorParameters<typeof ApiClientError>[1];
    getHistoryPageMock.mockRejectedValue(new ApiClientError(500, body));

    renderChat("/chat/ws-1/sess-1");

    // Banner is announced via role=alert so screen readers pick it up.
    await waitFor(() => expect(screen.getByRole("alert")).toBeInTheDocument());
    expect(screen.getByText("Chat history unavailable")).toBeInTheDocument();

    // Details expandable — clicking reveals HTTP status, message, and
    // ref for operators. The message specifically must come from
    // opencode's body.data.message (NOT the literal string "undefined"
    // that ApiClientError.super(body.error) would produce for a body
    // without a top-level `error`).
    fireEvent.click(screen.getByText("Details"));
    expect(screen.getByText("HTTP 500")).toBeInTheDocument();
    expect(screen.getByText(/Unexpected server error/)).toBeInTheDocument();
    expect(screen.getByText("Ref: err_b8d02ae9")).toBeInTheDocument();
    // Regression guard for the first #491 review finding — the literal
    // word "undefined" must never appear.
    expect(screen.queryByText(/^undefined$/)).not.toBeInTheDocument();
  });

  it("does NOT render the banner on the happy path (success with messages)", async () => {
    getHistoryPageMock.mockResolvedValue({
      messages: [
        { id: "msg_1", role: "user", parts: [{ type: "text", text: "hello" }], createdAt: "2026-01-01T00:00:00Z" },
      ],
      nextCursor: undefined,
    });

    renderChat("/chat/ws-1/sess-1");

    // Wait for the loading spinner to clear.
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    expect(screen.queryByText("Chat history unavailable")).not.toBeInTheDocument();
  });

  it("does NOT render the banner on the happy path with an empty session (no messages yet)", async () => {
    // Distinguishes empty from error — this is the exact user experience
    // that was ambiguous pre-#490.
    getHistoryPageMock.mockResolvedValue({ messages: [], nextCursor: undefined });

    renderChat("/chat/ws-1/sess-1");

    await waitFor(() => {
      // Empty state is fine — no banner should be up.
      expect(screen.queryByText("Chat history unavailable")).not.toBeInTheDocument();
    });
  });

  it("Retry button triggers a refetch of the message history", async () => {
    // First call fails; second call (after Retry click) succeeds.
    getHistoryPageMock
      .mockRejectedValueOnce(new ApiClientError(503, { error: "workspace connection failed", retryAfter: 5 }))
      .mockResolvedValueOnce({ messages: [], nextCursor: undefined });

    renderChat("/chat/ws-1/sess-1");

    // Banner up initially.
    await waitFor(() => expect(screen.getByRole("alert")).toBeInTheDocument());
    expect(getHistoryPageMock).toHaveBeenCalledTimes(1);

    // Click Retry.
    fireEvent.click(screen.getByRole("button", { name: /retry/i }));

    // Refetch happened; banner should be gone (mock succeeded on 2nd call).
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    expect(getHistoryPageMock).toHaveBeenCalledTimes(2);
  });
});
