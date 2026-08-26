import { describe, expect, it, vi, beforeEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { useChatStream } from "./useChatStream";
import { ApiClientError } from "../api/client";

vi.mock("../api/messages", () => ({
  messagesApi: {
    sendAsync: vi.fn(), queueMessage: vi.fn().mockResolvedValue({ messageID: "msg_q_mock" }), getQueue: vi.fn().mockResolvedValue({ messages: [] }), deleteQueueMessage: vi.fn().mockResolvedValue(undefined),
    getHistory: vi.fn(),
  },
}));

vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getSession: vi.fn().mockResolvedValue({ status: "idle" }),
  },
}));

import { messagesApi } from "../api/messages";
import { workspacesApi } from "../api/workspaces";

// Helper: sends a message and signals idle after sendAsync resolves
async function sendAndIdle(
  result: ReturnType<typeof renderHook<ReturnType<typeof useChatStream>, unknown>>["result"],
  text: string,
  onComplete = vi.fn(),
) {
  let sendPromise!: Promise<void>;
  act(() => { sendPromise = result.current.send(text, onComplete); });
  // Wait for sendAsync to have been called (means idle resolver is set)
  await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());
  act(() => { result.current.notifySessionIdle("sess-1"); });
  await act(async () => { await sendPromise; });
  return onComplete;
}

