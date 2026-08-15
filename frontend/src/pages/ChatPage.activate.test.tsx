import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";

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
    useWorkspaceInputSnapshot: () => undefined,
  SessionActivityProvider: ({ children }: { children: any }) => <>{children}</>,
}));
vi.mock("../api/messages", () => {
  const gh = vi.fn().mockResolvedValue([]);
  return { messagesApi: { getHistory: gh, getHistoryPage: vi.fn().mockImplementation(async () => { const msgs = await gh(); return { messages: msgs, nextCursor: undefined }; }), sendAsync: vi.fn(), queueMessage: vi.fn().mockResolvedValue({ messageID: "msg_q_mock" }), getQueue: vi.fn().mockResolvedValue({ messages: [] }) } };
});
vi.mock("../api/sessions", () => ({ sessionsApi: { create: vi.fn() } }));
vi.mock("../hooks/useEventStream", () => ({ useEventStream: vi.fn() }));

import { workspacesApi } from "../api/workspaces";

function renderChat(path: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, refetchInterval: false } } });
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

describe("Workspace activate flow — state machine", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } });
  });

  it("suspended workspace shows banner with resume button, composer disabled", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Suspended" });
    renderChat("/chat/ws-1");
    await waitFor(() => expect(screen.getByText(/is suspended/)).toBeInTheDocument());
    expect(screen.getByRole("button", { name: "Resume to chat" })).toBeInTheDocument();
    expect(document.querySelector("textarea")).toBeDisabled();
  });

  it("clicking resume calls activate API", async () => {
    const user = userEvent.setup();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Suspended" });
    (workspacesApi.activate as ReturnType<typeof vi.fn>).mockResolvedValue({ resumed: "ws-1" });
    renderChat("/chat/ws-1");
    await waitFor(() => expect(screen.getByRole("button", { name: "Resume to chat" })).toBeInTheDocument());
    await user.click(screen.getByRole("button", { name: "Resume to chat" }));
    expect(workspacesApi.activate).toHaveBeenCalledWith("ws-1");
  });

  it("resuming state shows spinner with transitioning message", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Resuming" });
    renderChat("/chat/ws-1");
    await waitFor(() => expect(screen.getByText(/resuming/i)).toBeInTheDocument());
  });

  it("active workspace with session enables composer", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    renderChat("/chat/ws-1/sess-1");
    // Composer enabled when workspaceId + sessionId present and not suspended
    await waitFor(() => expect(document.querySelector("textarea")).not.toBeDisabled());
  });

  it("active workspace without sessionId keeps composer disabled (no session selected)", async () => {
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    renderChat("/chat/ws-1");
    // No sessionId in URL — composer disabled
    await waitFor(() => expect(document.querySelector("textarea")).toBeDisabled());
  });
});
