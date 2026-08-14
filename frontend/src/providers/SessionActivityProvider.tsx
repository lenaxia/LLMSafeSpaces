import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useParams } from "react-router-dom";
import { useUserEventStream } from "../hooks/useUserEventStream";
import type { QuestionRequest, PermissionRequest } from "../api/types";

interface SessionActivityContextValue {
  isSessionBusy: (sessionId: string) => boolean;
  isSessionUnread: (sessionId: string) => boolean;
  workspaceBusyCount: (workspaceId: string) => number;
  clearPendingUnread: (sessionId: string) => void;
  isSessionPendingAction: (sessionId: string) => boolean;
  pendingActionSessionIds: Set<string>;
  addPendingAction: (workspaceId: string, sessionId: string, requestId: string) => void;
  removePendingAction: (requestId: string) => void;
  clearWorkspacePendingActions: (workspaceId: string) => void;
  // Pending prompt CONTENT (issue #346). The indicator (pendingActions above)
  // drives the sidebar pulse; the content (question/permission bodies) drives
  // the in-chat prompt UI. Content lives in this global layer — not in
  // ChatPage session-local state — so it survives within-tab navigation
  // between a parent session and its subtasks. Filtered by session at read.
  addPendingQuestion: (workspaceId: string, req: QuestionRequest) => void;
  addPendingPermission: (workspaceId: string, req: PermissionRequest) => void;
  pendingQuestionsForSession: (sessionId: string) => QuestionRequest[];
  pendingPermissionsForSession: (sessionId: string) => PermissionRequest[];
  clearSessionPendingPrompts: (sessionId: string) => void;
  // D10: outcome of the most recent agent.input.snapshot_complete per
  // workspace. ChatPage gates its stuck-session auto-abort on a successful
  // (ok:true) snapshot so a failed pod fetch can never read as "no pending
  // input".
  workspaceInputSnapshot: (workspaceId: string) => { ok: boolean; at: number } | undefined;
}

const SessionActivityContext = createContext<SessionActivityContextValue | null>(null);

const NON_ACTIVE_PHASES = new Set(["Suspending", "Suspended", "Terminating", "Terminated", "Failed"]);

const KNOWN_EVENT_TYPES = new Set([
  "agent.question", "agent.question.resolved", "agent.permission", "agent.permission.resolved",
  "agent.input.snapshot_begin", "agent.input.snapshot_complete", "session.status", "agent_died", "workspace.phase",
  "resync",
]);

// pruneMany returns a copy of m with every key in doomed removed, or m itself
// if none matched (avoids needless re-renders).
function pruneMany<V>(m: Map<string, V>, doomed: Set<string>): Map<string, V> {
  let next: Map<string, V> | null = null;
  for (const k of doomed) {
    if (m.has(k)) {
      if (!next) next = new Map(m);
      next.delete(k);
    }
  }
  return next ?? m;
}

