import { describe, expect, it, vi, beforeEach } from "vitest";
import { renderHook, act, waitFor } from "@testing-library/react";

vi.mock("../api/messages", () => ({
  messagesApi: {
    queueMessage: vi.fn(),
    getQueue: vi.fn().mockResolvedValue({ messages: [] }),
    deleteQueueMessage: vi.fn().mockResolvedValue(undefined),
    retryQueueMessage: vi.fn(),
    getHistory: vi.fn().mockResolvedValue([]),
    sendAsync: vi.fn(),
  },
}));

import { messagesApi } from "../api/messages";
import { ApiClientError } from "../api/client";
import { useMessageQueue } from "./useMessageQueue";

function err503() {
  return new ApiClientError(503, { error: "session busy delivering; retry shortly" });
}

function render(workspaceId = "ws-1", sessionId = "ses-1") {
  return renderHook(() => useMessageQueue(workspaceId, sessionId));
}

describe("useMessageQueue (refresh-based reconciliation)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValue({ messageID: "msg_test_1" });
    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({ messages: [] });
    (messagesApi.deleteQueueMessage as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
  });

  it("refreshes queue from backend on mount", async () => {
    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({
      messages: [
        { id: "msg_existing", text: "persisted", session_id: "ses-1", workspace_id: "ws-1", enqueued_at: "", retry_count: 0 },
      ],
    });

    const { result } = render();

    await waitFor(() => {
      expect(result.current.queuedMessages).toHaveLength(1);
    });
    expect(result.current.queuedMessages[0]!.text).toBe("persisted");
    expect(result.current.queuedMessages[0]!.status).toBe("pending");
  });

  it("enqueue calls backend and adds optimistic pill", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    await act(async () => { await result.current.enqueue("hello"); });

    expect(messagesApi.queueMessage).toHaveBeenCalledWith("ws-1", "ses-1", "hello", undefined, expect.any(String));
    expect(result.current.queuedMessages).toHaveLength(1);
    expect(result.current.queuedMessages[0]!.text).toBe("hello");
    expect(result.current.queuedMessages[0]!.status).toBe("pending");
  });

  it("enqueue on failure shows error pill", async () => {
    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("network"));
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    await act(async () => { await result.current.enqueue("will fail"); });

    expect(result.current.queuedMessages).toHaveLength(1);
    expect(result.current.queuedMessages[0]!.status).toBe("error");
  });

  it("refreshQueue removes sent messages by syncing with Redis", async () => {
    (messagesApi.getQueue as ReturnType<typeof vi.fn>)
      .mockResolvedValueOnce({ messages: [] })
      .mockResolvedValueOnce({ messages: [] })
      .mockResolvedValueOnce({ messages: [] });

    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    await act(async () => { await result.current.enqueue("hello"); });
    expect(result.current.queuedMessages).toHaveLength(1);

    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({ messages: [] });
    await act(async () => { await result.current.refreshQueue(); });

    expect(result.current.queuedMessages).toHaveLength(0);
  });

  it("refreshQueue keeps error pills even when not in Redis", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    await act(async () => { await result.current.enqueue("hello"); });
    expect(result.current.queuedMessages).toHaveLength(1);

    act(() => { result.current.markError("msg_test_1", "failed"); });
    expect(result.current.queuedMessages[0]!.status).toBe("error");

    await act(async () => { await result.current.refreshQueue(); });

    expect(result.current.queuedMessages).toHaveLength(1);
    expect(result.current.queuedMessages[0]!.status).toBe("error");
  });

  it("markError sets error status on a message", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    await act(async () => { await result.current.enqueue("hello"); });

    act(() => { result.current.markError("msg_test_1", "send failed"); });

    expect(result.current.queuedMessages[0]!.status).toBe("error");
    expect(result.current.queuedMessages[0]!.error).toBe("send failed");
  });

  it("dismiss removes pill locally and calls DELETE API", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    await act(async () => { await result.current.enqueue("hello"); });
    expect(result.current.queuedMessages).toHaveLength(1);

    await act(async () => { await result.current.dismiss("msg_test_1"); });

    expect(messagesApi.deleteQueueMessage).toHaveBeenCalledWith("ws-1", "ses-1", "msg_test_1");
    expect(result.current.queuedMessages).toHaveLength(0);
  });

  // #1318 + #1320: contended delivery is not "unavailable". The 503 from
  // Retry/Dismiss means the entry is mid-delivery server-side — a local
  // re-enqueue mints a duplicate entry (new clientMessageID) while the
  // original stays deliverable: the ses_f73747f8 duplicate-send shape,
  // manufactured by the client.
  it("retry on 503 keeps the pill and never re-enqueues", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValue({ messageID: "msg_1" });
    await act(async () => { await result.current.enqueue("contended"); });
    act(() => { result.current.markError("msg_1", "failed"); });

    (messagesApi.retryQueueMessage as ReturnType<typeof vi.fn>).mockRejectedValue(err503());
    await act(async () => { await result.current.retry("msg_1"); });

    expect(messagesApi.queueMessage).toHaveBeenCalledTimes(1);
    expect(result.current.queuedMessages).toHaveLength(1);
    expect(result.current.queuedMessages[0]!.id).toBe("msg_1");
    expect(result.current.queuedMessages[0]!.status).toBe("error");
    expect(result.current.queuedMessages[0]!.error).toContain("busy delivering");
  });

  it("dismiss on 503 keeps the pill (the entry will still deliver)", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    await act(async () => { await result.current.enqueue("contended"); });
    expect(result.current.queuedMessages).toHaveLength(1);

    (messagesApi.deleteQueueMessage as ReturnType<typeof vi.fn>).mockRejectedValue(err503());
    await act(async () => { await result.current.dismiss("msg_test_1"); });

    expect(messagesApi.deleteQueueMessage).toHaveBeenCalledWith("ws-1", "ses-1", "msg_test_1");
    expect(result.current.queuedMessages).toHaveLength(1);
    expect(result.current.queuedMessages[0]!.error).toContain("busy delivering");
  });

  // The pre-emptive-removal bug's most common trigger: dismissing a
  // PENDING pill while its delivery is contended.
  it("dismiss on 503 of a pending pill keeps it (markError surfaces the hint)", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    await act(async () => { await result.current.enqueue("pending-contended"); });

    (messagesApi.deleteQueueMessage as ReturnType<typeof vi.fn>).mockRejectedValue(err503());
    await act(async () => { await result.current.dismiss("msg_test_1"); });

    expect(result.current.queuedMessages).toHaveLength(1);
    expect(result.current.queuedMessages[0]!.status).toBe("error");
    expect(result.current.queuedMessages[0]!.error).toContain("busy delivering");
  });

  // #1320 r1 missing-case 3: the reconcile safety net the network-error
  // comment relies on — a delete whose outcome is unknown AND a server
  // still holding the entry must RE-ADD the pill (a broken re-add merge
  // would silently drop messages with the suite green).
  it("dismiss on network error keeps the pill through a refresh whose entry is delivering (display contract)", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    await act(async () => { await result.current.enqueue("maybe-gone"); });
    expect(result.current.queuedMessages).toHaveLength(1);

    (messagesApi.deleteQueueMessage as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("network"));
    // r4 missing-case 2: the delete's outcome is unknown AND the entry
    // is mid-delivery — the one case the old reconcile claim was blind
    // to (refresh excludes delivering from the re-add set). The pill
    // must survive the refresh (error-status is what carries it).
    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({
      messages: [
        { id: "msg_test_1", text: "maybe-gone", session_id: "ses-1", workspace_id: "ws-1", enqueued_at: "", retry_count: 0, status: "delivering" },
      ],
    });
    await act(async () => { await result.current.dismiss("msg_test_1"); });

    expect(result.current.queuedMessages).toHaveLength(1);
    expect(result.current.queuedMessages[0]!.id).toBe("msg_test_1");
    expect(result.current.queuedMessages[0]!.status).toBe("error");
  });

  // #1320 r1 finding 1: the local re-enqueue fall-through must carry a
  // dedupe identity — one cmid per composed message, reused across
  // re-enqueues (the backend outbox dedupes on it; a keyless re-enqueue
  // reopens the duplicate-send window on the lost-outcome path).
  it("enqueue mints a clientMessageID and sends it to the backend", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    await act(async () => { await result.current.enqueue("identity"); });

    expect(messagesApi.queueMessage).toHaveBeenCalledWith(
      "ws-1", "ses-1", "identity", undefined,
      expect.any(String),
    );
  });

  it("re-enqueue (retry fall-through) REUSES the pill's clientMessageID", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValue({ messageID: "msg_1" });
    await act(async () => { await result.current.enqueue("retry me"); });
    act(() => { result.current.markError("msg_1", "failed"); });
    const firstCmid = (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mock.calls[0]![4] as string;
    expect(firstCmid).toBeTruthy();

    (messagesApi.retryQueueMessage as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("404"));
    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValue({ messageID: "msg_2" });
    await act(async () => { await result.current.retry("msg_1"); });

    const secondCall = (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mock.calls[1]!;
    expect(secondCall![4]).toBe(firstCmid);
  });

  it("re-enqueue of a server-known pill reuses the cmid captured from getQueue", async () => {
    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({
      messages: [
        { id: "srv_1", text: "from server", session_id: "ses-1", workspace_id: "ws-1", enqueued_at: "", retry_count: 0, status: "error", clientMessageID: "cmid-srv" },
      ],
    });
    const { result } = render();
    await waitFor(() => expect(result.current.queuedMessages).toHaveLength(1));

    (messagesApi.retryQueueMessage as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("404"));
    await act(async () => { await result.current.retry("srv_1"); });

    const call = (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mock.calls[0]!;
    expect(call![4]).toBe("cmid-srv");
  });

  it("dismiss on non-503 error keeps the pill as error-status (unknown outcome; safe beats tidy)", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    await act(async () => { await result.current.enqueue("gone"); });
    expect(result.current.queuedMessages).toHaveLength(1);

    (messagesApi.deleteQueueMessage as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("network"));
    await act(async () => { await result.current.dismiss("msg_test_1"); });

    // r4: the old claim — remove locally, refresh reconciles — is FALSE
    // for delivering/verifying entries (the display contract excludes
    // them from the re-add set): the pill must stay, error-marked so it
    // survives refreshQueue, until the sent-event or a manual dismiss.
    expect(result.current.queuedMessages).toHaveLength(1);
    expect(result.current.queuedMessages[0]!.status).toBe("error");
    expect(result.current.queuedMessages[0]!.error).toContain("outcome unknown");
  });

  it("retry re-enqueues message text", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValue({ messageID: "msg_1" });
    await act(async () => { await result.current.enqueue("retry me"); });

    act(() => { result.current.markError("msg_1", "failed"); });

    // Server-side retry fails (e.g. already delivered / network) so the
    // local re-enqueue fallback runs.
    (messagesApi.retryQueueMessage as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("404"));
    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValue({ messageID: "msg_2" });
    await act(async () => { await result.current.retry("msg_1"); });

    expect(messagesApi.queueMessage).toHaveBeenCalledTimes(2);
    expect(result.current.queuedMessages[0]!.id).toBe("msg_2");
    expect(result.current.queuedMessages[0]!.status).toBe("pending");
  });

  it("onPhaseChange clears on restart phases", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    await act(async () => { await result.current.enqueue("hello"); });
    expect(result.current.queuedMessages).toHaveLength(1);

    act(() => { result.current.onPhaseChange("Suspending"); });
    expect(result.current.queuedMessages).toHaveLength(0);
  });

  it("clearAll removes pills and calls DELETE for each pending message", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValueOnce({ messageID: "msg_a" });
    await act(async () => { await result.current.enqueue("a"); });
    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValueOnce({ messageID: "msg_b" });
    await act(async () => { await result.current.enqueue("b"); });
    expect(result.current.queuedMessages).toHaveLength(2);

    await act(async () => { await result.current.clearAll(); });

    expect(result.current.queuedMessages).toHaveLength(0);
    expect(messagesApi.deleteQueueMessage).toHaveBeenCalledWith("ws-1", "ses-1", "msg_a");
    expect(messagesApi.deleteQueueMessage).toHaveBeenCalledWith("ws-1", "ses-1", "msg_b");
  });

  // r2 finding: the Abort button's queue sweep had the pre-#1318 swallow
  // shape — a contended 503 entry was wiped locally while it went on to
  // deliver (silent un-dismissal).
  it("clearAll on 503 keeps the contended pill with a hint; confirmed deletes clear", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValueOnce({ messageID: "msg_a" });
    await act(async () => { await result.current.enqueue("busy"); });
    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValueOnce({ messageID: "msg_b" });
    await act(async () => { await result.current.enqueue("free"); });
    expect(result.current.queuedMessages).toHaveLength(2);

    (messagesApi.deleteQueueMessage as ReturnType<typeof vi.fn>)
      .mockRejectedValueOnce(err503())
      .mockResolvedValueOnce(undefined);
    await act(async () => { await result.current.clearAll(); });

    expect(result.current.queuedMessages).toHaveLength(1);
    expect(result.current.queuedMessages[0]!.id).toBe("msg_a");
    expect(result.current.queuedMessages[0]!.status).toBe("error");
    expect(result.current.queuedMessages[0]!.error).toContain("delivery in progress");
  });

  // r4 finding 1: error pills (e.g. a dismiss-503 hinted pill) were
  // skipped by the pending-only sweep and then wiped by the final filter
  // — dismiss under contention, hit Abort, and the hinted entry
  // delivered silently. They now join the sweep with the same
  // per-outcome discipline.
  it("clearAll sweeps error-status pills too: 503 keeps them hinted, 204 clears them", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValueOnce({ messageID: "msg_hint" });
    await act(async () => { await result.current.enqueue("hinted"); });
    act(() => { result.current.markError("msg_hint", "delivery in progress — dismiss applies after the current delivery"); });
    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValueOnce({ messageID: "msg_ok" });
    await act(async () => { await result.current.enqueue("plain"); });
    expect(result.current.queuedMessages).toHaveLength(2);

    (messagesApi.deleteQueueMessage as ReturnType<typeof vi.fn>)
      .mockRejectedValueOnce(err503())
      .mockResolvedValueOnce(undefined);
    await act(async () => { await result.current.clearAll(); });

    expect(result.current.queuedMessages).toHaveLength(1);
    expect(result.current.queuedMessages[0]!.id).toBe("msg_hint");
    expect(result.current.queuedMessages[0]!.error).toContain("delivery in progress");
  });

  it("clearAll on unknown-outcome deletes keeps the pills error-marked (safe beats tidy)", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValueOnce({ messageID: "msg_u" });
    await act(async () => { await result.current.enqueue("unknown"); });

    (messagesApi.deleteQueueMessage as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("network"));
    await act(async () => { await result.current.clearAll(); });

    expect(result.current.queuedMessages).toHaveLength(1);
    expect(result.current.queuedMessages[0]!.status).toBe("error");
    expect(result.current.queuedMessages[0]!.error).toContain("outcome unknown");
  });

  it("removeById removes a message by id regardless of status", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    await act(async () => { await result.current.enqueue("hello"); });
    act(() => { result.current.markError("msg_test_1", "fail"); });

    act(() => { result.current.removeById("msg_test_1"); });
    expect(result.current.queuedMessages).toHaveLength(0);
  });

  it("refreshQueue after enqueue correctly syncs with Redis", async () => {
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValue({ messageID: "msg_test_1" });
    await act(async () => { await result.current.enqueue("hello"); });
    expect(result.current.queuedMessages).toHaveLength(1);

    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({
      messages: [{ id: "msg_test_1", text: "hello", session_id: "ses-1", workspace_id: "ws-1", enqueued_at: "", retry_count: 0 }],
    });
    await act(async () => { await result.current.refreshQueue(); });

    expect(result.current.queuedMessages).toHaveLength(1);
  });

  it("refreshQueue does not clobber messages from other sessions", async () => {
    const { result, rerender } = renderHook(
      (props: { sid: string }) => useMessageQueue("ws-1", props.sid),
      { initialProps: { sid: "ses-A" } },
    );

    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    await act(async () => { await result.current.enqueue("for A"); });
    expect(result.current.queuedMessages).toHaveLength(1);

    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({ messages: [] });
    rerender({ sid: "ses-B" });
    await waitFor(() => expect(result.current.queuedMessages).toHaveLength(0));

    rerender({ sid: "ses-A" });
    await waitFor(() => expect(result.current.queuedMessages).toHaveLength(1));
  });
});

  it("hides server verifying/delivering entries; unknown statuses degrade to pending (#987 display contract)", async () => {
    // In-flight entries are server-side durability plumbing, not queue
    // display state: an entry delivering for a whole multi-minute turn
    // must not render as "queued" (TUI parity — once the agent owns the
    // message it is in the conversation). Failures resurface as error
    // pills when delivery ultimately fails.
    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({
      messages: [
        { id: "msg_v", text: "sent unconfirmed", session_id: "ses-1", workspace_id: "ws-1", enqueued_at: "", retry_count: 0, status: "verifying" },
        { id: "msg_d", text: "in flight", session_id: "ses-1", workspace_id: "ws-1", enqueued_at: "", retry_count: 0, status: "delivering" },
        { id: "msg_u", text: "unknown future status", session_id: "ses-1", workspace_id: "ws-1", enqueued_at: "", retry_count: 0, status: "teleported" },
        { id: "msg_e", text: "parked failure", session_id: "ses-1", workspace_id: "ws-1", enqueued_at: "", retry_count: 0, status: "error", lastError: "agent unreachable" },
      ],
    });

    const { result } = render();

    await waitFor(() => {
      expect(result.current.queuedMessages).toHaveLength(2);
    });
    expect(result.current.queuedMessages.map((m) => m.id)).toEqual(["msg_u", "msg_e"]);
    expect(result.current.queuedMessages[0]!.status).toBe("pending"); // unknown server statuses degrade to pending
    expect(result.current.queuedMessages[1]!.status).toBe("error");
  });

  it("a local pending pill is dropped once its server entry goes delivering", async () => {
    // The mid-turn staleness bug: the worker stages the entry out (POST
    // in flight for the whole turn) yet GET /queue still reports it —
    // the refresh must not RETAIN the local pill for a now-invisible id.
    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({
      messages: [{ id: "msg_d", text: "in flight", session_id: "ses-1", workspace_id: "ws-1", enqueued_at: "", retry_count: 0, status: "pending" }],
    });
    const { result } = render();
    await waitFor(() => expect(result.current.queuedMessages).toHaveLength(1));

    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({
      messages: [{ id: "msg_d", text: "in flight", session_id: "ses-1", workspace_id: "ws-1", enqueued_at: "", retry_count: 0, status: "delivering" }],
    });
    await act(async () => { await result.current.refreshQueue(); });

    expect(result.current.queuedMessages).toHaveLength(0);
  });

