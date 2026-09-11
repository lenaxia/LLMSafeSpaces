import { useState, useCallback, useRef, useEffect } from "react";
import { messagesApi } from "../api/messages";
import { ApiClientError } from "../api/client";

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
  /** D3/#907 + #1320: one clientMessageID per composed message, stable
   * across local re-enqueues — the backend's outbox dedupes on it, so a
   * re-enqueue whose original entry actually delivered collapses instead
   * of minting a duplicate turn. Captured from the server queue where
   * present (getQueue returns it), minted at enqueue otherwise. */
  clientMessageID?: string;
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
            // The server's dedupe identity for this entry — a later
            // local re-enqueue reuses it (#1320).
            clientMessageID: m.clientMessageID,
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

  const enqueue = useCallback(async (text: string, files?: string[], clientMessageID?: string) => {
    if (!workspaceId || !sessionId) return;
    // D3/#907 + #1320: the cmid is the composed message's dedupe
    // identity — one per user intent, minted here if absent and REUSED
    // by every local re-enqueue of the same pill.
    const cmid = clientMessageID ?? crypto.randomUUID();
    try {
      const res = await messagesApi.queueMessage(workspaceId, sessionId, text, files, cmid);
      setQueuedMessages((prev) => [
        ...prev,
        { id: res.messageID, text, status: "pending", sessionId, files, clientMessageID: cmid },
      ]);
    } catch {
      setQueuedMessages((prev) => [
        ...prev,
        { id: "err_" + Date.now(), text, status: "error", sessionId, error: "Failed to queue", files, clientMessageID: cmid },
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
      } catch (err) {
        // #1318 + #1320: contended delivery is NOT "retry unavailable".
        // A 503 means the session is busy delivering — the entry is
        // mid-flight server-side and resolves on its own. Re-enqueueing
        // here mints a duplicate entry (new clientMessageID) while the
        // original stays deliverable: the ses_f73747f8 duplicate-send
        // shape, manufactured by the client. Keep the pill + hint.
        if (err instanceof ApiClientError && err.status === 503) {
          markError(id, err.body?.error || "session busy delivering — the entry is mid-flight and resolves on its own");
          return;
        }
        // 404 already-delivered / network — fall through to local
        // re-enqueue.
      }
    }
    const msg = queuedMessages.find((m) => m.id === id);
    removeById(id);
    // Re-enqueue carries the pill's cmid: if the original entry actually
    // delivered (the lost-outcome case this fall-through exists for),
    // the backend dedupes instead of minting a second turn (#1320).
    if (msg) await enqueue(msg.text, msg.files, msg.clientMessageID);
  }, [workspaceId, sessionId, queuedMessages, enqueue, removeById, refreshQueue, markError]);

  const dismiss = useCallback(async (id: string) => {
    if (!workspaceId || !sessionId) return;
    // Delete-first (#1318 + #1320): removing the pill before the server
    // confirms would silently un-dismiss an entry that still delivers.
    try {
      await messagesApi.deleteQueueMessage(workspaceId, sessionId, id);
      removeById(id);
    } catch (err) {
      if (err instanceof ApiClientError && err.status === 503) {
        // The entry is busy delivering — it WILL send; keep the pill
        // with a hint so the user can re-dismiss after the turn.
        markError(id, err.body?.error || "delivery in progress — dismiss applies after the current delivery");
      } else {
        // Network/other: the delete outcome is unknown; remove locally
        // and let refreshQueue reconcile from the server (re-adds the
        // entry if the delete never landed).
        removeById(id);
      }
    }
    void refreshQueue();
  }, [workspaceId, sessionId, refreshQueue, removeById, markError]);

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
