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
  const busyTwo = useIsSessionBusy("phrase-two");
  return (
    <>
      <span data-testid="busy">{busy ? "yes" : "no"}</span>
      <span data-testid="busy-two">{busyTwo ? "yes" : "no"}</span>
    </>
  );
}

// A complete ok=true busy-snapshot flight for ws-1 whose rows say
// phrase-one is busy (and phrase-two is not).
function pushBusyFlight(
  stream: ReturnType<typeof pushedReader>,
  ids: { begin: number; snap: number; complete: number },
  rows: Array<{ session_id: string; derived_busy: boolean; session_id_deprecated?: boolean; internal_session_id?: string }>,
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

// ===== Round-2 review findings (PR #1848 r0) =====
//
// F1: cursor-0 snapshots and REST seeds must be governable by an
//     authoritative idle snapshot (no permanently-protected classes).
// F2: an older overlapping flight completing after a newer one must not
//     re-latch stale busy.
// F3: out-of-order broker delivery (id 100 applied before 99) must not
//     drop the never-applied 99.
// F5/F6: a degraded (harness-keyed) snapshot row heals through the
//     client's learned alias table.
describe("SessionActivityProvider — busy snapshot review findings (#1848 r0)", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let stream: ReturnType<typeof pushedReader>;

  beforeEach(() => {
    vi.clearAllMocks();
    fetchMock = vi.fn();
    global.fetch = fetchMock;
    mockGetAlerts.mockResolvedValue([]);
    mockRequestStatusSnapshot.mockResolvedValue(undefined);
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

  function renderProbe(sessionId?: string) {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <SessionsProbe sessionId={sessionId} />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
  }

  it("F1a a busy row latched by a cursor-0 snapshot is cleared by a later authoritative idle snapshot", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe();

    // Cursor 0 is legitimate (a fresh replica's broker): the row is
    // ordering evidence, not an unorderable legacy frame.
    pushBusyFlight(stream, { begin: 1, snap: 2, complete: 3 }, [
      { session_id: "phrase-one", derived_busy: true },
    ], 0);
    await advanceHealthy(stream, 3_000);
    await settle(100);
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    // The authority later reports idle — the entry must clear.
    pushBusyFlight(stream, { begin: 11, snap: 12, complete: 13 }, [
      { session_id: "phrase-one", derived_busy: false },
    ], 13);
    await advanceHealthy(stream, 3_000);
    await settle(100);
    expect(screen.getByTestId("busy").textContent).toBe("no");
  });

  it("F1b a REST-seeded active row is cleared by an authoritative idle snapshot (the common refresh race)", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([
      { id: "phrase-one", title: "t", messageCount: 0, status: "active", hasUnread: false },
    ] as SessionListItem[]);
    renderProbe();

    await settle(1_000);
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    pushBusyFlight(stream, { begin: 8, snap: 9, complete: 11 }, [
      { session_id: "phrase-one", derived_busy: false },
    ], 10);
    await advanceHealthy(stream, 3_000);
    await settle(100);
    expect(screen.getByTestId("busy").textContent).toBe("no");
  });

  it("F2 an older overlapping flight completing after a newer commit must not re-latch stale busy", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe();

    // F2 (newer): cursor 21, session idle → clears.
    pushBusyFlight(stream, { begin: 20, snap: 21, complete: 22 }, [
      { session_id: "phrase-one", derived_busy: false },
    ], 21);
    await settle(500);

    // F1 (older, overlapping — its begin predates F2): cursor 10, busy
    // row. Completing after F2's commit, it must be discarded as stale.
    stream.push(`id: 8\ndata: {"event_id":8,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-old"}\n\n`);
    stream.push(`id: 9\ndata: {"event_id":9,"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"f-old","data":{"cursor":10,"sessions":[{"session_id":"phrase-one","derived_busy":true}]}}\n\n`);
    stream.push(`id: 14\ndata: {"event_id":14,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-old","snapshot_ok":true}\n\n`);
    await advanceHealthy(stream, 3_000);
    await settle(100);

    expect(screen.getByTestId("busy").textContent).toBe("no");
  });

  it("F3 out-of-order delta delivery does not drop a never-applied older delta", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe();

    // Broker delivers id 100 (phrase-one busy) before the never-applied
    // 99 (phrase-two busy) — 99 must still apply.
    stream.push(`id: 100\ndata: {"event_id":100,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n`);
    await settle(300);
    stream.push(`id: 99\ndata: {"event_id":99,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-two","status":"busy"}\n\n`);
    await advanceHealthy(stream, 3_000);
    await settle(100);

    expect(screen.getByTestId("busy").textContent).toBe("yes");
    expect(screen.getByTestId("busy-two").textContent).toBe("yes");
  });

  it("F5 a degraded harness-keyed snapshot row heals through the client's learned alias", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe("phrase-two");

    // The seam learns the pair from a live delta (phrase wire id +
    // additive harness id).
    stream.push(`id: 5\ndata: {"event_id":5,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-two","status":"idle","data":{"internal_session_id":"h-2"}}\n\n`);
    await settle(300);

    // The flight's row degraded server-side (Register failed): harness
    // id on the wire, flagged deprecated — the client must resolve it
    // through the alias table and badge the phrase.
    pushBusyFlight(stream, { begin: 8, snap: 9, complete: 11 }, [
      { session_id: "h-2", derived_busy: true, session_id_deprecated: true },
    ], 10);
    await advanceHealthy(stream, 3_000);
    await settle(100);

    expect(screen.getByTestId("busy").textContent).toBe("yes");
  });
});

