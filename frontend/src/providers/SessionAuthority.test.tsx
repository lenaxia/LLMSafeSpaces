import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, screen, act } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import {
  SessionActivityProvider,
  useIsSessionBusy,
  useSessionAuthority,
} from "./SessionActivityProvider";
import { workspacesApi } from "../api/workspaces";
import type { ActiveSessionsResponse } from "../api/types";

let capturedOnEvent: ((data: unknown) => void) | undefined;
let capturedOnReconnect: (() => void) | undefined;

vi.mock("../hooks/useUserEventStream", () => ({
  useUserEventStream: (options?: { onEvent?: (data: unknown) => void; onReconnect?: () => void }) => {
    capturedOnEvent = options?.onEvent;
    capturedOnReconnect = options?.onReconnect;
  },
}));

vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getActiveSessions: vi.fn(),
  },
}));

const getActiveSessionsMock = vi.mocked(workspacesApi.getActiveSessions);

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

function BusyIndicator({ sessionId }: { sessionId: string }) {
  const isBusy = useIsSessionBusy(sessionId);
  return <span data-testid={`busy-${sessionId}`}>{isBusy ? "busy" : "idle"}</span>;
}

describe("SessionActivityProvider authority reconcile (busy truthfulness)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    capturedOnEvent = undefined;
    capturedOnReconnect = undefined;
    getActiveSessionsMock.mockRejectedValue(new Error("not configured"));
  });

  // The core property: a fresh VERIFIED authority read replaces this
  // workspace's busy state — adds missed busy, clears stale busy.
  it("statusz read replaces busy state (add missed busy, clear stale busy)", () => {
    // Fake ONLY Date: the gap must elapse for the grace-window logic,
    // while react-query's fetch/cache machinery keeps real timers.
    vi.useFakeTimers({ toFake: ["Date"] });
    try {
      const { qc } = renderProvider(
        <>
          <BusyIndicator sessionId="sess-missed" />
          <BusyIndicator sessionId="sess-stale" />
        </>,
      );

      // sess-stale went busy via SSE before a disconnect gap (turn ended
      // during the gap; idle event lost). sess-missed started during the gap
      // (busy event lost).
      act(() => {
        capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-stale", status: "busy" });
      });
      expect(screen.getByTestId("busy-sess-stale").textContent).toBe("busy");

      // The gap lasts well past the merge grace window — the busy event is
      // stale, not an in-flight race.
      act(() => {
        vi.advanceTimersByTime(10_000);
      });

      // Authoritative read: sess-missed busy, sess-stale idle.
      act(() => {
        qc.setQueryData(["sessions-active", "ws-1"], {
          active: ["sess-missed"],
          maxActive: 5,
          source: "statusz",
        });
      });

      expect(screen.getByTestId("busy-sess-missed").textContent).toBe("busy");
      expect(screen.getByTestId("busy-sess-stale").textContent).toBe("idle");
    } finally {
      vi.useRealTimers();
    }
  });

  // A failed read must never render as confident idle: "unverified" holds
  // prior state in BOTH directions.
  it("unverified read holds prior state (no clear, no add)", () => {
    const { qc } = renderProvider(
      <>
        <BusyIndicator sessionId="sess-holds-busy" />
        <BusyIndicator sessionId="sess-holds-idle" />
      </>,
    );

    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-holds-busy", status: "busy" });
    });
    expect(screen.getByTestId("busy-sess-holds-busy").textContent).toBe("busy");

    act(() => {
      qc.setQueryData(["sessions-active", "ws-1"], {
        active: [],
        maxActive: 5,
        source: "unverified",
      });
    });

    expect(screen.getByTestId("busy-sess-holds-busy").textContent, "unverified must NOT clear busy (the 946a incident shape)").toBe("busy");
    expect(screen.getByTestId("busy-sess-holds-idle").textContent, "unverified must NOT add busy").toBe("idle");
  });

  // no_pod: the workspace has no pod — nothing can run. An empty set is
  // certain, so it clears (the zombie-import fix's client side).
  it("no_pod read clears busy (zombie display killed)", () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    try {
      const { qc } = renderProvider(<BusyIndicator sessionId="sess-zombie" />);

      act(() => {
        capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-zombie", status: "busy" });
      });
      expect(screen.getByTestId("busy-sess-zombie").textContent).toBe("busy");

      // Suspend cycle: past the grace window before the read.
      act(() => {
        vi.advanceTimersByTime(10_000);
      });

      act(() => {
        qc.setQueryData(["sessions-active", "ws-1"], {
          active: [],
          maxActive: 5,
          source: "no_pod",
        });
      });

      expect(screen.getByTestId("busy-sess-zombie").textContent).toBe("idle");
    } finally {
      vi.useRealTimers();
    }
  });

  // Merge-order rule: a busy SSE event that arrived near the snapshot read
  // wins over a snapshot that missed the turn start (read-in-flight race).
  it("late busy event survives an idle snapshot within the grace window", () => {
    const { qc } = renderProvider(<BusyIndicator sessionId="sess-late" />);

    // Turn starts while the authority read is in flight: busy event
    // arrives client-side just before the snapshot resolves.
    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-late", status: "busy" });
    });

    // Snapshot resolved idle (statusz was read before the turn began).
    act(() => {
      qc.setQueryData(["sessions-active", "ws-1"], {
        active: [],
        maxActive: 5,
        source: "statusz",
      });
    });

    expect(screen.getByTestId("busy-sess-late").textContent, "recent busy event must outrank the in-flight-race snapshot").toBe("busy");
  });

  // The grace window is bounded: an old busy event does NOT protect
  // against a fresh authoritative idle.
  it("stale busy event does not survive a fresh idle snapshot", () => {
    vi.useFakeTimers();
    try {
      const { qc } = renderProvider(<BusyIndicator sessionId="sess-old" />);

      act(() => {
        capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-old", status: "busy" });
      });
      expect(screen.getByTestId("busy-sess-old").textContent).toBe("busy");

      // Well past the 2s grace window.
      act(() => {
        vi.advanceTimersByTime(10_000);
      });

      act(() => {
        qc.setQueryData(["sessions-active", "ws-1"], {
          active: [],
          maxActive: 5,
          source: "statusz",
        });
      });

      expect(screen.getByTestId("busy-sess-old").textContent).toBe("idle");
    } finally {
      vi.useRealTimers();
    }
  });

  // Reconnect is the primary trigger: it must fetch the authority for
  // every workspace with session state and reconcile from the result.
  it("SSE reconnect fetches authority and reconciles (the 946a scenario)", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    try {
      getActiveSessionsMock.mockResolvedValue({ active: ["sess-running"], maxActive: 5, source: "statusz" });

      const { qc } = renderProvider(
        <>
          <BusyIndicator sessionId="sess-running" />
          <BusyIndicator sessionId="sess-was-busy" />
        </>,
      );
      // The client has session state for ws-1 (seededRef/queries exist).
      qc.setQueryData(["sessions", "ws-1"], [
        { id: "sess-running", title: "R", messageCount: 1, status: "idle", hasUnread: false },
        { id: "sess-was-busy", title: "W", messageCount: 1, status: "idle", hasUnread: false },
      ]);

      // API replica died mid-turn; browser reconnects. sess-was-busy went
      // busy via SSE before the drop; its idle was lost in the gap.
      act(() => {
        capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-was-busy", status: "busy" });
      });

      // The disconnect gap spans well past the merge grace window.
      act(() => {
        vi.advanceTimersByTime(30_000);
      });

      await act(async () => {
        capturedOnReconnect!();
      });

      expect(getActiveSessionsMock).toHaveBeenCalledWith("ws-1");
      expect(screen.getByTestId("busy-sess-running").textContent, "turn running through the reconnect must converge to busy").toBe("busy");
      expect(screen.getByTestId("busy-sess-was-busy").textContent, "turn ended during the gap must clear from the authority read").toBe("idle");
    } finally {
      vi.useRealTimers();
    }
  });

  // Broker backpressure (resync) is the drop-without-disconnect window —
  // same reconciliation as reconnect.
  it("resync event fetches authority and reconciles", async () => {
    getActiveSessionsMock.mockResolvedValue({ active: ["sess-resynced"], maxActive: 5, source: "statusz" });

    const { qc } = renderProvider(<BusyIndicator sessionId="sess-resynced" />);
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-resynced", title: "R", messageCount: 1, status: "idle", hasUnread: false },
    ]);

    await act(async () => {
      capturedOnEvent!({ type: "resync" });
    });

    expect(getActiveSessionsMock).toHaveBeenCalledWith("ws-1");
    expect(screen.getByTestId("busy-sess-resynced").textContent).toBe("busy");
  });

  // Authority fetch failure on reconnect holds state (no confident idle).
  it("authority fetch failure on reconnect holds prior state", async () => {
    getActiveSessionsMock.mockRejectedValue(new Error("api down"));

    const { qc } = renderProvider(<BusyIndicator sessionId="sess-hold" />);
    qc.setQueryData(["sessions", "ws-1"], [
      { id: "sess-hold", title: "H", messageCount: 1, status: "idle", hasUnread: false },
    ]);

    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-hold", status: "busy" });
    });
    expect(screen.getByTestId("busy-sess-hold").textContent).toBe("busy");

    await act(async () => {
      capturedOnReconnect!();
    });

    expect(screen.getByTestId("busy-sess-hold").textContent, "failed authority read must hold, not clear").toBe("busy");
  });
});

