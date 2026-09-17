import { describe, expect, it, vi, beforeEach } from "vitest";
import { renderHook } from "@testing-library/react";
import { render, screen, act } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { useWhileAwayStalenessSweep, SessionActivityProvider, useIsSessionBusy, useIsSessionUnread, useWorkspaceBusyCount, useClearPendingUnread, useIsSessionPendingAction, useAddPendingAction, useRemovePendingAction, useDropPendingAction, useSessionPendingActions, useAddPendingQuestion, useAddPendingPermission, usePendingQuestionsForSession, usePendingPermissionsForSession, useClearSessionPendingPrompts, resolveSessionStatus, useWorkspaceInputSnapshot } from "./SessionActivityProvider";
import type { InputRequest } from "../api/types";

let capturedOnEvent: ((data: unknown) => void) | undefined;
let capturedOnReconnect: (() => void) | undefined;

vi.mock("../hooks/useUserEventStream", () => ({
  useUserEventStream: (options?: { onEvent?: (data: unknown) => void; onReconnect?: () => void }) => {
    capturedOnEvent = options?.onEvent;
    capturedOnReconnect = options?.onReconnect;
  },
}));

function renderProvider(children?: React.ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const result = render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <SessionActivityProvider>
          {children ?? <div data-testid="child" />}
        </SessionActivityProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { qc, ...result };
}

