// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// RED-FIRST PIN (live-update reliability): a `resync` event on the user
// stream must reconcile busy state, exactly like a reconnect does.
//
// Server-verified premise (api/internal/services/eventbroker/broker.go:64,
// api/internal/handlers/stream_user_events.go:266): when a subscriber's
// channel overflows, the broker DROPS events for that subscriber and tells
// it so by sending {"type":"resync"}. The dropped events may include the
// session.status=idle that would have cleared a latched busy badge.
//
// The repro, at the integration level (real useUserEventStream against a
// push-controllable fake fetch stream):
//   1. connect #1 delivers session.status=busy → busy latches via SSE
//      (REST says idle the whole time — the session genuinely finished).
//   2. The broker drops the idle for this subscriber and sends resync
//      ON THE SAME HEALTHY CONNECTION (no reconnect ever fires — the
//      stream keeps flowing; heartbeats are not even needed inside the
//      assertion window, which stays far below the 35s read timeout and
//      the 60s liveness watchdog).
//   3. Desired: resync = admitted event loss → the client must reconcile
//      busy from REST (wipe + sessions refetch + gated re-seed), like
//      onReconnect does. The badge must clear.
//
// Against current code this pin is RED: the provider's resync branch
// (SessionActivityProvider.tsx resync handler) clears hung/flights/
// snapshots but never busySessions, and the hook's resync branch
// (useUserEventStream.ts) invalidates workspaces/workspace-status but
// never ["sessions"] — so no reseed can even fire. Busy latches until a
// reconnect, a phase change, or the user hitting stop.

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

// A push-controlled reader: delivers chunks as they are pushed by the
// test; a read with nothing queued pends until the next push (the
// connection stays open and healthy — no close, no error).
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

function SessionsProbe() {
  useQuery({
    queryKey: ["sessions", "ws-1"],
    queryFn: () => mockGetSessions("ws-1"),
  });
  const busy = useIsSessionBusy("sess-1");
  return <span data-testid="busy">{busy ? "yes" : "no"}</span>;
}

describe("SessionActivityProvider — resync must reconcile busy (live-update reliability)", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  let stream: ReturnType<typeof pushedReader>;

  beforeEach(() => {
    vi.clearAllMocks();
    fetchMock = vi.fn();
    global.fetch = fetchMock;
    mockGetAlerts.mockResolvedValue([]);
    // REST truth for the whole test: the session FINISHED (idle).
    mockGetSessions.mockResolvedValue([
      {
        id: "sess-1",
        title: "t",
        messageCount: 0,
        status: "idle",
        hasUnread: false,
      } as SessionListItem,
    ]);

    stream = pushedReader([
      // connect #1: the busy transition arrives live…
      `id: 7\ndata: {"event_id":7,"type":"session.status","workspace_id":"ws-1","session_id":"sess-1","status":"busy"}\n\n`,
    ]);
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

  it("clears SSE-latched busy when the broker reports dropped events (resync) on a healthy connection", async () => {
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

    // Initial convergence: busy event delivered on the live stream →
    // badge latches (REST idle does not clobber SSE truth by design).
    await vi.advanceTimersByTimeAsync(1_000);
    expect(screen.getByTestId("busy").textContent).toBe("yes");

    // The broker overflowed this subscriber: the idle was dropped, and
    // the server says so — same connection, still healthy.
    stream.push(`data: {"type":"resync"}\n\n`);

    // Reconciliation window: long enough for a wipe + sessions refetch
    // + re-seed to land, far below any timeout/watchdog that could
    // ALSO heal via a reconnect (which would mask the resync path).
    await vi.advanceTimersByTimeAsync(5_000);

    // Exactly one connection — proves no reconnect did the healing.
    expect(fetchMock.mock.calls.length).toBe(1);
    // The badge must have reconciled from REST truth: the session is idle.
    expect(screen.getByTestId("busy").textContent).toBe("no");
  });
});
