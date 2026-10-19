/**
 * Tests for ChatPage's #1760 agent-restart surfacing:
 *  - chat-load banner from workspace status (supervisor.lastAgentRestart)
 *  - live agent_restarted platform event → same dismissible banner
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, waitFor, act, screen, fireEvent } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";
import type { WorkspaceStreamEvent } from "../api/types";

// --- Mocks (mirrors ChatPage.sse.test.tsx) ---

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

let capturedPlatformHandler: ((data: unknown) => void) | null = null;
vi.mock("../hooks/useEventStream", () => ({
  useEventStream: vi.fn((_workspaceId: string | undefined, handler: (data: unknown) => void) => {
    capturedPlatformHandler = handler;
  }),
}));

vi.mock("../hooks/useContractStream", () => ({
  useContractStream: vi.fn(() => ({ seq: 0n, sessions: new Map() })),
}));

// Mock ChatView (mirrors ChatPage.sse.test.tsx) — the page is "ready"
// once the chat surface mounts.
vi.mock("../components/chat/ChatView", () => ({
  ChatView: () => <div data-testid="chat-view" />,
}));

import { workspacesApi } from "../api/workspaces";

function makeQueryClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

async function renderReady(qc: QueryClient) {
  render(
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
  await waitFor(() => {
    expect(capturedPlatformHandler).not.toBeNull();
  });
  await waitFor(() => {
    expect(screen.getByTestId("chat-view")).toBeTruthy();
  });
}

function sendPlatformEvent(evt: WorkspaceStreamEvent) {
  act(() => {
    capturedPlatformHandler?.(evt);
  });
}

describe("ChatPage agent-restart surfacing (#1760)", () => {
  beforeEach(() => {
    capturedPlatformHandler = null;
    vi.clearAllMocks();
  });

  it("shows the banner on chat load when status carries a recorded restart", async () => {
    const qc = makeQueryClient();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({
      phase: "Active",
      supervisor: {
        agentRestarts: 1,
        lastAgentRestart: {
          reason: "health_watchdog",
          verdict: "hung",
          at: "2026-10-19T17:08:14Z",
        },
        suppressedRestarts: 78,
        lastSuppressedVerdict: "starved",
      },
    });
    await renderReady(qc);

    const banner = await screen.findByTestId("agent-restart-banner");
    expect(banner).toHaveTextContent("restarted by the supervisor health watchdog");
    expect(banner).toHaveTextContent("stopped responding to health checks");
  });

  it("shows no banner when no restart was recorded", async () => {
    const qc = makeQueryClient();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({
      phase: "Active",
      supervisor: { agentRestarts: 0, suppressedRestarts: 74, lastSuppressedVerdict: "starved" },
    });
    await renderReady(qc);

    expect(screen.queryByTestId("agent-restart-banner")).toBeNull();
  });

  it("shows the banner live on the agent_restarted platform event", async () => {
    const qc = makeQueryClient();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    await renderReady(qc);
    expect(screen.queryByTestId("agent-restart-banner")).toBeNull();

    sendPlatformEvent({
      type: "agent_restarted",
      workspace_id: "ws-1",
      data: { reason: "health_watchdog", verdict: "hung", agentRestarts: "1", at: "2026-10-19T17:08:14Z" },
    } as never);

    const banner = await screen.findByTestId("agent-restart-banner");
    expect(banner).toHaveTextContent("restarted by the supervisor health watchdog");
  });

  it("is dismissible", async () => {
    const qc = makeQueryClient();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    await renderReady(qc);

    sendPlatformEvent({
      type: "agent_restarted",
      data: { reason: "health_watchdog", at: "2026-10-19T17:08:14Z" },
    } as never);

    const banner = await screen.findByTestId("agent-restart-banner");
    fireEvent.click(banner.querySelector("button")!);
    expect(screen.queryByTestId("agent-restart-banner")).toBeNull();
  });
});
