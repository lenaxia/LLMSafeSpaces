import { useState, useCallback, useRef, useEffect } from "react";
import { messagesApi } from "../api/messages";

export type QueuedMessage = {
  id: string;
  text: string;
  /** Display state. Delivering/verifying are server-side durability
   * plumbing (POST in flight / ambiguous outcome being resolved) and are
   * deliberately NOT displayed: an entry delivering for a whole
   * multi-minute turn must not render as "queued" — once the agent owns
   * the message it is in the conversation, not the queue (TUI parity).
   * Failures resurface here as error pills. */
  status: "pending" | "error";
  error?: string;
  sessionId: string;
  /** Epic 68: upload paths attached to the queued entry (local re-enqueue only). */
  files?: string[];
};

const RESTART_PHASES = ["Creating", "Pending", "Suspending"];

export function useMessageQueue(
  workspaceId: string | undefined,
  sessionId: string | undefined,
) {
  const [queuedMessages, setQueuedMessages] = useState<QueuedMessage[]>([]);
  const refreshInFlightRef = useRef(false);

  const refreshQueue = useCallback(async () => {
    if (!workspaceId || !sessionId) return;
    if (refreshInFlightRef.current) return;
    refreshInFlightRef.current = true;
    try {
      const res = await messagesApi.getQueue(workspaceId, sessionId);
      setQueuedMessages((prev) => {
        // Only pending and error entries are displayed. A server-side
        // delivering/verifying entry must neither add a pill nor RETAIN
        // a local pending pill for the same id (the mid-turn staleness
        // bug: the GET races the turn, and an entry staged for delivery
        // is no longer "queued" from the user's perspective).
        // Only the known in-flight states are hidden; anything else —
        // missing, "pending", "error", or a future unknown status —
        // displays (degraded to pending), so a server newer than this
        // client never silently vanishes entries.
        const displayed = res.messages.filter((m) => {
          const st = m.status ?? "";
          return st !== "delivering" && st !== "verifying";
        });
        const redisIds = new Set(displayed.map((m) => m.id));
        const kept = prev.filter((m) =>
          m.status === "error" ||
          redisIds.has(m.id) ||
          m.sessionId !== sessionId,
        );
        const existingIds = new Set(kept.map((m) => m.id));
        const added: QueuedMessage[] = displayed
          .filter((m) => !existingIds.has(m.id))
          .map((m) => ({
            id: m.id,
            text: m.text,
            // Unknown/missing statuses degrade to pending (a server that
            // predates the status field still reports plain queued
            // entries — they must stay visible).
            status: (m.status === "error" ? "error" : "pending") as QueuedMessage["status"],
            error: m.lastError,
            sessionId: m.session_id,
          }));
        return [...kept, ...added];
      });
    } catch {
      // Best-effort queue refresh; stale UI recovers on next poll.
    } finally {
      refreshInFlightRef.current = false;
    }
  }, [workspaceId, sessionId]);

  useEffect(() => {
    refreshQueue();
  }, [refreshQueue]);

  const enqueue = useCallback(async (text: string, files?: string[]) => {
    if (!workspaceId || !sessionId) return;
    try {
      const res = await messagesApi.queueMessage(workspaceId, sessionId, text, files);
      setQueuedMessages((prev) => [
        ...prev,
        { id: res.messageID, text, status: "pending", sessionId, files },
      ]);
    } catch {
      setQueuedMessages((prev) => [
        ...prev,
        { id: "err_" + Date.now(), text, status: "error", sessionId, error: "Failed to queue", files },
      ]);
    }
  }, [workspaceId, sessionId]);

  const markError = useCallback((id: string, error: string) => {
    setQueuedMessages((prev) =>
      prev.map((m) => (m.id === id ? { ...m, status: "error", error } : m)),
    );
  }, []);

  const removeById = useCallback((id: string) => {
    setQueuedMessages((prev) => prev.filter((m) => m.id !== id));
  }, []);

  // Echo-based pill clear (TUI parity): when the user's own message
  // lands in the stream, the matching pending pill is no longer
  // "queued" — the agent has admitted it. FIFO: with duplicate texts
  // queued as separate entries, the first pending match goes (each echo
  // consumes exactly one).
  const removeFirstByText = useCallback((text: string) => {
    setQueuedMessages((prev) => {
      const idx = prev.findIndex(
        (m) => m.sessionId === sessionId && m.status === "pending" && m.text === text,
      );
      if (idx < 0) return prev;
      return prev.filter((_, i) => i !== idx);
    });
  }, [sessionId]);

  const retry = useCallback(async (id: string) => {
    if (!workspaceId || !sessionId) return;
    // Server-side retry first (D3 #907): re-arms the SAME entry (attempts
    // reset, dedupe identity kept, ordering preserved). Local re-enqueue
    // is the fallback for client-only entries (id not known server-side,
    // e.g. a failed local enqueue).
    if (!id.startsWith("err_")) {
      try {
        await messagesApi.retryQueueMessage(workspaceId, sessionId, id);
        void refreshQueue();
        return;
      } catch {
        // Server retry unavailable (404 already-delivered, network) —
        // fall through to local re-enqueue.
      }
    }
    const msg = queuedMessages.find((m) => m.id === id);
    removeById(id);
    if (msg) await enqueue(msg.text, msg.files);
  }, [workspaceId, sessionId, queuedMessages, enqueue, removeById, refreshQueue]);

  const dismiss = useCallback(async (id: string) => {
    if (!workspaceId || !sessionId) return;
    removeById(id);
    try {
      await messagesApi.deleteQueueMessage(workspaceId, sessionId, id);
    } catch {
      // Local removal already happened; server-side cleanup is best-effort.
    }
    void refreshQueue();
  }, [workspaceId, sessionId, refreshQueue, removeById]);

  const clearAll = useCallback(async () => {
    if (!workspaceId || !sessionId) return;
    const toDelete = queuedMessages.filter((m) => m.sessionId === sessionId && m.status === "pending");
    setQueuedMessages((prev) => prev.filter((m) => m.sessionId !== sessionId));
    await Promise.allSettled(
      toDelete.map((m) => messagesApi.deleteQueueMessage(workspaceId, sessionId, m.id)),
    );
  }, [workspaceId, sessionId, queuedMessages]);

  const onPhaseChange = useCallback((phase: string) => {
    if (RESTART_PHASES.includes(phase)) {
      setQueuedMessages([]);
    }
  }, []);

  const sessionQueue = sessionId
    ? queuedMessages.filter((m) => m.sessionId === sessionId)
    : [];

  return {
    queuedMessages: sessionQueue,
    enqueue,
    refreshQueue,
    markError,
    removeById,
    removeFirstByText,
    retry,
    dismiss,
    clearAll,
    onPhaseChange,
  };
}
