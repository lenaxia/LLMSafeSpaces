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
});