describe("SessionActivityProvider", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    capturedOnEvent = undefined;
    capturedOnReconnect = undefined;
  });

  it("registers onEvent callback with useUserEventStream", () => {
    renderProvider();
    expect(capturedOnEvent).toBeDefined();
  });

  it("tracks busy session on session.status busy event", () => {
    function BusyIndicator() {
      const isBusy = useIsSessionBusy("sess-1");
      return <span data-testid="busy">{isBusy ? "yes" : "no"}</span>;
    }

    renderProvider(<BusyIndicator />);
    expect(screen.getByTestId("busy").textContent).toBe("no");

    act(() => {
      capturedOnEvent!({
        type: "session.status",
        workspace_id: "ws-1",
        session_id: "sess-1",
        status: "busy",
      });
    });

    expect(screen.getByTestId("busy").textContent).toBe("yes");
  });

  it("clears busy and marks unread on session.status idle event (non-current)", () => {
    function StatusDisplay() {
      const isBusy = useIsSessionBusy("sess-1");
      const isUnread = useIsSessionUnread("sess-1");
      return (
        <>
          <span data-testid="busy">{isBusy ? "yes" : "no"}</span>
          <span data-testid="unread">{isUnread ? "yes" : "no"}</span>
        </>
      );
    }

    renderProvider(<StatusDisplay />);

    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "busy" });
    });
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "idle" });
    });
    expect(screen.getByTestId("busy").textContent).toBe("no");
    expect(screen.getByTestId("unread").textContent).toBe("yes");
  });

  it("does not mark unread for current session on idle", () => {
    function UnreadDisplay() {
      const isUnread = useIsSessionUnread("sess-1");
      return <span data-testid="unread">{isUnread ? "yes" : "no"}</span>;
    }

    render(
      <QueryClientProvider client={new QueryClient()}>
        <MemoryRouter initialEntries={["/chat/ws-1/sess-1"]}>
          <Routes>
            <Route path="/chat/:workspaceId/:sessionId" element={
              <SessionActivityProvider>
                <UnreadDisplay />
              </SessionActivityProvider>
            } />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "busy" });
    });
    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "idle" });
    });

    expect(screen.getByTestId("unread").textContent).toBe("no");
  });

  it("clears all workspace sessions on workspace.phase non-Active", () => {
    function Display() {
      const busy1 = useIsSessionBusy("sess-1");
      const busy2 = useIsSessionBusy("sess-2");
      const unread1 = useIsSessionUnread("sess-1");
      return (
        <>
          <span data-testid="busy1">{busy1 ? "yes" : "no"}</span>
          <span data-testid="busy2">{busy2 ? "yes" : "no"}</span>
          <span data-testid="unread1">{unread1 ? "yes" : "no"}</span>
        </>
      );
    }

    renderProvider(<Display />);

    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "busy" });
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-2", status: "busy" });
    });
    expect(screen.getByTestId("busy1").textContent).toBe("yes");
    expect(screen.getByTestId("busy2").textContent).toBe("yes");

    act(() => {
      capturedOnEvent!({ type: "workspace.phase", workspace_id: "ws-1", phase: "Suspended" });
    });
    expect(screen.getByTestId("busy1").textContent).toBe("no");
    expect(screen.getByTestId("busy2").textContent).toBe("no");
    expect(screen.getByTestId("unread1").textContent).toBe("no");
  });

  it("workspace.phase only clears sessions for that workspace, not others", () => {
    function Display() {
      const busy1 = useIsSessionBusy("sess-1");
      const busy3 = useIsSessionBusy("sess-3");
      const unread1 = useIsSessionUnread("sess-1");
      const unread3 = useIsSessionUnread("sess-3");
      return (
        <>
          <span data-testid="busy1">{busy1 ? "yes" : "no"}</span>
          <span data-testid="busy3">{busy3 ? "yes" : "no"}</span>
          <span data-testid="unread1">{unread1 ? "yes" : "no"}</span>
          <span data-testid="unread3">{unread3 ? "yes" : "no"}</span>
        </>
      );
    }

    renderProvider(<Display />);

    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "busy" });
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-2", session_id: "sess-3", status: "busy" });
    });
    expect(screen.getByTestId("busy1").textContent).toBe("yes");
    expect(screen.getByTestId("busy3").textContent).toBe("yes");

    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "idle" });
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-2", session_id: "sess-3", status: "idle" });
    });
    expect(screen.getByTestId("unread1").textContent).toBe("yes");
    expect(screen.getByTestId("unread3").textContent).toBe("yes");

    act(() => {
      capturedOnEvent!({ type: "workspace.phase", workspace_id: "ws-1", phase: "Suspended" });
    });
    expect(screen.getByTestId("busy1").textContent).toBe("no");
    expect(screen.getByTestId("unread1").textContent).toBe("no");
    expect(screen.getByTestId("busy3").textContent).toBe("no");
    expect(screen.getByTestId("unread3").textContent).toBe("yes");
  });

  it("workspaceBusyCount returns correct count for workspace", () => {
    function CountDisplay() {
      const count = useWorkspaceBusyCount("ws-1");
      return <span data-testid="count">{count}</span>;
    }

    renderProvider(<CountDisplay />);
    expect(screen.getByTestId("count").textContent).toBe("0");

    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "busy" });
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-2", status: "busy" });
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-2", session_id: "sess-3", status: "busy" });
    });
    expect(screen.getByTestId("count").textContent).toBe("2");
  });

  it("clearPendingUnread removes session from unread set", async () => {
    function UnreadWithClear() {
      const isUnread = useIsSessionUnread("sess-1");
      const clear = useClearPendingUnread();
      return (
        <>
          <span data-testid="unread">{isUnread ? "yes" : "no"}</span>
          <button data-testid="clear" onClick={() => clear("sess-1")} />
        </>
      );
    }

    renderProvider(<UnreadWithClear />);

    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "busy" });
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "idle" });
    });
    expect(screen.getByTestId("unread").textContent).toBe("yes");

    await act(() => {
      screen.getByTestId("clear").click();
    });
    expect(screen.getByTestId("unread").textContent).toBe("no");
  });

  it("updates session cache query data on status events", () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Test", messageCount: 0, status: "idle" },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <div />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "busy" });
    });

    const sessions = qc.getQueryData(["sessions", "ws-1"]) as Array<{ id: string; status: string }>;
    expect(sessions.find((s) => s.id === "sess-1")?.status).toBe("active");
  });

  // Test 17: Provider initializes busySessions from REST session cache on mount.
  it("initializes busy state from cached REST data with status:active (#17)", async () => {
    function BusyDisplay() {
      const isBusy = useIsSessionBusy("sess-active");
      return <span data-testid="busy">{isBusy ? "yes" : "no"}</span>;
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-active", title: "Active", messageCount: 0, status: "active", hasUnread: false },
      { id: "sess-idle",   title: "Idle",   messageCount: 0, status: "idle",   hasUnread: false },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <BusyDisplay />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId("busy").textContent).toBe("yes");
  });

  // #752 F4: Cold-start busy detection. The backend session.Status enum is
  // {unknown, idle, busy, error, compacting, archived} — there is NO "active"
  // status (pkg/session/session.go:26-33). The seedBusy path checked only
  // status === "active", so a session the backend marks "busy" was invisible
  // on cold start until an SSE event arrived. This test uses the real backend
  // status value ("busy") and must seed the busy indicator.
  it("initializes busy state from cached REST data with real backend status:busy (#752 F4)", async () => {
    function BusyDisplay() {
      const isBusy = useIsSessionBusy("sess-busy");
      return <span data-testid="busy">{isBusy ? "yes" : "no"}</span>;
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-busy", title: "Busy", messageCount: 0, status: "busy", hasUnread: false },
      { id: "sess-idle", title: "Idle", messageCount: 0, status: "idle", hasUnread: false },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <BusyDisplay />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId("busy").textContent).toBe("yes");
  });

  // Test 23: Provider initializes pendingUnread from REST session cache on mount.
  it("initializes unread state from cached REST data with hasUnread:true (#23)", async () => {
    function UnreadDisplay() {
      const isUnread = useIsSessionUnread("sess-unread");
      return <span data-testid="unread">{isUnread ? "yes" : "no"}</span>;
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-unread", title: "Unread", messageCount: 3, status: "idle", hasUnread: true },
      { id: "sess-seen",   title: "Seen",   messageCount: 1, status: "idle", hasUnread: false },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <UnreadDisplay />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId("unread").textContent).toBe("yes");
  });

  it("does not mark idle+read session as unread on REST init (#23 boundary)", async () => {
    function UnreadDisplay() {
      const isUnread = useIsSessionUnread("sess-seen");
      return <span data-testid="unread">{isUnread ? "yes" : "no"}</span>;
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-seen", title: "Seen", messageCount: 1, status: "idle", hasUnread: false },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <UnreadDisplay />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId("unread").textContent).toBe("no");
  });

  // Regression: SSE idle event must not be clobbered by seedFromCache when
  // the query cache is populated (the clobbering bug introduced by the
  // queryCache.subscribe pattern). The idle handler now writes hasUnread:true
  // into the cache so seedFromCache re-seeds the correct unread state.
  it("SSE idle event preserves unread indicator even when cache is pre-populated (regression)", () => {
    function UnreadDisplay() {
      const isUnread = useIsSessionUnread("sess-1");
      return <span data-testid="unread">{isUnread ? "yes" : "no"}</span>;
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    // Pre-populate cache with hasUnread:false (simulates REST data loaded before SSE)
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Test", messageCount: 0, status: "active", hasUnread: false },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <UnreadDisplay />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    // Session starts not-unread
    expect(screen.getByTestId("unread").textContent).toBe("no");

    // SSE: session goes idle on a non-current workspace (user is not viewing it)
    act(() => {
      capturedOnEvent!({
        type: "session.status",
        workspace_id: "ws-1",
        session_id: "sess-1",
        status: "idle",
      });
    });

    // Unread indicator must survive — seedFromCache must read hasUnread:true
    // (written by the idle handler) and not clobber pendingUnread with stale data
    expect(screen.getByTestId("unread").textContent).toBe("yes");

    // The cache entry must also reflect hasUnread:true for the next seedFromCache
    const sessions = qc.getQueryData(["sessions", "ws-1"]) as Array<{ id: string; hasUnread: boolean }>;
    expect(sessions.find((s) => s.id === "sess-1")?.hasUnread).toBe(true);
  });

  // Regression: clearPendingUnread must suppress re-adding the session from a
  // stale REST refetch (markSessionSeen PUT racing the GET) until REST confirms
  // hasUnread:false. This replaces the old "write hasUnread:false to cache"
  // approach with clearedRef suppression that survives a stale refetch.
  it("clearPendingUnread suppresses stale refetch and releases on REST confirm", () => {
    function Display() {
      const isUnread = useIsSessionUnread("sess-1");
      const clear = useClearPendingUnread();
      return (
        <>
          <span data-testid="unread">{isUnread ? "yes" : "no"}</span>
          <button data-testid="clear" onClick={() => clear("sess-1")}>clear</button>
        </>
      );
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    // Start with session already unread in cache
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Test", messageCount: 1, status: "idle", hasUnread: true },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <Display />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId("unread").textContent).toBe("yes");

    // Clear unread (user navigates to the session)
    act(() => {
      screen.getByTestId("clear").click();
    });

    expect(screen.getByTestId("unread").textContent).toBe("no");

    // Stale refetch: REST still returns hasUnread:true (PUT not committed).
    // reconcileUnread must NOT re-add it (clearedRef suppression).
    act(() => {
      qc.setQueryData(["sessions", "ws-1"], [
        { id: "sess-1", title: "Test", messageCount: 1, status: "idle", hasUnread: true },
      ]);
    });
    expect(screen.getByTestId("unread").textContent).toBe("no");

    // Real refetch: REST now confirms hasUnread:false (PUT committed).
    // clearedRef is released so a future unread response will pulse again.
    act(() => {
      qc.setQueryData(["sessions", "ws-1"], [
        { id: "sess-1", title: "Test", messageCount: 1, status: "idle", hasUnread: false },
      ]);
    });
    expect(screen.getByTestId("unread").textContent).toBe("no");

    // New unread response arrives via REST — should pulse again now that
    // clearedRef was released.
    act(() => {
      qc.setQueryData(["sessions", "ws-1"], [
        { id: "sess-1", title: "Test", messageCount: 2, status: "idle", hasUnread: true },
      ]);
    });
    expect(screen.getByTestId("unread").textContent).toBe("yes");
  });

  // Regression: SSE-tracked busy session must survive a cache refetch that
  // returns status:"idle" for that session. This happens when the REST API
  // enrichment misses the busy state (multi-replica, timing gap, etc.).
  // seedFromCache must preserve SSE-tracked sessions instead of clobbering.
  it("SSE busy state survives cache refetch returning status:idle (regression)", () => {
    function BusyDisplay() {
      const isBusy = useIsSessionBusy("sess-1");
      return <span data-testid="busy">{isBusy ? "yes" : "no"}</span>;
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Test", messageCount: 0, status: "idle", hasUnread: false },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <BusyDisplay />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId("busy").textContent).toBe("no");

    act(() => {
      capturedOnEvent!({
        type: "session.status",
        workspace_id: "ws-1",
        session_id: "sess-1",
        status: "busy",
      });
    });
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    act(() => {
      qc.setQueryData(["sessions", "ws-1"], [
        { id: "sess-1", title: "Test", messageCount: 0, status: "idle", hasUnread: false },
      ]);
    });

    expect(screen.getByTestId("busy").textContent).toBe("yes");
  });

  // Variant: SSE busy in ws-1 survives a refetch of ws-2 sessions
  it("SSE busy state in ws-1 survives cache update for ws-2", () => {
    function Display() {
      const busy1 = useIsSessionBusy("sess-1");
      const busy2 = useIsSessionBusy("sess-2");
      return (
        <>
          <span data-testid="busy1">{busy1 ? "yes" : "no"}</span>
          <span data-testid="busy2">{busy2 ? "yes" : "no"}</span>
        </>
      );
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Test", messageCount: 0, status: "idle", hasUnread: false },
    ]);
    qc.setQueryData(["sessions", "ws-2"], [
      { id: "sess-2", title: "Test", messageCount: 0, status: "idle", hasUnread: false },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <Display />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    act(() => {
      capturedOnEvent!({
        type: "session.status",
        workspace_id: "ws-1",
        session_id: "sess-1",
        status: "busy",
      });
    });
    expect(screen.getByTestId("busy1").textContent).toBe("yes");
    expect(screen.getByTestId("busy2").textContent).toBe("no");

    act(() => {
      qc.setQueryData(["sessions", "ws-2"], [
        { id: "sess-2", title: "Test", messageCount: 0, status: "idle", hasUnread: false },
      ]);
    });

    expect(screen.getByTestId("busy1").textContent).toBe("yes");
    expect(screen.getByTestId("busy2").textContent).toBe("no");
  });

  // Regression: workspace suspend/resume re-seeds from REST. After suspend
  // clears state and removes the workspace from seeded, the next cache update
  // (triggered by sidebar's invalidateQueries on activate) re-seeds busy
  // sessions that were active before the suspend.
  it("workspace suspend then resume re-seeds busy state from REST (regression)", () => {
    function BusyDisplay() {
      const isBusy = useIsSessionBusy("sess-1");
      return <span data-testid="busy">{isBusy ? "yes" : "no"}</span>;
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Test", messageCount: 0, status: "active", hasUnread: false },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <BusyDisplay />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId("busy").textContent).toBe("yes");

    act(() => {
      capturedOnEvent!({ type: "workspace.phase", workspace_id: "ws-1", phase: "Suspended" });
    });
    expect(screen.getByTestId("busy").textContent).toBe("no");

    // Simulate REST returning the session as active after resume
    act(() => {
      qc.setQueryData(["sessions", "ws-1"], [
        { id: "sess-1", title: "Test", messageCount: 0, status: "active", hasUnread: false },
      ]);
    });

    expect(screen.getByTestId("busy").textContent).toBe("yes");
  });

  // Regression: workspace Active phase resets seeding so stale REST data
  // doesn't block re-seeding. This handles the case where the SSE tracker
  // reconnects after a resume and the pod has sessions that are already busy.
  it("workspace.phase Active resets seeding for re-seed", () => {
    function BusyDisplay() {
      const isBusy = useIsSessionBusy("sess-1");
      return <span data-testid="busy">{isBusy ? "yes" : "no"}</span>;
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Test", messageCount: 0, status: "idle", hasUnread: false },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <BusyDisplay />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId("busy").textContent).toBe("no");

    // Simulate resume: phase goes Active, REST now says session is active
    act(() => {
      capturedOnEvent!({ type: "workspace.phase", workspace_id: "ws-1", phase: "Active" });
      qc.setQueryData(["sessions", "ws-1"], [
        { id: "sess-1", title: "Test", messageCount: 0, status: "active", hasUnread: false },
      ]);
    });

    expect(screen.getByTestId("busy").textContent).toBe("yes");
  });

  // Regression: SSE reconnect clears seeded set so workspaces get re-seeded
  // from current REST data. Covers the gap where events are missed during
  // reconnection and REST has the correct state.
  it("SSE reconnect re-seeds from REST (regression)", () => {
    function BusyDisplay() {
      const isBusy = useIsSessionBusy("sess-1");
      return <span data-testid="busy">{isBusy ? "yes" : "no"}</span>;
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Test", messageCount: 0, status: "idle", hasUnread: false },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <BusyDisplay />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId("busy").textContent).toBe("no");

    // Session goes busy via SSE
    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "busy" });
    });
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    // Simulate SSE reconnect — clears seeded
    act(() => {
      capturedOnReconnect!();
    });

    // REST now shows session as active (e.g., different replica or the
    // enrichment caught up). The re-seed should pick it up.
    act(() => {
      qc.setQueryData(["sessions", "ws-1"], [
        { id: "sess-1", title: "Test", messageCount: 0, status: "active", hasUnread: false },
      ]);
    });

    expect(screen.getByTestId("busy").textContent).toBe("yes");
  });

  // Regression: stale busy sessions must be cleared on SSE reconnect.
  // Without clearing, a session that completed during the disconnect gap
  // (idle event lost to replay buffer overflow) shows as permanently busy.
  it("SSE reconnect clears stale busy state (regression)", () => {
    function BusyDisplay() {
      const isBusy = useIsSessionBusy("sess-1");
      return <span data-testid="busy">{isBusy ? "yes" : "no"}</span>;
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Test", messageCount: 0, status: "idle", hasUnread: false },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <BusyDisplay />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "busy" });
    });
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    act(() => {
      capturedOnReconnect!();
    });
    expect(screen.getByTestId("busy").textContent).toBe("no");

    // Re-seed from REST (session completed during gap — REST says idle)
    act(() => {
      qc.setQueryData(["sessions", "ws-1"], [
        { id: "sess-1", title: "Test", messageCount: 0, status: "idle", hasUnread: false },
      ]);
    });
    expect(screen.getByTestId("busy").textContent).toBe("no");
  });

  // Regression: pendingUnread is preserved through SSE reconnect.
  // Unread represents durable information ("session completed that you
  // haven't looked at") and should survive reconnection. The REST re-seed
  // will also re-populate unread from hasUnread in the cache.
  it("SSE reconnect preserves pendingUnread state (regression)", () => {
    function Display() {
      const isBusy = useIsSessionBusy("sess-1");
      const isUnread = useIsSessionUnread("sess-1");
      return (
        <>
          <span data-testid="busy">{isBusy ? "yes" : "no"}</span>
          <span data-testid="unread">{isUnread ? "yes" : "no"}</span>
        </>
      );
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Test", messageCount: 0, status: "idle", hasUnread: false },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <Display />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "busy" });
    });
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "idle" });
    });
    expect(screen.getByTestId("busy").textContent).toBe("no");
    expect(screen.getByTestId("unread").textContent).toBe("yes");

    act(() => {
      capturedOnReconnect!();
    });

    // busy cleared by reconnect, unread preserved
    expect(screen.getByTestId("busy").textContent).toBe("no");
    expect(screen.getByTestId("unread").textContent).toBe("yes");

    // Re-seed from REST — hasUnread in cache preserves unread
    act(() => {
      qc.setQueryData(["sessions", "ws-1"], [
        { id: "sess-1", title: "Test", messageCount: 0, status: "idle", hasUnread: true },
      ]);
    });
    expect(screen.getByTestId("unread").textContent).toBe("yes");
  });

  // Regression (refresh): on a full page refresh, sessions that were already
  // idle with an unread response never receive an SSE idle event (only the
  // busy→idle transition emits one). The unread state must therefore come from
  // the REST hasUnread field. The old seed-once design locked in whatever the
  // FIRST cache read returned — if that read was stale (hasUnread:false because
  // last_message_at hadn't persisted), the session never pulsed. Reconcile
  // re-reads hasUnread on every cache update, so a delayed/stale first read
  // self-heals on the next refetch.
  it("refresh: stale first read self-heals on subsequent refetch (regression)", () => {
    function UnreadDisplay() {
      const isUnread = useIsSessionUnread("sess-1");
      return <span data-testid="unread">{isUnread ? "yes" : "no"}</span>;
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    // Simulate a stale first REST response (hasUnread:false — the async
    // RecordMessage queue hasn't persisted last_message_at yet)
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Test", messageCount: 1, status: "idle", hasUnread: false },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <UnreadDisplay />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId("unread").textContent).toBe("no");

    // The queue drains and the next refetch returns the correct hasUnread:true.
    // Reconcile picks it up immediately — no SSE event required.
    act(() => {
      qc.setQueryData(["sessions", "ws-1"], [
        { id: "sess-1", title: "Test", messageCount: 1, status: "idle", hasUnread: true },
      ]);
    });
    expect(screen.getByTestId("unread").textContent).toBe("yes");
  });

  // Regression (refresh, multiple workspaces): reconcile must add unread
  // sessions from any workspace whose cache data arrives, not just the first.
  it("refresh: reconcile adds unread across multiple workspaces", () => {
    function Display() {
      const u1 = useIsSessionUnread("sess-a");
      const u2 = useIsSessionUnread("sess-b");
      return (
        <>
          <span data-testid="unread-a">{u1 ? "yes" : "no"}</span>
          <span data-testid="unread-b">{u2 ? "yes" : "no"}</span>
        </>
      );
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <Display />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId("unread-a").textContent).toBe("no");
    expect(screen.getByTestId("unread-b").textContent).toBe("no");

    // ws-1 data arrives
    act(() => {
      qc.setQueryData(["sessions", "ws-1"], [
        { id: "sess-a", title: "A", messageCount: 2, status: "idle", hasUnread: true },
      ]);
    });
    expect(screen.getByTestId("unread-a").textContent).toBe("yes");

    // ws-2 data arrives later
    act(() => {
      qc.setQueryData(["sessions", "ws-2"], [
        { id: "sess-b", title: "B", messageCount: 1, status: "idle", hasUnread: true },
      ]);
    });
    expect(screen.getByTestId("unread-a").textContent).toBe("yes");
    expect(screen.getByTestId("unread-b").textContent).toBe("yes");
  });

  // Regression (clear then new activity): after clearPendingUnread, a new SSE
  // idle event (a genuinely new response) must release the clearedRef
  // suppression so the session pulses again.
  it("new SSE idle after clear releases suppression and re-pulses", () => {
    function Display() {
      const isUnread = useIsSessionUnread("sess-1");
      const clear = useClearPendingUnread();
      return (
        <>
          <span data-testid="unread">{isUnread ? "yes" : "no"}</span>
          <button data-testid="clear" onClick={() => clear("sess-1")}>clear</button>
        </>
      );
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Test", messageCount: 1, status: "idle", hasUnread: true },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <Display />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId("unread").textContent).toBe("yes");

    act(() => { screen.getByTestId("clear").click(); });
    expect(screen.getByTestId("unread").textContent).toBe("no");

    // A stale refetch still returns hasUnread:true but must stay suppressed
    act(() => {
      qc.setQueryData(["sessions", "ws-1"], [
        { id: "sess-1", title: "Test", messageCount: 1, status: "idle", hasUnread: true },
      ]);
    });
    expect(screen.getByTestId("unread").textContent).toBe("no");

    // A brand-new response arrives via SSE (busy → idle). The idle handler
    // releases clearedRef and re-marks unread.
    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "busy" });
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "idle" });
    });
    expect(screen.getByTestId("unread").textContent).toBe("yes");
  });

  // Regression (add-only reconcile): an SSE-set unread (a response that just
  // arrived) must survive a stale REST refetch returning hasUnread:false. This
  // happens when RecordMessage hasn't persisted last_message_at yet but the
  // sessions query refetches (e.g. ChatPage invalidates on a session.status
  // event for the current session). The old seed-once design preserved this by
  // never re-reading; reconcile preserves it by being ADD-ONLY.
  it("SSE-set unread survives stale refetch returning hasUnread:false", () => {
    function Display() {
      const isUnread = useIsSessionUnread("sess-1");
      return <span data-testid="unread">{isUnread ? "yes" : "no"}</span>;
    }

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-1", title: "Test", messageCount: 0, status: "idle", hasUnread: false },
    ]);

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <Display />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId("unread").textContent).toBe("no");

    // SSE: a response completes for a non-current session → unread
    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "busy" });
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "idle" });
    });
    expect(screen.getByTestId("unread").textContent).toBe("yes");

    // A refetch returns hasUnread:false (RecordMessage hasn't persisted yet).
    // The unread MUST survive — reconcile is add-only.
    act(() => {
      qc.setQueryData(["sessions", "ws-1"], [
        { id: "sess-1", title: "Test", messageCount: 0, status: "idle", hasUnread: false },
      ]);
    });
    expect(screen.getByTestId("unread").textContent).toBe("yes");
  });
});

