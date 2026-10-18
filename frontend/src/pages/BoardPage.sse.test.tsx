// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { render, screen, waitFor, act } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { describe, it, expect, beforeEach, vi } from "vitest";
// boardTestUtils FIRST: its vi.mock registrations must evaluate before
// BoardPage resolves ../api/board (import order is mock order here).
import {
  seedBoard,
  defaultTasks,
  mockListProjectTasks,
  mockListProjects,
  PROJECT_A,
  PROJECT_B,
} from "./boardTestUtils";
import { BoardPage } from "./BoardPage";
import { useUserEventStream } from "../hooks/useUserEventStream";

// §4.6 (design 0067, #1687-hardened): board rows update as the engine's
// events stream — board.event payloads on the user stream invalidate the
// board read model; SSE reconnect reseeds it. The harness mounts the REAL
// useUserEventStream beside the REAL BoardPage on one QueryClient and
// streams SSE frames through a mocked fetch (sseConnection's parser runs
// for real).

vi.mock("../env", () => ({ getEnv: () => ({ apiBaseUrl: "/api/v1" }) }));
vi.mock("../lib/wsLog", () => ({ wsLog: vi.fn() }));

let emitFrame: ((frame: string) => void) | null = null;
let closeStream: (() => void) | null = null;

function sseStream(): Response {
  const encoder = new TextEncoder();
  const stream = new ReadableStream<Uint8Array>({
    start(c) {
      emitFrame = (frame: string) => c.enqueue(encoder.encode(frame));
      closeStream = () => c.close();
    },
  });
  return new Response(stream, { status: 200, headers: { "Content-Type": "text/event-stream" } });
}

function Harness({ qc }: { qc: QueryClient }) {
  // The hook is what invalidates; the provider is app-level in prod —
  // both consume the SAME client here, exactly like the app shell.
  return (
    <QueryClientProvider client={qc}>
      <Inner />
    </QueryClientProvider>
  );
}

function Inner() {
  useUserEventStream();
  return (
    <MemoryRouter>
      <BoardPage />
    </MemoryRouter>
  );
}

describe("BoardPage — SSE live feed (§4.6, #1687 machinery reused)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    seedBoard();
    emitFrame = null;
    closeStream = null;
    global.fetch = vi.fn(async () => sseStream());
  });

  it("updates board rows when a board.event streams in", async () => {
    render(<Harness qc={new QueryClient({ defaultOptions: { queries: { retry: false } } })} />);
    // Card starts in in_review (tsk-cccc0003).
    await screen.findByTestId("task-card-tsk-cccc0003");
    expect(screen.getByTestId("task-card-tsk-cccc0003").getAttribute("data-stage")).toBe("in_review");

    // The engine moved it (e.g. the worker's token-guarded write) — the
    // event streams; the read model must refetch and move the card.
    const updated = defaultTasks().map((t) => (t.short_id === "tsk-cccc0003" ? { ...t, stage: "done" } : t));
    seedBoard(updated);

    await act(async () => {
      emitFrame?.(
        `data: ${JSON.stringify({
          type: "board.event",
          project_id: "proj-uuid-a",
          id: "evt-1",
          item_kind: "task",
          item_id: "task-uuid-3",
          action: "move_stage",
          payload: { to_stage: "done" },
        })}\n\n`,
      );
    });

    await waitFor(() => {
      expect(screen.getByTestId("task-card-tsk-cccc0003").getAttribute("data-stage")).toBe("done");
    });
    expect(mockListProjectTasks).toHaveBeenCalled();
  });

  it("reseeds the board on SSE reconnect (the #1646/#1687 discipline)", async () => {
    render(<Harness qc={new QueryClient({ defaultOptions: { queries: { retry: false } } })} />);
    await screen.findByTestId("swimlane-prj-aaaa1111");
    const callsAfterMount = mockListProjects.mock.calls.length;

    // A THIRD project appears while the tab is away; the stream closes
    // and sseConnection reconnects (fetch mock hands every connect a
    // fresh stream) — the onConnect path invalidates ["board"] and the
    // reseed picks the new project up.
    seedBoard(defaultTasks(), [
      PROJECT_A,
      PROJECT_B,
      { ...PROJECT_B, id: "proj-uuid-c", short_id: "prj-cccc3333", name: "Chandra" },
    ]);

    await act(async () => {
      closeStream?.();
    });

    // sseConnection backs off (jittered ~0.5–1.5s) before reconnecting.
    await waitFor(() => expect(mockListProjects.mock.calls.length).toBeGreaterThan(callsAfterMount), { timeout: 6000 });
    await waitFor(() => expect(screen.getByTestId("swimlane-prj-cccc3333")).toBeInTheDocument(), { timeout: 3000 });
  });

  it("leaves the board untouched by non-board events", async () => {
    render(<Harness qc={new QueryClient({ defaultOptions: { queries: { retry: false } } })} />);
    await screen.findByTestId("swimlane-prj-aaaa1111");
    const calls = mockListProjectTasks.mock.calls.length;

    await act(async () => {
      emitFrame?.(
        `data: ${JSON.stringify({ type: "workspace.phase", workspace_id: "ws-1", phase: "Active" })}\n\n`,
      );
    });
    await new Promise((r) => setTimeout(r, 50));
    expect(mockListProjectTasks.mock.calls.length).toBe(calls);
  });
});
