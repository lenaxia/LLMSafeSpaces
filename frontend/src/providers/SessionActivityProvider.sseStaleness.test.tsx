// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// #1646 pin: a silently-dead SSE stream must not freeze busy state.
//
// The repro, at the integration level: the REAL useUserEventStream runs
// (no captured-callback mock) against a controllable fake fetch stream.
// The stream connects live, then dies WITHOUT a close frame — the
// reader just never resolves again (an API pod restart behind a proxy
// that holds the connection object). No error fires, so the read
// timeout never sees it; only the #1365 liveness watchdog can. While
// the stream is dead, the server flips a session to busy — the event
// goes to the broker, but the browser's dead connection never delivers
// it. The UI must converge to busy within the reconciliation window
// (watchdog silence 60s + a check tick) via: forced reconnect →
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

    // 70s of stream silence: past the 60s liveness window plus a
    // watchdog check tick. The forced reconnect must reseed busy from
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
});
