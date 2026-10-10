import { useEffect, useRef } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { getEnv } from "../env";
import { createSSEConnection } from "../lib/sseConnection";
import { wsLog } from "../lib/wsLog";

const MIN_RECONNECT_MS = 1000;
const MAX_RECONNECT_MS = 30_000;
const READ_TIMEOUT_MS = 35_000; // Must exceed backend heartbeat interval (25s)
// #1365: semantic liveness window. The server heartbeats every 25s; a stream
// silent past two intervals + margin is dead even when the fetch loop is
// stuck (suspended tab resumed mid-read, half-open proxy) — a state the read
// timeout cannot see. On expiry the watchdog forces a reconnect, which
// re-runs the server-side snapshot flights and unfreezes the pending set.
const LIVENESS_SILENCE_MS = 60_000;
const LIVENESS_CHECK_MS = 10_000;

/**
 * useUserEventStream connects to the user-scoped SSE endpoint (GET /api/v1/events)
 * and invalidates workspace caches when phase events arrive for any workspace.
 *
 * This hook mounts once from the root layout and stays connected for the lifetime
 * of the app. It handles reconnection with exponential backoff and Last-Event-ID
 * replay. A liveness watchdog (#1365) reconnects a stream silent beyond the
 * heartbeat window so pending prompts cannot stay frozen on a dead connection.
 */
export function useUserEventStream(options?: { onEvent?: (event: unknown) => void; onReconnect?: () => void }) {
  const queryClient = useQueryClient();
  const lastEventIDRef = useRef<string | null>(null);
  const onEventRef = useRef(options?.onEvent);
  const onReconnectRef = useRef(options?.onReconnect);

  useEffect(() => {
    onEventRef.current = options?.onEvent;
  });
  useEffect(() => {
    onReconnectRef.current = options?.onReconnect;
  });

  useEffect(() => {
    const { apiBaseUrl } = getEnv();

    function buildHeaders(): Record<string, string> {
      const h: Record<string, string> = {};
      if (lastEventIDRef.current !== null) {
        h["Last-Event-ID"] = lastEventIDRef.current;
      }
      return h;
    }

    let conn: ReturnType<typeof createSSEConnection> | null = null;
    const lastAlive = { at: Date.now() };
    const touchAlive = () => {
      lastAlive.at = Date.now();
    };

    // r1 (#1646): ANY post-first connect is a reconnect. lastEventIDRef
    // is NOT a reconnect discriminator — the server writes snapshot/
    // anti-entropy/resync events with EventID 0 and NO id: line
    // (stream_user_events.go: "Snapshot event — no id: line"), so a
    // quiet stream can run (and reconnect) forever without ever setting
    // it. Gating the reconnect reset on it left a sticky-busy residual:
    // the provider's wipe + seed-gate clear were skipped, so a
    // mount-seeded busy could never clear after an event-less gap.
    // NOTE (r2 review): lastEventIDRef feeds buildHeaders(), which is
    // captured ONCE at start() and reused verbatim by sseConnection on
    // every internal reconnect — so the Last-Event-ID replay header is
    // effectively mount-frozen and does not fire post-mount. Known
    // follow-up (header-factory, see the #1646 thread); every #1646
    // convergence path is replay-independent by design (REST
    // invalidation for busy/list, US-55.3 snapshot anti-entropy for
    // asks, D6 getAlerts seed for hung, phase invalidations).
    let hasConnectedOnce = false;

    function start() {
      conn = createSSEConnection({
        url: `${apiBaseUrl}/events`,
        headers: buildHeaders(),
        onEvent: (data) => {
          touchAlive();
          const evt = data as {
            event_id?: number;
            workspace_id?: string;
            type: string;
            phase?: string;
          };

          // Track last event ID for replay on reconnect
          if (evt.event_id && evt.event_id > 0) {
            lastEventIDRef.current = String(evt.event_id);
          }

          if (evt.type === "workspace.phase" && evt.workspace_id) {
            wsLog("user_stream.phase", evt.workspace_id, `phase=${evt.phase}`);
            queryClient.invalidateQueries({ queryKey: ["workspaces"] });
            queryClient.invalidateQueries({
              queryKey: ["workspace-status", evt.workspace_id],
            });
          } else if (evt.type === "resync") {
            wsLog("user_stream.resync", "");
            queryClient.invalidateQueries({ queryKey: ["workspaces"] });
            queryClient.invalidateQueries({ queryKey: ["workspace-status"] });
          }

          onEventRef.current?.(data);
        },
        onKeepalive: touchAlive,
        onConnect: () => {
          touchAlive();
          // #1646: sessions reseed on every (re)connect — first included.
          // The provider's busy/hung state is event-tracked by design;
          // onReconnect wipes it so events missed during a dead window
          // cannot stick — but the re-seed only runs when a sessions
          // query UPDATES. Without this invalidation a silent stream
          // death (API pod restart: watchdog reconnects, the restarted
          // broker has no replay) leaves busy/session-list stale until
          // a remount — the #1646 freeze. Invalidation refetches ACTIVE
          // (mounted) sessions queries; inactive ones go stale and
          // converge on next mount (the reconnect-cleared seed gate
          // lets seedBusy re-run when their cache updates).
          queryClient.invalidateQueries({ queryKey: ["sessions"] });
          if (hasConnectedOnce) {
            wsLog("user_stream.reconnected", "");
            queryClient.invalidateQueries({ queryKey: ["workspaces"] });
            queryClient.invalidateQueries({ queryKey: ["workspace-status"] });
            onReconnectRef.current?.();
          } else {
            wsLog("user_stream.connected", "");
          }
          hasConnectedOnce = true;
        },
        logPrefix: "user_stream",
        readTimeoutMs: READ_TIMEOUT_MS,
        minReconnectMs: MIN_RECONNECT_MS,
        maxReconnectMs: MAX_RECONNECT_MS,
      });
    }

    start();

    // #1365: liveness watchdog. Heartbeat comment frames and data events both
    // refresh lastAlive; silence beyond LIVENESS_SILENCE_MS forces a
    // reconnect — the server's connect path re-runs the per-workspace
    // snapshot flights, so a frozen pending-input set converges without a
    // manual refresh. The visibility listener covers a resumed suspended tab
    // immediately instead of waiting out the (throttled) interval.
    const checkLiveness = () => {
      if (!conn) return;
      const silentFor = Date.now() - lastAlive.at;
      if (silentFor > LIVENESS_SILENCE_MS) {
        wsLog("user_stream.liveness_reconnect", "", `silent for ${silentFor}ms`);
        conn.reconnect();
        touchAlive();
      }
    };
    const watchdog = setInterval(checkLiveness, LIVENESS_CHECK_MS);
    document.addEventListener("visibilitychange", checkLiveness);

    return () => {
      clearInterval(watchdog);
      document.removeEventListener("visibilitychange", checkLiveness);
      conn?.destroy();
    };
  }, [queryClient]);
}
