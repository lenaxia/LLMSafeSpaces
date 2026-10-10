// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// #1646 pin: a silently-dead SSE stream must not freeze busy state.
//
// The repro, at the integration level: the REAL useUserEventStream runs
// (no captured-callback mock) against a controllable fake fetch stream.
// The stream connects live, then dies WITHOUT a close frame — the
// reader just never resolves again (an API pod restart behind a proxy
// that holds the connection object). No error fires and no bytes flow,
// so the 35s READ TIMEOUT is what forces the reconnect here (the
// watchdog-only shape — bytes flowing but no heartbeats/events — is
// pinned separately in useEventStream.test.ts; both paths converge on
// the same onConnect reset). While the stream is dead, the server
// flips a session to busy — the event goes to the broker, but the
// browser's dead connection never delivers it. The UI must converge to
// busy within the reconciliation window via: forced reconnect →
// onConnect sessions invalidation → REST refetch → busyDelta re-seed.
//
// Against the pre-fix code this pin is RED: the watchdog reconnects
// and onReconnect wipes event-tracked busy, but nothing ever
// invalidates/refetches the sessions query, so the re-seed never fires
// and the UI shows not-busy until a remount (the owner-observed bug).

import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { SessionActivityProvider, useIsSessionBusy } from "./SessionActivityProvider";
import type { SessionListItem } from "../api/types";

vi.mock("../env", () => ({ getEnv: () => ({ apiBaseUrl: "/api/v1" }) }));
vi.mock("../lib/wsLog", () => ({ wsLog: vi.fn() }));

const mockGetSessions = vi.fn();
const mockGetAlerts = vi.fn();
vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getSessions: (id: string) => mockGetSessions(id),
    getAlerts: (id: string) => mockGetAlerts(id),
  },
}));

const encoder = new TextEncoder();

// A scripted reader: delivers the given chunks in order, then goes
// SILENT — the read never resolves again. This is the "no close frame"
// death: no error, no done, no bytes.
function scriptedReader(chunks: string[]) {
  let i = 0;
  return {
    read: () =>
      i < chunks.length
        ? Promise.resolve({ done: false, value: encoder.encode(chunks[i++]!) })
        : new Promise(() => {}),
    cancel: () => Promise.resolve(),
  };
}

// A probe holding a MOUNTED sessions query (the sidebar's shape) plus
// the busy readout under test. The mounted case matters: invalidation
// only refetches ACTIVE queries — unmounted workspaces converge on
// next mount instead (stale-while-revalidate; the re-seed gate was
// cleared at reconnect, so seedBusy re-runs when the cache updates).
function SessionsProbe() {
  useQuery({
    queryKey: ["sessions", "ws-1"],
    queryFn: () => mockGetSessions("ws-1"),
  });
  const busy = useIsSessionBusy("sess-1");
  return <span data-testid="busy">{busy ? "yes" : "no"}</span>;
}

