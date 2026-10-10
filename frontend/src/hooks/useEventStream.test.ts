import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/react";
import { useEventStream } from "./useEventStream";

// --- Fetch mock helpers ---

type MockStreamController = {
  send: (line: string) => void;
  close: () => void;
};

function makeMockFetch(): {
  mock: ReturnType<typeof vi.fn>;
  controllers: MockStreamController[];
} {
  const controllers: MockStreamController[] = [];

  const mock = vi.fn().mockImplementation(() => {
    let closed = false;
    const chunks: Uint8Array[] = [];
    const listeners: Array<(chunk: Uint8Array) => void> = [];

    const ctrl: MockStreamController = {
      send(line: string) {
        if (closed) return;
        const chunk = new TextEncoder().encode(line);
        if (listeners.length > 0) {
          listeners.splice(0).forEach((l) => l(chunk));
        } else {
          chunks.push(chunk);
        }
      },
      close() {
        closed = true;
        listeners.splice(0).forEach((l) => l(new Uint8Array(0)));
      },
    };
    controllers.push(ctrl);

    const body = {
      getReader: () => ({
        read: () =>
          new Promise<{ done: boolean; value: Uint8Array }>((resolve) => {
            if (closed) return resolve({ done: true, value: new Uint8Array(0) });
            const next = chunks.shift();
            if (next) return resolve({ done: false, value: next });
            listeners.push((chunk) => {
              if (chunk.length === 0) resolve({ done: true, value: chunk });
              else resolve({ done: false, value: chunk });
            });
          }),
      }),
    };

    return Promise.resolve({ ok: true, body, status: 200 });
  });

  return { mock, controllers };
}