// ===== Round-3 review findings (PR #1848 r1) =====
//
// X1: per-workspace subsumption — workspace B's snapshot cursor must
//     not suppress workspace A's never-applied delta.
// X2: per-workspace stale-flight discard — B's evidence must not
//     discard A's flight.
// X3: resync resets the ordering state (per-replica id spaces) and
//     triggers the authority re-seed for workspaces that had busy.
// X4: a degraded row with NO learned alias must not clear a
//     REST-seeded phrase-keyed entry (the join is partial).
// X5: a flight completing after agent_died must not re-latch busy.
describe("SessionActivityProvider — busy ordering review findings (#1848 r1)", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let stream: ReturnType<typeof pushedReader>;

  beforeEach(() => {
    vi.clearAllMocks();
    fetchMock = vi.fn();
    global.fetch = fetchMock;
    mockGetAlerts.mockResolvedValue([]);
    mockRequestStatusSnapshot.mockResolvedValue(undefined);
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

  function renderProbe(sessionId?: string) {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <SessionsProbe sessionId={sessionId} />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
  }

  function ws2Flight(rows: Array<{ session_id: string; derived_busy: boolean }>, cursor: number) {
    stream.push(`id: ${cursor + 1}\ndata: {"event_id":${cursor + 1},"type":"session.status.snapshot_begin","workspace_id":"ws-2","snapshot_id":"f-ws2"}\n\n`);
    stream.push(`id: ${cursor + 2}\ndata: {"event_id":${cursor + 2},"type":"session.status.snapshot","workspace_id":"ws-2","snapshot_id":"f-ws2","data":{"cursor":${cursor},"sessions":${JSON.stringify(rows)}}}\n\n`);
    stream.push(`id: ${cursor + 3}\ndata: {"event_id":${cursor + 3},"type":"session.status.snapshot_complete","workspace_id":"ws-2","snapshot_id":"f-ws2","snapshot_ok":true}\n\n`);
  }

  it("X1 workspace B's snapshot cursor does not suppress workspace A's never-applied delta", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe();

    // Workspace 2 commits at cursor 100 (out-of-order delivery put its
    // high ids on the wire first).
    ws2Flight([{ session_id: "ws2-sess", derived_busy: false }], 100);
    await settle(500);

    // Workspace 1's delta id 90 — never applied, delivered late, and
    // below ws-2's cursor. It must STILL apply (ws-2's rows say
    // nothing about ws-1).
    stream.push(`id: 90\ndata: {"event_id":90,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n`);
    await advanceHealthy(stream, 3_000);
    await settle(100);

    expect(screen.getByTestId("busy").textContent).toBe("yes");
  });

  it("X2 workspace B's delta does not discard workspace A's overlapping flight", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe();

    // ws-1 flight opens (begin at id 8, cursor will be 10)…
    stream.push(`id: 8\ndata: {"event_id":8,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-a"}\n\n`);
    // …ws-2's delta id 50 lands mid-flight (its own workspace's
    // evidence — must not enter ws-1's high-water)…
    stream.push(`id: 50\ndata: {"event_id":50,"type":"session.status","workspace_id":"ws-2","session_id":"ws2-sess","status":"busy"}\n\n`);
    // …and the ws-1 flight stages + completes at cursor 10.
    stream.push(`id: 9\ndata: {"event_id":9,"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"f-a","data":{"cursor":10,"sessions":[{"session_id":"phrase-one","derived_busy":true}]}}\n\n`);
    stream.push(`id: 11\ndata: {"event_id":11,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-a","snapshot_ok":true}\n\n`);
    await advanceHealthy(stream, 3_000);
    await settle(100);

    expect(screen.getByTestId("busy").textContent).toBe("yes");
  });

  it("X3 resync resets ordering state (per-replica ids) and re-seeds via the authority", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe();

    // Old-replica state: a high cursor for ws-1 and latched busy.
    pushBusyFlight(stream, { begin: 198, snap: 199, complete: 200 }, [
      { session_id: "phrase-one", derived_busy: true },
    ], 200);
    await settle(500);
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    // The broker overflowed: resync. The wipe clears busy, and the
    // authority recheck fires for the workspace that had busy.
    stream.push(`data: {"type":"resync"}\n\n`);
    await advanceHealthy(stream, 2_000);
    expect(mockRequestStatusSnapshot.mock.calls.filter((c: unknown[]) => c[0] === "ws-1").length).toBe(1);

    // A fresh replica answers with LOW ids (its broker restarted):
    // cursor 5 + delta 3 must not be suppressed by the old bounds.
    stream.push(`id: 4\ndata: {"event_id":4,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-new"}\n\n`);
    stream.push(`id: 5\ndata: {"event_id":5,"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"f-new","data":{"cursor":5,"sessions":[]}}\n\n`);
    stream.push(`id: 6\ndata: {"event_id":6,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-new","snapshot_ok":true}\n\n`);
    stream.push(`id: 7\ndata: {"event_id":7,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n`);
    await advanceHealthy(stream, 3_000);
    await settle(100);

    expect(screen.getByTestId("busy").textContent).toBe("yes");
  });

  it("X4 a degraded row with no learned alias must not clear a REST-seeded phrase-keyed entry", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([
      { id: "phrase-one", title: "t", messageCount: 0, status: "active", hasUnread: false },
    ] as SessionListItem[]);
    renderProbe();
    await settle(1_000);
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    // The flight degraded server-side AND the client knows no pair:
    // the harness-keyed busy row latches additively, but the partial
    // join must NOT delete the phrase-keyed entry.
    pushBusyFlight(stream, { begin: 8, snap: 9, complete: 11 }, [
      { session_id: "h-1", derived_busy: true, session_id_deprecated: true },
    ], 10);
    await advanceHealthy(stream, 3_000);
    await settle(100);

    expect(screen.getByTestId("busy").textContent).toBe("yes");
  });

  it("X5 a flight completing after agent_died must not re-latch busy", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe();

    // The flight fetched while the pod was alive; its begin + snapshot
    // (busy row) are staged…
    stream.push(`id: 8\ndata: {"event_id":8,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-dead"}\n\n`);
    stream.push(`id: 9\ndata: {"event_id":9,"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"f-dead","data":{"cursor":10,"sessions":[{"session_id":"phrase-one","derived_busy":true}]}}\n\n`);
    // …then the agent dies (clears everything)…
    stream.push(`id: 12\ndata: {"event_id":12,"type":"agent_died","workspace_id":"ws-1"}\n\n`);
    await settle(300);
    expect(screen.getByTestId("busy").textContent).toBe("no");

    // …and the flight's complete arrives late. It must not re-latch.
    stream.push(`id: 13\ndata: {"event_id":13,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-dead","snapshot_ok":true}\n\n`);
    await advanceHealthy(stream, 3_000);
    await settle(100);

    expect(screen.getByTestId("busy").textContent).toBe("no");
  });
});

