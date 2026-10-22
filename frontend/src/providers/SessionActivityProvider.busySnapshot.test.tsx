// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// #1821 pieces 2-4 — T0.2 tier: the busy-snapshot handshake and the
// busy lease, pinned at the provider level against a push-controllable
// user-event stream (the resyncBusy harness pattern).
//
// RED-FIRST: at the time these pins were written the provider knows no
// session.status.snapshot* events (they log as unhandled), applies
// every session.status delta in arrival order, and busy carries no
// lease — the snapshot-seed, stale-delta-skip, and lease-recheck pins
// below all fail against that code.

import { act, render, screen } from "@testing-library/react";
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

// #1821 piece 4: the lease's authoritative recheck surface.
const mockRequestStatusSnapshot = vi.fn();
vi.mock("../api/sessions", () => ({
  sessionsApi: {
    create: vi.fn(),
    requestStatusSnapshot: (id: string) => mockRequestStatusSnapshot(id),
  },
}));

const encoder = new TextEncoder();

function pushedReader(initial: string[]) {
  const queue: string[] = [...initial];
  const waiters: Array<(r: { done: boolean; value: Uint8Array }) => void> = [];
  return {
    push(chunk: string) {
      const w = waiters.shift();
      if (w) w({ done: false, value: encoder.encode(chunk) });
      else queue.push(chunk);
    },
    reader: {
      read: () =>
        queue.length > 0
          ? Promise.resolve({ done: false, value: encoder.encode(queue.shift()!) })
          : new Promise<{ done: boolean; value: Uint8Array }>((resolve) => {
              waiters.push(resolve);
            }),
      cancel: () => Promise.resolve(),
    },
  };
}

// advanceHealthy advances fake time in bounded steps, pushing an SSE
// heartbeat comment before each one — the faithful lost-idle scenario:
// the connection stays byte-alive (no 35s read-timeout reconnect, no
// onReconnect wipe) while no busy evidence arrives. Steps stay well
// under the 35s read timeout.
async function advanceHealthy(stream: ReturnType<typeof pushedReader>, ms: number) {
  const STEP = 8_000;
  let left = ms;
  while (left > 0) {
    stream.push(":\n\n");
    const step = Math.min(STEP, left);
    await vi.advanceTimersByTimeAsync(step);
    left -= step;
  }
}

// settle gives React's scheduler (MessageChannel-based, unfaked by
// fake timers) a deterministic act-flush after stream pushes.
async function settle(ms = 250) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

function SessionsProbe({ sessionId = "phrase-one" }: { sessionId?: string }) {
  useQuery({
    queryKey: ["sessions", "ws-1"],
    queryFn: () => mockGetSessions("ws-1"),
  });
  const busy = useIsSessionBusy(sessionId);
  return <span data-testid="busy">{busy ? "yes" : "no"}</span>;
}

// A complete ok=true busy-snapshot flight for ws-1 whose rows say
// phrase-one is busy (and phrase-two is not).
function pushBusyFlight(
  stream: ReturnType<typeof pushedReader>,
  ids: { begin: number; snap: number; complete: number },
  rows: Array<{ session_id: string; derived_busy: boolean }>,
  cursor = 10,
) {
  stream.push(`id: ${ids.begin}\ndata: {"event_id":${ids.begin},"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-1"}\n\n`);
  stream.push(`id: ${ids.snap}\ndata: {"event_id":${ids.snap},"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"f-1","data":{"cursor":${cursor},"sessions":${JSON.stringify(rows)}}}\n\n`);
  stream.push(`id: ${ids.complete}\ndata: {"event_id":${ids.complete},"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-1","snapshot_ok":true}\n\n`);
}

