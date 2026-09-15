import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { vi, describe, it, expect, beforeEach, afterEach } from "vitest";
import { useUserEventStream } from "./useUserEventStream";
import React from "react";

// Mock getEnv
vi.mock("../env", () => ({
  getEnv: () => ({ apiBaseUrl: "/api/v1" }),
}));

// Mock wsLog
vi.mock("../lib/wsLog", () => ({
  wsLog: vi.fn(),
}));

function createWrapper() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return {
    qc,
    wrapper: ({ children }: { children: React.ReactNode }) => (
      <QueryClientProvider client={qc}>{children}</QueryClientProvider>
    ),
  };
}

describe("useUserEventStream", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    global.fetch = fetchMock;
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("connects to /api/v1/events on mount", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      body: { getReader: () => ({ read: () => new Promise(() => {}) }) },
    });

    const { wrapper } = createWrapper();
    renderHook(() => useUserEventStream(), { wrapper });

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalledWith(
        "/api/v1/events",
        expect.objectContaining({
          credentials: "include",
          headers: expect.objectContaining({ Accept: "text/event-stream" }),
        }),
      );
    });
  });

  it("does NOT send Last-Event-ID header on first connect", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      body: { getReader: () => ({ read: () => new Promise(() => {}) }) },
    });

    const { wrapper } = createWrapper();
    renderHook(() => useUserEventStream(), { wrapper });

    await waitFor(() => {
      expect(fetchMock).toHaveBeenCalled();
    });

    const headers = fetchMock.mock.calls[0]?.[1]?.headers as Record<string, string>;
    expect(headers["Last-Event-ID"]).toBeUndefined();
  });

  it("invalidates workspace queries on workspace.phase event", async () => {
    const encoder = new TextEncoder();
    const phaseEvent = `id: 1\ndata: {"event_id":1,"workspace_id":"ws-1","type":"workspace.phase","phase":"Active"}\n\n`;

    let readCount = 0;
    fetchMock.mockResolvedValue({
      ok: true,
      body: {
        getReader: () => ({
          read: () => {
            readCount++;
            if (readCount === 1) {
              return Promise.resolve({ done: false, value: encoder.encode(phaseEvent) });
            }
            return new Promise(() => {}); // hang after first event
          },
        }),
      },
    });

    const { qc, wrapper } = createWrapper();
    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");

    renderHook(() => useUserEventStream(), { wrapper });

    await waitFor(() => {
      expect(invalidateSpy).toHaveBeenCalledWith(
        expect.objectContaining({ queryKey: ["workspace-status", "ws-1"] }),
      );
    });
    expect(invalidateSpy).toHaveBeenCalledWith(
      expect.objectContaining({ queryKey: ["workspaces"] }),
    );
  });

  it("invalidates all workspace queries on resync event", async () => {
    const encoder = new TextEncoder();
    const resyncEvent = `data: {"type":"resync"}\n\n`;

    let readCount = 0;
    fetchMock.mockResolvedValue({
      ok: true,
      body: {
        getReader: () => ({
          read: () => {
            readCount++;
            if (readCount === 1) {
              return Promise.resolve({ done: false, value: encoder.encode(resyncEvent) });
            }
            return new Promise(() => {});
          },
        }),
      },
    });

    const { qc, wrapper } = createWrapper();
    const invalidateSpy = vi.spyOn(qc, "invalidateQueries");

    renderHook(() => useUserEventStream(), { wrapper });

    await waitFor(() => {
      expect(invalidateSpy).toHaveBeenCalledWith(
        expect.objectContaining({ queryKey: ["workspace-status"] }),
      );
    });
  });

  it("does NOT call onReconnect on initial connect", async () => {
    const onReconnect = vi.fn();
    const encoder = new TextEncoder();
    const event = `id: 1\ndata: {"event_id":1,"type":"workspace.phase","workspace_id":"ws-1","phase":"Active"}\n\n`;

    let readCount = 0;
    fetchMock.mockResolvedValue({
      ok: true,
      body: {
        getReader: () => ({
          read: () => {
            readCount++;
            if (readCount === 1) {
              return Promise.resolve({ done: false, value: encoder.encode(event) });
            }
            return new Promise(() => {});
          },
        }),
      },
    });

    const { wrapper } = createWrapper();
    renderHook(() => useUserEventStream({ onReconnect }), { wrapper });

    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    expect(onReconnect).not.toHaveBeenCalled();
  });

  it("calls onReconnect on second connect (reconnect)", async () => {
    const onReconnect = vi.fn();
    const encoder = new TextEncoder();
    const event = `id: 1\ndata: {"event_id":1,"type":"workspace.phase","workspace_id":"ws-1","phase":"Active"}\n\n`;

    let readCount = 0;
    fetchMock.mockResolvedValue({
      ok: true,
      body: {
        getReader: () => ({
          read: () => {
            readCount++;
            if (readCount === 1) {
              return Promise.resolve({ done: false, value: encoder.encode(event) });
            }
            if (readCount === 2) {
              return Promise.resolve({ done: true });
            }
            return new Promise(() => {});
          },
        }),
      },
    });

    const { wrapper } = createWrapper();
    renderHook(() => useUserEventStream({ onReconnect }), { wrapper });

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2), { timeout: 5000 });
    expect(onReconnect).toHaveBeenCalledTimes(1);
  });
});

