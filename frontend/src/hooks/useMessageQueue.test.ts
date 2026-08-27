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
import { useMessageQueue } from "./useMessageQueue";

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

    expect(messagesApi.queueMessage).toHaveBeenCalledWith("ws-1", "ses-1", "hello", undefined);
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

describe("useMessageQueue files (Epic 67 U1.6.8)", () => {
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

    expect(messagesApi.queueMessage).toHaveBeenCalledWith("ws-1", "ses-1", "queued with file", files);
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

    expect(messagesApi.queueMessage).toHaveBeenCalledWith("ws-1", "ses-1", "retry me", files);
  });
});
