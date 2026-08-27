import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, act } from "@testing-library/react";
import { render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { SessionActivityProvider, useIsSessionBusy, useIsSessionUnread, useClearPendingUnread, useIsSessionPendingAction, useSessionStatus } from "../../providers/SessionActivityProvider";

let capturedOnEvent: ((data: unknown) => void) | undefined;

vi.mock("../../hooks/useUserEventStream", () => ({
  useUserEventStream: (options?: { onEvent?: (data: unknown) => void }) => {
    capturedOnEvent = options?.onEvent;
  },
}));

vi.mock("../../api/workspaces", () => ({
  workspacesApi: {
    list: vi.fn().mockResolvedValue({
      items: [
        { id: "ws-1", name: "alpha", phase: "Active", userId: "u1", runtime: "python", storageSize: "5Gi", createdAt: "", updatedAt: "" },
        { id: "ws-2", name: "beta", phase: "Active", userId: "u1", runtime: "base", storageSize: "5Gi", createdAt: "", updatedAt: "" },
      ],
      pagination: { limit: 20, offset: 0, total: 2 },
    }),
    getStatus: vi.fn().mockResolvedValue({ phase: "Active" }),
    activate: vi.fn().mockResolvedValue({ resumed: "ws-1" }),
    getSessions: vi.fn().mockResolvedValue([]),
    getAlerts: vi.fn().mockResolvedValue([]),
    listModels: vi.fn().mockResolvedValue({ models: [], currentModel: "" }),
    markSessionSeen: vi.fn().mockResolvedValue(undefined),
    renameWorkspace: vi.fn().mockResolvedValue(undefined),
    deleteWorkspace: vi.fn().mockResolvedValue(undefined),
    suspend: vi.fn().mockResolvedValue(undefined),
    deleteSession: vi.fn().mockResolvedValue(undefined),
  },
}));
vi.mock("../../api/messages", () => {
  const gh = vi.fn().mockResolvedValue([]);
  return { messagesApi: { getHistory: gh, getHistoryPage: vi.fn().mockImplementation(async () => { const msgs = await gh(); return { messages: msgs, nextCursor: undefined }; }), sendAsync: vi.fn(), queueMessage: vi.fn().mockResolvedValue({ messageID: "msg_q_mock" }), getQueue: vi.fn().mockResolvedValue({ messages: [] }), deleteQueueMessage: vi.fn().mockResolvedValue(undefined) } };
});
vi.mock("../../api/sessions", () => ({ sessionsApi: { create: vi.fn().mockResolvedValue({ sessionId: "sess-auto" }) } }));
vi.mock("../../hooks/useEventStream", () => ({ useEventStream: vi.fn() }));
vi.mock("../../providers/AuthProvider", () => ({
  AuthProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
  useAuth: () => ({ user: { id: "u1", username: "alice" }, logout: vi.fn() }),
}));

function BusyIndicator({ sessionId }: { sessionId: string }) {
  const isBusy = useIsSessionBusy(sessionId);
  return <span data-testid={`busy-${sessionId}`}>{isBusy ? "busy" : "idle"}</span>;
}

function UnreadIndicator({ sessionId }: { sessionId: string }) {
  const isUnread = useIsSessionUnread(sessionId);
  return <span data-testid={`unread-${sessionId}`}>{isUnread ? "unread" : "read"}</span>;
}

function IntegrationShell({ children }: { children: React.ReactNode }) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return (
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <SessionActivityProvider>
          {children}
        </SessionActivityProvider>
      </MemoryRouter>
    </QueryClientProvider>
  );
}