describe("SessionActivityProvider — pending actions", () => {
  it("addPendingAction marks session as pending", async () => {
    function PendingDisplay() {
      const add = useAddPendingAction();
      const isPending = useIsSessionPendingAction("sess-1");
      return (
        <>
          <span data-testid="pending">{isPending ? "yes" : "no"}</span>
          <button data-testid="add" onClick={() => add("ws-1", "sess-1", "req-1")} />
        </>
      );
    }

    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })}>
        <MemoryRouter>
          <SessionActivityProvider>
            <PendingDisplay />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId("pending").textContent).toBe("no");

    await act(async () => { screen.getByTestId("add").click(); });
    expect(screen.getByTestId("pending").textContent).toBe("yes");
  });

  it("removePendingAction clears pending state", async () => {
    function PendingDisplay() {
      const add = useAddPendingAction();
      const remove = useRemovePendingAction();
      const isPending = useIsSessionPendingAction("sess-1");
      return (
        <>
          <span data-testid="pending">{isPending ? "yes" : "no"}</span>
          <button data-testid="add" onClick={() => add("ws-1", "sess-1", "req-1")} />
          <button data-testid="remove" onClick={() => remove("req-1")} />
        </>
      );
    }

    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })}>
        <MemoryRouter>
          <SessionActivityProvider>
            <PendingDisplay />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    await act(async () => { screen.getByTestId("add").click(); });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    await act(async () => { screen.getByTestId("remove").click(); });
    expect(screen.getByTestId("pending").textContent).toBe("no");
  });

  it("multiple pending requests — session stays pending until last removed", async () => {
    function PendingDisplay() {
      const add = useAddPendingAction();
      const remove = useRemovePendingAction();
      const isPending = useIsSessionPendingAction("sess-1");
      return (
        <>
          <span data-testid="pending">{isPending ? "yes" : "no"}</span>
          <button data-testid="add1" onClick={() => add("ws-1", "sess-1", "req-1")} />
          <button data-testid="add2" onClick={() => add("ws-1", "sess-1", "req-2")} />
          <button data-testid="rem1" onClick={() => remove("req-1")} />
          <button data-testid="rem2" onClick={() => remove("req-2")} />
        </>
      );
    }

    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })}>
        <MemoryRouter>
          <SessionActivityProvider>
            <PendingDisplay />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    await act(async () => { screen.getByTestId("add1").click(); });
    await act(async () => { screen.getByTestId("add2").click(); });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    await act(async () => { screen.getByTestId("rem1").click(); });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    await act(async () => { screen.getByTestId("rem2").click(); });
    expect(screen.getByTestId("pending").textContent).toBe("no");
  });

  it("session idle clears pending actions", async () => {
    function PendingDisplay() {
      const add = useAddPendingAction();
      const isPending = useIsSessionPendingAction("sess-1");
      return (
        <>
          <span data-testid="pending">{isPending ? "yes" : "no"}</span>
          <button data-testid="add" onClick={() => add("ws-1", "sess-1", "req-1")}>add</button>
        </>
      );
    }

    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })}>
        <MemoryRouter>
          <SessionActivityProvider>
            <PendingDisplay />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    await act(async () => { screen.getByTestId("add").click(); });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "idle" });
    });
    expect(screen.getByTestId("pending").textContent).toBe("no");
  });

  it("useSessionPendingActions returns set of pending session IDs", () => {
    function Display() {
      const pending = useSessionPendingActions();
      return <span data-testid="count">{pending.size}</span>;
    }

    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })}>
        <MemoryRouter>
          <SessionActivityProvider>
            <Display />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId("count").textContent).toBe("0");
  });
});

