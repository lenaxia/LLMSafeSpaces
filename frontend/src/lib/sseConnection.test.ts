import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { createSSEConnection } from "./sseConnection";

// --- Helpers ---

function makeMockReader(opts?: { hangForever?: boolean; chunks?: string[] }) {
  const chunks = (opts?.chunks ?? []).map((c) => new TextEncoder().encode(c));
  let idx = 0;
  return {
    read: () => {
      if (opts?.hangForever || idx >= chunks.length) {
        return new Promise<{ done: boolean; value: Uint8Array }>(() => {});
      }
      const value = chunks[idx++]!;
      return Promise.resolve({ done: false, value });
    },
    cancel: vi.fn(() => Promise.resolve()),
  };
}

function makeMockFetch(reader: ReturnType<typeof makeMockReader>, status = 200) {
  return vi.fn().mockResolvedValue({
    ok: status >= 200 && status < 300,
    status,
    body: { getReader: () => reader },
  });
}

describe("createSSEConnection", () => {
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

  it("calls fetch with correct URL, headers, credentials, and signal", async () => {
    const reader = makeMockReader({ hangForever: true });
    const mock = makeMockFetch(reader);
    globalThis.fetch = mock;

    const conn = createSSEConnection({
      url: "/api/v1/events",
      onEvent: vi.fn(),
    });

    await vi.advanceTimersByTimeAsync(0);

    expect(mock).toHaveBeenCalledWith(
      "/api/v1/events",
      expect.objectContaining({
        credentials: "include",
        headers: expect.objectContaining({ Accept: "text/event-stream" }),
        signal: expect.any(AbortSignal),
      }),
    );

    conn.destroy();
  });

  it("passes custom headers to fetch", async () => {
    const reader = makeMockReader({ hangForever: true });
    const mock = makeMockFetch(reader);
    globalThis.fetch = mock;

    const conn = createSSEConnection({
      url: "/api/v1/events",
      headers: { "Last-Event-ID": "42" },
      onEvent: vi.fn(),
    });

    await vi.advanceTimersByTimeAsync(0);

    const headers = mock.mock.calls[0]?.[1]?.headers;
    expect(headers["Last-Event-ID"]).toBe("42");

    conn.destroy();
  });

  it("parses SSE data lines and calls onEvent", async () => {
    const reader = makeMockReader({
      chunks: [`data: {"type":"workspace.phase","phase":"Active"}\n\n`],
    });
    const mock = makeMockFetch(reader);
    globalThis.fetch = mock;
    const onEvent = vi.fn();

    const conn = createSSEConnection({ url: "/test", onEvent });

    await vi.advanceTimersByTimeAsync(0);
    // Let microtasks settle
    await vi.advanceTimersByTimeAsync(0);

    expect(onEvent).toHaveBeenCalledWith({ type: "workspace.phase", phase: "Active" });
    conn.destroy();
  });

  it("ignores malformed JSON data lines without throwing", async () => {
    const reader = makeMockReader({
      chunks: [`data: not-json\n\n`, `data: {"type":"ok"}\n\n`],
    });
    const mock = makeMockFetch(reader);
    globalThis.fetch = mock;
    const onEvent = vi.fn();

    const conn = createSSEConnection({ url: "/test", onEvent });

    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(0);

    expect(onEvent).toHaveBeenCalledTimes(1);
    expect(onEvent).toHaveBeenCalledWith({ type: "ok" });
    conn.destroy();
  });

  it("calls reader.cancel() and reconnects after READ_TIMEOUT_MS of silence", async () => {
    const reader = makeMockReader({ hangForever: true });
    let connectCount = 0;
    const mock = vi.fn().mockImplementation(() => {
      connectCount++;
      return Promise.resolve({
        ok: true,
        status: 200,
        body: { getReader: () => reader },
      });
    });
    globalThis.fetch = mock;

    const conn = createSSEConnection({
      url: "/test",
      onEvent: vi.fn(),
      readTimeoutMs: 35_000,
      minReconnectMs: 1_000,
    });

    await vi.advanceTimersByTimeAsync(0);
    expect(connectCount).toBe(1);

    // Advance past timeout
    await vi.advanceTimersByTimeAsync(35_000);

    // reader.cancel() should have been called
    expect(reader.cancel).toHaveBeenCalled();

    // Advance past reconnect delay (with jitter, max is minReconnect * 1.5)
    await vi.advanceTimersByTimeAsync(1_500);

    expect(connectCount).toBe(2);
    conn.destroy();
  });

  it("aborts the old AbortController on reconnect", async () => {
    const abortCalls: number[] = [];
    let connectCount = 0;

    const mock = vi.fn().mockImplementation((_url: string, opts: RequestInit) => {
      connectCount++;
      const n = connectCount;
      (opts.signal as AbortSignal).addEventListener("abort", () => abortCalls.push(n));
      return Promise.resolve({
        ok: true,
        status: 200,
        body: {
          getReader: () => ({
            read: () => new Promise(() => {}),
            cancel: vi.fn(() => Promise.resolve()),
          }),
        },
      });
    });
    globalThis.fetch = mock;

    const conn = createSSEConnection({
      url: "/test",
      onEvent: vi.fn(),
      readTimeoutMs: 100,
      minReconnectMs: 50,
    });

    await vi.advanceTimersByTimeAsync(0);
    expect(connectCount).toBe(1);

    // Timeout + reconnect
    await vi.advanceTimersByTimeAsync(100 + 75); // 75 to cover jitter

    expect(abortCalls).toContain(1);
    expect(connectCount).toBe(2);
    conn.destroy();
  });

  it("applies exponential backoff with jitter on repeated failures", async () => {
    // Pin Math.random to 0 so jitter = base * 0.5 (deterministic).
    // jitteredDelay(base) = base * (0.5 + 0) = base * 0.5
    vi.spyOn(Math, "random").mockReturnValue(0);

    let connectCount = 0;

    const mock = vi.fn().mockImplementation(() => {
      connectCount++;
      return Promise.resolve({ ok: false, status: 503, body: null });
    });
    globalThis.fetch = mock;

    const conn = createSSEConnection({
      url: "/test",
      onEvent: vi.fn(),
      minReconnectMs: 1_000,
      maxReconnectMs: 30_000,
    });

    // Initial connect (synchronous kick-off)
    await vi.advanceTimersByTimeAsync(0);
    expect(connectCount).toBe(1);

    // First retry delay = 1000 * 0.5 = 500ms — retryDelay doubles to 2000 inside timer
    await vi.advanceTimersByTimeAsync(500);
    expect(connectCount).toBe(2);

    // Second retry delay = 2000 * 0.5 = 1000ms
    await vi.advanceTimersByTimeAsync(1_000);
    expect(connectCount).toBe(3);

    conn.destroy();
  });

  it("resets backoff after successful connection", async () => {
    let connectCount = 0;

    const mock = vi.fn().mockImplementation(() => {
      connectCount++;
      if (connectCount === 1) {
        // First attempt fails
        return Promise.resolve({ ok: false, status: 503, body: null });
      }
      // Second attempt succeeds but hangs
      return Promise.resolve({
        ok: true,
        status: 200,
        body: {
          getReader: () => ({
            read: () => new Promise(() => {}),
            cancel: vi.fn(() => Promise.resolve()),
          }),
        },
      });
    });
    globalThis.fetch = mock;

    const onConnect = vi.fn();
    const conn = createSSEConnection({
      url: "/test",
      onEvent: vi.fn(),
      onConnect,
      minReconnectMs: 1_000,
    });

    await vi.advanceTimersByTimeAsync(0); // fail
    await vi.advanceTimersByTimeAsync(1_500); // retry succeeds

    expect(onConnect).toHaveBeenCalledTimes(1);
    conn.destroy();
  });

  it("calls onDisconnect when stream ends", async () => {
    const reader = {
      read: vi.fn()
        .mockResolvedValueOnce({ done: false, value: new TextEncoder().encode("data: {}\n\n") })
        .mockResolvedValueOnce({ done: true, value: new Uint8Array(0) }),
      cancel: vi.fn(() => Promise.resolve()),
    };
    const mock = makeMockFetch(reader as any);
    globalThis.fetch = mock;

    const onDisconnect = vi.fn();
    const conn = createSSEConnection({
      url: "/test",
      onEvent: vi.fn(),
      onDisconnect,
    });

    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(0);

    expect(onDisconnect).toHaveBeenCalled();
    conn.destroy();
  });

  it("destroy() cancels everything and does not reconnect", async () => {
    const reader = makeMockReader({ hangForever: true });
    let connectCount = 0;
    const mock = vi.fn().mockImplementation(() => {
      connectCount++;
      return Promise.resolve({
        ok: true,
        status: 200,
        body: { getReader: () => reader },
      });
    });
    globalThis.fetch = mock;

    const conn = createSSEConnection({
      url: "/test",
      onEvent: vi.fn(),
      readTimeoutMs: 100,
      minReconnectMs: 50,
    });

    await vi.advanceTimersByTimeAsync(0);
    expect(connectCount).toBe(1);

    conn.destroy();

    // Advance well past timeout + reconnect
    await vi.advanceTimersByTimeAsync(10_000);

    expect(connectCount).toBe(1); // no reconnect after destroy
  });

  it("reader.cancel() in finally block does not throw when reader already cancelled by timeout", async () => {
    // When read timeout fires, the production code does `await reader.cancel()` then breaks.
    // The `finally` block then calls `reader.cancel()` again on the same cancelled reader.
    // The second call must not propagate — it is caught by `.catch(() => {})`.
    const cancelSpy = vi.fn()
      .mockResolvedValueOnce(undefined) // first cancel (timeout path) succeeds
      .mockRejectedValueOnce(new TypeError("Cannot cancel a stream that already has a reader")); // second cancel (finally) throws
    const reader = {
      read: () => new Promise<never>(() => {}), // hangs forever — timeout fires first
      cancel: cancelSpy,
    };
    const mock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      body: { getReader: () => reader },
    });
    globalThis.fetch = mock;
    const onDisconnect = vi.fn();

    const conn = createSSEConnection({
      url: "/test",
      onEvent: vi.fn(),
      onDisconnect,
      readTimeoutMs: 100,
      minReconnectMs: 1_000_000, // prevent reconnect from obscuring the result
    });

    await vi.advanceTimersByTimeAsync(0);
    // Fire read timeout — first cancel called, then finally calls second cancel (rejected)
    await expect(vi.advanceTimersByTimeAsync(100)).resolves.not.toThrow();

    // Both cancel calls were made
    expect(cancelSpy).toHaveBeenCalledTimes(2);
    // Error was suppressed — onDisconnect still fires normally
    expect(onDisconnect).toHaveBeenCalled();

    conn.destroy();
  });

  it("caps retryDelay at maxReconnectMs", async () => {
    vi.spyOn(Math, "random").mockReturnValue(0);

    let connectCount = 0;
    const mock = vi.fn().mockImplementation(() => {
      connectCount++;
      return Promise.resolve({ ok: false, status: 503, body: null });
    });
    globalThis.fetch = mock;

    const conn = createSSEConnection({
      url: "/test",
      onEvent: vi.fn(),
      minReconnectMs: 1_000,
      maxReconnectMs: 2_000, // cap at 2s
    });

    await vi.advanceTimersByTimeAsync(0);
    expect(connectCount).toBe(1);

    // Retry 1: delay = 1000 * 0.5 = 500ms, then retryDelay doubles to 2000
    await vi.advanceTimersByTimeAsync(500);
    expect(connectCount).toBe(2);

    // Retry 2: delay = 2000 * 0.5 = 1000ms, then retryDelay would double to 4000 but is capped at 2000
    await vi.advanceTimersByTimeAsync(1_000);
    expect(connectCount).toBe(3);

    // Retry 3: delay = 2000 * 0.5 = 1000ms (capped — same as retry 2, not 4000*0.5=2000)
    await vi.advanceTimersByTimeAsync(1_000);
    expect(connectCount).toBe(4);

    conn.destroy();
  });
});

