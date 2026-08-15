import { useCallback, useEffect, useRef, useState } from "react";
import { messagesApi } from "../api/messages";
import { ApiClientError } from "../api/client";
import type { Message } from "../api/types";

// If session.status=idle SSE never arrives (e.g. connection drops), fall
// back to getHistory after this many ms.
//
// #752 F5: The busy-guard at the timeout-fire site (line ~118) prevents
// false "interrupted" banners when the server is still actively processing
// (serverBusyRef.current === true). Combined with v0.15.3's SSE scanner
// fix (which prevents connection drops on large events), the 60s timeout
// is sufficient: the only scenario where a false positive could occur is
// if the SSE connection drops AND serverBusy is stale (false), which
// requires both a network failure and a missed busy event — a narrow race
// that v0.15.3's fix makes extremely unlikely.
const IDLE_WAIT_TIMEOUT_MS = 60_000;

// When the workspace is restarting (opencode down for a credential reload,
// OOM recovery, crash, or relay injection), the proxy returns 503 with a
// retryAfter hint. The restart window is ~5-10s. Bounded auto-retry keeps
// the user's message instead of dropping it silently (issue 440's "hang").
const SEND_MAX_503_RETRIES = 3;

const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

export function useChatStream(workspaceId: string | undefined, sessionId: string | undefined, serverBusy = false) {
  const [localStreaming, setLocalStreaming] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [atCapRetryAfter, setAtCapRetryAfter] = useState<number | null>(null);
  const [streamTimedOut, setStreamTimedOut] = useState(false);
  const idleResolverRef = useRef<((sid: string) => void) | null>(null);
  const currentSessionRef = useRef(sessionId);
  // Keep a live ref to serverBusy so the send closure can read the current
  // value at timeout-fire time without being in the useCallback dep array.
  const serverBusyRef = useRef(serverBusy);
  useEffect(() => { serverBusyRef.current = serverBusy; }, [serverBusy]);

  // Reset UI state when session changes — the in-flight send uses
  // capturedSessionId internally and handles its own scoping, so we don't
  // abort it. We just clear the visual indicators for the new session.
  const prevSessionRef = useRef(sessionId);
  useEffect(() => {
    if (prevSessionRef.current !== sessionId) {
      prevSessionRef.current = sessionId;
      currentSessionRef.current = sessionId;
      setLocalStreaming(false);
      setError(null);
      setAtCapRetryAfter(null);
      setStreamTimedOut(false);
    }
  }, [sessionId]);

  const notifySessionIdle = useCallback((idleSessionId: string) => {
    idleResolverRef.current?.(idleSessionId);
  }, []);

  const send = useCallback(
    async (text: string, onComplete: (msg: Message) => void, model?: { providerID: string; modelID: string }) => {
      if (!workspaceId || !sessionId) return;
      const capturedSessionId = sessionId;
      setLocalStreaming(true);
      setError(null);
      setAtCapRetryAfter(null);

      try {
        let idleAlreadyFired = false;
        idleResolverRef.current = (idleSessionId: string) => {
          if (idleSessionId === capturedSessionId) {
            idleAlreadyFired = true;
          }
        };

        // Retry loop: the proxy returns 503+retryAfter during an in-place
        // opencode restart (credential reload / OOM / crash / relay). The
        // window is transient; drop the user's message only if it persists
        // across the bounded retry count. The loop exits only via `break`
        // (success) or `throw` (non-503 error, or retries exhausted).
        for (let attempt = 0; attempt <= SEND_MAX_503_RETRIES; attempt++) {
          try {
            await messagesApi.sendAsync(workspaceId, sessionId, {
              parts: [{ type: "text", text }],
              ...(model && { model }),
            });
            break;
          } catch (err: unknown) {
            const is503 = err instanceof ApiClientError && err.status === 503;
            if (!is503 || attempt === SEND_MAX_503_RETRIES) throw err;
            const retryAfter = Number(err.body.retryAfter ?? 10);
            await sleep(Math.min(isNaN(retryAfter) ? 10 : retryAfter, 30) * 1000);
          }
        }

        // Wait for session.status=idle SSE OR a timeout fallback.
        // The SSE path is preferred (real-time), but if the connection drops
        // before idle fires we still need to fetch the response.
        // Note: only flag streamTimedOut when the server is NOT still busy —
        // if serverBusyRef.current is true the agent is legitimately still
        // running (slow response), not an interrupted connection.
        let resolvedViaSSE = false;
        await new Promise<void>((resolve) => {
          if (idleAlreadyFired) {
            resolvedViaSSE = true;
            idleResolverRef.current = null;
            resolve();
            return;
          }
          const timeoutId = setTimeout(() => {
            idleResolverRef.current = null;
            resolve();
            // resolvedViaSSE stays false — timeout path
          }, IDLE_WAIT_TIMEOUT_MS);

          idleResolverRef.current = (idleSessionId: string) => {
            if (idleSessionId === capturedSessionId) {
              resolvedViaSSE = true;
              clearTimeout(timeoutId);
              idleResolverRef.current = null;
              resolve();
            }
          };
        });

        // Only show the interrupted banner when:
        // 1. The idle SSE never arrived (resolvedViaSSE=false), AND
        // 2. The server is not still actively busy (would indicate slow response)
        if (!resolvedViaSSE && !serverBusyRef.current && currentSessionRef.current === capturedSessionId) {
          setStreamTimedOut(true);
        }

        const history = await messagesApi.getHistory(workspaceId, capturedSessionId);
        const lastAssistant = [...history].reverse().find((m) => m.role === "assistant");

        const msg: Message = lastAssistant ?? {
          id: `msg-${Date.now()}`,
          role: "assistant",
          parts: [],
        };
        onComplete(msg);
      } catch (err: unknown) {
        if (err instanceof ApiClientError && err.status === 429) {
          const retryAfter = Number(err.body.retryAfter ?? 60);
          setAtCapRetryAfter(isNaN(retryAfter) ? 60 : retryAfter);
        } else if (err instanceof ApiClientError && err.status === 503) {
          // Surface the API's structured message for service-unavailable
          // errors (agent unreachable, restarting, etc.) instead of the
          // raw "workspace connection failed" string.
          const apiMessage = err.body?.message;
          setError(
            typeof apiMessage === "string" && apiMessage.length > 0
              ? apiMessage
              : "The agent is not responding. Please try again in a moment.",
          );
        } else {
          const message = err instanceof Error ? err.message : "Failed to send message";
          setError(message);
        }
      } finally {
        if (currentSessionRef.current === capturedSessionId) {
          setLocalStreaming(false);
        }
        idleResolverRef.current = null;
      }
    },
    [workspaceId, sessionId],
  );

  const clearError = useCallback(() => setError(null), []);
  const clearAtCap = useCallback(() => setAtCapRetryAfter(null), []);
  const clearStreamTimedOut = useCallback(() => setStreamTimedOut(false), []);

  // effectiveStreaming: local send takes priority; server state supplements
  const effectiveStreaming = localStreaming || serverBusy;

  return {
    send,
    streaming: effectiveStreaming,
    localStreaming,
    notifySessionIdle,
    error,
    clearError,
    atCapRetryAfter,
    clearAtCap,
    streamTimedOut,
    clearStreamTimedOut,
  };
}