// --- Pending prompt content (issue #346): content lives in the global layer so
// it survives within-tab session navigation. Filtered by session at read time. ---

function makeQuestion(id: string, sessionId: string, rootSessionId?: string): InputRequest {
  return { id, sessionId, rootSessionId: rootSessionId ?? sessionId, kind: "question" };
}

function makePermission(id: string, sessionId: string, rootSessionId?: string): InputRequest {
  return { id, sessionId, rootSessionId: rootSessionId ?? sessionId, kind: "permission", permission: "bash", patterns: [] };
}

describe("SessionActivityProvider — pending prompt content", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    capturedOnEvent = undefined;
    capturedOnReconnect = undefined;
  });

  it("stores question content and returns it filtered by session", () => {
    function Display({ sessionId }: { sessionId: string }) {
      const addQ = useAddPendingQuestion();
      const questions = usePendingQuestionsForSession(sessionId);
      return (
        <>
          <span data-testid="count">{questions.length}</span>
          <span data-testid="ids">{questions.map((q) => q.id).join(",")}</span>
          <button data-testid="add" onClick={() => addQ("ws-1", makeQuestion("q1", "sess-A"))} />
        </>
      );
    }

    renderProvider(<Display sessionId="sess-A" />);
    expect(screen.getByTestId("count").textContent).toBe("0");

    act(() => {
      screen.getByTestId("add").click();
    });

    expect(screen.getByTestId("count").textContent).toBe("1");
    expect(screen.getByTestId("ids").textContent).toBe("q1");
  });

  it("isolates prompts per session (no clear-on-navigation) — the #346 fix", () => {
    // The provider holds content keyed by request, not by the viewed session.
    // Adding a prompt for session A must NOT be affected by also adding/viewing
    // session B; querying A after touching B still returns A's prompt.
    function Display({ sessionId }: { sessionId: string }) {
      const addQ = useAddPendingQuestion();
      const questions = usePendingQuestionsForSession(sessionId);
      return (
        <>
          <span data-testid={`count-${sessionId}`}>{questions.length}</span>
          <button data-testid="add-A" onClick={() => addQ("ws-1", makeQuestion("qA", "sess-A"))} />
          <button data-testid="add-B" onClick={() => addQ("ws-1", makeQuestion("qB", "sess-B"))} />
        </>
      );
    }

    const { rerender } = renderProvider(<Display sessionId="sess-A" />);
    act(() => screen.getByTestId("add-A").click());
    // "Navigate" to sess-B and add a prompt there.
    rerender(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })}>
        <MemoryRouter>
          <SessionActivityProvider>
            <Display sessionId="sess-B" />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    act(() => screen.getByTestId("add-B").click());
    expect(screen.getByTestId("count-sess-B").textContent).toBe("1");

    // Navigate back to sess-A — its prompt MUST still be present (the bug would
    // have cleared it on the session switch).
    rerender(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })}>
        <MemoryRouter>
          <SessionActivityProvider>
            <Display sessionId="sess-A" />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(screen.getByTestId("count-sess-A").textContent).toBe("1");
  });

  it("matches a subtask prompt to its parent via root_session_id", () => {
    function Display({ sessionId }: { sessionId: string }) {
      const addQ = useAddPendingQuestion();
      const questions = usePendingQuestionsForSession(sessionId);
      return (
        <>
          <span data-testid={`count-${sessionId}`}>{questions.length}</span>
          <button
            data-testid="add"
            // Subtask session "child" whose root is the parent "parent".
            onClick={() => addQ("ws-1", makeQuestion("q1", "child", "parent"))}
          />
        </>
      );
    }

    renderProvider(<Display sessionId="parent" />);
    act(() => screen.getByTestId("add").click());
    // The prompt bubbles to the parent view (root_session_id match)…
    expect(screen.getByTestId("count-parent").textContent).toBe("1");
  });

  it("removePendingAction clears the stored content (resolved event)", () => {
    function Display({ sessionId }: { sessionId: string }) {
      const addQ = useAddPendingQuestion();
      const remove = useRemovePendingAction();
      const questions = usePendingQuestionsForSession(sessionId);
      return (
        <>
          <span data-testid="count">{questions.length}</span>
          <button data-testid="add" onClick={() => addQ("ws-1", makeQuestion("q1", "sess-A"))} />
          <button data-testid="resolve" onClick={() => remove("q1")} />
        </>
      );
    }

    renderProvider(<Display sessionId="sess-A" />);
    act(() => screen.getByTestId("add").click());
    expect(screen.getByTestId("count").textContent).toBe("1");
    act(() => screen.getByTestId("resolve").click());
    expect(screen.getByTestId("count").textContent).toBe("0");
  });

  it("clearSessionPendingPrompts clears content + indicator for one session only (US-16.12 idle/error)", () => {
    // Renders BOTH sessions' pulses/content independently of a viewed-session
    // prop, so we can assert sess-B is untouched after clearing sess-A.
    function Display() {
      const addQ = useAddPendingQuestion();
      const clear = useClearSessionPendingPrompts();
      const questionsA = usePendingQuestionsForSession("sess-A");
      const questionsB = usePendingQuestionsForSession("sess-B");
      const pendingA = useIsSessionPendingAction("sess-A");
      const pendingB = useIsSessionPendingAction("sess-B");
      return (
        <>
          <span data-testid="count-A">{questionsA.length}</span>
          <span data-testid="count-B">{questionsB.length}</span>
          <span data-testid="pulse-A">{pendingA ? "1" : "0"}</span>
          <span data-testid="pulse-B">{pendingB ? "1" : "0"}</span>
          <button data-testid="add-A" onClick={() => addQ("ws-1", makeQuestion("qA", "sess-A"))} />
          <button data-testid="add-B" onClick={() => addQ("ws-1", makeQuestion("qB", "sess-B"))} />
          <button data-testid="clear-A" onClick={() => clear("sess-A")} />
        </>
      );
    }

    renderProvider(<Display />);
    act(() => screen.getByTestId("add-A").click());
    act(() => screen.getByTestId("add-B").click());
    expect(screen.getByTestId("pulse-A").textContent).toBe("1");
    expect(screen.getByTestId("pulse-B").textContent).toBe("1");

    act(() => screen.getByTestId("clear-A").click());

    expect(screen.getByTestId("count-A").textContent).toBe("0");
    expect(screen.getByTestId("pulse-A").textContent).toBe("0");
    // sess-B untouched.
    expect(screen.getByTestId("count-B").textContent).toBe("1");
    expect(screen.getByTestId("pulse-B").textContent).toBe("1");
  });

  it("clearSessionPendingPrompts on a parent does NOT clear or orphan a subtask's prompt", () => {
    // Regression guard: clearing is scoped to session_id, NOT root_session_id.
    // A parent going idle/error must leave its subtask's live prompt + indicator
    // intact (and not delete requestToSessionRef so the subtask can still resolve).
    function Display() {
      const addQ = useAddPendingQuestion();
      const remove = useRemovePendingAction();
      const clear = useClearSessionPendingPrompts();
      // The subtask prompt bubbles to the parent view (root match)…
      const parentView = usePendingQuestionsForSession("parent");
      // …and is present on the subtask's own session too.
      const subtaskOwn = usePendingQuestionsForSession("child");
      const subtaskPulse = useIsSessionPendingAction("child");
      const parentPulse = useIsSessionPendingAction("parent");
      return (
        <>
          <span data-testid="parent-view">{parentView.length}</span>
          <span data-testid="subtask-own">{subtaskOwn.length}</span>
          <span data-testid="subtask-pulse">{subtaskPulse ? "1" : "0"}</span>
          <span data-testid="parent-pulse">{parentPulse ? "1" : "0"}</span>
          <button data-testid="add" onClick={() => addQ("ws-1", makeQuestion("q1", "child", "parent"))} />
          <button data-testid="clear-parent" onClick={() => clear("parent")} />
          <button data-testid="resolve-subtask" onClick={() => remove("q1")} />
        </>
      );
    }

    renderProvider(<Display />);
    act(() => screen.getByTestId("add").click());
    expect(screen.getByTestId("parent-view").textContent).toBe("1");
    expect(screen.getByTestId("subtask-own").textContent).toBe("1");
    expect(screen.getByTestId("subtask-pulse").textContent).toBe("1");

    // Parent goes idle/error — the subtask's prompt must survive.
    act(() => screen.getByTestId("clear-parent").click());
    expect(screen.getByTestId("subtask-own").textContent).toBe("1");
    expect(screen.getByTestId("subtask-pulse").textContent).toBe("1");
    expect(screen.getByTestId("parent-pulse").textContent).toBe("0");

    // And the subtask must still be resolvable (requestToSessionRef intact).
    act(() => screen.getByTestId("resolve-subtask").click());
    expect(screen.getByTestId("subtask-own").textContent).toBe("0");
    expect(screen.getByTestId("subtask-pulse").textContent).toBe("0");
  });

  it("clearWorkspacePendingActions prunes prompt content in lockstep with the indicator", () => {
    // clearWorkspacePendingActions is wired to the workspace.phase event path;
    // a bug that forgets to prune one content map must not pass silently.
    function Display() {
      const addQ = useAddPendingQuestion();
      const addP = useAddPendingPermission();
      const remove = useRemovePendingAction();
      // Access clearWorkspacePendingActions via the context (not exported as a
      // hook, so drive it through a workspace.phase event on the user stream).
      const questionsA = usePendingQuestionsForSession("sess-A");
      const permsA = usePendingPermissionsForSession("sess-A");
      const questionsB = usePendingQuestionsForSession("sess-B");
      return (
        <>
          <span data-testid="q-A">{questionsA.length}</span>
          <span data-testid="p-A">{permsA.length}</span>
          <span data-testid="q-B">{questionsB.length}</span>
          <button data-testid="add-A-q" onClick={() => addQ("ws-1", makeQuestion("qA", "sess-A"))} />
          <button data-testid="add-A-p" onClick={() => addP("ws-1", makePermission("pA", "sess-A"))} />
          <button data-testid="add-B-q" onClick={() => addQ("ws-2", makeQuestion("qB", "sess-B"))} />
          <button data-testid="resolve-A-q" onClick={() => remove("qA")} />
        </>
      );
    }

    renderProvider(<Display />);
    act(() => screen.getByTestId("add-A-q").click());
    act(() => screen.getByTestId("add-A-p").click());
    act(() => screen.getByTestId("add-B-q").click());
    expect(screen.getByTestId("q-A").textContent).toBe("1");
    expect(screen.getByTestId("p-A").textContent).toBe("1");
    expect(screen.getByTestId("q-B").textContent).toBe("1");

    // A non-Active workspace.phase for ws-1 clears all of ws-1's prompt state.
    act(() => {
      capturedOnEvent!({ type: "workspace.phase", workspace_id: "ws-1", phase: "Suspended" });
    });

    expect(screen.getByTestId("q-A").textContent).toBe("0");
    expect(screen.getByTestId("p-A").textContent).toBe("0");
    // ws-2 untouched.
    expect(screen.getByTestId("q-B").textContent).toBe("1");
  });

  it("stores permission content and returns it filtered by session", () => {
    function Display({ sessionId }: { sessionId: string }) {
      const addP = useAddPendingPermission();
      const permissions = usePendingPermissionsForSession(sessionId);
      return (
        <>
          <span data-testid="count">{permissions.length}</span>
          <button data-testid="add" onClick={() => addP("ws-1", makePermission("p1", "sess-A"))} />
        </>
      );
    }

    renderProvider(<Display sessionId="sess-A" />);
    act(() => screen.getByTestId("add").click());
    expect(screen.getByTestId("count").textContent).toBe("1");
  });
});