describe("SessionActivityProvider — silent SSE death convergence (#1646)", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let connects: number;

  beforeEach(() => {
    vi.clearAllMocks();
    fetchMock = vi.fn();
    global.fetch = fetchMock;
    connects = 0;
    mockGetAlerts.mockResolvedValue([]);

    fetchMock.mockImplementation(() => {
      connects++;
      const chunks =
        connects === 1
          ? [
              // A live stream carrying an event (sets Last-Event-ID
              // bookkeeping so the next connect is a RE-connect) …
              `id: 7\ndata: {"event_id":7,"type":"session.status","workspace_id":"ws-1","session_id":"sess-1","status":"idle"}\n\n`,
              // … and one heartbeat comment frame (the 25s server cadence).
              ":\n\n",
            ]
          : [
              // The reconnected stream: alive (heartbeats flow), but the
              // busy transition happened during the dead window and the
              // post-restart broker has no replay for it.
              ":\n\n",
            ];
      return Promise.resolve({
        ok: true,
        body: { getReader: () => scriptedReader(chunks) },
      });
    });
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("converges busy within the reconciliation window after a silent stream death (#1646)", async () => {
    vi.useFakeTimers();

    let restBusy = false;
    mockGetSessions.mockImplementation(() =>
      Promise.resolve([
        {
          id: "sess-1",
          title: "t",
          messageCount: 0,
          status: restBusy ? "busy" : "idle",
          hasUnread: false,
        } as SessionListItem,
      ]),
    );

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <SessionsProbe />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    // Initial convergence: stream live, REST idle → not busy.
    await vi.advanceTimersByTimeAsync(1_000);
    expect(connects).toBe(1);
    // Post-fix, the first onConnect invalidation may legitimately
    // refetch the still-mounted query — only assert it fetched.
    expect(mockGetSessions.mock.calls.length).toBeGreaterThanOrEqual(1);
    expect(screen.getByTestId("busy").textContent).toBe("no");

    // Server-side: the session goes busy while the browser's stream is
    // silently dead. The event is emitted to the broker but can never
    // be delivered on this connection — REST is now the only path.
    restBusy = true;

    // 70s past the death: the byte-silent read timeout (35s) forces the
    // reconnect (the watchdog would too at 60s silence — same reset);
    // the reconnect reseed must refetch sessions and re-seed busy from
    // REST within this window.
    await vi.advanceTimersByTimeAsync(70_000);
    // Let the refetch → seed microtasks land.
    await vi.advanceTimersByTimeAsync(5_000);

    expect(connects).toBeGreaterThanOrEqual(2);
    expect(screen.getByTestId("busy").textContent).toBe("yes");
  });

  it("clears mount-seeded busy when a reconnect happens without any id-carrying event (#1646 r1)", async () => {
    // r1 finding: the reconnect reset (busy wipe + seed-gate clear) was
    // gated on lastEventIDRef !== null — but lastEventIDRef is only set
    // by id-carrying events, and the server writes snapshot/anti-entropy/
    // resync events with NO id: line. A quiet stream can reconnect
    // forever without ever setting it, so a busy seeded at mount that
    // went idle during a dead window could never clear (add-only seed).
    vi.useFakeTimers();

    let restIdle = false;
    mockGetSessions.mockImplementation(() =>
      Promise.resolve([
        {
          id: "sess-1",
          title: "t",
          messageCount: 0,
          status: restIdle ? "idle" : "busy",
          hasUnread: false,
        } as SessionListItem,
      ]),
    );

    // connect#1: ONLY a heartbeat comment frame — no id: line, so the
    // client's Last-Event-ID bookkeeping never engages (the quiet-system
    // shape the server's EventID:0 snapshot events produce).
    fetchMock.mockImplementation(() => {
      connects++;
      return Promise.resolve({
        ok: true,
        body: { getReader: () => scriptedReader([":\n\n"]) },
      });
    });

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <SessionsProbe />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    // Initial convergence: REST says busy → seeded busy at mount.
    await vi.advanceTimersByTimeAsync(1_000);
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    // The session finishes while the stream is silently dead; REST now
    // says idle. No id-carrying event was ever seen.
    restIdle = true;

    // Past the watchdog window the forced reconnect must run the FULL
    // reset: wipe busy, clear the seed gate, refetch, re-seed (from idle
    // → nothing) — busy must CLEAR.
    await vi.advanceTimersByTimeAsync(70_000);
    await vi.advanceTimersByTimeAsync(5_000);

    expect(connects).toBeGreaterThanOrEqual(2);
    expect(screen.getByTestId("busy").textContent).toBe("no");
  });

  // r2 finding: query-core notifies the cache "updated" for EVERY
  // dispatch — including "failed" (attempt failed, retries left) and
  // "error" (retries exhausted) — both of which PRESERVE stale
  // state.data. After a reconnect opened the seed gate (onReconnect
  // wipe), a failed refetch attempt re-seeded busy from the stale
  // pre-outage rows and re-latched the gate, suppressing the retry's
  // fresh idle rows and every later floor success — sticky busy until
  // the next reconnect/phase-change/remount. Production runs retry:1,
  // so this is the rolling-restart racing-GET shape (SSE reconnects to
  // the new pod while the REST GET hits the dying one).
  //
  // The reconnected stream is held HEALTHY (heartbeats flow, reads
  // resolve) so no second reconnect can heal the latch — connects===2
  // is enforced (the reviewer's own probe lesson: scripted connections
  // that die on their read timeout accidentally self-heal at ~71s).
  function ResilientSessionsProbe() {
    useQuery({
      queryKey: ["sessions", "ws-1"],
      queryFn: () => mockGetSessions("ws-1"),
      refetchInterval: 60_000, // sidebar floor parity: a later success exists
    });
    const busy = useIsSessionBusy("sess-1");
    return <span data-testid="busy">{busy ? "yes" : "no"}</span>;
  }

  function healthyHeartbeatReader() {
    const enc = new TextEncoder();
    return {
      read: () =>
        new Promise<{ done: boolean; value: Uint8Array }>((resolve) => {
          setTimeout(() => resolve({ done: false, value: enc.encode(":\n\n") }), 25_000);
        }),
      cancel: () => Promise.resolve(),
    };
  }

  it("does not re-latch stale busy when the post-reconnect refetch attempt fails once (#1646 r2)", async () => {
    vi.useFakeTimers();

    let restBusy = true;
    let failNextRefetch = false;
    mockGetSessions.mockImplementation(() => {
      if (failNextRefetch) {
        failNextRefetch = false;
        return Promise.reject(new Error("dying pod: connection reset"));
      }
      return Promise.resolve([
        {
          id: "sess-1",
          title: "t",
          messageCount: 0,
          status: restBusy ? "busy" : "idle",
          hasUnread: false,
        } as SessionListItem,
      ]);
    });

    // connect#1: one heartbeat then silent death (no bytes, no error).
    // connect#2: healthy heartbeats — the reconnect must NOT repeat.
    fetchMock.mockImplementation(() => {
      connects++;
      const reader = connects === 1 ? scriptedReader([":\n\n"]) : healthyHeartbeatReader();
      return Promise.resolve({ ok: true, body: { getReader: () => reader } });
    });

    // Production parity: retry: 1 (QueryClientProvider.tsx default).
    const qc = new QueryClient({ defaultOptions: { queries: { retry: 1 } } });
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <ResilientSessionsProbe />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    // Mount: REST busy → seeded busy.
    await vi.advanceTimersByTimeAsync(1_000);
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    // The session finished while the stream was dead; REST now idle.
    // The first post-reconnect refetch hits the dying pod (attempt 1
    // rejects); the retry lands on the new pod with fresh idle rows.
    restBusy = false;
    failNextRefetch = true;

    await vi.advanceTimersByTimeAsync(75_000);
    await vi.advanceTimersByTimeAsync(5_000);

    expect(connects).toBe(2); // reconnected stream held healthy
    expect(screen.getByTestId("busy").textContent).toBe("no");
  });

  it("does not re-latch stale busy when both refetch attempts fail (exhausted error, floor heals) (#1646 r2)", async () => {
    vi.useFakeTimers();

    let restBusy = true;
    let outage = false;
    mockGetSessions.mockImplementation(() => {
      if (outage) return Promise.reject(new Error("pod dying"));
      return Promise.resolve([
        {
          id: "sess-1",
          title: "t",
          messageCount: 0,
          status: restBusy ? "busy" : "idle",
          hasUnread: false,
        } as SessionListItem,
      ]);
    });

    fetchMock.mockImplementation(() => {
      connects++;
      const reader = connects === 1 ? scriptedReader([":\n\n"]) : healthyHeartbeatReader();
      return Promise.resolve({ ok: true, body: { getReader: () => reader } });
    });

    const qc = new QueryClient({ defaultOptions: { queries: { retry: 1 } } });
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <ResilientSessionsProbe />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    await vi.advanceTimersByTimeAsync(1_000);
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    // Session finished; the API pod is mid-rollover — both post-
    // reconnect attempts fail (retry exhausted → error dispatch), and
    // only at +60s does the floor refetch reach the healthy new pod.
    restBusy = false;
    outage = true;
    await vi.advanceTimersByTimeAsync(75_000);
    outage = false;
    await vi.advanceTimersByTimeAsync(65_000);

    expect(connects).toBe(2);
    expect(screen.getByTestId("busy").textContent).toBe("no");
  });

  // r3 finding: seedBusy's cache subscription walks queryCache.getAll()
  // and latches seeded.add(wsId) for EVERY sessions query with array
  // data — not just the dispatching one. With the sidebar's ordinary
  // multi-workspace state (one query per expanded workspace), the FIRST
  // workspace's post-reconnect success runs the full pass while the
  // second workspace's refetch is still in flight (the fetch dispatch
  // preserves prior data) or is inactive-cached (collapsed workspace,
  // no refetch at all): those are the stale pre-outage rows, so the
  // second workspace's gate latches and its stale busy re-adds; its own
  // fresh success is then gate-suppressed. Sticky per workspace.
  function TwoWorkspaceProbe({ mountWs2 = true }: { mountWs2?: boolean }) {
    useQuery({ queryKey: ["sessions", "ws-1"], queryFn: () => mockGetSessions("ws-1") });
    useQuery({
      queryKey: ["sessions", "ws-2"],
      queryFn: () => mockGetSessions("ws-2"),
      enabled: mountWs2,
      refetchInterval: 60_000,
    });
    const busy1 = useIsSessionBusy("sess-ws1");
    const busy2 = useIsSessionBusy("sess-ws2");
    return (
      <>
        <span data-testid="busy-ws1">{busy1 ? "yes" : "no"}</span>
        <span data-testid="busy-ws2">{busy2 ? "yes" : "no"}</span>
      </>
    );
  }

  it("does not latch ws-2's gate from stale rows while its refetch is in flight (#1646 r3)", async () => {
    vi.useFakeTimers();

    // ws-1: idle always. ws-2: busy at mount; its post-reconnect
    // refetch is HELD in flight until the test releases it with fresh
    // idle rows (the racing-GET shape across workspaces).
    let holdWs2Refetch = false;
    let releaseWs2: ((rows: SessionListItem[]) => void) | null = null;
    mockGetSessions.mockImplementation((id: string) => {
      if (id === "ws-1") {
        return Promise.resolve([{ id: "sess-ws1", title: "t", messageCount: 0, status: "idle", hasUnread: false } as SessionListItem]);
      }
      if (holdWs2Refetch) {
        return new Promise<SessionListItem[]>((resolve) => {
          releaseWs2 = () => resolve([{ id: "sess-ws2", title: "t", messageCount: 0, status: "idle", hasUnread: false } as SessionListItem]);
        });
      }
      return Promise.resolve([{ id: "sess-ws2", title: "t", messageCount: 0, status: "busy", hasUnread: false } as SessionListItem]);
    });

    fetchMock.mockImplementation(() => {
      connects++;
      const reader = connects === 1 ? scriptedReader([":\n\n"]) : healthyHeartbeatReader();
      return Promise.resolve({ ok: true, body: { getReader: () => reader } });
    });

    const qc = new QueryClient({ defaultOptions: { queries: { retry: 1 } } });
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <TwoWorkspaceProbe />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    // Mount: ws-2 busy (seeded), ws-1 idle.
    await vi.advanceTimersByTimeAsync(1_000);
    expect(screen.getByTestId("busy-ws1").textContent).toBe("no");
    expect(screen.getByTestId("busy-ws2").textContent).toBe("yes");

    // ws-2's session finished server-side; hold its post-reconnect
    // refetch in flight so ws-1's success lands first.
    holdWs2Refetch = true;

    await vi.advanceTimersByTimeAsync(75_000);
    // ws-1's fresh success has run the (full-pass) seedBusy; ws-2's
    // refetch is still pending. Now release it with fresh idle rows.
    // (Cast on read: the resolver is assigned inside the queryFn
    // closure, which TS control-flow analysis cannot track.)
    const release = releaseWs2 as ((rows: SessionListItem[]) => void) | null;
    expect(release).not.toBeNull();
    if (release) {
      release([{ id: "sess-ws2", title: "t", messageCount: 0, status: "idle", hasUnread: false } as SessionListItem]);
    }
    await vi.advanceTimersByTimeAsync(5_000);

    expect(connects).toBe(2); // reconnected stream held healthy
    expect(screen.getByTestId("busy-ws2").textContent).toBe("no");
  });

  it("does not latch an inactive cached workspace's stale rows after reconnect (#1646 r3)", async () => {
    vi.useFakeTimers();

    // ws-1 mounted (idle). ws-2: collapsed workspace — cache
    // pre-populated with busy rows, NO mounted query (its rows are the
    // pre-outage state the full pass must not re-latch after a wipe).
    mockGetSessions.mockImplementation((id: string) =>
      Promise.resolve(
        id === "ws-1"
          ? [{ id: "sess-ws1", title: "t", messageCount: 0, status: "idle", hasUnread: false } as SessionListItem]
          : [{ id: "sess-ws2", title: "t", messageCount: 0, status: "idle", hasUnread: false } as SessionListItem],
      ),
    );

    fetchMock.mockImplementation(() => {
      connects++;
      const reader = connects === 1 ? scriptedReader([":\n\n"]) : healthyHeartbeatReader();
      return Promise.resolve({ ok: true, body: { getReader: () => reader } });
    });

    const qc = new QueryClient({ defaultOptions: { queries: { retry: 1 } } });
    // ws-2's cache entry predates the provider mount — mount-seeded
    // busy, then the workspace collapses (query inactive, cache kept).
    qc.setQueryData(["sessions", "ws-2"], [
      { id: "sess-ws2", title: "t", messageCount: 0, status: "busy", hasUnread: false } as SessionListItem,
    ]);
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <TwoWorkspaceProbe mountWs2={false} />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );

    await vi.advanceTimersByTimeAsync(1_000);
    expect(screen.getByTestId("busy-ws2").textContent).toBe("yes"); // mount-seeded from fresh cache

    // Silent death → reconnect wipes busy. ws-1's success then runs
    // the full pass; ws-2's INACTIVE cache still holds the pre-wipe
    // busy rows — it must not re-latch them.
    await vi.advanceTimersByTimeAsync(75_000);
    await vi.advanceTimersByTimeAsync(5_000);

    expect(connects).toBe(2);
    expect(screen.getByTestId("busy-ws2").textContent).toBe("no");
  });
});