export function SessionActivityProvider({ children }: { children: ReactNode }) {
  const [busySessions, setBusySessions] = useState<Map<string, string>>(new Map());
  const [pendingUnread, setPendingUnread] = useState<Map<string, string>>(new Map());
  const queryClient = useQueryClient();
  const params = useParams();
  const currentSessionId = params.sessionId;

  // Track which workspaces have been seeded from REST for BUSY state. Once
  // seeded, SSE events are the sole source of truth for busy — REST refetches
  // (triggered by invalidateQueries in ChatPage on every session.status event)
  // must not clobber SSE-tracked state. The set is per-provider-instance and
  // lives for the mount lifetime.
  const seededRef = useRef(new Set<string>());

  // Sessions the user explicitly cleared (viewed). Reconcile suppresses
  // re-adding them from a stale REST refetch (markSessionSeen PUT racing the
  // GET) until REST confirms hasUnread:false, or new activity arrives
  // (busy/idle SSE events). Keyed by sessionId → workspaceId.
  const clearedRef = useRef(new Map<string, string>());

  // Sessions with pending agent questions or permission requests.
  // Keyed by sessionId → Set<requestId> so multiple concurrent prompts
  // per session are tracked independently.
  const [pendingActions, setPendingActions] = useState<Map<string, Set<string>>>(new Map());

  // Reverse lookup: requestId → sessionId so resolved events (which carry
  // only request_id) can find the correct session to decrement.
  const requestToSessionRef = useRef(new Map<string, string>());

  // Track which workspace each session belongs to so clearWorkspacePendingActions
  // can scope its clearing to a single workspace.
  const pendingActionWsRef = useRef(new Map<string, string>());

  // Pending prompt CONTENT, keyed by requestId. Paired with pendingActions
  // (the ID-only indicator). Content is global so it survives within-tab
  // session navigation (#346); consumers filter by session at read time.
  const [pendingQuestionContent, setPendingQuestionContent] = useState<Map<string, QuestionRequest>>(new Map());
  const [pendingPermissionContent, setPendingPermissionContent] = useState<Map<string, PermissionRequest>>(new Map());

  // D9: Legacy staging buffer for snapshot anti-entropy — used when no
  // snapshot flight is open (markers without snapshot_id, older API) and the
  // workspace has not committed a marker since the last reconnect.
  // Keyed by workspaceId → Map<requestId, sessionId>.
  const legacyStagingRef = useRef(new Map<string, Map<string, string>>());

  // D9: Workspaces whose snapshot marker has been received since the last reconnect.
  // Events for committed workspaces go live; events for uncommitted workspaces stage.
  const committedWsRef = useRef(new Set<string>());

  // D10: Open snapshot flights, keyed by workspaceId → Map<flightId, Map<requestId, sessionId>>.
  // A hard page load opens TWO flights per workspace (workspace-SSE connect +
  // user-stream connect); per-flight staging (keyed by the marker's
  // snapshot_id) ensures one flight's commit cannot consume another's staged
  // events — the pre-flight single shared map made the second complete commit
  // an authoritative empty over live prompts (PR #852 review C2).
  const flightsRef = useRef(new Map<string, Map<string, Map<string, string>>>());

  // D10: Most recent snapshot outcome per workspace (ok + arrival time).
  const [inputSnapshots, setInputSnapshots] = useState<Map<string, { ok: boolean; at: number }>>(new Map());

  useEffect(() => {
    const queryCache = queryClient.getQueryCache();
    const seeded = seededRef.current;
    const cleared = clearedRef.current;

    // Busy is seeded from REST once per workspace; afterwards SSE is the sole
    // authority (REST enrichment can lag on multi-replica or timing gaps, so a
    // refetch returning status:"idle" must not clear an SSE-tracked busy session).
    function seedBusy() {
      let busyDelta: Map<string, string> | null = null;

      for (const query of queryCache.getAll()) {
        const key = query.queryKey;
        if (!Array.isArray(key) || key[0] !== "sessions" || typeof key[1] !== "string") continue;
        const wsId = key[1];
        if (seeded.has(wsId)) continue;
        const data = query.state.data;
        if (!Array.isArray(data)) continue;

        seeded.add(wsId);

        for (const session of data as Array<{ id: string; status?: string }>) {
          // Backend session.Status enum is {unknown, idle, busy, error,
          // compacting, archived} (pkg/session/session.go). "active" is not
          // a real status — accept both for backward compat with older API
          // responses and any cache entries written by the SSE handler.
          if (session.status === "busy" || session.status === "active") {
            if (!busyDelta) busyDelta = new Map();
            busyDelta.set(session.id, wsId);
          }
        }
      }

      if (busyDelta && busyDelta.size > 0) {
        setBusySessions((prev) => {
          const next = new Map(prev);
          for (const [sid, wid] of busyDelta!) {
            next.set(sid, wid);
          }
          return next;
        });
      }
    }

    // Unread is reconciled against the durable REST `hasUnread` field on every
    // cache update. REST is the source of truth for unread (computed from
    // last_message_at vs last_seen_at in the DB), so re-reading it keeps the
    // pulse accurate across page refreshes, where no SSE idle event is replayed
    // for sessions that were already idle before the page loaded.
    //
    // Reconcile is ADD-ONLY: it never removes a session from pendingUnread when
    // REST says hasUnread:false. An SSE-set unread (a response that just
    // arrived) must survive a stale refetch where last_message_at hasn't
    // persisted yet. Removal happens only via clearPendingUnread (user viewed
    // the session) or workspace.phase. `cleared` suppresses re-adding a session
    // the user just viewed, bridging the window until REST confirms
    // hasUnread:false (at which point the suppression is released).
    function reconcileUnread() {
      const add: Map<string, string> = new Map();
      const releaseCleared: Set<string> = new Set();

      for (const query of queryCache.getAll()) {
        const key = query.queryKey;
        if (!Array.isArray(key) || key[0] !== "sessions" || typeof key[1] !== "string") continue;
        const wsId = key[1];
        const data = query.state.data;
        if (!Array.isArray(data)) continue;

        for (const session of data as Array<{ id: string; hasUnread?: boolean }>) {
          const sid = session.id;
          if (session.hasUnread) {
            if (!cleared.has(sid)) {
              add.set(sid, wsId);
            }
          } else if (cleared.has(sid)) {
            releaseCleared.add(sid);
          }
        }
      }

      if (add.size > 0) {
        setPendingUnread((prev) => {
          let next = prev;
          let changed = false;
          for (const [sid, wid] of add) {
            if (!next.has(sid)) {
              if (!changed) { next = new Map(prev); changed = true; }
              next.set(sid, wid);
            }
          }
          return next;
        });
      }

      for (const sid of releaseCleared) {
        cleared.delete(sid);
      }
    }

    seedBusy();
    reconcileUnread();

    const unsubscribe = queryCache.subscribe((event) => {
      if (event.type === "updated" || event.type === "added") {
        const key = event.query.queryKey;
        if (Array.isArray(key) && key[0] === "sessions") {
          seedBusy();
          reconcileUnread();
        }
      }
    });

    return unsubscribe;
  }, [queryClient]);

  useUserEventStream({
    onReconnect: () => {
      seededRef.current.clear();
      setBusySessions(new Map());
      // D9: do NOT wipe pendingActions — rebuild is async via per-workspace
      // snapshot markers. A global wipe would blank all ?s until the slowest
      // pod fetch completes (visible flicker).
      legacyStagingRef.current.clear();
      committedWsRef.current.clear();
      flightsRef.current.clear();
    },
    onEvent: (data) => {
      const evt = data as {
        type: string;
        workspace_id?: string;
        request_id?: string;
        session_id?: string;
        status?: string;
        phase?: string;
        snapshot_id?: string;
      };

      if (evt.type === "agent.question" || evt.type === "agent.permission") {
        if (evt.workspace_id && evt.session_id && evt.request_id) {
          const wsId = evt.workspace_id;
          const sessionId = evt.session_id;
          const requestId = evt.request_id;
          // Always apply optimistically for responsiveness.
          addPendingAction(wsId, sessionId, requestId);
          // Stage if an anti-entropy window is open: into EVERY open flight
          // for the workspace (the event may belong to any of them, and a
          // flight's commit must include it), or — when no flight is open —
          // into the legacy bucket if the workspace has not committed a
          // marker since the last reconnect (D9 rule).
          const openFlights = flightsRef.current.get(wsId);
          if (openFlights && openFlights.size > 0) {
            for (const flight of openFlights.values()) {
              flight.set(requestId, sessionId);
            }
          } else if (!committedWsRef.current.has(wsId)) {
            let staged = legacyStagingRef.current.get(wsId);
            if (!staged) {
              staged = new Map();
              legacyStagingRef.current.set(wsId, staged);
            }
            staged.set(requestId, sessionId);
          }
        }
        return;
      }

      if (evt.type === "agent.question.resolved" || evt.type === "agent.permission.resolved") {
        if (evt.request_id) {
          const requestId = evt.request_id;
          const wsId = evt.workspace_id;
          // Unstage BEFORE removePendingAction — removePendingAction deletes
          // from requestToSessionRef, so we must capture the sessionId first.
          if (wsId) {
            const openFlights = flightsRef.current.get(wsId);
            if (openFlights) {
              for (const flight of openFlights.values()) {
                flight.delete(requestId);
              }
            }
            const legacy = legacyStagingRef.current.get(wsId);
            if (legacy) {
              legacy.delete(requestId);
            }
          }
          // Always remove optimistically.
          removePendingAction(requestId);
        }
        return;
      }

      // D10: a snapshot attempt is starting. Open a per-flight staging map —
      // concurrent flights for the same workspace each get their own map, so
      // one flight's commit cannot consume another's staged events.
      if (evt.type === "agent.input.snapshot_begin") {
        const wsId = evt.workspace_id;
        if (!wsId) return;
        const flightId = evt.snapshot_id ?? "";
        let openFlights = flightsRef.current.get(wsId);
        if (!openFlights) {
          openFlights = new Map();
          flightsRef.current.set(wsId, openFlights);
        }
        openFlights.set(flightId, new Map());
        return;
      }

      // D9/D10: Per-workspace marker commit. On receiving the marker for
      // wsId, authoritatively replace pendingActions for that workspace's
      // sessions with the flight's staged set. This clears ghost entries
      // (questions resolved during disconnect that the pod no longer lists)
      // without flickering.
      //
      // D10: snapshot_ok === false means the pod fetch failed — the staged
      // set is NOT authoritative. Keep existing pending state (a failed fetch
      // must never read as "no pending input") and leave the commit gate
      // (committedWsRef) untouched so a later marker can still commit.
      // Markers without the field (older API) are treated as ok for
      // backwards compatibility.
      if (evt.type === "agent.input.snapshot_complete") {
        const wsId = evt.workspace_id;
        if (!wsId) return;
        const ok = (evt as { snapshot_ok?: boolean }).snapshot_ok !== false;
        const flightId = evt.snapshot_id ?? "";

        setInputSnapshots((prev) => {
          const next = new Map(prev);
          next.set(wsId, { ok, at: Date.now() });
          return next;
        });

        // Take this flight's staging (per-flight when snapshot_id is present;
        // the legacy bucket otherwise).
        let staged: Map<string, string> | undefined;
        const openFlights = flightsRef.current.get(wsId);
        if (openFlights && openFlights.has(flightId)) {
          staged = openFlights.get(flightId);
          openFlights.delete(flightId);
          if (openFlights.size === 0) {
            flightsRef.current.delete(wsId);
          }
        } else if (flightId !== "" && committedWsRef.current.has(wsId)) {
          // R2 (review round 3): a complete carrying a flight ID that matches
          // no open flight, on an already-committed workspace — its begin was
          // lost (the broker drops events when the subscriber channel is
          // full), so events that should have staged into that flight staged
          // nowhere. Committing the (empty) legacy bucket here would wipe
          // live prompts — the exact false-abort class this provider guards
          // against. Treat as non-authoritative: keep pending state.
          return;
        } else {
          staged = legacyStagingRef.current.get(wsId);
          legacyStagingRef.current.delete(wsId);
        }

        if (!ok) {
          return;
        }
        committedWsRef.current.add(wsId);

        // Compute doomed requestIds from the CURRENT pendingActions snapshot
        // in the closure — NOT inside the setPendingActions updater (which
        // React runs asynchronously during render, so the array would be empty
        // at the content-pruning call). Mirrors clearWorkspacePendingActions.
        const stagedRequestIds = new Set(staged?.keys() ?? []);
        const doomedRequestIds: string[] = [];
        for (const [sessionId, rids] of pendingActions) {
          if (pendingActionWsRef.current.get(sessionId) === wsId) {
            for (const rid of rids) {
              if (!stagedRequestIds.has(rid)) {
                doomedRequestIds.push(rid);
              }
            }
          }
        }

        setPendingActions((prev) => {
          const next = new Map(prev);
          for (const [sessionId] of next) {
            if (pendingActionWsRef.current.get(sessionId) === wsId) {
              next.delete(sessionId);
            }
          }
          if (staged) {
            for (const [requestId, sessionId] of staged) {
              const existing = next.get(sessionId);
              const set = new Set(existing ?? []);
              set.add(requestId);
              next.set(sessionId, set);
              requestToSessionRef.current.set(requestId, sessionId);
              pendingActionWsRef.current.set(sessionId, wsId);
            }
          }
          return next;
        });

        // Prune content maps for dropped requestIds via the setters (React state).
        if (doomedRequestIds.length > 0) {
          for (const rid of doomedRequestIds) {
            requestToSessionRef.current.delete(rid);
          }
          setPendingQuestionContent((prev) => {
            const next = new Map(prev);
            for (const rid of doomedRequestIds) next.delete(rid);
            return next;
          });
          setPendingPermissionContent((prev) => {
            const next = new Map(prev);
            for (const rid of doomedRequestIds) next.delete(rid);
            return next;
          });
        }
        return;
      }

      if (evt.type === "session.status" && evt.session_id && evt.workspace_id) {
        if (evt.status === "busy") {
          clearedRef.current.delete(evt.session_id);
          setBusySessions((prev) => {
            const next = new Map(prev);
            next.set(evt.session_id!, evt.workspace_id!);
            return next;
          });

          const sessionsKey = ["sessions", evt.workspace_id];
          const existing = queryClient.getQueryData(sessionsKey);
          if (existing) {
            queryClient.setQueryData(sessionsKey, (old: unknown) => {
              if (!Array.isArray(old)) return old;
              return old.map((s: Record<string, unknown>) =>
                s.id === evt.session_id ? { ...s, status: "active" } : s
              );
            });
          }
        } else if (evt.status === "idle") {
          setBusySessions((prev) => {
            const next = new Map(prev);
            next.delete(evt.session_id!);
            return next;
          });

          if (evt.session_id !== currentSessionId) {
            clearedRef.current.delete(evt.session_id);
            setPendingActions((prev) => {
              if (!prev.has(evt.session_id!)) return prev;
              const next = new Map(prev);
              next.delete(evt.session_id!);
              return next;
            });
            setPendingUnread((prev) => {
              const next = new Map(prev);
              next.set(evt.session_id!, evt.workspace_id!);
              return next;
            });

            const sessionsKey = ["sessions", evt.workspace_id];
            const existing = queryClient.getQueryData(sessionsKey);
            if (existing) {
              queryClient.setQueryData(sessionsKey, (old: unknown) => {
                if (!Array.isArray(old)) return old;
                return old.map((s: Record<string, unknown>) =>
                  s.id === evt.session_id ? { ...s, status: "idle", hasUnread: true } : s
                );
              });
            }
          } else {
            setPendingActions((prev) => {
              if (!prev.has(evt.session_id!)) return prev;
              const next = new Map(prev);
              next.delete(evt.session_id!);
              return next;
            });
            const sessionsKey = ["sessions", evt.workspace_id];
            const existing = queryClient.getQueryData(sessionsKey);
            if (existing) {
              queryClient.setQueryData(sessionsKey, (old: unknown) => {
                if (!Array.isArray(old)) return old;
                return old.map((s: Record<string, unknown>) =>
                  s.id === evt.session_id ? { ...s, status: "idle" } : s
                );
              });
            }
          }
        }
      }

      if (evt.type === "agent_died" && evt.workspace_id) {
        const wsId = evt.workspace_id;
        setBusySessions((prev) => {
          const next = new Map();
          for (const [sid, wid] of prev) {
            if (wid !== wsId) next.set(sid, wid);
          }
          return next;
        });
        clearWorkspacePendingActions(wsId);
      }

      // resync: the broker dropped events for this subscriber (channel
      // backpressure). Snapshot flight state is unreliable — a begin may have
      // been dropped while its complete arrives. Clear the commit gate and
      // any open flights so subsequent events stage again (legacy rule) and
      // the next flight rebuilds authoritatively.
      if (evt.type === "resync") {
        committedWsRef.current.clear();
        flightsRef.current.clear();
        legacyStagingRef.current.clear();
        return;
      }

      if (evt.type === "workspace.phase" && evt.workspace_id && evt.phase) {
        const wsId = evt.workspace_id;
        if (NON_ACTIVE_PHASES.has(evt.phase)) {
          seededRef.current.delete(wsId);
          setBusySessions((prev) => {
            const next = new Map();
            for (const [sid, wid] of prev) {
              if (wid !== wsId) next.set(sid, wid);
            }
            return next;
          });
          setPendingUnread((prev) => {
            const next = new Map<string, string>();
            for (const [sid, wid] of prev) {
              if (wid !== wsId) next.set(sid, wid);
            }
            return next;
          });
          for (const [sid, wid] of clearedRef.current) {
            if (wid === wsId) clearedRef.current.delete(sid);
          }
          clearWorkspacePendingActions(wsId);
        } else if (evt.phase === "Active") {
          seededRef.current.delete(wsId);
        }
      }

      if (evt.type && !KNOWN_EVENT_TYPES.has(evt.type)) {
        console.debug("[SessionActivityProvider] unhandled SSE event type:", evt.type);
      }
    },
  });

  const isSessionBusy = useCallback(
    (sessionId: string) => busySessions.has(sessionId),
    [busySessions]
  );

  const isSessionUnread = useCallback(
    (sessionId: string) => pendingUnread.has(sessionId),
    [pendingUnread]
  );

  const workspaceBusyCount = useCallback(
    (workspaceId: string) => {
      let count = 0;
      for (const wid of busySessions.values()) {
        if (wid === workspaceId) count++;
      }
      return count;
    },
    [busySessions]
  );

  const clearPendingUnread = useCallback((sessionId: string) => {
    setPendingUnread((prev) => {
      if (!prev.has(sessionId)) return prev;
      const next = new Map(prev);
      next.delete(sessionId);
      return next;
    });

    // Record the clear so reconcileUnread suppresses a stale REST refetch
    // (markSessionSeen PUT racing the GET) from re-adding the session. The
    // entry is released once REST confirms hasUnread:false, or on new activity
    // (busy/idle SSE events). Resolve the workspaceId from the unread set or
    // the session cache so workspace.phase cleanup can target it.
    let wsId = pendingUnread.get(sessionId);
    if (!wsId) {
      for (const query of queryClient.getQueryCache().getAll()) {
        const key = query.queryKey;
        if (!Array.isArray(key) || key[0] !== "sessions" || typeof key[1] !== "string") continue;
        const data = query.state.data;
        if (Array.isArray(data) && (data as Array<{ id: string }>).some((s) => s.id === sessionId)) {
          wsId = key[1];
          break;
        }
      }
    }
    clearedRef.current.set(sessionId, wsId ?? "");
  }, [pendingUnread, queryClient]);

  const addPendingAction = useCallback((workspaceId: string, sessionId: string, requestId: string) => {
    requestToSessionRef.current.set(requestId, sessionId);
    pendingActionWsRef.current.set(sessionId, workspaceId);
    setPendingActions((prev) => {
      const existing = prev.get(sessionId);
      if (existing?.has(requestId)) return prev;
      const next = new Map(prev);
      const set = new Set(existing ?? []);
      set.add(requestId);
      next.set(sessionId, set);
      return next;
    });
  }, []);

  const removePendingAction = useCallback((requestId: string) => {
    // Clear prompt content first (unconditionally) so a resolved event always
    // drops the in-chat prompt even if the indicator entry was already cleared
    // by a session-scoped clear.
    setPendingQuestionContent((prev) => {
      if (!prev.has(requestId)) return prev;
      const next = new Map(prev);
      next.delete(requestId);
      return next;
    });
    setPendingPermissionContent((prev) => {
      if (!prev.has(requestId)) return prev;
      const next = new Map(prev);
      next.delete(requestId);
      return next;
    });

    const sessionId = requestToSessionRef.current.get(requestId);
    if (!sessionId) return;
    requestToSessionRef.current.delete(requestId);
    setPendingActions((prev) => {
      const existing = prev.get(sessionId);
      if (!existing || !existing.has(requestId)) return prev;
      const next = new Map(prev);
      const set = new Set(existing);
      set.delete(requestId);
      if (set.size === 0) {
        next.delete(sessionId);
      } else {
        next.set(sessionId, set);
      }
      return next;
    });
  }, []);

  const clearWorkspacePendingActions = useCallback((workspaceId: string) => {
    // Collect doomed requestIds from the CURRENT pendingActions snapshot. This
    // must happen outside a setState updater: updaters run asynchronously at
    // render time, so collecting inside one would race the content prune and
    // leave prompt content stranded after the indicator was cleared.
    const doomedRequests = new Set<string>();
    for (const [sid, rids] of pendingActions) {
      if (pendingActionWsRef.current.get(sid) === workspaceId) {
        for (const rid of rids) doomedRequests.add(rid);
      }
    }
    if (doomedRequests.size === 0) return;
    setPendingActions((prev) => {
      let next: Map<string, Set<string>> | null = null;
      for (const sid of prev.keys()) {
        if (pendingActionWsRef.current.get(sid) === workspaceId) {
          if (!next) next = new Map(prev);
          next.delete(sid);
        }
      }
      return next ?? prev;
    });
    setPendingQuestionContent((prev) => pruneMany(prev, doomedRequests));
    setPendingPermissionContent((prev) => pruneMany(prev, doomedRequests));
    for (const rid of doomedRequests) requestToSessionRef.current.delete(rid);
  }, [pendingActions]);

  const addPendingQuestion = useCallback((workspaceId: string, req: QuestionRequest) => {
    addPendingAction(workspaceId, req.session_id, req.id);
    setPendingQuestionContent((prev) => {
      if (prev.has(req.id)) return prev;
      const next = new Map(prev);
      next.set(req.id, req);
      return next;
    });
  }, [addPendingAction]);

  const addPendingPermission = useCallback((workspaceId: string, req: PermissionRequest) => {
    addPendingAction(workspaceId, req.session_id, req.id);
    setPendingPermissionContent((prev) => {
      if (prev.has(req.id)) return prev;
      const next = new Map(prev);
      next.set(req.id, req);
      return next;
    });
  }, [addPendingAction]);

  // pendingQuestionsForSession matches both the owning session and any session
  // whose root_session_id points here, so subtask prompts bubble to the parent
  // view (same rule ChatPage previously applied at write time — now applied at
  // read time so the content is stored regardless of the currently-viewed session).
  const pendingQuestionsForSession = useCallback((sessionId: string): QuestionRequest[] => {
    const out: QuestionRequest[] = [];
    for (const q of pendingQuestionContent.values()) {
      const root = q.root_session_id ?? q.session_id;
      if (root === sessionId || q.session_id === sessionId) out.push(q);
    }
    return out;
  }, [pendingQuestionContent]);

  const pendingPermissionsForSession = useCallback((sessionId: string): PermissionRequest[] => {
    const out: PermissionRequest[] = [];
    for (const p of pendingPermissionContent.values()) {
      const root = p.root_session_id ?? p.session_id;
      if (root === sessionId || p.session_id === sessionId) out.push(p);
    }
    return out;
  }, [pendingPermissionContent]);

  // clearSessionPendingPrompts drops all prompt content + the indicator for one
  // session (US-16.12: clear stale prompts on session idle/error). Scoped to the
  // session's OWN prompts (session_id match) — NOT root_session_id — so a parent
  // going idle/error does not clear or orphan its subtasks' live prompts (the
  // subtask indicator lives under the subtask's session_id and must survive).
  const clearSessionPendingPrompts = useCallback((sessionId: string) => {
    const doomed = new Set<string>();
    for (const rid of pendingActions.get(sessionId) ?? []) doomed.add(rid);
    const collect = <T extends { id: string; session_id: string }>(m: Map<string, T>) => {
      for (const v of m.values()) {
        if (v.session_id === sessionId) doomed.add(v.id);
      }
    };
    collect(pendingQuestionContent);
    collect(pendingPermissionContent);
    if (doomed.size === 0) return;
    setPendingQuestionContent((prev) => pruneMany(prev, doomed));
    setPendingPermissionContent((prev) => pruneMany(prev, doomed));
    // Clear the indicator entry for this session too.
    setPendingActions((prev) => {
      if (!prev.has(sessionId)) return prev;
      const next = new Map(prev);
      next.delete(sessionId);
      return next;
    });
    for (const rid of doomed) requestToSessionRef.current.delete(rid);
  }, [pendingActions, pendingQuestionContent, pendingPermissionContent]);

  const isSessionPendingAction = useCallback(
    (sessionId: string) => pendingActions.has(sessionId),
    [pendingActions]
  );

  const pendingActionSessionIds = useMemo(
    () => new Set(pendingActions.keys()),
    [pendingActions]
  );

  const workspaceInputSnapshot = useCallback(
    (workspaceId: string) => inputSnapshots.get(workspaceId),
    [inputSnapshots],
  );

  return (
    <SessionActivityContext.Provider
      value={{ isSessionBusy, isSessionUnread, workspaceBusyCount, clearPendingUnread, isSessionPendingAction, pendingActionSessionIds, addPendingAction, removePendingAction, clearWorkspacePendingActions, addPendingQuestion, addPendingPermission, pendingQuestionsForSession, pendingPermissionsForSession, clearSessionPendingPrompts, workspaceInputSnapshot }}
    >
      {children}
    </SessionActivityContext.Provider>
  );
}

