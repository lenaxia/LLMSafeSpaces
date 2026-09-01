// useContractStream: the browser consumer of the API-proxied contract
// stream (US-69.10 part 1's GET /workspaces/:id/contract-events). Parses
// protojson StreamFrames with the generated schema, feeds them through
// the discard-rule fold, and enforces the client-side stream rules:
// snapshot-first per connection, reseed/re sync → immediate reconnect
// for a fresh stamped snapshot.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { useContractStream } from "./useContractStream";

function sseBody(obj: unknown): string {
  return `data: ${JSON.stringify(obj)}\n\n`;
}

function snapshotFrame(atSeq: number, sessions: Array<Record<string, unknown>> = []): string {
  return sseBody({ snapshot: { atSeq: String(atSeq), snapshot: { sessions } } });
}

function eventFrame(seq: number, ev: Record<string, unknown>): string {
  return sseBody({ event: { seq: String(seq), event: ev } });
}

function makeResponse(chunks: string[], opts?: { hangForever?: boolean; endAfterChunks?: boolean }) {
  const encoded = chunks.map((c) => new TextEncoder().encode(c));
  let idx = 0;
  // Chunks deliver in order; then the reader either parks forever (an
  // idle SSE connection) or ends the stream (server closed — the natural
  // reconnect trigger), per opts.
  const reader = {
    read: (): Promise<{ done: boolean; value?: Uint8Array }> => {
      if (idx >= encoded.length) {
        if (opts?.endAfterChunks) return Promise.resolve({ done: true });
        return new Promise<{ done: boolean; value: Uint8Array }>(() => {});
      }
      const value = encoded[idx++]!;
      return Promise.resolve({ done: false, value });
    },
    cancel: vi.fn(() => Promise.resolve()),
  };
  return { ok: true, status: 200, body: { getReader: () => reader } };
}

function makeMockFetch(chunks: string[], opts?: { hangForever?: boolean; endAfterChunks?: boolean }) {
  return vi.fn().mockResolvedValue(makeResponse(chunks, opts));
}

