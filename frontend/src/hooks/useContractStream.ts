import { useEffect, useRef, useState } from "react";
import { fromJson, type JsonValue } from "@bufbuild/protobuf";
import { StreamFrameSchema } from "../abi/llmsafespaces/abi/v1/abi_pb";
import type { Event } from "../abi/llmsafespaces/abi/v1/contract_pb";
import { EventType } from "../abi/llmsafespaces/abi/v1/contract_pb";
import { getEnv } from "../env";
import { createSSEConnection, type SSEConnection } from "../lib/sseConnection";
import { wsLog } from "../lib/wsLog";
import { applySequenced, applySnapshot, cloneSessionSnapshot, newFold, type SessionFold } from "../session/fold";

const MIN_RECONNECT_MS = 2_000;
const MAX_RECONNECT_MS = 30_000;
const READ_TIMEOUT_MS = 35_000; // Must exceed backend heartbeat interval (25s)

/** The render-consumable projection of the fold: a stable shallow copy of
 * the session map. Updated on significant frames (everything except
 * PART_DELTA — high-frequency streaming text renders via onEvent, not via
 * React state). */
export interface ContractStreamState {
  seq: bigint;
  sessions: ReadonlyMap<string, ReturnType<typeof cloneSessionSnapshot>>;
}

export interface ContractStreamOptions {
  /** Called for every APPLIED contract event (post discard rule), in
   * stream order. Imperative by design — rendering consumers dispatch
   * from here rather than re-rendering per frame. */
  onEvent?: (event: Event, seq: bigint) => void;
  /** Called after every applied snapshot frame (initial connect and
   * re-seeds) — the I12 standing-state source. */
  onSnapshot?: (state: ContractStreamState) => void;
  /** Fires on re-connections only (mirrors useEventStream). */
  onReconnect?: () => void;
}

export function useContractStream(workspaceId: string | undefined, options: ContractStreamOptions) {
  const optionsRef = useRef(options);
  optionsRef.current = options;

  const foldRef = useRef<SessionFold>(newFold());
  const [state, setState] = useState<ContractStreamState>(() => projectState(foldRef.current));

  useEffect(() => {
    if (!workspaceId) return;

    foldRef.current = newFold();
    setState(projectState(foldRef.current));

    let hasConnectedOnce = false;
    let seeded = false;
    const conn: { current: SSEConnection | undefined } = { current: undefined };
    const { apiBaseUrl } = getEnv();
    const url = `${apiBaseUrl}/workspaces/${workspaceId}/contract-events`;

    const publish = () => setState(projectState(foldRef.current));

    conn.current = createSSEConnection({
      url,
      onEvent: (data, eventName) => {
        if (eventName === "resync") {
          // Slow-consumer sentinel: the fold is stale by announcement —
          // reconnect now for a fresh stamped snapshot instead of waiting
          // out the read timeout.
          conn.current?.reconnect();
          return;
        }
        let frame;
        try {
          frame = fromJson(StreamFrameSchema, data as JsonValue);
        } catch {
          wsLog("contract-stream.malformed_frame", workspaceId);
          return;
        }
        switch (frame.frame.case) {
          case "snapshot": {
            applySnapshot(foldRef.current, frame.frame.value);
            seeded = true;
            publish();
            optionsRef.current.onSnapshot?.(projectState(foldRef.current));
            return;
          }
          case "event": {
            if (!seeded) {
              // Client rule (Go reference): a non-snapshot first frame
              // violates the protocol — drop and reconnect.
              conn.current?.reconnect();
              return;
            }
            const seqed = frame.frame.value;
            if (applySequenced(foldRef.current, seqed)) {
              if (seqed.event?.type !== EventType.PART_DELTA) publish();
              optionsRef.current.onEvent?.(seqed.event!, seqed.seq);
            }
            return;
          }
          case "reseeded": {
            // I3: the projection was reseeded — the fold MUST re-snapshot.
            // The API manager normally turns upstream reseeds into a fresh
            // connection itself; a reseeded frame reaching us means it
            // didn't — reconnect to fetch a fresh snapshot.
            conn.current?.reconnect();
            return;
          }
          default:
            return;
        }
      },
      onConnect: () => {
        seeded = false;
        if (hasConnectedOnce) {
          optionsRef.current.onReconnect?.();
        }
        hasConnectedOnce = true;
      },
      logPrefix: "contract-stream",
      logId: workspaceId,
      readTimeoutMs: READ_TIMEOUT_MS,
      minReconnectMs: MIN_RECONNECT_MS,
      maxReconnectMs: MAX_RECONNECT_MS,
    });

    return () => conn.current!.destroy();
  }, [workspaceId]);

  return state;
}

function projectState(fold: SessionFold): ContractStreamState {
  const sessions = new Map<string, ReturnType<typeof cloneSessionSnapshot>>();
  for (const [k, v] of fold.sessions) sessions.set(k, cloneSessionSnapshot(v));
  return { seq: fold.seq, sessions };
}