// ===== Round-4 review findings (PR #1848 r2) =====
//
// Y2: same-session out-of-order delivery (idle 100 before busy 99)
//     must end idle — ordering enforced per session.
// Y3: one degraded row must not starve a DIFFERENT resolved session's
//     authoritative clear.
// Y4: an unrelated same-workspace delta during a recheck flight must
//     not discard the recheck's rows.
// Y5: ok=false after a wipe-initiated re-seed schedules exactly ONE
//     authority retry per lease window; the retry's answer re-badges.
describe("SessionActivityProvider — ordering/partial-join findings (#1848 r2)", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let stream: ReturnType<typeof pushedReader>;

  beforeEach(() => {
    vi.clearAllMocks();
    fetchMock = vi.fn();
    global.fetch = fetchMock;
    mockGetAlerts.mockResolvedValue([]);
    mockRequestStatusSnapshot.mockResolvedValue(undefined);
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

  function renderProbe(sessionId?: string) {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <SessionsProbe sessionId={sessionId} />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
  }

  it("Y2 same-session out-of-order delivery ends at the newer transition's state", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe();

    // Inverted delivery: idle id 100 first, then the older busy id 99
    // for the SAME session. The newer truth (idle) must win.
    stream.push(`id: 100\ndata: {"event_id":100,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"idle"}\n\n`);
    await settle(300);
    stream.push(`id: 99\ndata: {"event_id":99,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n`);
    await advanceHealthy(stream, 3_000);
    await settle(100);

    expect(screen.getByTestId("busy").textContent).toBe("no");
  });

  it("Y3 a degraded row does not starve a different resolved session's authoritative clear", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe();

    // Session A latches busy via a delta carrying its harness id
    // additively; session B is busy but its row will degrade.
    stream.push(`id: 5\ndata: {"event_id":5,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy","data":{"internal_session_id":"h-A"}}\n\n`);
    await settle(300);
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    // The flight: A resolved+idle, B busy+degraded (harness "h-B").
    stream.push(`id: 8\ndata: {"event_id":8,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-mix"}\n\n`);
    stream.push(`id: 9\ndata: {"event_id":9,"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"f-mix","data":{"cursor":10,"sessions":[{"session_id":"phrase-one","derived_busy":false},{"session_id":"h-B","derived_busy":true,"session_id_deprecated":true}]}}\n\n`);
    stream.push(`id: 11\ndata: {"event_id":11,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-mix","snapshot_ok":true}\n\n`);
    await advanceHealthy(stream, 3_000);
    await settle(100);

    // A's clear is not poisoned by B's degraded row.
    expect(screen.getByTestId("busy").textContent).toBe("no");
  });

  it("Y4 an unrelated same-workspace delta during a recheck flight does not discard its rows", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe();

    // phrase-one latched busy (confirm 5).
    stream.push(`id: 5\ndata: {"event_id":5,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n`);
    await settle(300);

    // The recheck flight opens (cursor 10); an UNRELATED session in
    // the same workspace transitions at id 11 mid-flight; the flight's
    // authoritative idle answer for phrase-one must still clear it.
    stream.push(`id: 8\ndata: {"event_id":8,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-r"}\n\n`);
    stream.push(`id: 11\ndata: {"event_id":11,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-two","status":"busy"}\n\n`);
    stream.push(`id: 9\ndata: {"event_id":9,"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"f-r","data":{"cursor":10,"sessions":[{"session_id":"phrase-one","derived_busy":false},{"session_id":"phrase-two","derived_busy":true}]}}\n\n`);
    stream.push(`id: 12\ndata: {"event_id":12,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-r","snapshot_ok":true}\n\n`);
    await advanceHealthy(stream, 3_000);
    await settle(100);

    expect(screen.getByTestId("busy").textContent).toBe("no");
    expect(screen.getByTestId("busy-two").textContent).toBe("yes");
  });

  it("Y5 ok=false after a wipe-initiated re-seed retries exactly once per window and re-badges on the answer", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe();

    // Busy latched, then the broker overflows: resync wipes busy and
    // requests re-seed flight #1.
    stream.push(`id: 5\ndata: {"event_id":5,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n`);
    await settle(300);
    stream.push(`data: {"type":"resync"}\n\n`);
    await advanceHealthy(stream, 2_000);
    expect(mockRequestStatusSnapshot.mock.calls.length).toBe(1);

    // The authority is down: the flight answers ok=false. The wipe's
    // memory re-latches as UNKNOWN (the charter hold — the badge never
    // reads idle) and the pending-recheck path owns the retry — one
    // more request per lease window, not a hammer.
    stream.push(`id: 20\ndata: {"event_id":20,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-down"}\n\n`);
    stream.push(`id: 21\ndata: {"event_id":21,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-down","snapshot_ok":false}\n\n`);
    await settle(500);
    expect(screen.getByTestId("busy").textContent).toBe("yes");
    await advanceHealthy(stream, 80_000);
    expect(mockRequestStatusSnapshot.mock.calls.length).toBe(1);

    await advanceHealthy(stream, 20_000);
    expect(mockRequestStatusSnapshot.mock.calls.length).toBe(2);

    // The retry's flight answers authoritatively: the unknown-held
    // badge is CONFIRMED busy (it was re-latched by the ok=false
    // unknown-hold, so "yes" never flickered).
    expect(screen.getByTestId("busy").textContent).toBe("yes");
    stream.push(`id: 30\ndata: {"event_id":30,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-up"}\n\n`);
    stream.push(`id: 31\ndata: {"event_id":31,"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"f-up","data":{"cursor":31,"sessions":[{"session_id":"phrase-one","derived_busy":true}]}}\n\n`);
    stream.push(`id: 32\ndata: {"event_id":32,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-up","snapshot_ok":true}\n\n`);
    await advanceHealthy(stream, 3_000);
    await settle(100);

    expect(screen.getByTestId("busy").textContent).toBe("yes");
    // The authoritative answer cleared the pending retry. The
    // confirmed-busy lease then ticks at its designed cadence — one
    // recheck per ~90s window (sweep-tick jitter ≤15s) for as long as
    // the session stays genuinely busy. Pin the SPACING: nothing fires
    // inside an 85s quiet window, and the next one lands within a
    // window + jitter.
    await advanceHealthy(stream, 85_000);
    expect(mockRequestStatusSnapshot.mock.calls.length).toBe(2);
    await advanceHealthy(stream, 30_000);
    expect(mockRequestStatusSnapshot.mock.calls.length).toBe(3);
  });
});

