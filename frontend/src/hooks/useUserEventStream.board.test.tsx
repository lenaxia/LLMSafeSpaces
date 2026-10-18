// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { vi, describe, it, expect, beforeEach, afterEach } from "vitest";
import React from "react";
import { useUserEventStream } from "./useUserEventStream";

// #1702: the user stream's board branch — board.event invalidates the
// board read model; reconnect reseeds it (§4.6 ii). Hook-level pins
// beside the page-level integration (BoardPage.sse.test.tsx).

vi.mock("../env", () => ({ getEnv: () => ({ apiBaseUrl: "/api/v1" }) }));
vi.mock("../lib/wsLog", () => ({ wsLog: vi.fn() }));

function streamFactory(frames: string[], closeAfter: boolean) {
  const encoder = new TextEncoder();
  let connectCount = 0;
  const fetchMock = vi.fn(async () => {
    connectCount++;
    const queued = [...frames];
    const stream = new ReadableStream<Uint8Array>({
      start(c) {
        for (const f of queued) c.enqueue(encoder.encode(f));
        if (closeAfter) c.close();
        // else hang open
      },
    });
    return new Response(stream, { status: 200, headers: { "Content-Type": "text/event-stream" } });
  });
  global.fetch = fetchMock as unknown as typeof fetch;
  return { fetchMock, connectCount: () => connectCount };
}

function setup() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const boards = { fetches: 0 };
  const BoardConsumer = () => {
    useQuery({
      queryKey: ["board", "tasks"],
      queryFn: async () => {
        boards.fetches++;
        return [];
      },
    });
    return null;
  };
  const wrapper = ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={qc}>
      {children}
      <BoardConsumer />
    </QueryClientProvider>
  );
  return { boards, wrapper };
}

describe("useUserEventStream — board.event branch (#1702)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("invalidates board queries on a board.event", async () => {
    const { fetchMock } = streamFactory(
      [`data: ${JSON.stringify({ type: "board.event", project_id: "p1", id: "e1", action: "move_stage" })}\n\n`],
      false,
    );
    const { boards, wrapper } = setup();
    renderHook(() => useUserEventStream(), { wrapper });

    // fetch 1 = the consumer's mount fetch; the streamed board.event
    // invalidates ["board"] → fetch 2 (the event frame rides stream #1,
    // so both may land before this assertion runs — >= 2 is the pin).
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    await waitFor(() => expect(boards.fetches).toBeGreaterThanOrEqual(2), { timeout: 4000 });
  });

  it("reseeds board queries on reconnect (the onConnect board invalidation)", async () => {
    // Stream #1 closes immediately → sseConnection sees done, backs off
    // (jittered ~0.5–1.5s), reconnects → onConnect runs with
    // hasConnectedOnce=true → ["board"] invalidation → refetch.
    const { connectCount } = streamFactory([], true);
    const { boards, wrapper } = setup();
    renderHook(() => useUserEventStream(), { wrapper });

    await waitFor(() => expect(connectCount()).toBeGreaterThanOrEqual(2), { timeout: 6000 });
    await waitFor(() => expect(boards.fetches).toBeGreaterThanOrEqual(2), { timeout: 4000 });
  });

  it("does not refetch board queries on non-board events", async () => {
    const { fetchMock } = streamFactory(
      [`data: ${JSON.stringify({ type: "workspace.phase", workspace_id: "ws-1", phase: "Active" })}\n\n`],
      false,
    );
    const { boards, wrapper } = setup();
    renderHook(() => useUserEventStream(), { wrapper });
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    // Exactly the mount fetch — no board invalidation on foreign events.
    await new Promise((r) => setTimeout(r, 300));
    expect(boards.fetches).toBe(1);
  });
});
