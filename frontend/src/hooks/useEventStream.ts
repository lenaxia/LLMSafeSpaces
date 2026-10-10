import { useEffect, useRef } from "react";
import { getEnv } from "../env";
import { createSSEConnection } from "../lib/sseConnection";
import { wsLog } from "../lib/wsLog";

const MIN_RECONNECT_MS = 2_000;
const MAX_RECONNECT_MS = 30_000;
const READ_TIMEOUT_MS = 35_000; // Must exceed backend heartbeat interval (25s)
// #1646: semantic liveness window — parity with the user stream's #1365
// watchdog. Both server endpoints heartbeat every 25s (heartbeatLoop →
// ":\n\n" comment frames), so silence past two intervals + margin means
// the stream is dead even when a proxy keeps feeding the connection
// bytes (read timeout never fires). N = 60s, not less, so a healthy
// idle stream that merely jittered its heartbeat cadence cannot flap;
// not more, because every second past it is a second of frozen UI.
const LIVENESS_SILENCE_MS = 60_000;
const LIVENESS_CHECK_MS = 10_000;

export function useEventStream(
  workspaceId: string | undefined,
  onEvent: (data: unknown) => void,
  options?: { onReconnect?: () => void },
) {
  const onEventRef = useRef(onEvent);
  onEventRef.current = onEvent;
  const onReconnectRef = useRef(options?.onReconnect);
  onReconnectRef.current = options?.onReconnect;

  useEffect(() => {
    if (!workspaceId) return;

    let hasConnectedOnce = false;
    const { apiBaseUrl } = getEnv();
    const url = `${apiBaseUrl}/workspaces/${workspaceId}/session-events`;

    // #1646: liveness bookkeeping — data events and heartbeat comment
    // frames both refresh lastAlive; silence beyond LIVENESS_SILENCE_MS
    // forces a reconnect (see useUserEventStream's #1365 watchdog for
    // the user-scoped twin).
    const lastAlive = { at: Date.now() };
    const touchAlive = () => {
      lastAlive.at = Date.now();
    };

    const conn = createSSEConnection({
      url,
      onEvent: (data) => {
        touchAlive();
        onEventRef.current(data);
      },
      onKeepalive: touchAlive,
      onConnect: () => {
        touchAlive();
        if (hasConnectedOnce) {
          onReconnectRef.current?.();
        }
        hasConnectedOnce = true;
      },
      logPrefix: "sse",
      logId: workspaceId,
      readTimeoutMs: READ_TIMEOUT_MS,
      minReconnectMs: MIN_RECONNECT_MS,
      maxReconnectMs: MAX_RECONNECT_MS,
    });

    const checkLiveness = () => {
      const silentFor = Date.now() - lastAlive.at;
      if (silentFor > LIVENESS_SILENCE_MS) {
        wsLog("sse.liveness_reconnect", workspaceId, `silent for ${silentFor}ms`);
        conn.reconnect();
        touchAlive();
      }
    };
    const watchdog = setInterval(checkLiveness, LIVENESS_CHECK_MS);
    document.addEventListener("visibilitychange", checkLiveness);

    return () => {
      clearInterval(watchdog);
      document.removeEventListener("visibilitychange", checkLiveness);
      conn.destroy();
    };
  }, [workspaceId]);
}