describe("resolveSessionStatus", () => {
  it("returns pending_input when busy AND pending (the bug that F7 missed)", () => {
    expect(resolveSessionStatus({ isPendingInput: true, isBusy: true, isUnread: false })).toBe("pending_input");
  });

  it("returns pending_input when idle AND pending", () => {
    expect(resolveSessionStatus({ isPendingInput: true, isBusy: false, isUnread: false })).toBe("pending_input");
  });

  it("returns pending_input when pending AND unread (pending outranks unread)", () => {
    expect(resolveSessionStatus({ isPendingInput: true, isBusy: false, isUnread: true })).toBe("pending_input");
  });

  it("returns busy when busy, not pending", () => {
    expect(resolveSessionStatus({ isPendingInput: false, isBusy: true, isUnread: false })).toBe("busy");
  });

  it("returns busy when busy AND unread (busy outranks unread)", () => {
    expect(resolveSessionStatus({ isPendingInput: false, isBusy: true, isUnread: true })).toBe("busy");
  });

  it("returns unread when idle, unread, not pending", () => {
    expect(resolveSessionStatus({ isPendingInput: false, isBusy: false, isUnread: true })).toBe("unread");
  });

  it("returns idle when all false", () => {
    expect(resolveSessionStatus({ isPendingInput: false, isBusy: false, isUnread: false })).toBe("idle");
  });
});

describe("US-55.3: provider onEvent input-event handling", () => {
  function PendingIndicator({ sessionId }: { sessionId: string }) {
    const isPending = useIsSessionPendingAction(sessionId);
    return <span data-testid="pending">{isPending ? "yes" : "no"}</span>;
  }

  it("handles agent.question on user stream → addPendingAction", () => {
    renderProvider(<PendingIndicator sessionId="ses-1" />);
    expect(screen.getByTestId("pending").textContent).toBe("no");

    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_1",
      });
    });

    expect(screen.getByTestId("pending").textContent).toBe("yes");
  });

  it("handles agent.permission on user stream → addPendingAction", () => {
    renderProvider(<PendingIndicator sessionId="ses-1" />);
    expect(screen.getByTestId("pending").textContent).toBe("no");

    act(() => {
      capturedOnEvent!({
        type: "agent.permission",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "per_1",
      });
    });

    expect(screen.getByTestId("pending").textContent).toBe("yes");
  });

  it("handles agent.question.resolved → removePendingAction", () => {
    renderProvider(<PendingIndicator sessionId="ses-1" />);

    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_1",
      });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    act(() => {
      capturedOnEvent!({
        type: "agent.question.resolved",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_1",
      });
    });
    expect(screen.getByTestId("pending").textContent).toBe("no");
  });

  it("handles agent.permission.resolved → removePendingAction", () => {
    renderProvider(<PendingIndicator sessionId="ses-1" />);

    act(() => {
      capturedOnEvent!({
        type: "agent.permission",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "per_1",
      });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    act(() => {
      capturedOnEvent!({
        type: "agent.permission.resolved",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "per_1",
      });
    });
    expect(screen.getByTestId("pending").textContent).toBe("no");
  });

  it("does NOT wipe pendingActions on onReconnect (D9 — no flicker)", () => {
    renderProvider(<PendingIndicator sessionId="ses-1" />);

    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_1",
      });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    act(() => {
      capturedOnReconnect!();
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");
  });

  it("marker commit clears ghost entries (resolved during disconnect)", () => {
    renderProvider(<PendingIndicator sessionId="ses-1" />);

    // Q1 is pending
    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_ghost",
      });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    // Reconnect: no wipe (D9). Then snapshot fires for ws-1 with NO events
    // (Q1 was resolved during disconnect — pod doesn't list it).
    // Marker fires with empty snapshot → pendingActions[ws-1 sessions] cleared.
    act(() => {
      capturedOnReconnect!();
    });
    // Still pending — marker hasn't arrived yet
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    act(() => {
      capturedOnEvent!({
        type: "agent.input.snapshot_complete",
        workspace_id: "ws-1",
      });
    });
    // Ghost cleared by marker commit
    expect(screen.getByTestId("pending").textContent).toBe("no");
  });

  it("marker commit preserves live entries (still pending on pod)", () => {
    renderProvider(<PendingIndicator sessionId="ses-1" />);

    // Q1 is pending
    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_live",
      });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    // Reconnect → snapshot re-emits Q1 (still pending on pod) → marker
    act(() => {
      capturedOnReconnect!();
    });

    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_live",
      });
    });

    act(() => {
      capturedOnEvent!({
        type: "agent.input.snapshot_complete",
        workspace_id: "ws-1",
      });
    });
    // Still pending — marker commit preserves it (re-emitted in snapshot)
    expect(screen.getByTestId("pending").textContent).toBe("yes");
  });

  it("marker commit adds new entries from snapshot", () => {
    renderProvider(<PendingIndicator sessionId="ses-2" />);

    // No pending initially
    expect(screen.getByTestId("pending").textContent).toBe("no");

    // Snapshot delivers Q2 for a different session
    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-2",
        request_id: "que_new",
      });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    act(() => {
      capturedOnEvent!({
        type: "agent.input.snapshot_complete",
        workspace_id: "ws-1",
      });
    });
    // Still pending — marker commit includes it
    expect(screen.getByTestId("pending").textContent).toBe("yes");
  });

  it("live resolve during snapshot window is respected by marker commit", () => {
    renderProvider(<PendingIndicator sessionId="ses-1" />);

    // Q1 pending
    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_race",
      });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    // Reconnect → snapshot re-emits Q1 (still pending) → then live resolve arrives
    act(() => {
      capturedOnReconnect!();
    });

    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_race",
      });
    });

    // Live resolve: removePendingAction fires
    act(() => {
      capturedOnEvent!({
        type: "agent.question.resolved",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_race",
      });
    });
    expect(screen.getByTestId("pending").textContent).toBe("no");

    // Marker fires — should NOT re-add que_race (it was resolved before marker)
    act(() => {
      capturedOnEvent!({
        type: "agent.input.snapshot_complete",
        workspace_id: "ws-1",
      });
    });
    expect(screen.getByTestId("pending").textContent).toBe("no");
  });

  it("marker commit prunes prompt content for ghosted requestIds", () => {
    function ContentDisplay({ sessionId }: { sessionId: string }) {
      const addQuestion = useAddPendingQuestion();
      const questions = usePendingQuestionsForSession(sessionId);
      return (
        <>
          <span data-testid="q-count">{questions.length}</span>
          <button data-testid="add-q" onClick={() => addQuestion("ws-1", makeQuestion("que_content", "ses-1"))}>add</button>
        </>
      );
    }

    renderProvider(<ContentDisplay sessionId="ses-1" />);

    // Add content via addPendingQuestion
    act(() => { screen.getByTestId("add-q").click(); });
    expect(screen.getByTestId("q-count").textContent).toBe("1");

    // Reconnect then marker fires with empty staging (question resolved during disconnect)
    act(() => { capturedOnReconnect!(); });

    act(() => {
      capturedOnEvent!({
        type: "agent.input.snapshot_complete",
        workspace_id: "ws-1",
      });
    });

    // Content should be pruned — no ghost prompt card
    expect(screen.getByTestId("q-count").textContent).toBe("0");
  });
});

