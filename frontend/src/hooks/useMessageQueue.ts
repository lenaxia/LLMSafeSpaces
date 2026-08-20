import { useState, useCallback, useRef, useEffect } from "react";
import { messagesApi } from "../api/messages";

export type QueuedMessage = {
  id: string;
  text: string;
  status: "pending" | "delivering" | "verifying" | "error";
  error?: string;
  sessionId: string;
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
        const redisIds = new Set(res.messages.map((m) => m.id));
        const kept = prev.filter((m) =>
          m.status === "error" ||
          redisIds.has(m.id) ||
          m.sessionId !== sessionId,
        );
        const existingIds = new Set(kept.map((m) => m.id));
        const added: QueuedMessage[] = res.messages
          .filter((m) => !existingIds.has(m.id))
          .map((m) => ({
            id: m.id,
            text: m.text,
            // Server statuses map 1:1 (#987): error = retryable,
            // verifying = sent, outcome being confirmed (never
            // re-sent blindly), delivering = in flight. Unknown
            // statuses degrade to pending.
            status: (["error", "verifying", "delivering"].includes(m.status ?? "")
              ? m.status
              : "pending") as QueuedMessage["status"],
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

  const enqueue = useCallback(async (text: string) => {
    if (!workspaceId || !sessionId) return;
    try {
      const res = await messagesApi.queueMessage(workspaceId, sessionId, text);
      setQueuedMessages((prev) => [
        ...prev,
        { id: res.messageID, text, status: "pending", sessionId },
      ]);
    } catch {
      setQueuedMessages((prev) => [
        ...prev,
        { id: "err_" + Date.now(), text, status: "error", sessionId, error: "Failed to queue" },
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
    if (msg) await enqueue(msg.text);
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
    retry,
    dismiss,
    clearAll,
    onPhaseChange,
  };
}