export function useIsSessionBusy(sessionId: string): boolean {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return false;
  return ctx.isSessionBusy(sessionId);
}

export function useIsSessionUnread(sessionId: string): boolean {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return false;
  return ctx.isSessionUnread(sessionId);
}

export function useWorkspaceBusyCount(workspaceId: string): number {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return 0;
  return ctx.workspaceBusyCount(workspaceId);
}

export function useClearPendingUnread(): (sessionId: string) => void {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return () => {};
  return ctx.clearPendingUnread;
}

export function useIsSessionPendingAction(sessionId: string): boolean {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return false;
  return ctx.isSessionPendingAction(sessionId);
}

export function useAddPendingAction(): (workspaceId: string, sessionId: string, requestId: string) => void {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return () => {};
  return ctx.addPendingAction;
}

export function useRemovePendingAction(): (requestId: string) => void {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return () => {};
  return ctx.removePendingAction;
}

export function useSessionPendingActions(): Set<string> {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return new Set();
  return ctx.pendingActionSessionIds;
}

export function useAddPendingQuestion(): (workspaceId: string, req: QuestionRequest) => void {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return () => {};
  return ctx.addPendingQuestion;
}

export function useAddPendingPermission(): (workspaceId: string, req: PermissionRequest) => void {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return () => {};
  return ctx.addPendingPermission;
}