describe("useChatStream", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("starts with streaming=false and no error", () => {
    const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));
    expect(result.current.streaming).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it("does nothing when workspaceId is undefined", async () => {
    const { result } = renderHook(() => useChatStream(undefined, "sess-1"));
    await act(async () => { result.current.send("hi", vi.fn()); });
    expect(messagesApi.sendAsync).not.toHaveBeenCalled();
  });

  it("does nothing when sessionId is undefined", async () => {
    const { result } = renderHook(() => useChatStream("sb-1", undefined));
    await act(async () => { result.current.send("hi", vi.fn()); });
    expect(messagesApi.sendAsync).not.toHaveBeenCalled();
  });

  it("calls messagesApi.sendAsync with correct params", async () => {
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));

    await sendAndIdle(result, "hi");

    expect(messagesApi.sendAsync).toHaveBeenCalledWith("sb-1", "sess-1", {
      parts: [{ type: "text", text: "hi" }],
      // D3 (#907): every send carries a clientMessageID (idempotency key).
      clientMessageID: expect.any(String),
    });
  });

  it("includes model in sendAsync when provided", async () => {
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));

    const model = { providerID: "anthropic", modelID: "claude-sonnet-4-5" };
    let sendPromise!: Promise<void>;
    act(() => { sendPromise = result.current.send("hello", vi.fn(), model); });
    await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());
    act(() => { result.current.notifySessionIdle("sess-1"); });
    await act(async () => { await sendPromise; });

    expect(messagesApi.sendAsync).toHaveBeenCalledWith("sb-1", "sess-1", {
      parts: [{ type: "text", text: "hello" }],
      model: { providerID: "anthropic", modelID: "claude-sonnet-4-5" },
      clientMessageID: expect.any(String),
    });
  });

  it("waits for notifySessionIdle before calling getHistory", async () => {
    let resolveSendAsync!: () => void;
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockReturnValue(
      new Promise<void>((r) => { resolveSendAsync = r; }),
    );
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));

    let sendPromise!: Promise<void>;
    act(() => { sendPromise = result.current.send("hi", vi.fn()); });

    // Resolve sendAsync
    act(() => { resolveSendAsync(); });

    // Give microtasks a tick to settle
    await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());
    await new Promise<void>((r) => setTimeout(r, 20));

    // getHistory must NOT be called yet — waiting for idle signal
    expect(messagesApi.getHistory).not.toHaveBeenCalled();

    // Now fire idle
    act(() => { result.current.notifySessionIdle("sess-1"); });
    await act(async () => { await sendPromise; });

    expect(messagesApi.getHistory).toHaveBeenCalledWith("sb-1", "sess-1");
  });

  it("ignores notifySessionIdle for a different sessionId", async () => {
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));

    let sendPromise!: Promise<void>;
    act(() => { sendPromise = result.current.send("hi", vi.fn()); });
    await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());

    // Wrong session — should be ignored
    act(() => { result.current.notifySessionIdle("sess-OTHER"); });
    await new Promise<void>((r) => setTimeout(r, 30));
    expect(messagesApi.getHistory).not.toHaveBeenCalled();

    // Correct session
    act(() => { result.current.notifySessionIdle("sess-1"); });
    await act(async () => { await sendPromise; });
    expect(messagesApi.getHistory).toHaveBeenCalled();
  });

  it("calls onComplete with last assistant message after idle signal", async () => {
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "user-1", role: "user", parts: [{ type: "text", text: "hi" }] },
      { id: "asst-1", role: "assistant", parts: [{ type: "text", text: "response" }] },
    ]);
    const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));
    const onComplete = vi.fn();

    await sendAndIdle(result, "hi", onComplete);

    expect(onComplete).toHaveBeenCalledWith(expect.objectContaining({
      id: "asst-1",
      role: "assistant",
    }));
  });

  it("sets streaming=true during send and false after", async () => {
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));

    let sendPromise!: Promise<void>;
    act(() => { sendPromise = result.current.send("hi", vi.fn()); });

    await vi.waitFor(() => expect(result.current.streaming).toBe(true));

    act(() => { result.current.notifySessionIdle("sess-1"); });
    await act(async () => { await sendPromise; });

    expect(result.current.streaming).toBe(false);
  });

  it("sets error when sendAsync rejects", async () => {
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("network error"));

    const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));
    const onComplete = vi.fn();

    await act(async () => { await result.current.send("hi", onComplete); });

    expect(result.current.error).toBe("network error");
    expect(result.current.streaming).toBe(false);
    expect(onComplete).not.toHaveBeenCalled();
  });

  describe("503 workspace-restart retry (issue 440)", () => {
    it("retries sendAsync on 503 (workspace restarting) then succeeds", async () => {
      // Issue 440: a credential-add triggers an in-place opencode restart;
      // the proxy returns 503+retryAfter during the ~10s window. The send
      // must retry rather than drop the user's message. Real timers with a
      // 1s retryAfter keeps the test fast.
      const err503 = new ApiClientError(503, { error: "workspace_restarting", retryAfter: 1 });
      (messagesApi.sendAsync as ReturnType<typeof vi.fn>)
        .mockRejectedValueOnce(err503)
        .mockResolvedValueOnce(undefined);
      (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

      const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));
      const onComplete = vi.fn();

      let sendPromise!: Promise<void>;
      act(() => { sendPromise = result.current.send("hi", onComplete); });
      // Wait for the retry to land (2nd call = success), then signal idle
      // so send() completes instead of waiting the full 60s idle timeout.
      // The 1s retryAfter sleep means waitFor must tolerate ≥1s.
      await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalledTimes(2), { timeout: 3000 });
      act(() => { result.current.notifySessionIdle("sess-1"); });
      await act(async () => { await sendPromise; });

      expect(messagesApi.sendAsync).toHaveBeenCalledTimes(2);
      expect(result.current.error).toBeNull();
      expect(result.current.streaming).toBe(false);
    });

    it("gives up after SEND_MAX_503_RETRIES and surfaces the 503 as an error", async () => {
      // After the bounded retry count the message is dropped with a visible
      // error so the loop cannot spin forever on a wedged workspace.
      const err503 = new ApiClientError(503, { error: "workspace_restarting", message: "Custom test message — agent is restarting", reason: "agent_restarting", retryAfter: 1 });
      (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockRejectedValue(err503);
      (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

      const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));
      const onComplete = vi.fn();

      await act(async () => { await result.current.send("hi", onComplete); });

      // Initial attempt + 3 retries = 4 calls.
      expect(messagesApi.sendAsync).toHaveBeenCalledTimes(4);
      expect(result.current.streaming).toBe(false);
      // The 503 propagates to the outer catch and surfaces as the
      // user-visible error message (the ApiClientError carries body.error).
      expect(result.current.error).toBe("Custom test message — agent is restarting");
      expect(onComplete).not.toHaveBeenCalled();
    });

    it("does not retry on 429 (handled by the separate at-cap path)", async () => {
      // Sanity: only 503 is a transient restart signal. 429 must NOT enter
      // the 503 retry loop — it surfaces via atCapRetryAfter instead.
      const err429 = new ApiClientError(429, { error: "rate limited", retryAfter: 5 });
      (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockRejectedValue(err429);

      const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));
      const onComplete = vi.fn();

      await act(async () => { await result.current.send("hi", onComplete); });

      expect(messagesApi.sendAsync).toHaveBeenCalledTimes(1);
      expect(result.current.atCapRetryAfter).toBe(5);
    });
  });

  it("sets error when getHistory rejects after idle signal", async () => {
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("history failed"));
    const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));
    const onComplete = vi.fn();

    await sendAndIdle(result, "hi", onComplete);

    expect(result.current.error).toBe("history failed");
    expect(result.current.streaming).toBe(false);
    expect(onComplete).not.toHaveBeenCalled();
  });

  it("handles empty history gracefully after idle signal", async () => {
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));
    const onComplete = vi.fn();

    await sendAndIdle(result, "hi", onComplete);

    expect(onComplete).toHaveBeenCalledWith(expect.objectContaining({
      role: "assistant",
      parts: [],
    }));
  });

  it("clearError resets error to null", async () => {
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("oops"));
    const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));

    await act(async () => { await result.current.send("hi", vi.fn()); });
    expect(result.current.error).toBe("oops");

    act(() => { result.current.clearError(); });
    expect(result.current.error).toBeNull();
  });

  it("does NOT install a beforeunload handler — refresh must not abort the in-flight LLM response", async () => {
    // Regression: prior to this fix, useChatStream installed a beforeunload
    // handler that called navigator.sendBeacon('/abort'), which killed the
    // in-flight LLM response on F5. Epic 15's reconnect machinery is the
    // correct way to handle refresh; aborting actively defeats it.
    const addSpy = vi.spyOn(window, "addEventListener");
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));
    await sendAndIdle(result, "hi");

    const beforeUnloadCalls = addSpy.mock.calls.filter(([evt]) => evt === "beforeunload");
    expect(beforeUnloadCalls).toHaveLength(0);

    addSpy.mockRestore();
  });

  // ─────────────────────────────────────────────────────────────────────
  // Race condition coverage
  // ─────────────────────────────────────────────────────────────────────

  it("RACE: idle SSE fires DURING sendAsync (before 204 arrives) — must still resolve and fetch history", async () => {
    // Realistic scenario: opencode emits session.status idle very fast (e.g.
    // cached/instant response or trivial completion). The SSE event lands
    // before the prompt_async HTTP response. In the old code, the idle
    // resolver was only installed AFTER `await sendAsync` returned, so the
    // early idle was silently dropped and we waited 60s for the timeout.
    let resolveSendAsync!: () => void;
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockReturnValue(
      new Promise<void>((r) => { resolveSendAsync = r; }),
    );
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "asst-fast", role: "assistant", parts: [{ type: "text", text: "instant" }] },
    ]);

    const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));
    const onComplete = vi.fn();

    let sendPromise!: Promise<void>;
    act(() => { sendPromise = result.current.send("hi", onComplete); });

    // Idle SSE arrives BEFORE sendAsync resolves
    await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());
    act(() => { result.current.notifySessionIdle("sess-1"); });

    // Now let sendAsync resolve (simulating delayed 204)
    act(() => { resolveSendAsync(); });

    // The send must complete promptly — not hang for 60s
    await act(async () => {
      await Promise.race([
        sendPromise,
        new Promise((_, rej) => setTimeout(() => rej(new Error("send did not complete; idle was lost")), 1000)),
      ]);
    });

    expect(onComplete).toHaveBeenCalledWith(expect.objectContaining({ id: "asst-fast" }));
  });

  it("RACE: idle for previous session is dropped when a new session starts mid-flight", async () => {
    // The hook is parameterized by sessionId. If the user navigates from
    // session A to session B while A is still running, idle for A must not
    // resolve B's wait. Caller-level check (capturedSessionId) must hold.
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    const { result, rerender } = renderHook(
      ({ sid }: { sid: string }) => useChatStream("sb-1", sid),
      { initialProps: { sid: "sess-A" } },
    );

    let sendPromise!: Promise<void>;
    act(() => { sendPromise = result.current.send("hi", vi.fn()); });
    await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());

    // Switch session while A is still pending
    rerender({ sid: "sess-B" });

    // Idle for A — should resolve A's wait (capturedSessionId = "sess-A")
    act(() => { result.current.notifySessionIdle("sess-A"); });
    await act(async () => { await sendPromise; });

    expect(messagesApi.getHistory).toHaveBeenCalledWith("sb-1", "sess-A");
  });

  it("RACE: setTimeout resolver wins when notifySessionIdle is never called — does not double-resolve", async () => {
    // After my reorder, setTimeout is registered before idleResolverRef is
    // set. Verify that timeout firing first (no idle ever) cleanly resolves
    // exactly once and does not cause a state corruption when a late idle
    // signal arrives afterward.
    vi.useFakeTimers();
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));
    const onComplete = vi.fn();

    let sendPromise!: Promise<void>;
    act(() => { sendPromise = result.current.send("hi", onComplete); });
    await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());

    // Trigger timeout
    await act(async () => { vi.advanceTimersByTime(61_000); });
    await act(async () => { await sendPromise; });

    expect(onComplete).toHaveBeenCalledTimes(1);
    expect(messagesApi.getHistory).toHaveBeenCalledTimes(1);

    // Late idle after resolution must be a no-op (resolver was nulled)
    expect(() => {
      act(() => { result.current.notifySessionIdle("sess-1"); });
    }).not.toThrow();
    expect(onComplete).toHaveBeenCalledTimes(1);

    vi.useRealTimers();
  });

  it("RACE: pre-armed idle resolver is cleared when sendAsync rejects (no leak across sends)", async () => {
    // The fix for R1 installs the resolver BEFORE awaiting sendAsync. If
    // sendAsync rejects, the catch path runs but finally must still clear
    // the stranded resolver. Otherwise a late idle SSE could be captured
    // and silently affect the NEXT send's state.
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockRejectedValueOnce(new Error("boom"));

    const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));

    await act(async () => { await result.current.send("hi", vi.fn()); });

    // After a rejected send, calling notifySessionIdle must be a no-op
    // (no throw, no side effect). This proves the resolver was cleared.
    expect(() => {
      act(() => { result.current.notifySessionIdle("sess-1"); });
    }).not.toThrow();

    // Subsequent successful send should work normally.
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    const onComplete = vi.fn();
    await sendAndIdle(result, "second", onComplete);
    expect(onComplete).toHaveBeenCalledTimes(1);
  });

  it("RACE: idle resolver is cleared in finally so a stale idle from prior send does not resolve next send", async () => {
    // Send #1 completes via idle. Send #2 starts. A late, duplicate idle
    // for the same sessionId must NOT prematurely resolve send #2 (because
    // the resolver from send #1 was cleared in finally).
    //
    // This guards against the scenario where idle SSE arrives twice for the
    // same prompt completion (opencode could emit duplicates) — the second
    // copy must not affect a freshly-started send.
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));

    // Send #1 — completes via idle
    await sendAndIdle(result, "first");
    expect(messagesApi.getHistory).toHaveBeenCalledTimes(1);

    // Send #2 starts
    let send2!: Promise<void>;
    act(() => { send2 = result.current.send("second", vi.fn()); });
    await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalledTimes(2));

    // Send #2's resolver is now installed. The idle for "first" already
    // happened and was consumed. Sending another idle now resolves send #2.
    // (Test the symmetric case: stale-resolver bug would manifest as send #2
    // completing before sendAsync was even called — that's covered by
    // test "waits for notifySessionIdle before calling getHistory".)
    act(() => { result.current.notifySessionIdle("sess-1"); });
    await act(async () => { await send2; });

    expect(messagesApi.getHistory).toHaveBeenCalledTimes(2);
  });

  it("falls back to getHistory after timeout when notifySessionIdle never fires", async () => {
    vi.useFakeTimers();

    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([
      { id: "asst-timeout", role: "assistant", parts: [{ type: "text", text: "timeout response" }] },
    ]);

    const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));
    const onComplete = vi.fn();

    let sendPromise!: Promise<void>;
    act(() => { sendPromise = result.current.send("hi", onComplete); });
    await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());

    // Do NOT call notifySessionIdle — simulate SSE connection never delivers idle
    // Advance past the timeout
    await act(async () => { vi.advanceTimersByTime(61_000); });
    await act(async () => { await sendPromise; });

    expect(messagesApi.getHistory).toHaveBeenCalled();
    expect(onComplete).toHaveBeenCalledWith(expect.objectContaining({
      id: "asst-timeout",
      role: "assistant",
    }));

    vi.useRealTimers();
  });

  // ─────────────────────────────────────────────────────────────────────
  // streamTimedOut flag
  // ─────────────────────────────────────────────────────────────────────

  it("streamTimedOut starts as false", () => {
    const { result } = renderHook(() => useChatStream("sb-1", "sess-1"));
    expect(result.current.streamTimedOut).toBe(false);
  });

  it("sets streamTimedOut=true when timeout fires and server is not busy", async () => {
    vi.useFakeTimers();
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    // serverBusy=false: server is not actively busy — interrupted connection
    const { result } = renderHook(() => useChatStream("sb-1", "sess-1", false));
    let sendPromise!: Promise<void>;
    act(() => { sendPromise = result.current.send("hi", vi.fn()); });
    await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());

    await act(async () => { vi.advanceTimersByTime(61_000); });
    await act(async () => { await sendPromise; });

    expect(result.current.streamTimedOut).toBe(true);

    vi.useRealTimers();
  });

  it("does NOT set streamTimedOut when server is still busy at timeout (slow response)", async () => {
    vi.useFakeTimers();
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    // serverBusy=true: agent is legitimately still running — not an interrupted connection
    const { result } = renderHook(() => useChatStream("sb-1", "sess-1", true));
    let sendPromise!: Promise<void>;
    act(() => { sendPromise = result.current.send("hi", vi.fn()); });
    await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());

    await act(async () => { vi.advanceTimersByTime(61_000); });
    await act(async () => { await sendPromise; });

    expect(result.current.streamTimedOut).toBe(false);

    vi.useRealTimers();
  });

  it("does NOT set streamTimedOut when idle SSE arrives before timeout", async () => {
    vi.useFakeTimers();
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    const { result } = renderHook(() => useChatStream("sb-1", "sess-1", false));
    let sendPromise!: Promise<void>;
    act(() => { sendPromise = result.current.send("hi", vi.fn()); });
    await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());

    // Idle arrives before timeout
    act(() => { result.current.notifySessionIdle("sess-1"); });
    await act(async () => { await sendPromise; });

    expect(result.current.streamTimedOut).toBe(false);

    vi.useRealTimers();
  });

  it("clearStreamTimedOut resets streamTimedOut to false", async () => {
    vi.useFakeTimers();
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    const { result } = renderHook(() => useChatStream("sb-1", "sess-1", false));
    let sendPromise!: Promise<void>;
    act(() => { sendPromise = result.current.send("hi", vi.fn()); });
    await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());
    await act(async () => { vi.advanceTimersByTime(61_000); });
    await act(async () => { await sendPromise; });

    expect(result.current.streamTimedOut).toBe(true);
    act(() => { result.current.clearStreamTimedOut(); });
    expect(result.current.streamTimedOut).toBe(false);

    vi.useRealTimers();
  });

  it("streamTimedOut is reset to false on session change", async () => {
    vi.useFakeTimers();
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);

    const { result, rerender } = renderHook(
      ({ sid }: { sid: string }) => useChatStream("sb-1", sid, false),
      { initialProps: { sid: "sess-1" } },
    );

    let sendPromise!: Promise<void>;
    act(() => { sendPromise = result.current.send("hi", vi.fn()); });
    await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());
    await act(async () => { vi.advanceTimersByTime(61_000); });
    await act(async () => { await sendPromise; });
    expect(result.current.streamTimedOut).toBe(true);

    // Navigate to a different session
    rerender({ sid: "sess-2" });
    expect(result.current.streamTimedOut).toBe(false);

    vi.useRealTimers();
  });

  // ── Timeout recheck (session-status fetch before declaring interrupted) ──
  // Production incident 2026-08-26 21:41 (workspace fe8348c8): the first
  // message raced pod startup; the SSE subscription flapped ("no pod IP",
  // 16s backoff), serverBusy never went true locally, the 60s idle-wait
  // timed out, and the user saw "Response interrupted" while the agent
  // was mid-clone and healthy. serverBusy is only as live as the SSE
  // subscription — exactly the moment first messages are sent. Before
  // declaring interruption, recheck the session status over plain HTTP.

  it("does NOT set streamTimedOut when the post-timeout status recheck says busy", async () => {
    vi.useFakeTimers();
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    // serverBusy prop is STALE-false (SSE down); the direct fetch sees busy.
    (workspacesApi.getSession as ReturnType<typeof vi.fn>).mockResolvedValue({ status: "busy" });

    const { result } = renderHook(() => useChatStream("sb-1", "sess-1", false));
    let sendPromise!: Promise<void>;
    act(() => { sendPromise = result.current.send("hi", vi.fn()); });
    await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());

    await act(async () => { vi.advanceTimersByTime(61_000); });
    await act(async () => { await sendPromise; });

    expect(workspacesApi.getSession).toHaveBeenCalledWith("sb-1", "sess-1", expect.anything());
    expect(result.current.streamTimedOut).toBe(false);

    vi.useRealTimers();
  });

  it("sets streamTimedOut=true when the recheck confirms idle (real interruption)", async () => {
    vi.useFakeTimers();
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    (workspacesApi.getSession as ReturnType<typeof vi.fn>).mockResolvedValue({ status: "idle" });

    const { result } = renderHook(() => useChatStream("sb-1", "sess-1", false));
    let sendPromise!: Promise<void>;
    act(() => { sendPromise = result.current.send("hi", vi.fn()); });
    await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());

    await act(async () => { vi.advanceTimersByTime(61_000); });
    await act(async () => { await sendPromise; });

    expect(result.current.streamTimedOut).toBe(true);

    vi.useRealTimers();
  });

  it("fails open (sets streamTimedOut) when the recheck itself errors", async () => {
    vi.useFakeTimers();
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    (workspacesApi.getSession as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("pod gone"));

    const { result } = renderHook(() => useChatStream("sb-1", "sess-1", false));
    let sendPromise!: Promise<void>;
    act(() => { sendPromise = result.current.send("hi", vi.fn()); });
    await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());

    await act(async () => { vi.advanceTimersByTime(61_000); });
    await act(async () => { await sendPromise; });

    // Recheck unreachable → old behavior (banner) — better to warn on a
    // live agent than to hang silently on a dead one.
    expect(result.current.streamTimedOut).toBe(true);

    vi.useRealTimers();
  });

  // Review #1051: the recheck fetch must be BOUNDED — an unbounded fetch
  // during a partition would hold the banner hostage. The AbortController
  // aborts at RECHECK_TIMEOUT_MS (10s); the abort surfaces as a fetch
  // rejection → fail-open banner.
  it("fails open when the recheck hangs past its timeout", async () => {
    vi.useFakeTimers();
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    (workspacesApi.getSession as ReturnType<typeof vi.fn>).mockImplementation(
      (_ws: string, _sid: string, opts?: { signal?: AbortSignal }) =>
        new Promise((_resolve, reject) => {
          opts?.signal?.addEventListener("abort", () => reject(new Error("aborted")));
          // never resolves on its own — only the abort can end it
        }),
    );

    const { result } = renderHook(() => useChatStream("sb-1", "sess-1", false));
    let sendPromise!: Promise<void>;
    act(() => { sendPromise = result.current.send("hi", vi.fn()); });
    await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());

    await act(async () => { vi.advanceTimersByTime(61_000); });
    await act(async () => { vi.advanceTimersByTime(11_000); });
    await act(async () => { await sendPromise; });

    expect(result.current.streamTimedOut).toBe(true);

    vi.useRealTimers();
  });

  // Review #1051: session-switch during the async recheck must not paint
  // a stale-session banner. The recheck resolves busy=false for sess-1
  // AFTER the user navigated to sess-2 — the banner must stay clear (the
  // stale run's state must not leak into the new session's UI).
  it("does NOT set streamTimedOut when the user switches sessions during the recheck", async () => {
    vi.useFakeTimers();
    (messagesApi.sendAsync as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (messagesApi.getHistory as ReturnType<typeof vi.fn>).mockResolvedValue([]);
    let releaseRecheck!: (v: { status: string }) => void;
    (workspacesApi.getSession as ReturnType<typeof vi.fn>).mockImplementation(
      () => new Promise<{ status: string }>((resolve) => { releaseRecheck = resolve; }),
    );

    const { result, rerender } = renderHook(
      ({ sid }: { sid: string }) => useChatStream("sb-1", sid, false),
      { initialProps: { sid: "sess-1" } },
    );
    let sendPromise!: Promise<void>;
    act(() => { sendPromise = result.current.send("hi", vi.fn()); });
    await vi.waitFor(() => expect(messagesApi.sendAsync).toHaveBeenCalled());

    await act(async () => { vi.advanceTimersByTime(61_000); });

    // User navigates to sess-2 while the recheck is still in flight...
    rerender({ sid: "sess-2" });
    // ...then the recheck resolves idle (would have bannered sess-1).
    await act(async () => { releaseRecheck({ status: "idle" }); });
    await act(async () => { await sendPromise; });

    expect(result.current.streamTimedOut).toBe(false);

    vi.useRealTimers();
  });
});