describe("SessionActivityProvider — busy snapshot + lease (#1821 pieces 2-4)", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let stream: ReturnType<typeof pushedReader>;

  beforeEach(() => {
    vi.clearAllMocks();
    fetchMock = vi.fn();
    global.fetch = fetchMock;
    mockGetAlerts.mockResolvedValue([]);
    mockRequestStatusSnapshot.mockResolvedValue(undefined);
    mockGetSessions.mockResolvedValue([
      { id: "phrase-one", title: "t", messageCount: 0, status: "idle", hasUnread: false },
      { id: "phrase-two", title: "t2", messageCount: 0, status: "idle", hasUnread: false },
    ] as SessionListItem[]);

    stream = pushedReader([]);
    fetchMock.mockImplementation(() =>
      Promise.resolve({
        ok: true,
        body: { getReader: () => stream.reader },
      }),
    );
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("T0.2a seeds busy from a session.status.snapshot flight on connect (subscribe-after-flip)", async () => {
    vi.useFakeTimers();

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
    await vi.advanceTimersByTimeAsync(1_000);
    expect(screen.getByTestId("busy").textContent).toBe("no");

    pushBusyFlight(stream, { begin: 8, snap: 9, complete: 11 }, [
      { session_id: "phrase-one", derived_busy: true },
      { session_id: "phrase-two", derived_busy: false },
    ]);

    await vi.advanceTimersByTimeAsync(1_000);

    // The snapshot's busy row latches the badge without any delta.
    expect(screen.getByTestId("busy").textContent).toBe("yes");
  });

  it("T0.2b authority failure (snapshot_ok=false) holds latched busy — unknown, never idle", async () => {
    vi.useFakeTimers();

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

    // Busy latches from a live delta first.
    stream.push(`id: 5\ndata: {"event_id":5,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n`);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    // The authority is down: begin + complete(ok=false), no rows.
    stream.push(`id: 12\ndata: {"event_id":12,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-down"}\n\n`);
    stream.push(`id: 13\ndata: {"event_id":13,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-down","snapshot_ok":false}\n\n`);
    await settle(1_000);

    // A failed authority read must never read as an authoritative empty set.
    expect(screen.getByTestId("busy").textContent).toBe("yes");
  });

  it("T0.2c skips a stale-replayed session.status delta whose event_id ≤ the snapshot cursor (piece 3 ordering)", async () => {
    vi.useFakeTimers();

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
    await vi.advanceTimersByTimeAsync(1_000);

    // Busy via a live delta (id 7), then an authoritative snapshot at
    // cursor 10 confirming busy (the overlap window of a reconnect).
    stream.push(`id: 7\ndata: {"event_id":7,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n`);
    await settle(500);
    pushBusyFlight(stream, { begin: 8, snap: 9, complete: 11 }, [
      { session_id: "phrase-one", derived_busy: true },
    ]);
    await settle(500);
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    // A replayed idle with event_id 6 (< cursor 10) predates the
    // snapshot — applying it would resurrect pre-snapshot state.
    stream.push(`id: 6\ndata: {"event_id":6,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"idle"}\n\n`);
    await settle(500);

    expect(screen.getByTestId("busy").textContent).toBe("yes");
  });

  it("T0.2d an authoritative snapshot commit clears a latched busy the authority no longer reports", async () => {
    vi.useFakeTimers();

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

    stream.push(`id: 5\ndata: {"event_id":5,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n`);
    await settle(1_000);
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    // The authority answers: both sessions idle. The commit must clear
    // the latched busy (refresh-mid-busy convergence).
    pushBusyFlight(stream, { begin: 8, snap: 9, complete: 11 }, [
      { session_id: "phrase-one", derived_busy: false },
      { session_id: "phrase-two", derived_busy: false },
    ]);
    await advanceHealthy(stream, 5_000);
    // A second act window: the commit's state update lands on the
    // scheduler's deferred flush (React's MessageChannel scheduler is
    // not faked — one extra deterministic flush settles it).
    await settle(100);

    expect(screen.getByTestId("busy").textContent).toBe("no");
  });

  it("T0.2e lease expiry (90s without confirmation) triggers exactly ONE authoritative recheck, then the flight clears", async () => {
    vi.useFakeTimers();

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

    // Busy latches; the confirming idle is swallowed (lost idle).
    stream.push(`id: 5\ndata: {"event_id":5,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n`);
    await settle(1_000);
    expect(screen.getByTestId("busy").textContent).toBe("yes");
    expect(mockRequestStatusSnapshot).not.toHaveBeenCalled();

    // The heartbeat-healthy stream carries nothing for just under the
    // lease — no recheck may fire early.
    await advanceHealthy(stream, 89_000);
    expect(mockRequestStatusSnapshot).not.toHaveBeenCalled();

    // Expiry: exactly one recheck fires (one flight per workspace).
    await advanceHealthy(stream, 16_000);
    expect(mockRequestStatusSnapshot.mock.calls.length).toBe(1);
    expect(mockRequestStatusSnapshot.mock.calls[0]![0]).toBe("ws-1");

    // The recheck's flight answers idle — the badge clears (L+ε).
    stream.push(`id: 20\ndata: {"event_id":20,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-recheck"}\n\n`);
    stream.push(`id: 21\ndata: {"event_id":21,"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"f-recheck","data":{"cursor":21,"sessions":[{"session_id":"phrase-one","derived_busy":false}]}}\n\n`);
    stream.push(`id: 22\ndata: {"event_id":22,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-recheck","snapshot_ok":true}\n\n`);
    await advanceHealthy(stream, 3_000);
    await settle(100);

    expect(screen.getByTestId("busy").textContent).toBe("no");
    // Still exactly one request — the cleared badge stops leasing.
    expect(mockRequestStatusSnapshot.mock.calls.length).toBe(1);
  });

  it("T0.2f an unreachable recheck authority keeps the badge (unknown ≠ idle) and re-arms for one retry per window", async () => {
    vi.useFakeTimers();
    mockRequestStatusSnapshot.mockRejectedValue(new Error("network down"));

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

    stream.push(`id: 5\ndata: {"event_id":5,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n`);
    await settle(1_000);

    // Expiry fires the recheck; the request rejects.
    await advanceHealthy(stream, 105_000);
    expect(mockRequestStatusSnapshot.mock.calls.length).toBe(1);
    await advanceHealthy(stream, 3_000);

    // Unknown must not read as idle — the badge holds.
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    // The lease re-armed at trigger: exactly one more attempt a full
    // window later — not a hammering retry loop.
    await advanceHealthy(stream, 90_000);
    expect(mockRequestStatusSnapshot.mock.calls.length).toBe(2);
  });
});