describe("D10: snapshot begin/ok semantics (false-wipe fix)", () => {
  function PendingIndicator({ sessionId }: { sessionId: string }) {
    const isPending = useIsSessionPendingAction(sessionId);
    return <span data-testid="pending">{isPending ? "yes" : "no"}</span>;
  }

  function SnapshotStatus({ workspaceId }: { workspaceId: string }) {
    const snap = useWorkspaceInputSnapshot(workspaceId);
    return (
      <span data-testid="snapshot">
        {snap ? `${snap.ok}:${snap.at}` : "none"}
      </span>
    );
  }

  it("ok:false marker keeps existing pending state (fetch failure must not wipe)", () => {
    renderProvider(<PendingIndicator sessionId="ses-1" />);

    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_live",
      });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    // Snapshot attempt fails (pod restarting / transient fetch error)
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_begin", workspace_id: "ws-1" });
    });
    act(() => {
      capturedOnEvent!({
        type: "agent.input.snapshot_complete",
        workspace_id: "ws-1",
        snapshot_ok: false,
      });
    });

    // Live question must survive — no empty-commit wipe
    expect(screen.getByTestId("pending").textContent).toBe("yes");
  });

  it("post-commit live question survives a begin → re-emit → ok marker cycle", () => {
    renderProvider(<PendingIndicator sessionId="ses-1" />);

    // First snapshot: question staged + committed
    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_live",
      });
    });
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_complete", workspace_id: "ws-1" });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    // Second snapshot (e.g. ChatPage mount opening workspace SSE): begin
    // re-opens staging, the pod re-emits the still-live question, marker ok.
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_begin", workspace_id: "ws-1" });
    });
    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_live",
      });
    });
    act(() => {
      capturedOnEvent!({
        type: "agent.input.snapshot_complete",
        workspace_id: "ws-1",
        snapshot_ok: true,
      });
    });

    // Regression: the pre-D10 code never staged post-commit events, so this
    // marker wiped the live question.
    expect(screen.getByTestId("pending").textContent).toBe("yes");
  });

  it("post-commit ghost cleared by begin → ok marker (anti-entropy preserved)", () => {
    renderProvider(<PendingIndicator sessionId="ses-1" />);

    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_ghost",
      });
    });
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_complete", workspace_id: "ws-1" });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    // Question was resolved while disconnected; pod no longer lists it.
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_begin", workspace_id: "ws-1" });
    });
    act(() => {
      capturedOnEvent!({
        type: "agent.input.snapshot_complete",
        workspace_id: "ws-1",
        snapshot_ok: true,
      });
    });

    expect(screen.getByTestId("pending").textContent).toBe("no");
  });

  it("useWorkspaceInputSnapshot exposes ok:false after a failed snapshot", () => {
    renderProvider(
      <>
        <PendingIndicator sessionId="ses-1" />
        <SnapshotStatus workspaceId="ws-1" />
      </>,
    );

    expect(screen.getByTestId("snapshot").textContent).toBe("none");

    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_begin", workspace_id: "ws-1" });
    });
    act(() => {
      capturedOnEvent!({
        type: "agent.input.snapshot_complete",
        workspace_id: "ws-1",
        snapshot_ok: false,
      });
    });

    expect(screen.getByTestId("snapshot").textContent).toMatch(/^false:\d+$/);
  });

  it("useWorkspaceInputSnapshot exposes ok:true after a successful snapshot", () => {
    renderProvider(<SnapshotStatus workspaceId="ws-1" />);

    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_begin", workspace_id: "ws-1" });
    });
    act(() => {
      capturedOnEvent!({
        type: "agent.input.snapshot_complete",
        workspace_id: "ws-1",
        snapshot_ok: true,
      });
    });

    const atBeforeReplay = screen.getByTestId("snapshot").textContent;
    expect(atBeforeReplay).toMatch(/^true:\d+$/);
  });

  it("ok:false marker does not mark the workspace committed (legacy marker can still commit)", () => {
    renderProvider(<PendingIndicator sessionId="ses-1" />);

    // Failed snapshot first — no commit, workspace stays uncommitted
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_begin", workspace_id: "ws-1" });
    });
    act(() => {
      capturedOnEvent!({
        type: "agent.input.snapshot_complete",
        workspace_id: "ws-1",
        snapshot_ok: false,
      });
    });

    // Question arrives organically (uncommitted ws → staged per legacy rule)
    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_new",
      });
    });

    // A later legacy marker (no ok field) commits normally
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_complete", workspace_id: "ws-1" });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");
  });

  it("interleaved flights: live question survives two begin→complete cycles (hard page load)", () => {
    // PR #852 review C2: a hard page load opens the workspace SSE stream AND
    // the user stream ~simultaneously — two snapshot flights for one
    // workspace. The second complete must not commit an authoritative empty
    // over the first's commit.
    renderProvider(<PendingIndicator sessionId="ses-1" />);

    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_begin", workspace_id: "ws-1", snapshot_id: "flight-A" });
    });
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_begin", workspace_id: "ws-1", snapshot_id: "flight-B" });
    });
    // Both flights re-emit the live question
    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_live",
      });
    });
    // First flight completes ok — commits {que_live}
    act(() => {
      capturedOnEvent!({
        type: "agent.input.snapshot_complete",
        workspace_id: "ws-1",
        snapshot_ok: true,
        snapshot_id: "flight-A",
      });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    // Second flight completes ok — must commit ITS staged set ({que_live}),
    // not an empty map produced by consuming the first flight's staging.
    act(() => {
      capturedOnEvent!({
        type: "agent.input.snapshot_complete",
        workspace_id: "ws-1",
        snapshot_ok: true,
        snapshot_id: "flight-B",
      });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");
  });

  it("failed flight does not wipe a concurrent successful flight's commit", () => {
    renderProvider(<PendingIndicator sessionId="ses-1" />);

    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_begin", workspace_id: "ws-1", snapshot_id: "flight-A" });
    });
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_begin", workspace_id: "ws-1", snapshot_id: "flight-B" });
    });
    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_live",
      });
    });
    // Failed flight completes first — keeps state
    act(() => {
      capturedOnEvent!({
        type: "agent.input.snapshot_complete",
        workspace_id: "ws-1",
        snapshot_ok: false,
        snapshot_id: "flight-B",
      });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    // Successful flight commits {que_live}
    act(() => {
      capturedOnEvent!({
        type: "agent.input.snapshot_complete",
        workspace_id: "ws-1",
        snapshot_ok: true,
        snapshot_id: "flight-A",
      });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");
  });

  it("A1: replayed complete(ok) after reconnect does not wipe pending and records no evidence", () => {
    // Review round 4 A1: begin delivered → question delivered → connection
    // drops and reconnects (onReconnect clears flight/commit state) → the
    // broker's id-filtered replay does NOT re-deliver the already-seen
    // begin/question, but the complete arrives fresh. Its flight matches no
    // open flight — committing the empty legacy bucket wiped the live
    // question and the marker was recorded as ok:true evidence (feeding the
    // false auto-abort gate).
    renderProvider(
      <>
        <PendingIndicator sessionId="ses-1" />
        <SnapshotStatus workspaceId="ws-1" />
      </>,
    );

    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_begin", workspace_id: "ws-1", snapshot_id: "flight-A" });
    });
    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_live",
      });
    });
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_complete", workspace_id: "ws-1", snapshot_ok: true, snapshot_id: "flight-A" });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");
    const atBeforeReplay = screen.getByTestId("snapshot").textContent;
    expect(atBeforeReplay).toMatch(/^true:\d+$/);

    // Reconnect: flight/commit state cleared; complete(A) replays without
    // its begin (id-filtered replay skips already-seen events).
    act(() => {
      capturedOnReconnect!();
    });
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_complete", workspace_id: "ws-1", snapshot_ok: true, snapshot_id: "flight-A" });
    });

    // Live question survives AND the stale marker is not recorded as fresh
    // evidence — the recorded `at` is byte-identical (the auto-abort gate
    // requires a snapshot newer than mount; a refresh would re-arm it).
    expect(screen.getByTestId("pending").textContent).toBe("yes");
    expect(screen.getByTestId("snapshot").textContent).toBe(atBeforeReplay);
  });

  it("A2: resync-interleaved unknown-flight complete(ok) does not wipe pending; resync clears evidence", () => {
    renderProvider(
      <>
        <PendingIndicator sessionId="ses-1" />
        <SnapshotStatus workspaceId="ws-1" />
      </>,
    );

    // Commit a live question via a legacy flight
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_begin", workspace_id: "ws-1", snapshot_id: "flight-A" });
    });
    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_live",
      });
    });
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_complete", workspace_id: "ws-1", snapshot_ok: true, snapshot_id: "flight-A" });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");
    const atBeforeReplay = screen.getByTestId("snapshot").textContent;
    expect(atBeforeReplay).toMatch(/^true:\d+$/);

    // Broker signals a drop, then a complete whose begin was dropped arrives
    act(() => {
      capturedOnEvent!({ type: "resync" });
    });
    expect(screen.getByTestId("snapshot").textContent).toBe("none");
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_complete", workspace_id: "ws-1", snapshot_ok: true, snapshot_id: "flight-B" });
    });

    // Live question survives; unknown-flight marker records no evidence
    expect(screen.getByTestId("pending").textContent).toBe("yes");
    expect(screen.getByTestId("snapshot").textContent).toBe("none");
  });

  it("R2: unknown-flight complete(ok) on a committed workspace does not wipe pending (dropped begin)", () => {
    // Broker backpressure drops the begin; the question event and the
    // complete still arrive. The complete's flight matches no open flight and
    // the workspace is committed — committing the empty legacy bucket here
    // wiped live prompts (review round 3 R2).
    renderProvider(<PendingIndicator sessionId="ses-1" />);

    // First snapshot commits the question
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_begin", workspace_id: "ws-1", snapshot_id: "flight-A" });
    });
    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_live",
      });
    });
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_complete", workspace_id: "ws-1", snapshot_ok: true, snapshot_id: "flight-A" });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    // begin(flight-B) is DROPPED by the broker; the question event and
    // complete(flight-B) still arrive
    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_live",
      });
    });
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_complete", workspace_id: "ws-1", snapshot_ok: true, snapshot_id: "flight-B" });
    });

    // Live question must survive the unknown-flight complete
    expect(screen.getByTestId("pending").textContent).toBe("yes");
  });

  it("R2: resync clears the commit gate so subsequent events stage again", () => {
    renderProvider(<PendingIndicator sessionId="ses-1" />);

    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_1",
      });
    });
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_complete", workspace_id: "ws-1" });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    // Broker signals a drop — committed state is unreliable
    act(() => {
      capturedOnEvent!({ type: "resync" });
    });

    // A NEW question on the (now uncommitted) workspace stages again and a
    // legacy marker commit preserves it.
    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_2",
      });
    });
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_complete", workspace_id: "ws-1" });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");
  });

  it("superseded flight staging does not leak into a later flight's commit", () => {
    renderProvider(<PendingIndicator sessionId="ses-1" />);

    // Flight A opens, question staged into A
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_begin", workspace_id: "ws-1", snapshot_id: "flight-A" });
    });
    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_ghost",
      });
    });
    // Flight B opens AFTER — its fetch (post-resolve) does not list the
    // question; its begin starts a fresh staging map.
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_begin", workspace_id: "ws-1", snapshot_id: "flight-B" });
    });
    act(() => {
      capturedOnEvent!({
        type: "agent.input.snapshot_complete",
        workspace_id: "ws-1",
        snapshot_ok: true,
        snapshot_id: "flight-B",
      });
    });
    // Ghost cleared by B's authoritative commit
    expect(screen.getByTestId("pending").textContent).toBe("no");
  });
});