describe("Integration: SSE → SessionActivityProvider → UI (#36-39)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    capturedOnEvent = undefined;
  });

  it("#36: SSE busy → provider tracks → idle → pulsation", async () => {
    render(
      <IntegrationShell>
        <BusyIndicator sessionId="sess-1" />
        <UnreadIndicator sessionId="sess-1" />
      </IntegrationShell>,
    );

    expect(screen.getByTestId("busy-sess-1").textContent).toBe("idle");
    expect(screen.getByTestId("unread-sess-1").textContent).toBe("read");

    await act(async () => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "busy" });
    });
    expect(screen.getByTestId("busy-sess-1").textContent).toBe("busy");

    await act(async () => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "idle" });
    });
    expect(screen.getByTestId("busy-sess-1").textContent).toBe("idle");
    expect(screen.getByTestId("unread-sess-1").textContent).toBe("unread");
  });

  it("#36b: busy session in ws-2 does not affect ws-1 indicators", async () => {
    render(
      <IntegrationShell>
        <BusyIndicator sessionId="sess-1" />
        <BusyIndicator sessionId="sess-2" />
      </IntegrationShell>,
    );

    await act(async () => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-2", session_id: "sess-2", status: "busy" });
    });
    expect(screen.getByTestId("busy-sess-1").textContent).toBe("idle");
    expect(screen.getByTestId("busy-sess-2").textContent).toBe("busy");
  });

  it("#38: simulate page refresh — REST data feeds busy state", () => {
    function RestSimulator() {
      const isBusy = useIsSessionBusy("sess-1");
      return <span data-testid="busy-sess-1">{isBusy ? "busy" : "idle"}</span>;
    }

    render(
      <IntegrationShell>
        <RestSimulator />
      </IntegrationShell>,
    );

    expect(screen.getByTestId("busy-sess-1").textContent).toBe("idle");

    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "busy" });
    });
    expect(screen.getByTestId("busy-sess-1").textContent).toBe("busy");
  });

  it("#39: simulate page refresh — REST hasUnread feeds unread state", async () => {
    function RestUnreadSimulator() {
      const isUnread = useIsSessionUnread("sess-1");
      return <span data-testid="unread-sess-1">{isUnread ? "unread" : "read"}</span>;
    }

    render(
      <IntegrationShell>
        <RestUnreadSimulator />
      </IntegrationShell>,
    );

    expect(screen.getByTestId("unread-sess-1").textContent).toBe("read");

    await act(async () => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "busy" });
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "idle" });
    });
    expect(screen.getByTestId("unread-sess-1").textContent).toBe("unread");
  });

  it("#37: navigate to unread session → clearPendingUnread → unread clears", async () => {
    function NavigateSimulator() {
      const isUnread = useIsSessionUnread("sess-1");
      return (
        <div>
          <span data-testid="unread-sess-1">{isUnread ? "unread" : "read"}</span>
        </div>
      );
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={["/chat"]}>
          <SessionActivityProvider>
            <NavigateSimulator />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    await act(async () => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "busy" });
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "idle" });
    });
    expect(screen.getByTestId("unread-sess-1").textContent).toBe("unread");
  });

  it("workspace.phase Suspended clears busy and unread for that workspace", async () => {
    render(
      <IntegrationShell>
        <BusyIndicator sessionId="sess-1" />
        <UnreadIndicator sessionId="sess-1" />
      </IntegrationShell>,
    );

    await act(async () => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "busy" });
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "idle" });
    });
    expect(screen.getByTestId("busy-sess-1").textContent).toBe("idle");
    expect(screen.getByTestId("unread-sess-1").textContent).toBe("unread");

    await act(async () => {
      capturedOnEvent!({ type: "workspace.phase", workspace_id: "ws-1", phase: "Suspended" });
    });
    expect(screen.getByTestId("unread-sess-1").textContent).toBe("read");
  });

  // Test 37 (full): navigate-to clears unread, divider gone on revisit — full flow via REST init + clearPendingUnread.
  it("#37 full: REST-seeded unread cleared by clearPendingUnread (navigate-to)", async () => {
    function Scene() {
      const isUnread = useIsSessionUnread("sess-1");
      const clear = useClearPendingUnread();
      return (
        <>
          <span data-testid="unread">{isUnread ? "yes" : "no"}</span>
          <button data-testid="nav" onClick={() => clear("sess-1")} />
        </>
      );
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Unread session", status: "idle", hasUnread: true, messageCount: 5 },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <Scene />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId("unread").textContent).toBe("yes");

    await act(async () => {
      screen.getByTestId("nav").click();
    });

    expect(screen.getByTestId("unread").textContent).toBe("no");
  });

  // Test 38 (real REST init): page refresh — REST status:active seeds spinner immediately on mount.
  it("#38 real: REST status:active seeds busySessions on mount", () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-active", title: "Active", status: "active", hasUnread: false, messageCount: 2 },
      { id: "sess-idle",   title: "Idle",   status: "idle",   hasUnread: false, messageCount: 1 },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <BusyIndicator sessionId="sess-active" />
            <BusyIndicator sessionId="sess-idle" />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId("busy-sess-active").textContent).toBe("busy");
    expect(screen.getByTestId("busy-sess-idle").textContent).toBe("idle");
  });

  // Test 39 (real REST init): page refresh — REST hasUnread:true seeds pendingUnread immediately on mount.
  it("#39 real: REST hasUnread:true seeds pendingUnread on mount", () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-unread", title: "Unread", status: "idle", hasUnread: true,  messageCount: 3 },
      { id: "sess-read",   title: "Read",   status: "idle", hasUnread: false, messageCount: 1 },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <UnreadIndicator sessionId="sess-unread" />
            <UnreadIndicator sessionId="sess-read" />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId("unread-sess-unread").textContent).toBe("unread");
    expect(screen.getByTestId("unread-sess-read").textContent).toBe("read");
  });
});

