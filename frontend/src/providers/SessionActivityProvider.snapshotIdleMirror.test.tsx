// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// #1883 — the snapshot-commit cache mirror. The busy delta path flips
// the sessions-cache row to status:"active" when it latches busy, and
// the idle delta path flips the row back on its clear. The #1848
// busy-snapshot commit mirrors ONLY the busy direction onto the cache:
// when the 90s lease's authoritative flight answers idle for a session
// whose idle delta was lost (the #1883 incident shape), the provider's
// busy map clears but the cached row keeps the stale "active" — and
// seedBusy's mount-time pass is ADD-ONLY, so any provider remount
// (route-level remount under a shared QueryClient, error-boundary
// recovery, StrictMode) re-latches the cleared badge from that stale
// row and the add-only gate then ignores the fresh REST rows that say
// idle. The frozen busy indicator RETURNS after the authority already
// healed it — the cache projection silently diverging from harness
// truth, the same #1879/#1880 family.
//
// RED-FIRST: on HEAD the commit writes no idle mirror, so the row
// assertion and the remount assertion below fail.

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
// heartbeat comment before each one — the connection stays byte-alive
// (no read-timeout reconnect, no onReconnect wipe) while no busy
// evidence arrives (the lost-idle shape of the #1883 incident).
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

function SessionsProbe() {
  useQuery({
    queryKey: ["sessions", "ws-1"],
    queryFn: () => mockGetSessions("ws-1"),
  });
  const busy = useIsSessionBusy("phrase-one");
  return <span data-testid="busy">{busy ? "yes" : "no"}</span>;
}

function rowStatus(qc: QueryClient): string | undefined {
  const rows = qc.getQueryData<SessionListItem[]>(["sessions", "ws-1"]);
  return rows?.find((r) => r.id === "phrase-one")?.status;
}

function mount(qc: QueryClient) {
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <SessionActivityProvider>
          <SessionsProbe />
        </SessionActivityProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

// A complete ok=true busy-snapshot flight for ws-1.
function pushFlight(
  stream: ReturnType<typeof pushedReader>,
  ids: { begin: number; snap: number; complete: number },
  rows: Array<{ session_id: string; derived_busy: boolean; session_id_deprecated?: boolean; internal_session_id?: string }>,
  cursor: number,
  flightId = "f-1",
) {
  stream.push(`id: ${ids.begin}\ndata: {"event_id":${ids.begin},"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"${flightId}"}\n\n`);
  stream.push(`id: ${ids.snap}\ndata: {"event_id":${ids.snap},"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"${flightId}","data":{"cursor":${cursor},"sessions":${JSON.stringify(rows)}}}\n\n`);
  stream.push(`id: ${ids.complete}\ndata: {"event_id":${ids.complete},"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"${flightId}","snapshot_ok":true}\n\n`);
}

describe("SessionActivityProvider — snapshot-commit idle cache mirror (#1883)", () => {
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

  it("M1 the lease flight's idle answer mirrors onto the cached row — a remount must not re-latch the cleared badge", async () => {
    vi.useFakeTimers();

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const first = mount(qc);
    await settle(1_000);
    expect(rowStatus(qc)).toBe("idle");

    // Busy latches (a visited background session); the cached row flips
    // to "active" (the busy delta's cache mirror).
    stream.push(`id: 5\ndata: {"event_id":5,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n`);
    await settle(1_000);
    expect(screen.getByTestId("busy").textContent).toBe("yes");
    expect(rowStatus(qc)).toBe("active");

    // The confirming idle frame is lost (never pushed). The lease
    // expires and fires the authoritative recheck.
    await advanceHealthy(stream, 105_000);
    expect(mockRequestStatusSnapshot.mock.calls.length).toBe(1);

    // The recheck flight answers idle — the badge clears.
    pushFlight(stream, { begin: 20, snap: 21, complete: 22 }, [{ session_id: "phrase-one", derived_busy: false }], 21, "f-recheck");
    await advanceHealthy(stream, 3_000);
    await settle(100);
    expect(screen.getByTestId("busy").textContent).toBe("no");

    // PIN (RED on HEAD): the authority's idle answer must reach the
    // cached row too — the projection may not contradict the truth it
    // just reconciled.
    expect(rowStatus(qc)).toBe("idle");

    // PIN (RED on HEAD): a provider remount (shared QueryClient — the
    // cache survives, exactly like a route-level remount in prod) must
    // NOT re-latch the badge from the stale row: seedBusy's mount-time
    // pass is add-only, so a stale "active" row freezes the badge
    // again and ignores the fresh idle rows the invalidation refetch
    // brings back.
    first.unmount();
    const second = mount(qc);
    await settle(1_500);
    expect(second.getByTestId("busy").textContent).toBe("no");
    expect(rowStatus(qc)).toBe("idle");
  });

  it("M2 an idle row superseded by a fresher same-session delta must not flip the row back (watermark guard)", async () => {
    vi.useFakeTimers();

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    mount(qc);
    await settle(1_000);

    stream.push(`id: 5\ndata: {"event_id":5,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n`);
    await settle(1_000);
    expect(screen.getByTestId("busy").textContent).toBe("yes");
    expect(rowStatus(qc)).toBe("active");

    await advanceHealthy(stream, 105_000);
    expect(mockRequestStatusSnapshot.mock.calls.length).toBe(1);

    // The flight's stale idle answer races a NEWER busy delta (id 25 >
    // the flight's cursor 21) that lands mid-flight: the delta is
    // newer per-session truth, the commit must keep the badge AND the
    // row must stay "active" — the idle mirror may not invert fresher
    // evidence (the same discipline the busy-row latch applies).
    stream.push(`id: 20\ndata: {"event_id":20,"type":"session.status.snapshot_begin","workspace_id":"ws-1","snapshot_id":"f-race"}\n\n`);
    stream.push(`id: 21\ndata: {"event_id":21,"type":"session.status.snapshot","workspace_id":"ws-1","snapshot_id":"f-race","data":{"cursor":21,"sessions":[{"session_id":"phrase-one","derived_busy":false}]}}\n\n`);
    stream.push(`id: 25\ndata: {"event_id":25,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n`);
    stream.push(`id: 22\ndata: {"event_id":22,"type":"session.status.snapshot_complete","workspace_id":"ws-1","snapshot_id":"f-race","snapshot_ok":true}\n\n`);
    await advanceHealthy(stream, 3_000);
    await settle(100);

    expect(screen.getByTestId("busy").textContent).toBe("yes");
    expect(rowStatus(qc)).toBe("active");
  });

  it("M3 a degraded harness-keyed idle row flips no phrase-keyed cache row", async () => {
    vi.useFakeTimers();

    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    mount(qc);
    await settle(1_000);

    stream.push(`id: 5\ndata: {"event_id":5,"type":"session.status","workspace_id":"ws-1","session_id":"phrase-one","status":"busy"}\n\n`);
    await settle(1_000);
    expect(screen.getByTestId("busy").textContent).toBe("yes");
    expect(rowStatus(qc)).toBe("active");

    // The flight's only idle answer is DEGRADED (harness-keyed, no
    // learnable pair): it may not flip phrase-keyed rows — its id join
    // is partial (never worse: leave the projection alone).
    pushFlight(stream, { begin: 20, snap: 21, complete: 22 }, [{ session_id: "harness-unknown", derived_busy: false, session_id_deprecated: true }], 21, "f-deg");
    await advanceHealthy(stream, 3_000);
    await settle(100);

    expect(rowStatus(qc)).toBe("active");
  });
});