describe("agent_died handler", () => {
  function BusyIndicator({ sessionId }: { sessionId: string }) {
    const isBusy = useIsSessionBusy(sessionId);
    return <span data-testid="busy">{isBusy ? "yes" : "no"}</span>;
  }

  it("clears busy state for the workspace on agent_died", () => {
    renderProvider(<BusyIndicator sessionId="ses-1" />);

    act(() => {
      capturedOnEvent!({
        type: "session.status",
        workspace_id: "ws-1",
        session_id: "ses-1",
        status: "busy",
      });
    });
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    act(() => {
      capturedOnEvent!({
        type: "agent_died",
        workspace_id: "ws-1",
      });
    });
    expect(screen.getByTestId("busy").textContent).toBe("no");
  });

  it("does not clear busy state for other workspaces on agent_died", () => {
    renderProvider(
      <>
        <BusyIndicator sessionId="ses-1" />
        <BusyIndicator sessionId="ses-2" />
      </>,
    );

    act(() => {
      capturedOnEvent!({
        type: "session.status",
        workspace_id: "ws-1",
        session_id: "ses-1",
        status: "busy",
      });
    });
    act(() => {
      capturedOnEvent!({
        type: "session.status",
        workspace_id: "ws-2",
        session_id: "ses-2",
        status: "busy",
      });
    });

    act(() => {
      capturedOnEvent!({
        type: "agent_died",
        workspace_id: "ws-1",
      });
    });

    const busyIndicators = screen.getAllByTestId("busy");
    expect(busyIndicators[0]!.textContent).toBe("no"); // ws-1 cleared
    expect(busyIndicators[1]!.textContent).toBe("yes"); // ws-2 untouched
  });

  // #752 F6: unknown SSE event types must not be silently dropped.
  it("logs unknown event types via console.debug (#752 F6)", () => {
    const debugSpy = vi.spyOn(console, "debug").mockImplementation(() => {});
    renderProvider();

    act(() => {
      capturedOnEvent!({ type: "plugin.added" });
    });

    const calls = debugSpy.mock.calls.map((c) => String(c[0]));
    expect(calls.some((msg) => msg.includes("plugin.added") || msg.includes("unhandled")),
      `expected a debug log mentioning "plugin.added" or "unhandled", got: ${JSON.stringify(calls)}`,
    ).toBe(true);

    debugSpy.mockRestore();
  });
});

// --- #1313 whileAway inbox rows ---

describe("whileAway inbox prompts (#1313)", () => {
  function QuestionListView() {
    const pending = usePendingQuestionsForSession("ses-1");
    return (
      <ul>
        {pending.map((q) => (
          <li key={q.id} data-testid={`q-${q.id}`}>
            {q.whileAway ? "AWAY: " : ""}
            {q.question ?? q.id}
          </li>
        ))}
      </ul>
    );
  }

  // 4a D3: the whileAway marker rides the event ENVELOPE; the data is
  // the contract InputRequest (flattened, camelCase).
  const awayEvent = (id: string, question: string) => ({
    type: "agent.question",
    workspace_id: "ws-1",
    session_id: "ses-1",
    request_id: id,
    whileAway: true,
    data: {
      id,
      sessionId: "ses-1",
      rootSessionId: "ses-1",
      kind: "question",
      question,
      header: "H",
      options: [{ label: "A", description: "" }],
    },
  });

  it("stores whileAway content from the user stream (the API-owned surface)", () => {
    renderProvider(<QuestionListView />);
    act(() => {
      capturedOnEvent!(awayEvent("que_w1", "Ship it?"));
    });
    expect(screen.getByTestId("q-que_w1").textContent).toContain("AWAY: Ship it?");
  });

  it("stack: multiple whileAway prompts render together", () => {
    renderProvider(<QuestionListView />);
    act(() => {
      capturedOnEvent!(awayEvent("que_w1", "First?"));
      capturedOnEvent!(awayEvent("que_w2", "Second?"));
      capturedOnEvent!(awayEvent("que_w3", "Third?"));
    });
    expect(screen.getByTestId("q-que_w1")).toBeInTheDocument();
    expect(screen.getByTestId("q-que_w2")).toBeInTheDocument();
    expect(screen.getByTestId("q-que_w3")).toBeInTheDocument();
  });

  it("live (untagged) question events do NOT store content", () => {
    renderProvider(<QuestionListView />);
    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_live1",
      });
    });
    expect(screen.queryByTestId("q-que_live1")).not.toBeInTheDocument();
  });

  it("resolved clears a whileAway prompt", () => {
    renderProvider(<QuestionListView />);
    act(() => {
      capturedOnEvent!(awayEvent("que_w1", "Ship it?"));
    });
    expect(screen.getByTestId("q-que_w1")).toBeInTheDocument();
    act(() => {
      capturedOnEvent!({
        type: "agent.question.resolved",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_w1",
      });
    });
    expect(screen.queryByTestId("q-que_w1")).not.toBeInTheDocument();
  });
});

// --- #1365: pill lifecycle — tombstones, flight-commit races, stack order ---