describe("useUserEventStream — read timeout & abort", () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchMock = vi.fn();
    global.fetch = fetchMock;
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("reconnects after READ_TIMEOUT_MS of silence", async () => {
    let connectCount = 0;
    fetchMock.mockImplementation(() => {
      connectCount++;
      return Promise.resolve({
        ok: true,
        body: {
          getReader: () => ({
            read: () => new Promise(() => {}), // hangs forever
            cancel: () => Promise.resolve(),
          }),
        },
      });
    });

    const { wrapper } = createWrapper();
    renderHook(() => useUserEventStream(), { wrapper });

    await vi.advanceTimersByTimeAsync(0);
    expect(connectCount).toBe(1);

    // Advance past READ_TIMEOUT_MS (35s)
    await vi.advanceTimersByTimeAsync(35_000);

    // After timeout, scheduleReconnect fires after MIN_RECONNECT_MS (1s) * jitter max 1.5
    await vi.advanceTimersByTimeAsync(1_500);

    expect(connectCount).toBe(2);
  });

  it("aborts old controller on reconnect so hanging read is released", async () => {
    const abortCalls: string[] = [];
    let connectCount = 0;

    fetchMock.mockImplementation((_url: string, opts: RequestInit) => {
      connectCount++;
      const signal = opts.signal as AbortSignal;
      signal.addEventListener("abort", () => abortCalls.push(`abort-${connectCount}`));
      return Promise.resolve({
        ok: true,
        body: {
          getReader: () => ({
            read: () => new Promise(() => {}),
            cancel: () => Promise.resolve(),
          }),
        },
      });
    });

    const { wrapper } = createWrapper();
    renderHook(() => useUserEventStream(), { wrapper });

    await vi.advanceTimersByTimeAsync(0);
    expect(connectCount).toBe(1);

    // Trigger timeout + reconnect delay (with jitter)
    await vi.advanceTimersByTimeAsync(35_000 + 1_500);

    expect(abortCalls).toContain("abort-1");
    expect(connectCount).toBe(2);
  });
});

// --- #1365: liveness watchdog — a semantically dead stream (bytes flowing,
// no events, no heartbeats) must be reconnected so snapshot flights re-run. ---

describe("useUserEventStream — liveness watchdog (#1365)", () => {
  let fetchMock: ReturnType<typeof vi.fn>;
  const encoder = new TextEncoder();

  // A reader that resolves one chunk every intervalMs — simulates a
  // proxy/keepalive cadence that keeps the read timeout fed without ever
  // carrying a data event or heartbeat comment.
  function periodicReader(intervalMs: number, chunk: string) {
    return {
      read: () =>
        new Promise<{ done: boolean; value: Uint8Array }>((resolve) => {
          setTimeout(() => resolve({ done: false, value: encoder.encode(chunk) }), intervalMs);
        }),
      cancel: () => Promise.resolve(),
    };
  }

  beforeEach(() => {
    fetchMock = vi.fn();
    global.fetch = fetchMock;
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("reconnects when the stream carries bytes but no events or heartbeats past the silence window", async () => {
    let connects = 0;
    fetchMock.mockImplementation(() => {
      connects++;
      return Promise.resolve({
        ok: true,
        body: { getReader: () => periodicReader(20_000, "x\n\n") },
      });
    });

    const { wrapper } = createWrapper();
    renderHook(() => useUserEventStream(), { wrapper });

    await vi.advanceTimersByTimeAsync(0);
    expect(connects).toBe(1);

    // 70s of garbage bytes: read timeout never fires (bytes flow every 20s),
    // but the watchdog sees 60s+ of event silence and forces a reconnect.
    await vi.advanceTimersByTimeAsync(70_000);
    expect(connects).toBeGreaterThanOrEqual(2);
  });

  it("does not reconnect while heartbeat comment frames flow", async () => {
    let connects = 0;
    fetchMock.mockImplementation(() => {
      connects++;
      return Promise.resolve({
        ok: true,
        body: { getReader: () => periodicReader(25_000, ":\n\n") },
      });
    });

    const { wrapper } = createWrapper();
    renderHook(() => useUserEventStream(), { wrapper });

    await vi.advanceTimersByTimeAsync(0);
    expect(connects).toBe(1);

    await vi.advanceTimersByTimeAsync(120_000);
    expect(connects).toBe(1); // heartbeats are liveness — a healthy idle stream must not flap
  });
});
