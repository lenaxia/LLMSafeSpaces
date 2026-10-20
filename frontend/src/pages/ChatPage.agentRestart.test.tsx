/**
 * Tests for ChatPage's #1760 agent-restart surfacing:
 *  - chat-load banner from workspace status (supervisor.lastAgentRestart)
 *  - live agent_restarted platform event → same dismissible banner
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, waitFor, act, screen, fireEvent } from "@testing-library/react";
import { useState } from "react";
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

// Mutable route params + a re-render bump (the hookcount pattern): the
// router reuses ONE ChatPage instance across session/workspace
// switches — the r1 stale-state regression pin needs an in-place
// params change, not a remount.
const paramsRef: Record<string, string | undefined> = {};
const bumpTickRef: { current: (() => void) | null } = { current: null };
vi.mock("react-router-dom", async (orig) => {
  const actual = await orig<typeof import("react-router-dom")>();
  return {
    ...actual,
    useParams: () => paramsRef,
    useNavigate: () => vi.fn(),
  };
});
// ChatPage is rendered INSIDE the wrapper (not passed as children): a
// children prop holds the SAME element reference across re-renders and
// React bails out of the subtree — the params change would never reach
// ChatPage.
function ReRenderWrapper({ qc }: { qc: QueryClient }) {
  const [tick, setTick] = useState(0);
  bumpTickRef.current = () => setTick((t) => t + 1);
  void tick;
  return (
    <QueryClientProvider client={qc}>
      <TooltipProvider delayDuration={0}>
        <ChatPage />
      </TooltipProvider>
    </QueryClientProvider>
  );
}
function bumpTick() {
  bumpTickRef.current?.();
}

import { workspacesApi } from "../api/workspaces";

function makeQueryClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

async function renderReady(qc: QueryClient) {
  paramsRef.workspaceId = "ws-1";
  paramsRef.sessionId = "sess-1";
  render(<ReRenderWrapper qc={qc} />);
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
    // Isolation: clear the params each test re-seeds via renderReady
    // (the hookcount-file pattern — no cross-test param bleed).
    paramsRef.workspaceId = undefined;
    paramsRef.sessionId = undefined;
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

  // r1 stale-state regression pin: the live toast payload must reset on
  // session/workspace switch — a toast received in workspace A must not
  // shadow workspace B's own (empty) status when the router reuses the
  // ChatPage instance.
  //
  // r2 discrimination fix: the assertion MUST run against the SETTLED
  // post-switch state. During ws-2's status-loading window isReady
  // drops and the banner unmounts transiently — asserting null there
  // passes against the pre-fix code too (the r1 shape, reproduced by
  // the reviewer). Only after ws-2 is READY does the stale payload
  // re-mount the banner: settled, pre-fix shows it (fails) and fixed
  // does not (passes).
  it("clears the live payload on session switch (no cross-workspace leak)", async () => {
    const qc = makeQueryClient();
    // Both workspaces Active; ws-2 carries NO restart evidence — the
    // exact shadowing scenario (resolving for ws-2 is what settles
    // readiness post-switch).
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    await renderReady(qc);

    sendPlatformEvent({
      type: "agent_restarted",
      data: { reason: "health_watchdog", at: "2026-10-19T17:08:14Z" },
    } as never);
    expect(await screen.findByTestId("agent-restart-banner")).toBeTruthy();

    // Switch workspace+session in place (same instance, params change).
    await act(async () => {
      paramsRef.workspaceId = "ws-2";
      paramsRef.sessionId = "sess-2";
      bumpTick();
    });

    // Settle FIRST: wait until the ws-2 view is ready again (chat view
    // back after the loading window), then confirm readiness is stable
    // past a render — the transient unmount is gone by then.
    await waitFor(() => {
      expect(screen.getByTestId("chat-view")).toBeTruthy();
    }, { timeout: 3000 });
    await act(async () => {
      bumpTick();
    });
    expect(screen.getByTestId("chat-view")).toBeTruthy();

    // Settled assertion: the banner must be absent in the READY state.
    expect(screen.queryByTestId("agent-restart-banner")).toBeNull();
  });
});