describe("useContractStream", () => {
  let fetchRestore: typeof globalThis.fetch;
  let consoleWarnRestore: typeof console.warn;

  beforeEach(() => {
    fetchRestore = globalThis.fetch;
    consoleWarnRestore = console.warn;
    console.warn = vi.fn();
    globalThis.window = Object.assign(window, {});
  });

  afterEach(() => {
    globalThis.fetch = fetchRestore;
    console.warn = consoleWarnRestore;
    vi.restoreAllMocks();
  });

  it("connects to /contract-events and applies the snapshot then events", async () => {
    const mock = makeMockFetch([
      snapshotFrame(7, [{ sessionId: "s0", status: "SESSION_STATUS_BUSY" }]),
      eventFrame(8, { type: "EVENT_TYPE_SESSION_STATUS", sessionId: "s0", status: "SESSION_STATUS_IDLE" }),
    ]);
    globalThis.fetch = mock;
    const onEvent = vi.fn();
    const onSnapshot = vi.fn();

    const { result } = renderHook(() =>
      useContractStream("ws1", { onEvent, onSnapshot }),
    );

    await waitFor(() => expect(mock).toHaveBeenCalledTimes(1));
    expect(mock).toHaveBeenCalledWith(
      "/api/v1/workspaces/ws1/contract-events",
      expect.objectContaining({ credentials: "include" }),
    );

    await waitFor(() => expect(onSnapshot).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(onEvent).toHaveBeenCalledTimes(1));

    expect(result.current.seq).toBe(8n);
    expect(result.current.sessions.get("s0")?.status).toBe(2); // IDLE
    // snapshot applied the BUSY status before events ran
    expect(onEvent).toHaveBeenCalledWith(
      expect.objectContaining({ type: 1, status: 2 }),
      8n,
    );
  });

  it("discards events with seq ≤ S (duplicate/stale delivery)", async () => {
    const mock = makeMockFetch([
      snapshotFrame(10, []),
      eventFrame(10, { type: "EVENT_TYPE_SESSION_STATUS", sessionId: "s0", status: "SESSION_STATUS_BUSY" }),
      eventFrame(9, { type: "EVENT_TYPE_SESSION_STATUS", sessionId: "s0", status: "SESSION_STATUS_BUSY" }),
      eventFrame(11, { type: "EVENT_TYPE_SESSION_STATUS", sessionId: "s0", status: "SESSION_STATUS_IDLE" }),
    ]);
    globalThis.fetch = mock;
    const onEvent = vi.fn();
    renderHook(() => useContractStream("ws1", { onEvent }));
    await waitFor(() => expect(onEvent).toHaveBeenCalledTimes(1));
    const seqs = onEvent.mock.calls.map((c) => c[1]);
    expect(seqs).toEqual([11n]);
  });

  it("a non-snapshot first frame is a protocol break — reconnects", async () => {
    const reader1Chunks = [eventFrame(1, { type: "EVENT_TYPE_SESSION_STATUS", sessionId: "s0", status: "SESSION_STATUS_BUSY" })];
    const mock = vi.fn()
      .mockImplementationOnce(async () => makeResponse(reader1Chunks))
      .mockImplementationOnce(async () => makeResponse([snapshotFrame(3, [])], { hangForever: true }));
    globalThis.fetch = mock;
    const onSnapshot = vi.fn();
    const onEvent = vi.fn();
    renderHook(() => useContractStream("ws1", { onEvent, onSnapshot }));
    await waitFor(() => expect(mock).toHaveBeenCalledTimes(2), { timeout: 3000 });
    await waitFor(() => expect(onSnapshot).toHaveBeenCalledTimes(1));
    expect(onEvent).not.toHaveBeenCalled();
  });

  it("named resync event forces an immediate reconnect", async () => {
    const mock = vi.fn()
      .mockImplementationOnce(async () => makeResponse([snapshotFrame(1, []), `event: resync\ndata: {}\n\n`]))
      .mockImplementationOnce(async () => makeResponse([snapshotFrame(5, [])], { hangForever: true }));
    globalThis.fetch = mock;
    const onSnapshot = vi.fn();
    renderHook(() => useContractStream("ws1", { onSnapshot }));
    await waitFor(() => expect(onSnapshot).toHaveBeenCalledTimes(2), { timeout: 3000 });
    expect(mock).toHaveBeenCalledTimes(2);
  });

  it("reseeded frame forces a reconnect whose fresh snapshot re-seeds the fold", async () => {
    const mock = vi.fn()
      .mockImplementationOnce(async () => makeResponse([
        snapshotFrame(1, []),
        sseBody({ reseeded: { seq: "2", reason: "RESEED_REASON_BOOT" } }),
      ]))
      .mockImplementationOnce(async () => makeResponse([snapshotFrame(9, [])], { hangForever: true }));
    globalThis.fetch = mock;
    const onSnapshot = vi.fn();
    const { result } = renderHook(() => useContractStream("ws1", { onSnapshot }));
    await waitFor(() => expect(onSnapshot).toHaveBeenCalledTimes(2), { timeout: 3000 });
    expect(result.current.seq).toBe(9n);
  });

  it("onReconnect fires on re-connections only", async () => {
    const mock = vi.fn()
      .mockImplementationOnce(async () => makeResponse([snapshotFrame(1, [])], { endAfterChunks: true }))
      .mockImplementationOnce(async () => makeResponse([snapshotFrame(2, [])]));
    globalThis.fetch = mock;
    const onReconnect = vi.fn();
    renderHook(() => useContractStream("ws1", { onReconnect }));
    await waitFor(() => expect(mock).toHaveBeenCalledTimes(2), { timeout: 8000 });
    await waitFor(() => expect(onReconnect).toHaveBeenCalledTimes(1));
    expect(mock).toHaveBeenCalledTimes(2);
  });

  it("skips connecting without a workspace id", async () => {
    const mock = makeMockFetch([]);
    globalThis.fetch = mock;
    renderHook(() => useContractStream(undefined, {}));
    await new Promise((r) => setTimeout(r, 20));
    expect(mock).not.toHaveBeenCalled();
  });

  it("ignores malformed frames without breaking the stream", async () => {
    const mock = makeMockFetch([
      `data: {not json\n\n`,
      snapshotFrame(4, []),
      eventFrame(5, { type: "EVENT_TYPE_SESSION_STATUS", sessionId: "s0", status: "SESSION_STATUS_BUSY" }),
    ]);
    globalThis.fetch = mock;
    const onEvent = vi.fn();
    const { result } = renderHook(() => useContractStream("ws1", { onEvent }));
    await waitFor(() => expect(onEvent).toHaveBeenCalledTimes(1));
    expect(result.current.seq).toBe(5n);
  });
});