describe("createSSEConnection named events (contract stream)", () => {
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

  it("delivers the event name alongside the parsed data", async () => {
    const reader = makeMockReader({
      chunks: [
        `event: resync\ndata: {"reason":"dropped"}\n\n`,
        `data: {"seq":"8"}\n\n`,
      ],
    });
    globalThis.fetch = makeMockFetch(reader);
    const onEvent = vi.fn();
    const conn = createSSEConnection({ url: "/test", onEvent });
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(0);
    expect(onEvent).toHaveBeenCalledTimes(2);
    expect(onEvent).toHaveBeenNthCalledWith(1, { reason: "dropped" }, "resync");
    expect(onEvent).toHaveBeenNthCalledWith(2, { seq: "8" });
    conn.destroy();
  });

  it("a nameless block after a named one is default again (no leak)", async () => {
    const reader = makeMockReader({
      chunks: [`event: resync\ndata: {}\n\ndata: {"a":1}\n\n`],
    });
    globalThis.fetch = makeMockFetch(reader);
    const onEvent = vi.fn();
    const conn = createSSEConnection({ url: "/test", onEvent });
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(0);
    expect(onEvent).toHaveBeenNthCalledWith(2, { a: 1 });
    conn.destroy();
  });

  it("event name set before any data in the same block only", async () => {
    const reader = makeMockReader({
      // An event: line in a block WITHOUT a data line is ignored entirely.
      chunks: [`event: resync\n\n`, `data: {"b":2}\n\n`],
    });
    globalThis.fetch = makeMockFetch(reader);
    const onEvent = vi.fn();
    const conn = createSSEConnection({ url: "/test", onEvent });
    await vi.advanceTimersByTimeAsync(0);
    await vi.advanceTimersByTimeAsync(0);
    expect(onEvent).toHaveBeenCalledTimes(1);
    expect(onEvent).toHaveBeenCalledWith({ b: 2 });
    conn.destroy();
  });

  it("reconnect() aborts the current fetch and reconnects immediately with reset backoff", async () => {
    const reader1 = makeMockReader({ hangForever: true });
    const reader2 = makeMockReader({ hangForever: true });
    const mock = vi
      .fn()
      .mockResolvedValueOnce({ ok: true, status: 200, body: { getReader: () => reader1 } })
      .mockResolvedValueOnce({ ok: true, status: 200, body: { getReader: () => reader2 } });
    globalThis.fetch = mock;
    const conn = createSSEConnection({ url: "/test", onEvent: vi.fn() });
    await vi.advanceTimersByTimeAsync(0);
    expect(mock).toHaveBeenCalledTimes(1);

    conn.reconnect();
    await vi.advanceTimersByTimeAsync(0);

    expect(mock).toHaveBeenCalledTimes(2);
    // A fresh AbortSignal per connection.
    expect(mock.mock.calls[1]?.[1]?.signal).not.toBe(mock.mock.calls[0]?.[1]?.signal);
    conn.destroy();
  });

  it("reconnect() on a destroyed connection is a no-op", async () => {
    const reader = makeMockReader({ hangForever: true });
    const mock = makeMockFetch(reader);
    globalThis.fetch = mock;
    const conn = createSSEConnection({ url: "/test", onEvent: vi.fn() });
    await vi.advanceTimersByTimeAsync(0);
    conn.destroy();
    conn.reconnect();
    await vi.advanceTimersByTimeAsync(0);
    expect(mock).toHaveBeenCalledTimes(1);
  });
});