describe("resolved pill lifecycle (#1365)", () => {
  function PillIndicator({ sessionId }: { sessionId: string }) {
    const pending = useIsSessionPendingAction(sessionId);
    return <span data-testid="pending">{pending ? "yes" : "no"}</span>;
  }

  beforeEach(() => {
    vi.clearAllMocks();
    capturedOnEvent = undefined;
    capturedOnReconnect = undefined;
  });

  it("a resolved ask is not resurrected by a re-presented live event (tombstone)", () => {
    renderProvider(<PillIndicator sessionId="ses-1" />);

    act(() => {
      capturedOnEvent!({ type: "agent.question", workspace_id: "ws-1", session_id: "ses-1", request_id: "que_1" });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    act(() => {
      capturedOnEvent!({ type: "agent.question.resolved", workspace_id: "ws-1", session_id: "ses-1", request_id: "que_1" });
    });
    expect(screen.getByTestId("pending").textContent).toBe("no");

    // A stale re-presentation of the same ask (e.g. an in-flight snapshot
    // event racing the resolution) must not bring the pill back.
    act(() => {
      capturedOnEvent!({ type: "agent.question", workspace_id: "ws-1", session_id: "ses-1", request_id: "que_1" });
    });
    expect(screen.getByTestId("pending").textContent).toBe("no");
  });

  it("a whileAway re-presentation of a resolved ask is ignored", () => {
    function QuestionListView() {
      const pending = usePendingQuestionsForSession("ses-1");
      return (
        <ul>
          {pending.map((q) => (
            <li key={q.id} data-testid={`q-${q.id}`}>{q.id}</li>
          ))}
        </ul>
      );
    }
    renderProvider(<QuestionListView />);

    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_w1",
        whileAway: true,
        data: { id: "que_w1", sessionId: "ses-1", rootSessionId: "ses-1", kind: "question", question: "Again?", options: [] },
      });
    });
    expect(screen.getByTestId("q-que_w1")).toBeDefined();

    act(() => {
      capturedOnEvent!({ type: "agent.question.resolved", workspace_id: "ws-1", session_id: "ses-1", request_id: "que_w1" });
    });
    expect(screen.queryByTestId("q-que_w1")).toBeNull();

    act(() => {
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: "ws-1",
        session_id: "ses-1",
        request_id: "que_w1",
        whileAway: true,
        data: { id: "que_w1", sessionId: "ses-1", rootSessionId: "ses-1", kind: "question", question: "Again?", options: [] },
      });
    });
    expect(screen.queryByTestId("q-que_w1")).toBeNull();
  });

  it("an optimistic 2xx clear survives a racing snapshot flight commit", () => {
    // The incident race: the flight fetched the pending set BEFORE the user's
    // click, and commits AFTER the optimistic clear. The optimistic clear (the
    // reply path's onResolved) does not unstage flight entries — only the
    // tombstone keeps the pill gone.
    function Harness() {
      const remove = useRemovePendingAction();
      return (
        <>
          <PillIndicator sessionId="ses-1" />
          <button data-testid="click-reply" onClick={() => remove("que_race")}>reply 2xx</button>
        </>
      );
    }
    renderProvider(<Harness />);

    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_begin", workspace_id: "ws-1", snapshot_id: "f1" });
      capturedOnEvent!({ type: "agent.question", workspace_id: "ws-1", session_id: "ses-1", request_id: "que_race", snapshot_id: "f1" });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    act(() => {
      screen.getByTestId("click-reply").click();
    });
    expect(screen.getByTestId("pending").textContent).toBe("no");

    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_complete", workspace_id: "ws-1", snapshot_id: "f1", snapshot_ok: true });
    });
    expect(screen.getByTestId("pending").textContent).toBe("no"); // the flight committed the pre-click staged set; the tombstone must hold
  });

  it("absence evidence does not tombstone: a flight-doomed ask re-adds when a later flight re-carries it", () => {
    // Review r1 robustness 1: a successful-but-stale flight can omit a LIVE
    // ask (the incident's own failure mode). The doom must remove the pill
    // but never tombstone — the next flight that re-carries the id re-adds it.
    renderProvider(<PillIndicator sessionId="ses-1" />);

    act(() => {
      capturedOnEvent!({ type: "agent.question", workspace_id: "ws-1", session_id: "ses-1", request_id: "que_live" });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    // Flight 1 commits ok WITHOUT staging que_live (the stale flight's
    // pending set omitted it) — the id is doomed.
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_begin", workspace_id: "ws-1", snapshot_id: "f1" });
      capturedOnEvent!({ type: "agent.input.snapshot_complete", workspace_id: "ws-1", snapshot_id: "f1", snapshot_ok: true });
    });
    expect(screen.getByTestId("pending").textContent).toBe("no"); // the flight's authority removed the omitted ask

    // Flight 2 carries the ask again — it was live all along.
    act(() => {
      capturedOnEvent!({ type: "agent.input.snapshot_begin", workspace_id: "ws-1", snapshot_id: "f2" });
      capturedOnEvent!({ type: "agent.question", workspace_id: "ws-1", session_id: "ses-1", request_id: "que_live", snapshot_id: "f2" });
      capturedOnEvent!({ type: "agent.input.snapshot_complete", workspace_id: "ws-1", snapshot_id: "f2", snapshot_ok: true });
    });
    expect(screen.getByTestId("pending").textContent).toBe("yes"); // absence evidence must not permanently hide a live ask
  });

  it("dropPendingAction (fold-sync absence) re-adds on re-presentation; removePendingAction (resolution) does not", () => {
    function Harness() {
      const drop = useDropPendingAction();
      const remove = useRemovePendingAction();
      return (
        <>
          <PillIndicator sessionId="ses-1" />
          <button data-testid="drop" onClick={() => drop("que_fold")}>drop</button>
          <button data-testid="resolve" onClick={() => remove("que_fold")}>resolve</button>
        </>
      );
    }
    renderProvider(<Harness />);

    const present = () => {
      act(() => {
        capturedOnEvent!({ type: "agent.question", workspace_id: "ws-1", session_id: "ses-1", request_id: "que_fold" });
      });
    };
    present();
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    act(() => { screen.getByTestId("drop").click(); });
    expect(screen.getByTestId("pending").textContent).toBe("no");
    present();
    expect(screen.getByTestId("pending").textContent).toBe("yes"); // fold-absence removal must not tombstone

    act(() => { screen.getByTestId("resolve").click(); });
    expect(screen.getByTestId("pending").textContent).toBe("no");
    present();
    expect(screen.getByTestId("pending").textContent).toBe("no"); // resolution removal tombstones — the pill stays cleared
  });

  it("bulk session clear is lifecycle, not resolution: whileAway re-presentation re-adds", () => {
    function Harness() {
      const clear = useClearSessionPendingPrompts();
      return (
        <>
          <PillIndicator sessionId="ses-1" />
          <button data-testid="clear" onClick={() => clear("ses-1")}>clear</button>
        </>
      );
    }
    const awayEvent = (id: string) => ({
      type: "agent.question",
      workspace_id: "ws-1",
      session_id: "ses-1",
      request_id: id,
      whileAway: true,
      data: { id, sessionId: "ses-1", rootSessionId: "ses-1", kind: "question", question: "Bulk?", options: [] },
    });
    renderProvider(<Harness />);

    act(() => { capturedOnEvent!(awayEvent("que_bulk")); });
    expect(screen.getByTestId("pending").textContent).toBe("yes");

    act(() => { screen.getByTestId("clear").click(); });
    expect(screen.getByTestId("pending").textContent).toBe("no");

    act(() => { capturedOnEvent!(awayEvent("que_bulk")); });
    expect(screen.getByTestId("pending").textContent).toBe("yes"); // a lifecycle clear is not resolution — the inbox may legitimately re-present
  });

  it("pending questions render newest-activity-first", () => {
    function QuestionListView() {
      const pending = usePendingQuestionsForSession("ses-1");
      return (
        <ul>
          {pending.map((q) => (
            <li key={q.id} data-testid={`q-${q.id}`}>{q.id}</li>
          ))}
        </ul>
      );
    }
    function AddButtons() {
      const addQ = useAddPendingQuestion();
      return (
        <>
          <button data-testid="add-1" onClick={() => addQ("ws-1", makeQuestion("q1", "ses-1"))}>1</button>
          <button data-testid="add-2" onClick={() => addQ("ws-1", makeQuestion("q2", "ses-1"))}>2</button>
          <button data-testid="add-3" onClick={() => addQ("ws-1", makeQuestion("q3", "ses-1"))}>3</button>
        </>
      );
    }
    renderProvider(
      <>
        <AddButtons />
        <QuestionListView />
      </>,
    );

    vi.useFakeTimers({ now: 1_000_000 });
    try {
      act(() => { vi.setSystemTime(1_000_000); screen.getByTestId("add-1").click(); });
      act(() => { vi.setSystemTime(1_001_000); screen.getByTestId("add-2").click(); });
      act(() => { vi.setSystemTime(1_002_000); screen.getByTestId("add-3").click(); });
    } finally {
      vi.useRealTimers();
    }

    const items = screen.getAllByTestId(/^q-/).map((el) => el.getAttribute("data-testid"));
    expect(items).toEqual(["q-q3", "q-q2", "q-q1"]); // the live ask (newest) leads the stack
  });
});

// #1365: the whileAway staleness backstop keys off a client receivedAt
// stamp — stamped on store entry, preserved across replays (a replay must
// not extend the lease).
describe("receivedAt stamping (#1365)", () => {
  function QuestionContentProbe({ requestId }: { requestId: string }) {
    const questions = usePendingQuestionsForSession("ses-stamp");
    const q = questions.find((x) => x.id === requestId);
    return <span data-testid="stamp">{q ? String(q.receivedAt ?? "none") : "gone"}</span>;
  }

  beforeEach(() => {
    vi.clearAllMocks();
    capturedOnEvent = undefined;
    capturedOnReconnect = undefined;
  });

  it("stamps receivedAt on entry and preserves the ORIGINAL stamp across a re-presented event", () => {
    vi.useFakeTimers({ now: 1_000_000 });
    try {
      renderProvider(<QuestionContentProbe requestId="que_stamp" />);
      // whileAway-tagged re-presentation carries content
      act(() => {
        capturedOnEvent!({
          type: "agent.question",
          workspace_id: "ws-1",
          session_id: "ses-stamp",
          request_id: "que_stamp",
          whileAway: true,
          data: { id: "que_stamp", sessionId: "ses-stamp", kind: "question", question: "q", options: [{ label: "a", description: "d" }] },
        });
      });
      expect(screen.getByTestId("stamp").textContent).toBe("1000000");
      // A replay an hour later: the ORIGINAL stamp is preserved (the pill
      // stays droppable by the staleness bound; replay never extends it)
      act(() => {
        vi.setSystemTime(1_000_000 + 3_600_000);
        capturedOnEvent!({
          type: "agent.question",
          workspace_id: "ws-1",
          session_id: "ses-stamp",
          request_id: "que_stamp",
          whileAway: true,
          data: { id: "que_stamp", sessionId: "ses-stamp", kind: "question", question: "q", options: [{ label: "a", description: "d" }] },
        });
      });
      expect(screen.getByTestId("stamp").textContent).toBe("1000000");
    } finally {
      vi.useRealTimers();
    }
  });
});

// #1365 case 4 (review r2): the staleness bound must be TIMER-driven —
// a lazy in-effect check never fires on a healthy-but-quiet stream whose
// fold-sync deps don't tick. Pins: within-bound retained, past-bound
// dropped WITH NO dep change (interval alone), non-whileAway untouched.
describe("useWhileAwayStalenessSweep (#1365, timer-driven)", () => {
  it("drops a stale whileAway pill via the interval with no list/dep changes", async () => {
    vi.useFakeTimers({ now: 1_000_000, shouldAdvanceTime: false });
    const drop = vi.fn();
    const stale = [{ id: "que_stale", sessionId: "s", kind: "question" as const, whileAway: true, receivedAt: 1_000_000 }];
    const fresh = [{ id: "que_fresh", sessionId: "s", kind: "question" as const, whileAway: true, receivedAt: 1_000_000 + 5 * 60 * 1000 }];
    const live = [{ id: "que_live", sessionId: "s", kind: "question" as const }];
    const { rerender } = renderHook(() => useWhileAwayStalenessSweep([...stale, ...fresh, ...live], [], drop));
    // Within the bound: nothing dropped (fresh sweep at mount)
    expect(drop).not.toHaveBeenCalledWith("que_stale");
    // Time passes with NO list changes, NO rerenders — interval only
    act(() => { vi.setSystemTime(1_000_000 + 11 * 60 * 1000); });
    act(() => { vi.advanceTimersByTime(61_000); });
    expect(drop).toHaveBeenCalledWith("que_stale");     // past bound → dropped
    expect(drop).not.toHaveBeenCalledWith("que_fresh"); // within bound → kept
    expect(drop).not.toHaveBeenCalledWith("que_live");  // non-whileAway → untouched
    rerender();
    vi.useRealTimers();
  });

  it("arms no timer when no whileAway pills are pending", () => {
    vi.useFakeTimers({ now: 1_000_000 });
    const drop = vi.fn();
    renderHook(() => useWhileAwayStalenessSweep([{ id: "que_live", sessionId: "s", kind: "question" as const }], [], drop));
    act(() => { vi.setSystemTime(1_000_000 + 60 * 60 * 1000); });
    act(() => { vi.advanceTimersByTime(60 * 60 * 1000); });
    expect(drop).not.toHaveBeenCalled();
    vi.useRealTimers();
  });
});