export function usePendingQuestionsForSession(sessionId: string): QuestionRequest[] {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return [];
  return ctx.pendingQuestionsForSession(sessionId);
}

export function usePendingPermissionsForSession(sessionId: string): PermissionRequest[] {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return [];
  return ctx.pendingPermissionsForSession(sessionId);
}

export function useClearSessionPendingPrompts(): (sessionId: string) => void {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return () => {};
  return ctx.clearSessionPendingPrompts;
}

// D10: most recent input-snapshot outcome for a workspace. undefined until
// the first marker arrives. ChatPage gates the stuck-session auto-abort on
// a successful snapshot ({ok: true}) received after reconnect-mode armed.
export function useWorkspaceInputSnapshot(workspaceId: string): { ok: boolean; at: number } | undefined {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return undefined;
  return ctx.workspaceInputSnapshot(workspaceId);
}

export type SessionDisplayStatus =
  | "pending_input" | "busy" | "unread" | "idle";

export function resolveSessionStatus(input: {
  isPendingInput: boolean;
  isBusy: boolean;
  isUnread: boolean;
}): SessionDisplayStatus {
  if (input.isPendingInput) return "pending_input";
  if (input.isBusy) return "busy";
  if (input.isUnread) return "unread";
  return "idle";
}

export function useSessionStatus(sessionId: string): SessionDisplayStatus {
  const isBusy = useIsSessionBusy(sessionId);
  const isUnread = useIsSessionUnread(sessionId);
  const isPendingInput = useIsSessionPendingAction(sessionId);
  return resolveSessionStatus({ isPendingInput, isBusy, isUnread });
}