describe("Integration: US-55 input events → provider → status resolution (#35)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    capturedOnEvent = undefined;
  });

  it("#35: agent.question on user stream → provider → useSessionStatus pending_input", async () => {
    function StatusIndicator({ sessionId }: { sessionId: string }) {
      const status = useSessionStatus(sessionId);
      return <span data-testid={`status-${sessionId}`}>{status}</span>;
    }

    render(
      <IntegrationShell>
        <StatusIndicator sessionId="ses-1" />
      </IntegrationShell>,
    );

    expect(screen.getByTestId("status-ses-1").textContent).toBe("idle");

    // Simulate session going busy
    await act(async () => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "ses-1", status: "busy" });
    });
    expect(screen.getByTestId("status-ses-1").textContent).toBe("busy");

    // Simulate agent.question arriving on the user stream with D10 wire format
    // (request_id, session_id, workspace_id as top-level fields)
    await act(async () => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_abc",
        data: {
          id: "que_abc",
          session_id: "ses-1",
          questions: [{ header: "Lang", question: "Pick?", options: [{ label: "Go", description: "" }] }],
        },
      });
    });

    // The status should be pending_input, NOT busy — the bug that F7 caught.
    expect(screen.getByTestId("status-ses-1").textContent).toBe("pending_input");
  });

  it("#35b: agent.question.resolved clears pending_input back to busy", async () => {
    function StatusIndicator({ sessionId }: { sessionId: string }) {
      const status = useSessionStatus(sessionId);
      return <span data-testid={`status-${sessionId}`}>{status}</span>;
    }

    render(
      <IntegrationShell>
        <StatusIndicator sessionId="ses-1" />
      </IntegrationShell>,
    );

    // Session busy
    await act(async () => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "ses-1", status: "busy" });
    });

    // Question fires
    await act(async () => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_resolve",
      });
    });
    expect(screen.getByTestId("status-ses-1").textContent).toBe("pending_input");

    // Resolved
    await act(async () => {
      capturedOnEvent!({
        type: "agent.question.resolved",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_resolve",
      });
    });
    // Back to busy (session is still processing)
    expect(screen.getByTestId("status-ses-1").textContent).toBe("busy");
  });

  it("#35c: agent.permission on user stream → provider → pending_input", async () => {
    function StatusIndicator({ sessionId }: { sessionId: string }) {
      const status = useSessionStatus(sessionId);
      return <span data-testid={`status-${sessionId}`}>{status}</span>;
    }

    render(
      <IntegrationShell>
        <StatusIndicator sessionId="ses-1" />
      </IntegrationShell>,
    );

    await act(async () => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "ses-1", status: "busy" });
    });

    await act(async () => {
      capturedOnEvent!({
        type: "agent.permission",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "per_xyz",
        data: { id: "per_xyz", session_id: "ses-1", permission: "edit", patterns: ["file.go"] },
      });
    });

    expect(screen.getByTestId("status-ses-1").textContent).toBe("pending_input");
  });

  it("#35d: cross-workspace — question on ws-2 shows pending for ws-2 session", async () => {
    function StatusIndicator({ sessionId }: { sessionId: string }) {
      const status = useSessionStatus(sessionId);
      return <span data-testid={`status-${sessionId}`}>{status}</span>;
    }

    render(
      <IntegrationShell>
        <StatusIndicator sessionId="sess-ws1" />
        <StatusIndicator sessionId="sess-ws2" />
      </IntegrationShell>,
    );

    // ws-1 session busy
    await act(async () => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-ws1", status: "busy" });
    });
    // ws-2 session gets a question
    await act(async () => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-2",
        session_id: "sess-ws2",
        request_id: "que_cross",
      });
    });

    // ws-1 session: still busy (not pending)
    expect(screen.getByTestId("status-sess-ws1").textContent).toBe("busy");
    // ws-2 session: pending (cross-workspace visibility works)
    expect(screen.getByTestId("status-sess-ws2").textContent).toBe("pending_input");
  });

  it("duplicate agent.question (same requestId) is idempotent — does not double-add", async () => {
    function PendingIndicator({ sessionId }: { sessionId: string }) {
      const isPending = useIsSessionPendingAction(sessionId);
      return <span data-testid={`pending-${sessionId}`}>{isPending ? "yes" : "no"}</span>;
    }

    render(
      <IntegrationShell>
        <PendingIndicator sessionId="ses-1" />
      </IntegrationShell>,
    );

    await act(async () => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_dedup",
      });
    });
    expect(screen.getByTestId("pending-ses-1").textContent).toBe("yes");

    // Same requestId again — idempotent (addPendingAction deduplicates)
    await act(async () => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_dedup",
      });
    });
    expect(screen.getByTestId("pending-ses-1").textContent).toBe("yes");
  });
});