describe("useEventStream", () => {
  let fetchRestore: typeof globalThis.fetch;

  beforeEach(() => {
    fetchRestore = globalThis.fetch;
  });

  afterEach(() => {
    globalThis.fetch = fetchRestore;
    vi.restoreAllMocks();
  });

  it("does not connect when workspaceId is undefined", () => {
    const fetchMock = vi.fn();
    globalThis.fetch = fetchMock;
    renderHook(() => useEventStream(undefined, vi.fn()));
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("connects to the correct SSE endpoint", async () => {
    const { mock, controllers } = makeMockFetch();
    globalThis.fetch = mock;

    renderHook(() => useEventStream("sb-123", vi.fn()));

    await waitFor(() => expect(mock).toHaveBeenCalled());
    const [url, opts] = mock.mock.calls[0] as [string, RequestInit];
    expect(url).toContain("/workspaces/sb-123/session-events");
    expect((opts.headers as Record<string, string>)?.Accept).toBe("text/event-stream");
    expect(opts.credentials).toBe("include");

    controllers[0]!.close();
  });

  it("calls onEvent when a data event is received", async () => {
    const { mock, controllers } = makeMockFetch();
    globalThis.fetch = mock;

    const onEvent = vi.fn();
    renderHook(() => useEventStream("sb-123", onEvent));

    await waitFor(() => expect(mock).toHaveBeenCalled());

    act(() => {
      controllers[0]!.send(`data: ${JSON.stringify({ type: "session.status" })}\n\n`);
    });

    await waitFor(() => expect(onEvent).toHaveBeenCalledWith({ type: "session.status" }));
    controllers[0]!.close();
  });

  it("ignores malformed data lines", async () => {
    const { mock, controllers } = makeMockFetch();
    globalThis.fetch = mock;

    const onEvent = vi.fn();
    renderHook(() => useEventStream("sb-123", onEvent));

    await waitFor(() => expect(mock).toHaveBeenCalled());

    act(() => { controllers[0]!.send("data: not-json\n\n"); });
    await new Promise((r) => setTimeout(r, 20));

    expect(onEvent).not.toHaveBeenCalled();
    controllers[0]!.close();
  });

  it("aborts fetch on unmount", async () => {
    const abortSpy = vi.spyOn(AbortController.prototype, "abort");
    const { mock, controllers } = makeMockFetch();
    globalThis.fetch = mock;

    const { unmount } = renderHook(() => useEventStream("sb-123", vi.fn()));
    await waitFor(() => expect(mock).toHaveBeenCalled());

    unmount();
    expect(abortSpy).toHaveBeenCalled();
    controllers[0]?.close();
  });

  it("reconnects when workspaceId changes", async () => {
    const { mock, controllers } = makeMockFetch();
    globalThis.fetch = mock;

    const { rerender } = renderHook(
      ({ id }) => useEventStream(id, vi.fn()),
      { initialProps: { id: "sb-1" as string | undefined } },
    );

    await waitFor(() => expect(mock).toHaveBeenCalledTimes(1));
    expect((mock.mock.calls[0]![0] as string)).toContain("sb-1");

    // Close first stream and change workspaceId
    controllers[0]!.close();
    rerender({ id: "sb-2" });

    // Should connect to new workspace (after reconnect delay — use fake timers)
    await waitFor(() => expect(mock).toHaveBeenCalledTimes(2), { timeout: 5000 });
    expect((mock.mock.calls[1]![0] as string)).toContain("sb-2");
    controllers[1]?.close();
  });
});

describe("useEventStream — read timeout", () => {
  let fetchRestore: typeof globalThis.fetch;

  beforeEach(() => {
    fetchRestore = globalThis.fetch;
    vi.useFakeTimers();
  });

  afterEach(() => {
    globalThis.fetch = fetchRestore;
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("reconnects after READ_TIMEOUT_MS of silence", async () => {
    let connectCount = 0;

    const mock = vi.fn().mockImplementation(() => {
      connectCount++;
      return Promise.resolve({
        ok: true,
        body: {
          getReader: () => ({
            read: () => new Promise(() => {}), // hangs forever
            cancel: () => Promise.resolve(),
          }),
        },
        status: 200,
      });
    });
    globalThis.fetch = mock;

    renderHook(() => useEventStream("sb-hang", vi.fn()));

    // Wait for initial connect
    await vi.advanceTimersByTimeAsync(0);
    expect(connectCount).toBe(1);

    // Advance past READ_TIMEOUT_MS (35s) to trigger timeout
    await vi.advanceTimersByTimeAsync(35_000);

    // After timeout fires, scheduleReconnect sets a retry timer (MIN_RECONNECT_MS=2s * jitter max 1.5=3s)
    await vi.advanceTimersByTimeAsync(3_000);

    expect(connectCount).toBe(2);
  });

  it("aborts old controller on reconnect so hanging read is released", async () => {
    const abortCalls: string[] = [];
    let connectCount = 0;

    const mock = vi.fn().mockImplementation((_url: string, opts: RequestInit) => {
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
        status: 200,
      });
    });
    globalThis.fetch = mock;

    renderHook(() => useEventStream("sb-abort", vi.fn()));

    await vi.advanceTimersByTimeAsync(0);
    expect(connectCount).toBe(1);

    // Trigger timeout + reconnect (with jitter, max delay is 2000*1.5=3000)
    await vi.advanceTimersByTimeAsync(35_000 + 3_000);

    expect(abortCalls).toContain("abort-1");
    expect(connectCount).toBe(2);
  });
});

// #1646: the workspace stream gets the same liveness watchdog the user
// stream has (#1365). Both server endpoints heartbeat every 25s
// (heartbeatLoop → ":\n\n" comment frames), so a stream that carries
// bytes but no heartbeats and no events past 60s (2× heartbeat + margin)
// is semantically dead even though the read timeout stays fed — force a
// reconnect instead of trusting it. Against the pre-fix code these are
// RED: no watchdog exists here, so a proxy-fed but broker-dead
// connection never reconnects.
describe("useEventStream — liveness watchdog (#1646)", () => {
  let fetchRestore: typeof globalThis.fetch;
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
    fetchRestore = globalThis.fetch;
    vi.useFakeTimers();
  });

  afterEach(() => {
    globalThis.fetch = fetchRestore;
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("reconnects when bytes flow but no events or heartbeats arrive past the silence window", async () => {
    let connects = 0;
    const mock = vi.fn().mockImplementation(() => {
      connects++;
      return Promise.resolve({
        ok: true,
        body: { getReader: () => periodicReader(20_000, "x\n\n") },
        status: 200,
      });
    });
    globalThis.fetch = mock;

    renderHook(() => useEventStream("sb-liveness", vi.fn()));

    await vi.advanceTimersByTimeAsync(0);
    expect(connects).toBe(1);

    // 70s of garbage bytes: the read timeout never fires (bytes flow
    // every 20s < 35s), but the watchdog sees 60s+ of event/heartbeat
    // silence and forces a reconnect.
    await vi.advanceTimersByTimeAsync(70_000);
    expect(connects).toBeGreaterThanOrEqual(2);
  });

  it("does not reconnect while heartbeat comment frames flow", async () => {
    let connects = 0;
    const mock = vi.fn().mockImplementation(() => {
      connects++;
      return Promise.resolve({
        ok: true,
        body: { getReader: () => periodicReader(25_000, ":\n\n") },
        status: 200,
      });
    });
    globalThis.fetch = mock;

    renderHook(() => useEventStream("sb-heartbeat", vi.fn()));

    await vi.advanceTimersByTimeAsync(0);
    expect(connects).toBe(1);

    await vi.advanceTimersByTimeAsync(120_000);
    expect(connects).toBe(1); // heartbeats are liveness — a healthy idle stream must not flap
  });

  it("fires onReconnect on a forced reconnect, never on the first connect", async () => {
    const onReconnect = vi.fn();
    let connects = 0;
    const mock = vi.fn().mockImplementation(() => {
      connects++;
      const reader = connects === 1 ? periodicReader(20_000, "x\n\n") : periodicReader(25_000, ":\n\n");
      return Promise.resolve({
        ok: true,
        body: { getReader: () => reader },
        status: 200,
      });
    });
    globalThis.fetch = mock;

    renderHook(() => useEventStream("sb-reconn", vi.fn(), { onReconnect }));

    await vi.advanceTimersByTimeAsync(1_000);
    expect(connects).toBe(1);
    expect(onReconnect).not.toHaveBeenCalled(); // first connect is not a reconnect

    await vi.advanceTimersByTimeAsync(70_000);
    expect(connects).toBeGreaterThanOrEqual(2);
    expect(onReconnect).toHaveBeenCalledTimes(1);
  });

  // Cadence FLOOR during a total outage (r1 non-blocking note): the
  // watchdog plus the backoff chain must keep attempting reconnects —
  // a floor, not a ceiling, because jitteredDelay makes exact attempt
  // counts jitter-dependent; lower bounds cannot flake (#1532 lesson).
  it("keeps retrying through a total outage (floor: >=2 reconnect attempts within 3 liveness periods)", async () => {
    let connects = 0;
    const mock = vi.fn().mockImplementation(() => {
      connects++;
      return Promise.resolve({ ok: false, status: 503 });
    });
    globalThis.fetch = mock;

    renderHook(() => useEventStream("sb-outage", vi.fn()));

    await vi.advanceTimersByTimeAsync(3 * 60_000);
    expect(connects).toBeGreaterThanOrEqual(2);
  });
});