describe("useSessionAuthority focus refetch (validation #1)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    capturedOnEvent = undefined;
    capturedOnReconnect = undefined;
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  function setVisibility(state: "visible" | "hidden") {
    Object.defineProperty(document, "visibilityState", {
      value: state,
      configurable: true,
    });
    window.dispatchEvent(new Event("visibilitychange"));
  }

  // Validates the blocking assumption of the design: the sessions-active
  // query refetches on window focus via react-query's DEFAULT
  // refetchOnWindowFocus (the app's QueryClientProvider does not disable
  // it), gated by the production staleTime (30s). This is the tab-sleep
  // recovery trigger — no custom wiring. Mirrors the production client
  // config (frontend/src/providers/QueryClientProvider.tsx).
  it("refetches on focus once stale (production config)", async () => {
    vi.useFakeTimers();
    const fetches = vi.fn((): Promise<ActiveSessionsResponse> =>
      Promise.resolve({ active: [], maxActive: 5, source: "statusz" }));
    getActiveSessionsMock.mockImplementation(fetches);

    const qc = new QueryClient({
      defaultOptions: { queries: { staleTime: 30_000, retry: false, gcTime: 0 } },
    });

    function Probe() {
      useSessionAuthority("ws-1");
      return <div data-testid="probe" />;
    }

    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <Probe />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    // Initial mount fetch.
    await act(async () => {
      await vi.runOnlyPendingTimersAsync();
    });
    expect(fetches).toHaveBeenCalledTimes(1);

    // Focus while fresh (within staleTime) — no refetch.
    act(() => {
      setVisibility("hidden");
      setVisibility("visible");
    });
    expect(fetches).toHaveBeenCalledTimes(1);

    // Go stale (past 30s), then focus — refetch fires.
    act(() => {
      vi.advanceTimersByTime(31_000);
      setVisibility("hidden");
      setVisibility("visible");
    });
    await act(async () => {
      await vi.runOnlyPendingTimersAsync();
    });
    expect(fetches).toHaveBeenCalledTimes(2);
  });
});