describe("useMessageQueue files (Epic 68 U1.6.8)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValue({ messageID: "msg_test_1" });
    (messagesApi.getQueue as ReturnType<typeof vi.fn>).mockResolvedValue({ messages: [] });
    (messagesApi.deleteQueueMessage as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
  });

  it("enqueue carries files[] to the queue endpoint", async () => {
    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValue({ messageID: "msg_f1" });
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    const files = ["/workspace/uploads/11111111-2222-3333-4444-555555555555-q.txt"];
    await act(async () => { await result.current.enqueue("queued with file", files); });

    expect(messagesApi.queueMessage).toHaveBeenCalledWith("ws-1", "ses-1", "queued with file", files, expect.any(String));
    expect(result.current.queuedMessages[0]).toMatchObject({ text: "queued with file", files });
  });

  it("local re-enqueue after a failed server retry preserves files", async () => {
    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValue({ messageID: "msg_r1" });
    const { result } = render();
    await waitFor(() => expect(messagesApi.getQueue).toHaveBeenCalled());

    const files = ["/workspace/uploads/11111111-2222-3333-4444-555555555555-r.txt"];
    await act(async () => { await result.current.enqueue("retry me", files); });
    (messagesApi.retryQueueMessage as ReturnType<typeof vi.fn>).mockRejectedValue(new Error("404"));
    (messagesApi.queueMessage as ReturnType<typeof vi.fn>).mockResolvedValue({ messageID: "msg_r2" });

    await act(async () => { await result.current.retry("msg_r1"); });

    expect(messagesApi.queueMessage).toHaveBeenCalledWith("ws-1", "ses-1", "retry me", files, expect.any(String));
  });
});