// ===== Round-5 review findings (PR #1848 r3) =====
//
// Z1: same-chunk dispatch staleness — a busy delta and an idle
//     snapshot commit delivered in ONE chunk must reconcile against
//     the chunk's own state (the newer evidence wins), in both orders.
// Z2: a REST-seeded phrase entry with an unrelated degraded busy row
//     in the flight is cleared by its own resolved idle answer.
// Z3: a resolved idle row wins over an older overlapping busy flight.
// Z4: agent_died with an outstanding pending recheck stops the
//     retry loop.
describe("SessionActivityProvider — commit-path correctness (#1848 r3)", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let stream: ReturnType<typeof pushedReader>;

  beforeEach(() => {
    vi.clearAllMocks();
    fetchMock = vi.fn();
    global.fetch = fetchMock;
    mockGetAlerts.mockResolvedValue([]);
    mockRequestStatusSnapshot.mockResolvedValue(undefined);
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

  function renderProbe(sessionId?: string) {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <MemoryRouter>
          <SessionActivityProvider>
            <SessionsProbe sessionId={sessionId} />
          </SessionActivityProvider>
        </MemoryRouter>
      </QueryClientProvider>,
    );
  }

  it("Z1 same-chunk delta + snapshot commit reconciles against the chunk's own state (both orders)", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe();

    // Order A: the flight's frames AND a newer busy delta (id 30) for
    // the session arrive in ONE chunk. The idle rows must not delete
    // the newer busy evidence.
    stream.push(
      `id: 9\ndata: {"event_id":9,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-chunk"}\n\n` +
      `id: 10\ndata: {"event_id":10,"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"f-chunk","data":{"cursor":10,"sessions":[{"session_id":"phrase-one","derived_busy":false}]}}\n\n` +
      `id: 11\ndata: {"event_id":11,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-chunk","snapshot_ok":true}\n\n` +
      `id: 30\ndata: {"event_id":30,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n`,
    );
    await advanceHealthy(stream, 3_000);
    await settle(100);
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    // Order B: one chunk carrying a newer idle delta (id 50) after the
    // flight confirmed busy at cursor 40 — ends idle.
    stream.push(
      `id: 41\ndata: {"event_id":41,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-chunk2"}\n\n` +
      `id: 42\ndata: {"event_id":42,"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"f-chunk2","data":{"cursor":40,"sessions":[{"session_id":"phrase-one","derived_busy":true}]}}\n\n` +
      `id: 43\ndata: {"event_id":43,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-chunk2","snapshot_ok":true}\n\n` +
      `id: 50\ndata: {"event_id":50,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"idle"}\n\n`,
    );
    await advanceHealthy(stream, 3_000);
    await settle(100);
    expect(screen.getByTestId("busy").textContent).toBe("no");
  });

  it("Z1b a same-chunk newer busy re-confirm survives an older-cursor idle commit (the stale-closure regression)", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe();

    // Pre-chunk: busy latched at confirm id 5.
    stream.push(`id: 5\ndata: {"event_id":5,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n`);
    await settle(300);
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    // ONE chunk: a NEWER busy delta (id 30 — re-confirm) followed by
    // an idle flight at cursor 10. The newer evidence must win: the
    // commit's cursor (10) is below the delta's confirm (30), so the
    // entry survives. The r3 regression computed doom from the stale
    // pre-chunk state (confirm=5 ≤ 10) and deleted the fresh signal.
    stream.push(
      `id: 30\ndata: {"event_id":30,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n` +
      `id: 31\ndata: {"event_id":31,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-z1b"}\n\n` +
      `id: 32\ndata: {"event_id":32,"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"f-z1b","data":{"cursor":10,"sessions":[{"session_id":"phrase-one","derived_busy":false}]}}\n\n` +
      `id: 33\ndata: {"event_id":33,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-z1b","snapshot_ok":true}\n\n`,
    );
    await advanceHealthy(stream, 3_000);
    await settle(100);
    expect(screen.getByTestId("busy").textContent).toBe("yes");
  });

  it("Z2 a REST-seeded entry is cleared by its resolved idle answer despite an unrelated degraded busy row", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([
      { id: "phrase-one", title: "t", messageCount: 0, status: "active", hasUnread: false },
    ] as SessionListItem[]);
    renderProbe();
    await settle(1_000);
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    // phrase-one's row RESOLVED idle (registered server-side); an
    // unrelated session's row degraded busy. The resolved answer owns
    // the entry — cleared.
    stream.push(`id: 8\ndata: {"event_id":8,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-z2"}\n\n`);
    stream.push(`id: 9\ndata: {"event_id":9,"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"f-z2","data":{"cursor":10,"sessions":[{"session_id":"phrase-one","derived_busy":false},{"session_id":"h-B","derived_busy":true,"session_id_deprecated":true}]}}\n\n`);
    stream.push(`id: 11\ndata: {"event_id":11,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-z2","snapshot_ok":true}\n\n`);
    await advanceHealthy(stream, 3_000);
    await settle(100);

    expect(screen.getByTestId("busy").textContent).toBe("no");
  });

  it("Z3 a resolved idle row wins over an older overlapping busy flight", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe();

    // Newer flight (cursor 30) answers idle; older flight (cursor 20)
    // completes after with a busy row — idle wins.
    stream.push(`id: 31\ndata: {"event_id":31,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-new"}\n\n`);
    stream.push(`id: 32\ndata: {"event_id":32,"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"f-new","data":{"cursor":30,"sessions":[{"session_id":"phrase-one","derived_busy":false}]}}\n\n`);
    stream.push(`id: 33\ndata: {"event_id":33,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-new","snapshot_ok":true}\n\n`);
    stream.push(`id: 21\ndata: {"event_id":21,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-old"}\n\n`);
    stream.push(`id: 22\ndata: {"event_id":22,"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"f-old","data":{"cursor":20,"sessions":[{"session_id":"phrase-one","derived_busy":true}]}}\n\n`);
    stream.push(`id: 34\ndata: {"event_id":34,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-old","snapshot_ok":true}\n\n`);
    await advanceHealthy(stream, 3_000);
    await settle(100);

    expect(screen.getByTestId("busy").textContent).toBe("no");
  });

  it("Z4 agent_died with an outstanding pending recheck stops the retry loop", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe();

    // Nothing latched and no wipe memory → the ok=false answer takes
    // the PENDING-recheck branch (scheduled at +90s).
    stream.push(`id: 20\ndata: {"event_id":20,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-d"}\n\n`);
    stream.push(`id: 21\ndata: {"event_id":21,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-d","snapshot_ok":false}\n\n`);
    await settle(300);

    // Terminal event: the workspace is dead — the pending recheck must
    // be dropped; zero POSTs ever (pre-fix: the loop re-arms forever).
    stream.push(`id: 22\ndata: {"event_id":22,"type":"agent_died","workspace_id":"ws-1"}\n\n`);
    await advanceHealthy(stream, 190_000);
    expect(mockRequestStatusSnapshot.mock.calls.length).toBe(0);
  });

  it("Z4b a LATE ok=false after the terminal event cannot re-arm the recheck loop", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe();

    // ok=false with nothing latched and no wipe memory → the pending
    // recheck is armed at +90s.
    stream.push(`id: 20\ndata: {"event_id":20,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-d"}\n\n`);
    stream.push(`id: 21\ndata: {"event_id":21,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-d","snapshot_ok":false}\n\n`);
    await settle(300);

    // Terminal: agent_died drops the pending recheck…
    stream.push(`id: 22\ndata: {"event_id":22,"type":"agent_died","workspace_id":"ws-1"}\n\n`);
    await settle(300);

    // …and a LATE ok=false flight (its begin predates the death — the
    // fetch raced the agent's death and the pod cannot answer) must not
    // re-arm it: the sweep would POST every window forever for a corpse.
    stream.push(`id: 23\ndata: {"event_id":23,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-late"}\n\n`);
    stream.push(`id: 24\ndata: {"event_id":24,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-late","snapshot_ok":false}\n\n`);
    await advanceHealthy(stream, 190_000);
    expect(mockRequestStatusSnapshot.mock.calls.length).toBe(0);
  });

  it("Z3b a degraded harness-keyed idle answer beats an older phrase-keyed busy flight", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe();

    // Newer flight (cursor 30): the idle row DEGRADED server-side —
    // harness-keyed, flagged, no pair learnable from it. Older flight
    // (cursor 20): healthy busy row, phrase key + additive harness id.
    // The newer answer must win (idle): the busy row's own harness id
    // carries the newer evidence the phrase key never saw.
    stream.push(`id: 31\ndata: {"event_id":31,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-new"}\n\n`);
    stream.push(`id: 32\ndata: {"event_id":32,"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"f-new","data":{"cursor":30,"sessions":[{"session_id":"h-1","derived_busy":false,"session_id_deprecated":true}]}}\n\n`);
    stream.push(`id: 33\ndata: {"event_id":33,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-new","snapshot_ok":true}\n\n`);
    stream.push(`id: 21\ndata: {"event_id":21,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-old"}\n\n`);
    stream.push(`id: 22\ndata: {"event_id":22,"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"f-old","data":{"cursor":20,"sessions":[{"session_id":"phrase-one","internal_session_id":"h-1","derived_busy":true}]}}\n\n`);
    stream.push(`id: 34\ndata: {"event_id":34,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-old","snapshot_ok":true}\n\n`);
    await advanceHealthy(stream, 3_000);
    await settle(100);

    expect(screen.getByTestId("busy").textContent).toBe("no");
  });

  it("Z6 a same-chunk busy delta + resync still triggers the post-wipe authority recheck", async () => {
    vi.useFakeTimers();
    mockGetSessions.mockResolvedValue([]);
    renderProbe();

    // ONE chunk: the busy delta lands and the broker signals resync in
    // the same synchronous dispatch. A render-closure read of the busy
    // set never sees the delta (stale within the chunk) — the capture
    // must ride the updater queue, or the wiped workspace is never
    // re-checked and reads idle forever.
    stream.push(
      `id: 5\ndata: {"event_id":5,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n` +
      `data: {"type":"resync"}\n\n`,
    );
    await settle(300);
    expect(screen.getByTestId("busy").textContent).toBe("no");

    await advanceHealthy(stream, 110_000);
    expect(mockRequestStatusSnapshot).toHaveBeenCalledTimes(1);
    expect(mockRequestStatusSnapshot).toHaveBeenCalledWith("ws-1");
  });
});
