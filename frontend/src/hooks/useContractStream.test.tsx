// useContractStream: the browser consumer of the API-proxied contract
// stream (US-69.10 part 1's GET /workspaces/:id/contract-events). Parses
// protojson StreamFrames with the generated schema, feeds them through
// the discard-rule fold, and enforces the client-side stream rules:
// snapshot-first per connection, reseed/re sync → immediate reconnect
// for a fresh stamped snapshot.

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import {
  learnSessionAlias,
  resetSessionAliases,
  resolveSessionAlias,
  sessionAliasRekeyCount,
} from "../session/sessionAliases";
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
    resetSessionAliases();
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

  // #1817: late-learned aliases must re-key the fold WITHOUT waiting for
  // the next contract frame. The pair arrives on the OTHER surface (the
  // user stream in SessionActivityProvider); the already-folded legacy row
  // is harness-keyed.
  it("re-keys a harness-keyed fold row immediately when a pair is learned from the other surface", async () => {
    const HARNESS = "ses_late_harness";
    const PHRASE = "late-phrase-7";
    const mock = makeMockFetch([
      snapshotFrame(4, [{ sessionId: HARNESS, status: "SESSION_STATUS_IDLE" }]),
    ]);
    globalThis.fetch = mock;
    const { result } = renderHook(() => useContractStream("ws-late", {}));
    await waitFor(() => expect(result.current.sessions.has(HARNESS)).toBe(true));

    // The contract reader is parked after the snapshot — no further frame
    // is delivered. Learning happens on the user stream.
    act(() => {
      expect(learnSessionAlias("ws-late", PHRASE, HARNESS)).toBe(true);
    });

    await waitFor(() => expect(result.current.sessions.has(PHRASE)).toBe(true));
    expect(result.current.sessions.has(HARNESS)).toBe(false);
    expect(result.current.sessions.get(PHRASE)?.internalSessionId).toBe(HARNESS);
  });

  // #1817 hot path: PART_DELTA frames carry no alias change, so the O(all
  // sessions) re-key must not run on them.
  it("does not re-key on PART_DELTA when no alias version changed", async () => {
    const PHRASE = "quiet-mountain-9";
    const HARNESS = "ses_hot_harness";
    const mock = makeMockFetch([
      snapshotFrame(5, [{
        sessionId: PHRASE,
        internalSessionId: HARNESS,
        status: "SESSION_STATUS_BUSY",
        inFlightParts: [{ id: "p1", type: "PART_TYPE_TEXT", text: "a" }],
      }]),
      eventFrame(6, { type: "EVENT_TYPE_PART_DELTA", sessionId: PHRASE, internalSessionId: HARNESS, partId: "p1", delta: "b" }),
    ]);
    globalThis.fetch = mock;
    let rekeysAtSnapshot = -1;
    const onSnapshot = vi.fn(() => {
      rekeysAtSnapshot = sessionAliasRekeyCount();
    });
    const onEvent = vi.fn();
    renderHook(() => useContractStream("ws-hot", { onSnapshot, onEvent }));

    await waitFor(() => expect(onSnapshot).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(onEvent).toHaveBeenCalledTimes(1));
    expect(rekeysAtSnapshot).toBeGreaterThanOrEqual(0);
    expect(sessionAliasRekeyCount()).toBe(rekeysAtSnapshot);
  });

  // #1817: only APPLIED frames may teach the alias map. A stale/duplicate
  // frame (seq <= fold.seq) is discarded before translation, so it cannot
  // overwrite the current harness→phrase mapping.
  it("does not learn aliases from a stale duplicate frame", async () => {
    const PHRASE_NEW = "fresh-phrase-1";
    const PHRASE_OLD = "stale-phrase-0";
    const HARNESS = "ses_stale_harness";
    const mock = makeMockFetch([
      snapshotFrame(10, [{ sessionId: PHRASE_NEW, internalSessionId: HARNESS, status: "SESSION_STATUS_IDLE" }]),
      // Stale duplicate carrying an OLD phrase for the same harness. If
      // learned, it would overwrite the fresh mapping.
      eventFrame(9, {
        type: "EVENT_TYPE_SESSION_STATUS",
        sessionId: PHRASE_OLD,
        internalSessionId: HARNESS,
        status: "SESSION_STATUS_BUSY",
      }),
      // Applied frame without an additive pair — does not re-teach the map,
      // so the assertion below reflects what the stale frame did.
      eventFrame(11, {
        type: "EVENT_TYPE_SESSION_STATUS",
        sessionId: PHRASE_NEW,
        status: "SESSION_STATUS_BUSY",
      }),
    ]);
    globalThis.fetch = mock;
    const onEvent = vi.fn();
    renderHook(() => useContractStream("ws-stale", { onEvent }));

    await waitFor(() => expect(onEvent).toHaveBeenCalledTimes(1));
    expect(onEvent.mock.calls[0]?.[1]).toBe(11n);
    expect(resolveSessionAlias("ws-stale", HARNESS)).toBe(PHRASE_NEW);
  });
});
