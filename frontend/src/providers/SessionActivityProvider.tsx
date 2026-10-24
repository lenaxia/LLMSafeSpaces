import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";
import type { Dispatch, ReactNode, SetStateAction } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useParams } from "react-router-dom";
import { useUserEventStream } from "../hooks/useUserEventStream";
import { workspacesApi } from "../api/workspaces";
import { sessionsApi } from "../api/sessions";
import type { InputRequest } from "../api/types";
import {
  eventInternalSessionId,
  resolveSessionAlias,
  reverseSessionAlias,
  subscribeSessionAliases,
  translateSessionIds,
} from "../session/sessionAliases";

/** Provider-local extension: the envelope's whileAway marker rides the stored object (D3). */
// #1365: receivedAt is stamped on store entry — the whileAway staleness
// bound in ChatPage keys off it, never off the (absent) event stream.
type StoredInputRequest = InputRequest & { whileAway?: boolean; receivedAt?: number };

// #1821 pieces 3-4 — busy is a LEASED cache of the backend authority's
// answer, never self-owned truth. Each latched busy entry carries its
// workspace, the timestamp of its last confirming evidence (delta or
// snapshot row), and — when orderable — the broker EventID of that
// evidence so a snapshot commit can tell a fresher delta (survives the
// commit) from one the authority already superseded (governed by the
// snapshot's rows).
export interface BusyEntry {
  workspaceId: string;
  confirmedAt: number;
  // The broker EventID of the confirming evidence. 0 = REST-overlay
  // evidence (no broker id — governed by ANY later snapshot). undefined
  // = unorderable legacy frame (no event_id at all — protected from
  // snapshot removal; unreachable in practice: every PublishToUser
  // event carries an id).
  confirmEventId?: number;
  // The session's HARNESS id when the confirming evidence carried it
  // additively (#1848 r2 finding 4): lets a degraded (harness-keyed)
  // snapshot row block removal of exactly the entry it may correspond
  // to, instead of one degraded row suppressing the whole workspace's
  // authoritative removals.
  internalId?: string;
}

// #1821 piece 4: the lease window. L = 90s ≈ 3× the 25s heartbeat and
// above the 60s liveness-silence window: only a genuinely
// confirmation-less span (heartbeats flowing, no busy evidence — the
// lost-idle case) expires, and expiry buys ONE authoritative recheck,
// not a self-made answer.
export const BUSY_LEASE_MS = 90_000;
const BUSY_LEASE_SWEEP_MS = 15_000;

// useWhileAwayStalenessSweep (#1365) drives the whileAway staleness bound
// with a TIMER while any whileAway pill is pending. The fold-sync effect's
// deps do not tick on a healthy-but-quiet stream (heartbeats mutate
// nothing it watches), so a lazy in-effect check never fires — the frozen-
// tab scenario the bound exists for. Dropping uses dropPendingAction (no
// tombstone: a genuine re-presentation may re-add later).
export const WHILE_AWAY_STALE_MS = 10 * 60 * 1000;

export function useWhileAwayStalenessSweep(
  questions: Array<StoredInputRequest>,
  permissions: Array<StoredInputRequest>,
  drop: (requestId: string) => void,
  staleMs: number = WHILE_AWAY_STALE_MS,
): void {
  const has = questions.some((q) => q.whileAway) || permissions.some((p) => p.whileAway);
  useEffect(() => {
    if (!has) return;
    const sweep = () => {
      const now = Date.now();
      for (const q of questions) {
        if (q.whileAway && now - (q.receivedAt ?? 0) > staleMs) drop(q.id);
      }
      for (const p of permissions) {
        if (p.whileAway && now - (p.receivedAt ?? 0) > staleMs) drop(p.id);
      }
    };
    sweep();
    const t = setInterval(sweep, Math.min(60_000, staleMs));
    return () => clearInterval(t);
    // questions/permissions/drop in deps: re-arms on list changes; the
    // interval is what guarantees time passes even when nothing changes.
  }, [has, staleMs, questions, permissions, drop]);
}


interface SessionActivityContextValue {
  isSessionBusy: (sessionId: string) => boolean;
  isSessionUnread: (sessionId: string) => boolean;
  workspaceBusyCount: (workspaceId: string) => number;
  hungWorkspaces: Set<string>;
  clearPendingUnread: (sessionId: string) => void;
  // #1687 (pin 2): bridge for the contract stream's SESSION_STATUS IDLE —
  // an equally authoritative end-of-turn signal as the user stream's
  // session.status=idle frame. ChatPage calls it so a busy latched by the
  // user stream clears even when that stream's idle frame was lost
  // (broker drop / reconnect gap).
  clearSessionBusy: (workspaceId: string, sessionId: string) => void;
  isSessionPendingAction: (sessionId: string) => boolean;
  pendingActionSessionIds: Set<string>;
  // #786: sessions interrupted by a force-stop — the "was interrupted"
  // indicator's surface. Cleared when a new turn starts (busy) or the
  // session is deleted.
  isSessionAborted: (sessionId: string) => boolean;
  // #786: sessions deleted while this tab was open — ChatPage navigates
  // away from a deleted current session.
  isSessionDeleted: (sessionId: string) => boolean;
  addPendingAction: (workspaceId: string, sessionId: string, requestId: string) => void;
  removePendingAction: (requestId: string) => void;
  // Absence-evidence removal (no tombstone) — see dropPendingAction below.
  dropPendingAction: (requestId: string) => void;
  clearWorkspacePendingActions: (workspaceId: string) => void;
  // Pending prompt CONTENT (issue #346). The indicator (pendingActions above)
  // drives the sidebar pulse; the content (question/permission bodies) drives
  // the in-chat prompt UI. Content lives in this global layer — not in
  // ChatPage session-local state — so it survives within-tab navigation
  // between a parent session and its subtasks. Filtered by session at read.
  addPendingQuestion: (workspaceId: string, req: InputRequest) => void;
  addPendingPermission: (workspaceId: string, req: InputRequest) => void;
  pendingQuestionsForSession: (sessionId: string) => StoredInputRequest[];
  pendingPermissionsForSession: (sessionId: string) => StoredInputRequest[];
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
  "resync", "workspace.alert", "workspace.alert_resolved",
  // #1821 piece 2: the busy-snapshot handshake flight.
  "session.status.snapshot_begin", "session.status.snapshot", "session.status.snapshot_complete",
]);

// #1821 piece 2: one staged busy-snapshot flight — the busy rows and
// cursor a begin marker opened, committed (or dropped) by the matching
// complete. hasDegradedRows: the flight carried a busy row that stayed
// harness-keyed after the client-side heal — its id join is PARTIAL, so
// the commit may add busy rows but must never remove (#1848 r1 f4).
interface BusyFlight {
  busyRowIds: Set<string>;
  cursor: number;
  // Degraded busy rows that stayed harness-keyed after the client-side
  // heal — their harness ids. A removal is blocked only for entries
  // that could BE one of these rows (matching internalId, or entries
  // with no known internal id — #1848 r2 finding 4).
  degradedBusyIds: Set<string>;
  // Every staged busy row's harness id, keyed by the row's client-side
  // key (the phrase for resolved rows, the harness id for degraded
  // ones) — latched entries carry it as BusyEntry.internalId so later
  // flights can block/protect them precisely.
  rowInternalIds: Map<string, string>;
  // #1883: RESOLVED idle rows (the authority answered this session
  // idle) — the commit mirrors them onto the sessions cache so the
  // row's status projection cannot contradict the truth the flight
  // just reconciled (the busy direction already mirrors; the idle
  // direction is the #1883 un-latch completion).
  idleRowIds: Set<string>;
  // EVERY row's client-side key (busy and idle alike) — the set of
  // sessions the authority ANSWERED. The commit stamps each answered
  // session's per-session ordering watermark with the cursor: a later
  // overlapping (older) flight's row for that session cannot latch
  // over this answer (#1848 r0 finding 2, the per-session form).
  answeredIds: Set<string>;
}

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

// tombstoneRequest marks a request ID as resolved (#1365): insertion-ordered
// with a FIFO cap of 1000 so a long-lived tab cannot grow it unbounded.
function tombstoneRequest(tombs: Map<string, true>, requestId: string): void {
  if (tombs.has(requestId)) return;
  tombs.set(requestId, true);
  if (tombs.size > 1000) {
    const oldest = tombs.keys().next().value;
    if (oldest !== undefined) tombs.delete(oldest);
  }
}

// registerFirstSeen records a request's first observation without calling
// Date.now() from a render-analyzed body (react-hooks/purity). Runs from
// event callbacks via module scope, like wipeBusyForEventLoss.
// Monotonic first-seen sequence (#1845): two asks seeded from the SAME
// fold snapshot (page reload / reconnect with several pending) can land
// in one tick — identical Date.now() collapsed the newest-first sort's
// comparator to a stable no-op, leaking fold insertion order through as
// the stack order. Stamps are strictly increasing per registration while
// staying ms-ordered versus real time, so the sort is total and the
// stack's "newest-activity-first" contract holds within batches too.
let firstSeenSeq = 0;

function registerFirstSeen(firstSeen: Map<string, number>, requestId: string): void {
  if (!firstSeen.has(requestId)) {
    firstSeenSeq = Math.max(firstSeenSeq + 1, Date.now());
    firstSeen.set(requestId, firstSeenSeq);
  }
}

// stampBusyNow is the lease-stamp seam: Date.now() for BusyEntry writes
// without calling it from a render-analyzed body (react-hooks/purity —
// same discipline as registerFirstSeen). Runs from event callbacks and
// effect timers via module scope.
function stampBusyNow(): number {
  return Date.now();
}

// #1824: a late-learned harness→phrase pair must re-key the provider's
// session-keyed activity maps — the contract fold's re-key alone leaves the
// sidebar unbadged. Only keys that resolve through the changed workspace's
// alias table move; a phrase key that already exists wins (it is the same
// live session, so the move never drops its state).
function rekeyWorkspaceValueMap<V>(
  prev: Map<string, V>,
  workspaceId: string,
  belongs: (value: V) => boolean,
): Map<string, V> | null {
  let next: Map<string, V> | null = null;
  for (const [sessionId, value] of Array.from(prev.entries())) {
    if (!belongs(value)) continue;
    const phrase = resolveSessionAlias(workspaceId, sessionId);
    if (!phrase || phrase === sessionId) continue;
    if (!next) next = new Map(prev);
    next.delete(sessionId);
    if (!next.has(phrase)) next.set(phrase, value);
  }
  return next;
}

function rekeyPendingContent(
  prev: Map<string, StoredInputRequest>,
  workspaceId: string,
  workspaceOf: (sessionId: string) => string | undefined,
): Map<string, StoredInputRequest> | null {
  let next: Map<string, StoredInputRequest> | null = null;
  for (const [requestId, request] of Array.from(prev.entries())) {
    // Never move a request known to belong to another workspace.
    const owner =
      (request.sessionId !== undefined ? workspaceOf(request.sessionId) : undefined) ??
      (request.rootSessionId !== undefined ? workspaceOf(request.rootSessionId) : undefined);
    if (owner !== undefined && owner !== workspaceId) continue;
    const phrase = request.sessionId
      ? resolveSessionAlias(workspaceId, request.sessionId)
      : request.sessionId;
    const rootPhrase = request.rootSessionId
      ? resolveSessionAlias(workspaceId, request.rootSessionId)
      : request.rootSessionId;
    const sessionMoved = phrase !== undefined && phrase !== request.sessionId;
    const rootMoved = rootPhrase !== undefined && rootPhrase !== request.rootSessionId;
    if (!sessionMoved && !rootMoved) continue;
    if (!next) next = new Map(prev);
    next.set(requestId, {
      ...request,
      ...(sessionMoved ? { sessionId: phrase } : {}),
      ...(rootMoved ? { rootSessionId: rootPhrase } : {}),
    });
  }
  return next;
}

// wipeBusyForEventLoss is the shared reconciliation core for
// admitted-event-loss on the user stream: the reconnect wipe (#1646) and
// the broker's resync signal (#1687 pin 1 — the dropped set may include
// the session.status=idle that would have cleared a latched badge; no
// onReconnect will fire on the still-healthy connection). It stamps the
// wipe, reopens the REST seed gate, and drops latched busy so the gated
// re-seed re-adds only sessions REST still reports busy. Callers MUST
// invalidate ["sessions"] only AFTER this returns — a fetch born before
// the stamp is refused by seedBusy's birth guard (#1646 r4/r5). Module
// scope: runs from event callbacks, outside any render-analyzed body.
function wipeBusyForEventLoss(
  wipeAtRef: { current: number },
  seededRef: { current: Set<string> },
  setBusySessions: Dispatch<SetStateAction<Map<string, BusyEntry>>>,
  wipedMemoryRef?: { current: Map<string, Map<string, BusyEntry>> },
): void {
  wipeAtRef.current = Date.now();
  seededRef.current.clear();
  setBusySessions((prev) => {
    // #1848 r3 finding 5: remember what the wipe cleared, per
    // workspace — an ok=false authority answer to the post-wipe
    // re-seed re-latches it as UNKNOWN (the charter's "hold unknown,
    // not idle"); the first ok=true flight reconciles definitively.
    // Idempotent under StrictMode double-invocation (same prev → same
    // entries).
    //
    // #1848 r5 finding 1: the memory is UNORDERABLE evidence — the
    // pre-wipe confirmEventId belongs to the OLD replica's id space
    // (the broker's ids are per-replica; a reconnect may land anywhere).
    // Carrying it verbatim would let a stale high id block the new
    // replica's authoritative clears forever (confirm > cursor). Store
    // orderable-at-zero, exactly like a REST seed — any later snapshot
    // governs it. internalId is id-space independent and stays.
    if (wipedMemoryRef) {
      for (const [sid, entry] of prev) {
        let wsMem = wipedMemoryRef.current.get(entry.workspaceId);
        if (!wsMem) {
          wsMem = new Map();
          wipedMemoryRef.current.set(entry.workspaceId, wsMem);
        }
        wsMem.set(sid, { ...entry, confirmEventId: 0 });
      }
    }
    return new Map();
  });
}

export function SessionActivityProvider({ children }: { children: ReactNode }) {
  // #1821 pieces 3-4: values carry the leased-cache bookkeeping
  // (BusyEntry) — workspace, last confirming evidence timestamp, and
  // the confirming delta's EventID when orderable.
  const [busySessions, setBusySessions] = useState<Map<string, BusyEntry>>(new Map());
  // #1821 piece 3 — delta-ordering state. The broker assigns monotonic
  // EventIDs but DELIVERS after releasing the shard lock, so arrival
  // order is not guaranteed (#1848 r0 finding 3); a snapshot only
  // covers ITS OWN workspace's rows, so workspace-level bounds are
  // PER-WORKSPACE (#1848 r1 findings 1-2); and out-of-order delivery
  // within ONE session (idle 100 before busy 99) can invert the last
  // transition, so ordering is ultimately enforced PER-SESSION
  // (#1848 r2 finding 3): a session's delta applies only when its id
  // exceeds the last id applied for THAT session, and a snapshot's row
  // for a session only latches when no newer per-session delta
  // supersedes it. A per-workspace snapshot cursor remains the
  // workspace-level subsumption bound (a delta ≤ cursor is already
  // reflected in that workspace's committed rows).
  const wsSnapshotCursorRef = useRef(new Map<string, number>());
  // Per-session last-applied evidence id (delta ids and snapshot
  // cursors). The one true per-session ordering watermark.
  const sessionLastDeltaIDRef = useRef(new Map<string, number>());

  // #1821 piece 2: open busy-snapshot flights, keyed workspaceId →
  // Map<flightId, BusyFlight> (the per-flight discipline of the input
  // snapshots — two flights can run concurrently for one workspace).
  const busyFlightsRef = useRef(new Map<string, Map<string, BusyFlight>>());

  // #1848 r2 finding 6: workspaces awaiting ONE authority retry after a
  // non-authoritative (ok=false / dropped) flight — wsId → earliest
  // next attempt. Without evidence latched (the post-wipe case) there
  // is no lease to expire, so the sweep drives these instead: one
  // request per lease window until an authoritative answer lands.
  const pendingRecheckRef = useRef(new Map<string, number>());
  // #1848 r3 finding 5: what the last wipe cleared, per workspace — the
  // unknown-hold memory. An ok=false answer to the post-wipe re-seed
  // re-latches it (unknown ≠ idle); ok=true / terminal events clear it.
  const wipedBusyRef = useRef(new Map<string, Map<string, BusyEntry>>());
  // #1848 r4 (finding 4, the late-complete half): workspaces in a
  // TERMINAL state (agent_died, non-Active phase). An ok=false answer
  // arriving AFTER the terminal event must arm nothing — the sweep
  // would POST every window for a corpse that can never answer. An
  // Active phase re-arms normal service.
  const terminalWsRef = useRef(new Set<string>());
  // #1848 r4 (finding 1, the resync half): recheck workspaces captured
  // at resync-wipe time. The capture rides the setBusySessions updater
  // queue (a render-closure read of busySessions is stale within one
  // chunk); the sweep fires the requests once the queue has settled.
  const resyncRecheckRef = useRef(new Set<string>());
  // Re-render trigger for pendingRecheckRef mutations (refs are not
  // reactive; the sweep effect re-arms on this tick).
  const [recheckTick, setRecheckTick] = useState(0);

  // D6 (#998): workspaces with an active hung-session alert. Set on
  // workspace.alert/session_hung; cleared on that workspace's next
  // session.status=idle (the hang resolved) — the map is workspace-keyed
  // so the sidebar badge survives session switches.
  const [hungWorkspaces, setHungWorkspaces] = useState<Set<string>>(new Set());
  // Terminal events (idle, aborted, deleted, agent_died, non-active
  // phase) end any hang for the workspace — no later SSE will clear it.
  const dropHungWorkspace = useCallback((wsId: string) => {
    setHungWorkspaces((prev) => {
      if (!prev.has(wsId)) return prev;
      const next = new Set(prev);
      next.delete(wsId);
      return next;
    });
  }, []);
  const [pendingUnread, setPendingUnread] = useState<Map<string, string>>(new Map());
  // #786: force-stopped sessions — the interrupted indicator's source.
  const [abortedSessions, setAbortedSessions] = useState<Set<string>>(new Set());
  // #786: deleted sessions — the route-level consumer (ChatPage) owns
  // navigation away; the provider records the fact because it lives
  // above the routes (useParams here is always empty in production).
  const [deletedSessions, setDeletedSessions] = useState<Set<string>>(new Set());
  const queryClient = useQueryClient();
  const params = useParams();
  const currentSessionId = params.sessionId;

  // Track which workspaces have been seeded from REST for BUSY state. Once
  // seeded, SSE events are the sole source of truth for busy — REST refetches
  // (triggered by invalidateQueries in ChatPage on every session.status event)
  // must not clobber SSE-tracked state. The set is per-provider-instance and
  // lives for the mount lifetime.
  const seededRef = useRef(new Set<string>());

  // #1646 r3: timestamp of the last onReconnect wipe (0 = never wiped).
  // seedBusy must never latch a workspace's seed gate from rows that
  // predate it — see the guard in seedBusy.
  const wipeAtRef = useRef(0);

  // #1646 r4: causality tracking for the seed gate (see seedBusy's
  // birth guard): pendingFetchStart = when the current/latest fetch
  // STARTED per workspace; dataBirth = when the committed data's
  // producing fetch started (or when an authoritative write landed).
  const pendingFetchStartRef = useRef(new Map<string, number>());
  const dataBirthRef = useRef(new Map<string, number>());

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
  const [pendingQuestionContent, setPendingQuestionContent] = useState<Map<string, StoredInputRequest>>(new Map());
  const [pendingPermissionContent, setPendingPermissionContent] = useState<Map<string, StoredInputRequest>>(new Map());

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

  // #1365: resolved-tombstones. A tombstone is RESOLUTION EVIDENCE ONLY:
  // the resolved event, the optimistic 2xx clear, a prompt dismissal. It
  // is never set on absence evidence (a fold the snapshot lacked, a bulk
  // lifecycle clear) — the pod projection is authority but it goes stale
  // (the incident itself), and a false tombstone would permanently hide a
  // live ask for the tab's lifetime. Absence-based removals use
  // dropPendingAction instead; a ghost pill left by absence clears on its
  // next click (every reply path emits the resolved event post-#1365).
  // IDs are unique per ask, so a tombstone never blocks a genuinely new
  // ask. Insertion-ordered Map with a FIFO cap so a long-lived tab cannot
  // grow it unbounded.
  const resolvedTombstonesRef = useRef(new Map<string, true>());

  // #1365: first-seen timestamps per request ID — the pill stack renders
  // newest-activity-first, so a stale pill cannot visually shadow the live
  // ask the user actually needs to answer.
  const requestFirstSeenRef = useRef(new Map<string, number>());

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
      let busyDelta: Map<string, BusyEntry> | null = null;

      for (const query of queryCache.getAll()) {
        const key = query.queryKey;
        if (!Array.isArray(key) || key[0] !== "sessions" || typeof key[1] !== "string") continue;
        const wsId = key[1];
        if (seeded.has(wsId)) continue;
        const data = query.state.data;
        if (!Array.isArray(data)) continue;

        // #1646 r3: never latch the gate from rows that predate the
        // last reconnect wipe. seedBusy is a FULL pass — any workspace's
        // success dispatch re-runs it against every cached sessions
        // query, including workspaces whose refetch is still in flight
        // (the fetch dispatch preserves prior rows) or inactive
        // (collapsed workspace, no refetch at all). Pre-wipe rows are
        // the stale pre-outage state: seeding from them re-adds stale
        // busy and suppresses that workspace's own fresh success.
        // Post-wipe data (dataUpdatedAt > wipeAt) is fresh by
        // construction; wipeAt=0 (no wipe yet) keeps the mount-time
        // full pass seeding everything. Strict `<` — a write landing
        // in the SAME millisecond as the wipe (the existing reconnect
        // re-seed pin drives setQueryData synchronously after
        // onReconnect) is a fresh authoritative write, not stale
        // pre-outage data.
        if (query.state.dataUpdatedAt < wipeAtRef.current) continue;

        // #1646 r4: commit time is not birth time. A fetch that STARTED
        // before the wipe and commits after it carries pre-outage rows
        // past the commit-time guard above, in two reachable shapes:
        // the invalidation REUSES an initial pending fetch when
        // state.data is undefined (query-core returns the existing
        // promise), and a zombie fetch on an unmounted query runs to
        // completion (getSessions takes no AbortSignal, so observer
        // removal cannot cancel it) — the incident's own racing-GET
        // window. dataBirth records when the data's producing fetch
        // STARTED (fetch dispatch) or when an authoritative write
        // landed (setState / manual success); seeding refuses data
        // BORN before the wipe. Strict `<`, same-ms rule as above.
        // Ordering guarantee (r5): the reconnect path wipes BEFORE
        // invalidating sessions (useUserEventStream onConnect), so
        // every invalidation-born fetch start — and therefore every
        // legitimate post-reconnect re-seed's birth — postdates the
        // wipe by construction, even when the clock ticks between the
        // statements.
        const birth = dataBirthRef.current.get(wsId) ?? 0;
        if (birth < wipeAtRef.current) continue;

        seeded.add(wsId);

        // Currently-busy session ids for this workspace — seeds busy
        // state below (the hung gate re-reads LIVE cache in its .then;
        // see the comment there).
        for (const session of data as Array<{ id: string; status?: string }>) {
          // Backend session.Status enum is {unknown, idle, busy, error,
          // compacting, archived} (pkg/session/session.go). "active" is not
          // a real status — accept both for backward compat with older API
          // responses and any cache entries written by the SSE handler.
          if (session.status === "busy" || session.status === "active") {
            if (!busyDelta) busyDelta = new Map();
            // #1821 piece 4 / #1848 r0 finding 1: REST-overlay evidence
            // is ORDERABLE-AT-ZERO — no broker id, so any later snapshot
            // (cursor ≥ 0) governs it. Marking it unorderable made
            // REST-seeded busy permanently unremovable, defeating the
            // lease's bounded-staleness guarantee.
            busyDelta.set(session.id, { workspaceId: wsId, confirmedAt: Date.now(), confirmEventId: 0 });
          }
        }

        // D6 (#998) history surface: seed hung state from persisted
        // alerts once per workspace so a reconnecting client recovers
        // alerts missed while no SSE stream was attached — but ONLY
        // for UNRESOLVED session_hung alerts (resolvedAt is the
        // server-side resolution flag, written when the hang ends:
        // D6 sweep observation, leave-Active watch event, or the
        // read-side heal aging a lost resolution; the SSE-emitting
        // writers also emit workspace.alert_resolved). The feed is
        // append-only 24h history, so
        // resolution — not client-side busy reconstruction — decides
        // liveness: a session that hung and recovered while no browser
        // watched must not badge on reload, and the poller's next-tick
        // resolution closes the between-recovery-and-sweep window for
        // connected clients via the alert_resolved event. Best-effort
        // — a failed fetch leaves the in-memory state untouched.
        workspacesApi
          .getAlerts(wsId)
          .then((alerts) => {
            if (!alerts || alerts.length === 0) return;
            const hasLiveHung = alerts.some((a) => a.alert === "session_hung" && !a.resolvedAt);
            if (!hasLiveHung) return;
            setHungWorkspaces((prev) => (prev.has(wsId) ? prev : new Set(prev).add(wsId)));
          })
          .catch(() => {});
      }

      if (busyDelta && busyDelta.size > 0) {
        setBusySessions((prev) => {
          const next = new Map(prev);
          for (const [sid, entry] of busyDelta!) {
            // Never downgrade: a delta-confirmed entry (broker id) is
            // stronger evidence than the REST overlay's 0.
            const existing = next.get(sid);
            if (existing && (existing.confirmEventId ?? 0) > (entry.confirmEventId ?? 0)) continue;
            next.set(sid, entry);
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

    // Adopt queries already fetching at subscription registration:
    // their "fetch" dispatch predates this subscription (child effects
    // run before this provider's), so stamp them conservatively NOW —
    // any later wipe postdates the stamp and their first commit is
    // correctly treated as born pre-wipe.
    for (const query of queryCache.getAll()) {
      const key = query.queryKey;
      if (
        Array.isArray(key) && key[0] === "sessions" && typeof key[1] === "string" &&
        query.state.fetchStatus === "fetching" &&
        !pendingFetchStartRef.current.has(key[1])
      ) {
        pendingFetchStartRef.current.set(key[1], Date.now());
      }
    }

    seedBusy();
    reconcileUnread();

    const unsubscribe = queryCache.subscribe((event) => {
      if (event.type === "updated" || event.type === "added") {
        const key = event.query.queryKey;
        if (Array.isArray(key) && key[0] === "sessions") {
          // #1646 r4: record data BIRTH (request-time causality) for
          // seedBusy's birth guard — fetch starts, authoritative
          // writes (setState, manual success), and fetch commits
          // inheriting their start time.
          if (event.type === "updated" && typeof key[1] === "string") {
            const wsId = key[1];
            const action = event.action;
            if (action.type === "fetch") {
              pendingFetchStartRef.current.set(wsId, Date.now());
            } else if (action.type === "success") {
              const manual = action.manual === true;
              const startedAt = manual ? Date.now() : (pendingFetchStartRef.current.get(wsId) ?? Date.now());
              dataBirthRef.current.set(wsId, startedAt);
            } else if (action.type === "setState") {
              dataBirthRef.current.set(wsId, Date.now());
            }
          }
          // #1646 r2/r3: query-core notifies the cache "updated" for
          // EVERY dispatch, and the no-new-data ones PRESERVE stale
          // state.data: "fetch" (fetch-START — by definition predates
          // the in-flight result), "failed" (attempt failed, retries
          // left), "error" (exhausted; production runs retry:1),
          // "pause"/"continue", "invalidate". Seeding from stale rows
          // re-latches a wiped-open seed gate and suppresses every
          // later success (the add-only seed can never clear) — sticky
          // busy until the next reconnect/phase-change/remount. Seed
          // ONLY on dispatches that carry data: "success" (a fetch
          // resolved — fresh rows; setQueryData dispatches this too,
          // with manual:true) and "setState" (query.setState — explicit
          // authoritative writes).
          const actionType = event.type === "updated" ? event.action.type : undefined;
          if (event.type === "added" || actionType === "success" || actionType === "setState") {
            seedBusy();
          }
          reconcileUnread();
        }
      }
    });

    return unsubscribe;
  }, [queryClient]);

  // #1824: a pair learned after a harness-keyed event already latched is
  // joined here. Aliases are per-workspace, so resolving a key through the
  // changed workspace's table can never translate another workspace's id.
  useEffect(() => {
    const unsubscribe = subscribeSessionAliases((workspaceId) => {
      // Snapshot the ownership table BEFORE mutating any ref — the
      // pendingActions updater runs at render time (twice under React
      // StrictMode) and must not observe a half-rewritten table.
      const workspaceOf = new Map(pendingActionWsRef.current);

      // #1824 r11: ref bookkeeping is done synchronously, OUTSIDE the
      // state updaters. React 19 StrictMode double-invokes updaters; a
      // ref write inside one makes the second invocation see the moved
      // state and return the stale `prev` (harness-keyed state, phrase-
      // keyed refs/content).
      for (const [sessionId, wsId] of Array.from(pendingActionWsRef.current)) {
        if (wsId !== workspaceId) continue;
        const phrase = resolveSessionAlias(workspaceId, sessionId);
        if (!phrase || phrase === sessionId) continue;
        pendingActionWsRef.current.delete(sessionId);
        pendingActionWsRef.current.set(phrase, workspaceId);
      }
      for (const [requestId, sessionId] of Array.from(requestToSessionRef.current)) {
        if (workspaceOf.get(sessionId) !== workspaceId) continue;
        const phrase = resolveSessionAlias(workspaceId, sessionId);
        if (!phrase || phrase === sessionId) continue;
        requestToSessionRef.current.set(requestId, phrase);
      }

      setBusySessions(
        (prev) =>
          rekeyWorkspaceValueMap(prev, workspaceId, (entry) => entry.workspaceId === workspaceId) ?? prev,
      );
      setPendingUnread(
        (prev) => rekeyWorkspaceValueMap(prev, workspaceId, (wid) => wid === workspaceId) ?? prev,
      );
      setAbortedSessions((prev) => {
        let next: Set<string> | null = null;
        for (const sessionId of Array.from(prev)) {
          const phrase = resolveSessionAlias(workspaceId, sessionId);
          if (!phrase || phrase === sessionId) continue;
          if (!next) next = new Set(prev);
          next.delete(sessionId);
          next.add(phrase);
        }
        return next ?? prev;
      });
      // #1824 r11: a delete recorded before the pair learned must follow
      // the session too — ChatPage navigates on the phrase key.
      setDeletedSessions((prev) => {
        let next: Set<string> | null = null;
        for (const sessionId of Array.from(prev)) {
          const phrase = resolveSessionAlias(workspaceId, sessionId);
          if (!phrase || phrase === sessionId) continue;
          if (!next) next = new Set(prev);
          next.delete(sessionId);
          next.add(phrase);
        }
        return next ?? prev;
      });
      setPendingActions((prev) => {
        let next: Map<string, Set<string>> | null = null;
        for (const [sessionId, requestIds] of Array.from(prev.entries())) {
          if (workspaceOf.get(sessionId) !== workspaceId) continue;
          const phrase = resolveSessionAlias(workspaceId, sessionId);
          if (!phrase || phrase === sessionId) continue;
          if (!next) next = new Map(prev);
          next.delete(sessionId);
          const existing = next.get(phrase);
          const merged = new Set(existing ?? []);
          for (const requestId of requestIds) merged.add(requestId);
          next.set(phrase, merged);
        }
        return next ?? prev;
      });
      const ownerOf = (sessionId: string) => workspaceOf.get(sessionId);
      setPendingQuestionContent(
        (prev) => rekeyPendingContent(prev, workspaceId, ownerOf) ?? prev,
      );
      setPendingPermissionContent(
        (prev) => rekeyPendingContent(prev, workspaceId, ownerOf) ?? prev,
      );
      for (const [sessionId, wsId] of Array.from(clearedRef.current)) {
        if (wsId !== workspaceId) continue;
        const phrase = resolveSessionAlias(workspaceId, sessionId);
        if (!phrase || phrase === sessionId) continue;
        clearedRef.current.delete(sessionId);
        if (!clearedRef.current.has(phrase)) clearedRef.current.set(phrase, wsId);
      }
    });
    return unsubscribe;
  }, []);

  useUserEventStream({
    onReconnect: () => {
      // #1646 r3: stamp the wipe. Any sessions data predating this
      // moment is pre-outage state that the full-pass seed must not
      // re-latch (see the guard in seedBusy). The hook's onConnect
      // invalidates ["sessions"] only after this returns. The wipe
      // MEMORIES what it cleared (#1848 r3 f5) for the unknown-hold.
      wipeBusyForEventLoss(wipeAtRef, seededRef, setBusySessions, wipedBusyRef);
      // A badge latched by live SSE survives a disconnect only as
      // memory: if the session recovered while the tab was offline,
      // no idle SSE will ever clear it (Last-Event-ID replay is
      // best-effort). Drop it and let the gated re-seed re-add it
      // when — and only when — the session is still genuinely busy.
      setHungWorkspaces(new Set());
      // D9: do NOT wipe pendingActions — rebuild is async via per-workspace
      // snapshot markers. A global wipe would blank all ?s until the slowest
      // pod fetch completes (visible flicker).
      legacyStagingRef.current.clear();
      committedWsRef.current.clear();
      flightsRef.current.clear();
      // #1821 piece 2: open busy flights died with the connection —
      // the reconnect's connect-path flights rebuild authoritatively.
      busyFlightsRef.current.clear();
      // #1848 r1 finding 3: the broker is process-local and its id
      // space starts at 1 per replica — a reconnect may land on a
      // fresh replica whose cursors/deltas are all "below" the old
      // bounds. Reset the ordering state; the connect-path flights
      // re-establish it.
      wsSnapshotCursorRef.current.clear();
      sessionLastDeltaIDRef.current.clear();
    },
    onEvent: (data) => {
      const rawEvt = data as {
        type: string;
        workspace_id?: string;
        request_id?: string;
        session_id?: string;
        status?: string;
        phase?: string;
        snapshot_id?: string;
        snapshot_ok?: boolean;
        event_id?: number;
        data?: { alert?: string };
      };
      // #1817 heal window: a legacy harness-keyed frame resolves to the
      // phrase through the additive pair learned from the other stream (or
      // an earlier event). A post-fix frame is already phrase-keyed and the
      // seam is the identity. Learning is side-effectful by design — the
      // same call that resolves this event teaches later ones.
      const evt = {
        ...rawEvt,
        session_id: translateSessionIds(
          rawEvt.workspace_id,
          rawEvt.session_id,
          eventInternalSessionId(rawEvt),
        ).sessionId,
      };

      // D6 (#998): hung-session alert bookkeeping (workspace-scoped).
      if (evt.type === "workspace.alert" && evt.data?.alert === "session_hung" && evt.workspace_id) {
        const wsId = evt.workspace_id;
        setHungWorkspaces((prev) => (prev.has(wsId) ? prev : new Set(prev).add(wsId)));
      }
      if (evt.type === "session.status" && evt.status === "idle" && evt.workspace_id) {
        dropHungWorkspace(evt.workspace_id);
      }

      // The server observed the hang end (D6 sweep, or the workspace
      // left Active while alerted): the persisted flag and the badge
      // drop together (authoritative resolution, server-side).
      if (evt.type === "workspace.alert_resolved" && evt.workspace_id) {
        dropHungWorkspace(evt.workspace_id);
      }

      if (evt.type === "agent.question" || evt.type === "agent.permission") {
        if (evt.workspace_id && evt.session_id && evt.request_id) {
          const wsId = evt.workspace_id;
          const sessionId = evt.session_id;
          const requestId = evt.request_id;
          // Always apply optimistically for responsiveness.
          addPendingAction(wsId, sessionId, requestId);
          // #1313 / 4a D3: an envelope-tagged whileAway event is an INBOX
          // re-presentation — API-owned content the pod-side contract
          // fold never carries (the live ask is gone; that is the
          // point). Store its content so the prompt renders; lifecycle
          // rides the resolved-event path exactly like a live prompt's.
          const envelope = evt as { whileAway?: boolean };
          if (envelope.whileAway === true) {
            // The envelope marker rides the STORED object (provider-local
            // extension of the contract shape — the ABI InputRequest
            // itself stays clean of browser concerns; ChatPage's fold
            // removal exempts on it).
            const tagged = { ...(evt as unknown as { data: InputRequest }).data, whileAway: true } as StoredInputRequest;
            if (evt.type === "agent.question") {
              addPendingQuestion(wsId, tagged);
            } else {
              addPendingPermission(wsId, tagged);
            }
          }
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
        const openFlights = flightsRef.current.get(wsId);
        const knownFlight = openFlights?.has(flightId) ?? false;

        // R2/A (review rounds 3-4): a complete carrying a flight ID that
        // matches no open flight is NON-AUTHORITATIVE regardless of committed
        // state. Reachable when the begin was dropped by broker backpressure,
        // OR when the complete replays after a reconnect/resync cleared the
        // flight table while its begin (and any staged events) did not
        // re-deliver — the id-filtered replay skips events the client already
        // saw. Committing the legacy bucket in that state (empty — events
        // staged into the lost flight, not the bucket) wipes live prompts,
        // and recording the marker as evidence would green-light the
        // stuck-session auto-abort. Skip both; pending state is kept and the
        // next flight rebuilds authoritatively.
        if (flightId !== "" && !knownFlight) {
          return;
        }

        setInputSnapshots((prev) => {
          const next = new Map(prev);
          next.set(wsId, { ok, at: Date.now() });
          return next;
        });

        // Take this flight's staging (per-flight when snapshot_id is present;
        // the legacy bucket otherwise).
        let staged: Map<string, string> | undefined;
        if (knownFlight && openFlights) {
          staged = openFlights.get(flightId);
          openFlights.delete(flightId);
          if (openFlights.size === 0) {
            flightsRef.current.delete(wsId);
          }
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

        // #1824 r11: resolve the staged ids at COMMIT time. The staging
        // map holds the raw event id; the additive pair may have been
        // learned while the flight was open. Committing the raw id would
        // resurrect the harness key and rewrite pendingActionWsRef to it.
        const wsSessionIds = new Set<string>();
        for (const [sessionId, wid] of pendingActionWsRef.current) {
          if (wid === wsId) wsSessionIds.add(sessionId);
        }
        const resolvedStaged: Array<[string, string]> = [];
        if (staged) {
          for (const [requestId, sessionId] of staged) {
            // #1365: the flight's staged set can carry an ask the user
            // resolved while the flight was in flight (fetch-before-click,
            // commit-after-clear). A tombstoned id stays cleared.
            if (resolvedTombstonesRef.current.has(requestId)) continue;
            resolvedStaged.push([
              requestId,
              resolveSessionAlias(wsId, sessionId) ?? sessionId,
            ]);
          }
        }

        // Ref bookkeeping stays OUTSIDE the updater (React 19 StrictMode
        // double-invokes updaters; a ref write inside one makes the second
        // invocation disagree with the first).
        for (const [requestId, sessionId] of resolvedStaged) {
          requestToSessionRef.current.set(requestId, sessionId);
          pendingActionWsRef.current.set(sessionId, wsId);
          registerFirstSeen(requestFirstSeenRef.current, requestId);
        }

        setPendingActions((prev) => {
          const next = new Map(prev);
          for (const sessionId of wsSessionIds) next.delete(sessionId);
          for (const [requestId, sessionId] of resolvedStaged) {
            const existing = next.get(sessionId);
            const set = new Set(existing ?? []);
            set.add(requestId);
            next.set(sessionId, set);
          }
          return next;
        });

        // Prune content maps for dropped requestIds via the setters (React state).
        if (doomedRequestIds.length > 0) {
          for (const rid of doomedRequestIds) {
            requestToSessionRef.current.delete(rid);
            // Absence, NOT resolution: a successful-but-stale projection
            // can omit a live ask (the incident's own failure mode), so no
            // tombstone here — a later flight that re-carries the id re-adds
            // it. Ghost pills left behind clear on click (every reply path
            // emits the resolved event post-#1365).
            requestFirstSeenRef.current.delete(rid);
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

      // ===== #1821 piece 2: the busy-snapshot flight =====
      //
      // D9/D10, the input-snapshot discipline applied to busy: begin
      // opens a per-flight staging window, the snapshot frame stages
      // the authority's rows + cursor, complete commits (ok=true) or
      // keeps state (ok=false — a failed authority read must never
      // read as an authoritative empty set).
      if (evt.type === "session.status.snapshot_begin") {
        const wsId = evt.workspace_id;
        if (!wsId) return;
        const flightId = evt.snapshot_id ?? "";
        let open = busyFlightsRef.current.get(wsId);
        if (!open) {
          open = new Map();
          busyFlightsRef.current.set(wsId, open);
        }
        open.set(flightId, { busyRowIds: new Set(), cursor: 0, degradedBusyIds: new Set(), rowInternalIds: new Map(), idleRowIds: new Set(), answeredIds: new Set() });
        return;
      }

      if (evt.type === "session.status.snapshot") {
        const wsId = evt.workspace_id;
        const flightId = evt.snapshot_id ?? "";
        // Stage into the named flight only: a snapshot frame with no
        // open flight had its begin dropped (broker backpressure) — its
        // complete is non-authoritative by the same rule the input
        // snapshots apply to unknown flight ids.
        const flight = wsId ? busyFlightsRef.current.get(wsId)?.get(flightId) : undefined;
        if (!wsId || !flight) return;
        const payload = (rawEvt as { data?: { cursor?: number; sessions?: Array<{ session_id?: string; internal_session_id?: string; derived_busy?: boolean; session_id_deprecated?: boolean }> } }).data;
        if (!payload || !Array.isArray(payload.sessions)) return;
        for (const row of payload.sessions) {
          if (!row.session_id) continue;
          {
            const answered = translateSessionIds(wsId, row.session_id, row.internal_session_id || undefined).sessionId;
            if (answered) flight.answeredIds.add(answered);
            // #1883: a RESOLVED idle row is the authority's explicit
            // "this session is not busy" answer — stage it for the
            // commit's cache mirror. A degraded row (Register failed
            // server-side, flagged deprecated, unresolved to a phrase)
            // stages nothing: its id join is partial and no
            // phrase-keyed cache row can match it anyway.
            if (!row.derived_busy && answered && !(row.session_id_deprecated && answered === row.session_id)) {
              flight.idleRowIds.add(answered);
            }
          }
          if (!row.derived_busy) continue;
          // #1848 r0 finding 5: a DEGRADED row (Register failed
          // server-side) carries the harness id on the wire. Run every
          // row through the alias seam — a healthy row is already the
          // phrase (identity, and the additive pair teaches the table);
          // a degraded row resolves through a learned pair or stays
          // harness-keyed (never worse).
          const pair = translateSessionIds(wsId, row.session_id, row.internal_session_id || undefined);
          if (pair.sessionId) {
            flight.busyRowIds.add(pair.sessionId);
            // The harness identity of the row: the additive internal id
            // on a healthy (phrase-keyed) row; the row key itself on a
            // degraded one.
            const harness = row.internal_session_id || row.session_id;
            if (harness) flight.rowInternalIds.set(pair.sessionId, harness);
          }
          // #1848 r1/r2 finding 4: a row that degraded AND could not
          // be healed client-side leaves the flight's id join PARTIAL —
          // remember the harness id so the commit blocks removal of
          // exactly the entries it may correspond to (never the whole
          // workspace's removals).
          if (row.session_id_deprecated && pair.sessionId === row.session_id) {
            flight.degradedBusyIds.add(row.session_id);
          }
        }
        flight.cursor = payload.cursor ?? 0;
        return;
      }

      if (evt.type === "session.status.snapshot_complete") {
        const wsId = evt.workspace_id;
        if (!wsId) return;
        const ok = rawEvt.snapshot_ok !== false;
        const flightId = evt.snapshot_id ?? "";
        const open = busyFlightsRef.current.get(wsId);
        const staged = open?.get(flightId);
        if (open) {
          open.delete(flightId);
          if (open.size === 0) busyFlightsRef.current.delete(wsId);
        }

        // D10: ok=false is NON-authoritative in the strongest sense:
        // latched busy stays, AND busy the last wipe cleared RE-LATCHES
        // as unknown (#1848 r3 finding 5 — the charter's "hold unknown,
        // not idle" across a post-reconnect authority failure), and ONE
        // authority retry per lease window is scheduled. A DROPPED
        // flight (staged missing — begin/snapshot lost to backpressure,
        // or invalidated by a terminal event) schedules nothing: there
        // is no evidence the authority answered (#1848 r3 finding 4).
        if (!ok) {
          // #1848 r4 (finding 4, late-complete half): a late ok=false
          // for a TERMINAL workspace (its begin predates agent_died or
          // a non-Active flip) must arm nothing — the sweep would POST
          // every window for a workspace that can never answer.
          if (terminalWsRef.current.has(wsId)) return;
          const now = stampBusyNow();
          const wiped = wipedBusyRef.current.get(wsId);
          if (wiped && wiped.size > 0) {
            // Unknown-hold: the wipe's memory re-latches, its lease
            // freshly stamped — the LEASE now owns the recovery cadence
            // (one recheck per window); no parallel pending recheck.
            setBusySessions((prev) => {
              const next = new Map(prev);
              for (const [sid, entry] of wiped) {
                if (!next.has(sid)) {
                  next.set(sid, { ...entry, confirmedAt: now });
                }
              }
              return next;
            });
            pendingRecheckRef.current.delete(wsId);
          } else {
            // Nothing to hold (no wipe memory): the pending recheck
            // owns the retry — one per lease window.
            pendingRecheckRef.current.set(wsId, now + BUSY_LEASE_MS);
          }
          setRecheckTick((t) => t + 1);
          return;
        }
        if (!staged) {
          return;
        }
        // An authoritative answer landed — the wipe's memory and any
        // pending retry are answered.
        wipedBusyRef.current.delete(wsId);
        pendingRecheckRef.current.delete(wsId);

        // The workspace is now seeded from a better source than the
        // REST overlay; the overlay's later add-only pass is harmless.
        seededRef.current.add(wsId);

        // #1821 piece 3: the cursor subsumes every delta at or below
        // it — record THIS workspace's subsumption bound.
        wsSnapshotCursorRef.current.set(wsId, Math.max(wsSnapshotCursorRef.current.get(wsId) ?? 0, staged.cursor));

        const cursor = staged.cursor;
        const busyRowIds = staged.busyRowIds;
        const degradedBusyIds = staged.degradedBusyIds;
        const rowInternalIds = staged.rowInternalIds;
        const now = stampBusyNow();
        // #1848 r3 finding 1: the reconcile runs INSIDE the updater
        // against its fresh `prev` — SSE frames dispatch synchronously
        // per chunk, so a render-closure read of busySessions is stale
        // within the chunk (a same-chunk busy delta had to be visible
        // to an idle commit's removal decision, and a same-chunk
        // re-confirm could be deleted by a decision made against the
        // pre-chunk state). Every ref write below is an IDEMPOTENT
        // max-stamp, so React 19 StrictMode's double-invocation is
        // harmless (the #1824 r11 hazard is non-idempotent
        // move/delete writes).
        setBusySessions((prev) => {
          const next = new Map(prev);
          // Authoritative removal: entries of THIS workspace whose
          // confirming evidence the cursor supersedes (ordered confirm
          // ≤ cursor — REST seeds at 0 included) are governed by the
          // rows, EXCEPT those a degraded busy row may correspond to.
          for (const [sid, entry] of prev) {
            if (entry.workspaceId !== wsId) continue;
            if (entry.confirmEventId === undefined || entry.confirmEventId > cursor) continue;
            if (busyRowIds.has(sid)) continue;
            if (degradedBusyIds.size > 0 && !staged.answeredIds.has(sid)) {
              // #1848 r2 finding 4, v2 (#1848 r3 finding 2): block
              // ONLY entries the degraded row could BE. A RESOLVED
              // answer under the entry's own key (answeredIds) proves
              // the authority spoke for THIS session — clearable. An
              // unresolved entry falls back to identity matching: its
              // own harness id, or the REVERSE alias of its phrase
              // (REST seeds carry no harness id); no identity at all →
              // protected (cannot disprove).
              const harness = entry.internalId ?? reverseSessionAlias(wsId, sid);
              if (harness === undefined || degradedBusyIds.has(harness)) continue;
            }
            next.delete(sid);
            // Idempotent max-stamp: the authority answered this session
            // (idle) at the cursor.
            sessionLastDeltaIDRef.current.set(sid, Math.max(sessionLastDeltaIDRef.current.get(sid) ?? 0, cursor));
          }
          // Every ANSWERED session's ordering watermark moves to the
          // cursor: the authority's row (busy or idle) is the newest
          // per-session truth at this id — an older overlapping flight's
          // row for the same session cannot latch over it, and an
          // in-flight per-session delta older than the cursor is
          // subsumed.
          for (const sid of staged.answeredIds) {
            const last = sessionLastDeltaIDRef.current.get(sid) ?? 0;
            if (cursor > last) sessionLastDeltaIDRef.current.set(sid, cursor);
          }
          // Busy rows latch or re-confirm: any busy-bearing snapshot
          // row refreshes the lease (piece 4). The confirm id is ALWAYS
          // a number here (cursor 0 included) — a cursor-0 snapshot is
          // ordering evidence, not an unorderable legacy frame.
          //
          // #1848 r2 findings 3+5: a row only latches when no NEWER
          // per-session delta supersedes it (an idle delta that landed
          // during the flight, or an overlapping newer flight's commit)
          // — per-entry guards replace the wholesale stale-flight
          // discard, so an unrelated session's delta can no longer
          // starve the recheck's rows for the sessions it knew nothing
          // about.
          for (const sid of busyRowIds) {
            if ((sessionLastDeltaIDRef.current.get(sid) ?? 0) > cursor) continue;
            // #1848 r4 (finding 3, client half): the row's HARNESS id
            // may carry a NEWER answer the phrase key never saw — a
            // degraded idle row (harness-keyed, no learnable pair)
            // stamps only the harness watermark. Check both keys, or
            // an older overlapping busy flight re-latches what the
            // newer idle answer already cleared.
            const rowHarness = rowInternalIds.get(sid);
            if (rowHarness !== undefined && (sessionLastDeltaIDRef.current.get(rowHarness) ?? 0) > cursor) continue;
            sessionLastDeltaIDRef.current.set(sid, cursor);
            const existing = prev.get(sid);
            const confirmEventId = Math.max(existing?.confirmEventId ?? 0, cursor);
            next.set(sid, {
              workspaceId: wsId,
              confirmedAt: now,
              confirmEventId,
              internalId: rowInternalIds.get(sid) ?? existing?.internalId,
            });
          }
          return next;
        });

        // Mirror the busy latch's cache flip so sidebar rows read
        // active for snapshot-busy sessions until the next refetch.
        const sessionsKey = ["sessions", wsId];
        const existingCache = queryClient.getQueryData(sessionsKey);
        if (existingCache && busyRowIds.size > 0) {
          queryClient.setQueryData(sessionsKey, (old: unknown) => {
            if (!Array.isArray(old)) return old;
            return old.map((s: Record<string, unknown>) =>
              busyRowIds.has(String(s.id)) ? { ...s, status: "active" } : s
            );
          });
        }
        // #1883: mirror the IDLE answers too — the symmetric half the
        // busy mirror lacked. The busy delta path stamps the row
        // "active" when it latches; when the authority's flight then
        // answers idle (the lost-idle shape the lease exists for), the
        // row must return to "idle" with the badge. Without this the
        // stale "active" projection survives the commit, and seedBusy's
        // mount-time pass (add-only) re-latches the cleared badge from
        // it on any provider remount — the frozen indicator returning
        // after the authority already healed it. The per-session
        // watermark guard is the same discipline the busy-row latch
        // applies: a row superseded by a NEWER same-session delta
        // (delta id > cursor, stamped at dispatch, before this read)
        // keeps the fresher answer. No hasUnread is invented here —
        // the flight is truth reconciliation, not turn-completion UX
        // (the unread flip belongs to the idle delta the user's stream
        // actually delivers).
        const idleRowIds = staged.idleRowIds;
        if (existingCache && idleRowIds.size > 0) {
          queryClient.setQueryData(sessionsKey, (old: unknown) => {
            if (!Array.isArray(old)) return old;
            return old.map((s: Record<string, unknown>) =>
              idleRowIds.has(String(s.id)) && (sessionLastDeltaIDRef.current.get(String(s.id)) ?? 0) <= cursor
                ? { ...s, status: "idle" }
                : s
            );
          });
        }
        return;
      }

      // ===== #1821 piece 3: EventID-ordered deltas =====
      //
      // The broker stamps every published event with a monotonic
      // EventID but delivers after releasing the shard lock, so two
      // concurrent publishers can deliver id 100 before 99 to the same
      // subscriber (#1848 r0 finding 3) — including the two halves of
      // ONE session's transition (idle 100 before busy 99, #1848 r2
      // finding 3). Ordering is therefore enforced at the finest grain
      // that exists: PER-SESSION. A delta applies only when its id
      // exceeds the last evidence id applied for THAT session (and is
      // not subsumed by its workspace's snapshot cursor). Deltas for
      // sessions nobody superseded always apply, in any arrival order.
      if (evt.type === "session.status" && evt.session_id && evt.workspace_id) {
        const deltaID = rawEvt.event_id ?? 0;
        if (deltaID > 0) {
          // #1848 r1 finding 1: the subsumption bound is THIS delta's
          // workspace cursor — another workspace's snapshot never
          // subsumes this evidence.
          const wsCursor = wsSnapshotCursorRef.current.get(evt.workspace_id) ?? 0;
          // #1848 r2 finding 3: the ordering bound is THIS session's
          // last applied evidence — an older transition for the same
          // session (delivered late) can never overwrite a newer one.
          const sessionWatermark = sessionLastDeltaIDRef.current.get(evt.session_id) ?? 0;
          if (deltaID <= wsCursor || deltaID <= sessionWatermark) {
            return;
          }
          sessionLastDeltaIDRef.current.set(evt.session_id, deltaID);
        }
        if (evt.status === "busy") {
          clearedRef.current.delete(evt.session_id);
          // #1848 r5 findings 2+3: fresh live evidence supersedes the
          // wipe's memory (an ok=false can no longer resurrect this
          // session) and hands the recheck cadence to the LEASE — the
          // pending-recheck path exists only for no-evidence workspaces.
          wipedBusyRef.current.get(evt.workspace_id)?.delete(evt.session_id);
          pendingRecheckRef.current.delete(evt.workspace_id);
          // #786: a new turn un-aborts — the interrupted marker dies
          // with the session's next activity.
          setAbortedSessions((prev) => {
            if (!prev.has(evt.session_id!)) return prev;
            const next = new Set(prev);
            next.delete(evt.session_id!);
            return next;
          });
          setBusySessions((prev) => {
            const next = new Map(prev);
            // #1821 piece 4: the delta is confirming evidence — the
            // lease stamps now; the orderable delta id lets a later
            // snapshot commit recognize this entry as fresher-than-
            // cursor when the flight raced the delta.
            next.set(evt.session_id!, {
              workspaceId: evt.workspace_id!,
              confirmedAt: stampBusyNow(),
              confirmEventId: deltaID > 0 ? deltaID : undefined,
              internalId: eventInternalSessionId(rawEvt),
            });
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
        } else if (evt.status === "aborted" || evt.status === "deleted") {
          // #786: aborted (force-stop) and deleted were silently dropped
          // here — the busy indicator kept stale state until a hard
          // refresh. Both end every in-flight thing for the session;
          // neither carries a response, so neither marks unread.
          const sid = evt.session_id!;
          const wsId = evt.workspace_id!;
          // #1848 r5 finding 2: aborted/deleted ends the session —
          // prune the wipe memory like the idle delta does.
          wipedBusyRef.current.get(wsId)?.delete(sid);
          // A stopped/deleted session can never emit the idle clear —
          // the hang is over by definition (and deleting a stuck
          // session is precisely what a user does with a hung one).
          dropHungWorkspace(wsId);
          clearedRef.current.delete(sid);
          // Collect the session's own ask ids BEFORE clearing the
          // indicator — the fold-sync reconcile (snapshot_complete)
          // walks pendingActions, so clearing first would orphan the
          // content maps behind a blinded prune path (r2 review
          // finding: stale un-dismissable pills in parent tabs).
          const doomed = new Set<string>();
          for (const rid of pendingActions.get(sid) ?? []) doomed.add(rid);
          const collectSessionAsks = (m: Map<string, StoredInputRequest>) => {
            for (const v of m.values()) {
              if (v.sessionId === sid) doomed.add(v.id);
            }
          };
          collectSessionAsks(pendingQuestionContent);
          collectSessionAsks(pendingPermissionContent);
          if (doomed.size > 0) {
            setPendingQuestionContent((prev) => pruneMany(prev, doomed));
            setPendingPermissionContent((prev) => pruneMany(prev, doomed));
            for (const rid of doomed) {
              requestToSessionRef.current.delete(rid);
              requestFirstSeenRef.current.delete(rid);
            }
          }
          setBusySessions((prev) => {
            if (!prev.has(sid)) return prev;
            const next = new Map(prev);
            next.delete(sid);
            return next;
          });
          setPendingActions((prev) => {
            if (!prev.has(sid)) return prev;
            const next = new Map(prev);
            next.delete(sid);
            return next;
          });
          if (evt.status === "aborted") {
            // The interrupted marker survives until a new turn starts —
            // the sidebar renders "was interrupted", not plain idle.
            setAbortedSessions((prev) => (prev.has(sid) ? prev : new Set(prev).add(sid)));
            const sessionsKey = ["sessions", wsId];
            const existing = queryClient.getQueryData(sessionsKey);
            if (existing) {
              queryClient.setQueryData(sessionsKey, (old: unknown) => {
                if (!Array.isArray(old)) return old;
                return old.map((s: Record<string, unknown>) =>
                  s.id === sid ? { ...s, status: "aborted" } : s
                );
              });
            }
          } else {
            // The session is gone: drop every trace — unread, the
            // aborted marker, the sessions cache row, and the
            // per-session query caches (messages, title, context) —
            // and leave the route if the user is viewing it.
            setAbortedSessions((prev) => {
              if (!prev.has(sid)) return prev;
              const next = new Set(prev);
              next.delete(sid);
              return next;
            });
            setPendingUnread((prev) => {
              if (!prev.has(sid)) return prev;
              const next = new Map(prev);
              next.delete(sid);
              return next;
            });
            const sessionsKey = ["sessions", wsId];
            const existing = queryClient.getQueryData(sessionsKey);
            if (existing) {
              queryClient.setQueryData(sessionsKey, (old: unknown) => {
                if (!Array.isArray(old)) return old;
                return old.filter((s: Record<string, unknown>) => s.id !== sid);
              });
            }
            queryClient.removeQueries({
              predicate: (q) => Array.isArray(q.queryKey) && q.queryKey.includes(sid),
            });
            setDeletedSessions((prev) => (prev.has(sid) ? prev : new Set(prev).add(sid)));
          }
        } else if (evt.status === "idle") {
          // #1848 r5 finding 2: a post-wipe idle delta is
          // authoritative evidence the session recovered — prune the
          // wipe memory so a later ok=false cannot resurrect it.
          wipedBusyRef.current.get(evt.workspace_id)?.delete(evt.session_id);
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

        // #1627: archive/unarchive transitions arrive on the same event
        // type — flip the cached flag in place so every open tab moves
        // the session into/out of the Archived group without a refetch.
        if (evt.status === "archived" || evt.status === "unarchived") {
          const sessionsKey = ["sessions", evt.workspace_id];
          const archived = evt.status === "archived";
          const existing = queryClient.getQueryData(sessionsKey);
          if (existing) {
            queryClient.setQueryData(sessionsKey, (old: unknown) => {
              if (!Array.isArray(old)) return old;
              return old.map((s: Record<string, unknown>) => {
                if (s.id !== evt.session_id) return s;
                if (archived) return { ...s, archived: true };
                // Unarchive restores the ABSENT-means-not-archived wire
                // contract — delete the key rather than parking an
                // explicit undefined on the cached row.
                const next = { ...s };
                delete next.archived;
                return next;
              });
            });
          }
        }
      }

      if (evt.type === "agent_died" && evt.workspace_id) {
        const wsId = evt.workspace_id;
        // #1848 r1 finding 5: the agent is dead — any in-flight
        // snapshot for this workspace fetched from a corpse or raced
        // its death; its commit could re-latch busy nothing can clear
        // (the recheck answers ok=false forever on a dead pod).
        busyFlightsRef.current.delete(wsId);
        // #1848 r3 finding 4: terminal state — no recheck, no
        // unknown-hold memory, no retry loop for a dead workspace.
        pendingRecheckRef.current.delete(wsId);
        wipedBusyRef.current.delete(wsId);
        // #1848 r4: mark terminal so a LATE ok=false complete (its
        // begin predates the death) cannot re-arm the loop.
        terminalWsRef.current.add(wsId);
        setBusySessions((prev) => {
          const next = new Map();
          for (const [sid, entry] of prev) {
            if (entry.workspaceId !== wsId) next.set(sid, entry);
          }
          return next;
        });
        // The hang is definitionally over when the agent dies.
        dropHungWorkspace(wsId);
        clearWorkspacePendingActions(wsId);
      }

      // resync: the broker dropped events for this subscriber (channel
      // backpressure). Snapshot flight state is unreliable — a begin may have
      // been dropped while its complete arrives. Clear the commit gate and
      // any open flights so subsequent events stage again (legacy rule) and
      // the next flight rebuilds authoritatively. Recorded snapshot evidence
      // is also unreliable (its staging may have been dropped) — clear it so
      // the stuck-session gate cannot lean on pre-resync proof.
      if (evt.type === "resync") {
        // #1687 (pin 1): the dropped set may include the
        // session.status=idle that would have cleared a latched busy
        // badge — resync is admitted event loss on a HEALTHY connection
        // (no onReconnect will fire), so run the same reconciliation as
        // onReconnect: wipe busy, reopen the seed gate, and re-derive
        // from REST. Ordering (#1646 r5): the wipe and its wipeAt stamp
        // MUST land BEFORE the sessions invalidation dispatches its
        // refetches, so every re-seed's data birth postdates the wipe —
        // mirror of the onConnect ordering in useUserEventStream.
        //
        // #1848 r1: the re-seed also rides the AUTHORITY path — one
        // status-snapshot flight per workspace that had latched busy
        // (the same surface the lease recheck uses) so resync converges
        // via the snapshot, not only the REST overlay. Capture the
        // pre-wipe busy workspaces BEFORE the wipe clears them.
        // #1848 r4 (finding 1, the resync read): capture the pre-wipe
        // busy workspaces INSIDE the updater queue, BEFORE the wipe's
        // own updater — one chunk's frames dispatch against the same
        // render closure, so a busy delta earlier in THIS chunk is
        // invisible to a closure read here and its workspace would
        // never be re-checked (reads idle until a manual refresh). The
        // queued capture sees the chunk's own state; idempotent
        // set-adds keep StrictMode's double-invocation harmless. The
        // sweep (post-render, refs settled) fires the requests.
        setBusySessions((prev) => {
          for (const entry of prev.values()) {
            resyncRecheckRef.current.add(entry.workspaceId);
          }
          return prev;
        });
        wipeBusyForEventLoss(wipeAtRef, seededRef, setBusySessions, wipedBusyRef);
        queryClient.invalidateQueries({ queryKey: ["sessions"] });
        committedWsRef.current.clear();
        flightsRef.current.clear();
        legacyStagingRef.current.clear();
        setInputSnapshots(new Map());
        // #1821 piece 2: the dropped set may include a busy flight's
        // begin or snapshot frame — open flights are unreliable, and
        // the reconnect-path flights rebuild authoritatively.
        busyFlightsRef.current.clear();
        // #1848 r1 finding 3: resync is admitted event loss on a
        // HEALTHY connection, but the broker's id space is per-replica
        // — reset the ordering bounds so post-resync flights and deltas
        // (possibly from another replica) are not suppressed by stale
        // high-waters.
        wsSnapshotCursorRef.current.clear();
        sessionLastDeltaIDRef.current.clear();
        // #1848 r4: the captured rechecks fire from the sweep, after
        // the capture updater has settled (bump re-arms the effect).
        setRecheckTick((t) => t + 1);
        // Events were dropped for this subscriber — possibly including
        // the idle that would have cleared a hung badge. Full clear,
        // like reconnect; the gated seed re-adds genuine hangs.
        setHungWorkspaces(new Set());
        return;
      }

      if (evt.type === "workspace.phase" && evt.workspace_id && evt.phase) {
        const wsId = evt.workspace_id;
        if (NON_ACTIVE_PHASES.has(evt.phase)) {
          seededRef.current.delete(wsId);
          // #1848 r1 finding 5: a flight that fetched while Active and
          // completes after the phase flip would re-latch busy for a
          // workspace whose recheck always answers ok=false (the phase
          // gate) — invalidate the workspace's open flights.
          busyFlightsRef.current.delete(wsId);
          // #1848 r3 finding 4: same terminal cleanup as agent_died.
          pendingRecheckRef.current.delete(wsId);
          wipedBusyRef.current.delete(wsId);
          // #1848 r4: terminal — a late ok=false cannot re-arm.
          terminalWsRef.current.add(wsId);
          setBusySessions((prev) => {
            const next = new Map();
            for (const [sid, entry] of prev) {
              if (entry.workspaceId !== wsId) next.set(sid, entry);
            }
            return next;
          });
          // No further SSE can arrive for a non-active workspace, so
          // the hung badge's only clear path would never fire — drop
          // it with the rest of the per-workspace state.
          dropHungWorkspace(wsId);
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
          // #1848 r4: Active re-arms normal snapshot service (e.g. a
          // suspended workspace resumed).
          terminalWsRef.current.delete(wsId);
        }
      }

      if (evt.type && !KNOWN_EVENT_TYPES.has(evt.type)) {
        console.debug("[SessionActivityProvider] unhandled SSE event type:", evt.type);
      }
    },
  });

  // #1821 piece 4: the busy lease sweep. Busy entries whose confirming
  // evidence is older than BUSY_LEASE_MS trigger exactly ONE
  // authoritative recheck per lease window — a session-status-snapshot
  // flight per workspace (one flight covers every expired session of
  // that workspace; the D10 marker on the resulting flight decides
  // keep-vs-clear). The lease re-arms at trigger time, so an
  // unreachable authority (request rejects, no flight) holds the badge
  // as unknown and retries once per window — never a hammering loop,
  // never a self-made clear.
  //
  // #1848 r2 finding 6: pending post-wipe rechecks (an ok=false flight
  // answered a wipe-initiated re-seed) ride the same cadence — one
  // request per lease window per workspace until an authoritative
  // answer lands.
  useEffect(() => {
    if (busySessions.size === 0 && pendingRecheckRef.current.size === 0 && resyncRecheckRef.current.size === 0) return;
    const sweep = () => {
      const now = Date.now();
      // #1848 r4: resync-captured rechecks fire here — the sweep runs
      // post-render, after the capture updater has settled. One-shot
      // per resync, like the connect-path recheck: the resulting
      // flight's markers decide keep-vs-clear.
      const resyncRechecks: string[] = [];
      for (const wsId of resyncRecheckRef.current) {
        resyncRecheckRef.current.delete(wsId);
        resyncRechecks.push(wsId);
      }
      const expiredWorkspaces = new Set<string>();
      for (const entry of busySessions.values()) {
        if (now - entry.confirmedAt > BUSY_LEASE_MS) {
          expiredWorkspaces.add(entry.workspaceId);
        }
      }
      const dueRechecks: string[] = [];
      for (const [wsId, at] of pendingRecheckRef.current) {
        if (now >= at && !expiredWorkspaces.has(wsId)) dueRechecks.push(wsId);
      }
      if (expiredWorkspaces.size === 0 && dueRechecks.length === 0 && resyncRechecks.length === 0) return;
      for (const wsId of resyncRechecks) {
        sessionsApi.requestStatusSnapshot(wsId).catch(() => {});
      }
      for (const wsId of expiredWorkspaces) {
        sessionsApi.requestStatusSnapshot(wsId).catch(() => {
          // Unknown, not idle: the badge holds; the re-armed lease
          // retries once per window.
        });
      }
      for (const wsId of dueRechecks) {
        sessionsApi.requestStatusSnapshot(wsId).catch(() => {});
        // One request per lease window; a subsequent ok=false
        // re-schedules, an authoritative answer clears the entry.
        pendingRecheckRef.current.set(wsId, now + BUSY_LEASE_MS);
      }
      // Re-arm at trigger (outside any updater — Date.now/refs in
      // render-analyzed bodies stay pure; this is an event-callback
      // context like wipeBusyForEventLoss).
      setBusySessions((prev) => {
        let next: Map<string, BusyEntry> | null = null;
        for (const [sid, entry] of prev) {
          if (now - entry.confirmedAt > BUSY_LEASE_MS) {
            if (!next) next = new Map(prev);
            next.set(sid, { ...entry, confirmedAt: now });
          }
        }
        return next ?? prev;
      });
    };
    sweep();
    const t = setInterval(sweep, BUSY_LEASE_SWEEP_MS);
    return () => clearInterval(t);
  }, [busySessions, recheckTick]);

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
      for (const entry of busySessions.values()) {
        if (entry.workspaceId === workspaceId) count++;
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

  // #1687 (pin 2): the contract stream's SESSION_STATUS IDLE clears the
  // busy map exactly like the user stream's session.status=idle frame.
  // Before this bridge the user-stream frame was the ONLY clear path —
  // lose it to a broker drop or a reconnect gap and the badge latched
  // until manual stop (and the send gate kept rerouting messages into
  // the queue). One stream reporting the turn's end must be enough.
  const clearSessionBusy = useCallback((workspaceId: string, sessionId: string) => {
    setBusySessions((prev) => {
      if (!prev.has(sessionId)) return prev;
      const next = new Map(prev);
      next.delete(sessionId);
      return next;
    });
    // Mirror the user-stream idle row flip so the sessions cache does
    // not keep the busy frame's "active" status until the next refetch.
    const sessionsKey = ["sessions", workspaceId];
    const existing = queryClient.getQueryData(sessionsKey);
    if (existing) {
      queryClient.setQueryData(sessionsKey, (old: unknown) => {
        if (!Array.isArray(old)) return old;
        return old.map((s: Record<string, unknown>) =>
          s.id === sessionId ? { ...s, status: "idle" } : s
        );
      });
    }
    // The turn ending resolves any hung alert for the workspace — same
    // rule as the user-stream idle path.
    dropHungWorkspace(workspaceId);
  }, [queryClient, dropHungWorkspace]);

  const addPendingAction = useCallback((workspaceId: string, sessionId: string, requestId: string) => {
    // #1365: a tombstoned ask stays cleared — a racing flight's staged
    // event or a whileAway re-presentation must not resurrect it.
    if (resolvedTombstonesRef.current.has(requestId)) return;
    requestToSessionRef.current.set(requestId, sessionId);
    pendingActionWsRef.current.set(sessionId, workspaceId);
    setPendingActions((prev) => {
      const existing = prev.get(sessionId);
      if (existing?.has(requestId)) return prev;
      const next = new Map(prev);
      const set = new Set(existing ?? []);
      set.add(requestId);
      next.set(sessionId, set);
      // Monotonic seam (#1845): identical Date.now() within one fold
      // batch broke the newest-first sort — share registerFirstSeen so
      // batch-mates can never tie.
      registerFirstSeen(requestFirstSeenRef.current, requestId);
      return next;
    });
  }, []);

  // removeNow is the shared removal core — clears content, indicator, and
  // bookkeeping. Tombstoning is the caller's decision: resolution evidence
  // (removePendingAction) tombstones; absence evidence (dropPendingAction)
  // must not (#1365 review r1: a false tombstone permanently hides a live
  // ask, and stale projections do produce false absences).
  const removeNow = useCallback((requestId: string) => {
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
    requestFirstSeenRef.current.delete(requestId);
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

  const removePendingAction = useCallback((requestId: string) => {
    // The removal is resolution evidence — tombstone the id (#1365).
    tombstoneRequest(resolvedTombstonesRef.current, requestId);
    removeNow(requestId);
  }, [removeNow]);

  // dropPendingAction removes on ABSENCE evidence (the fold/snapshot no
  // longer carries the ask) without tombstoning — a later re-presentation
  // of the same id may be a live ask the stale projection briefly lacked.
  const dropPendingAction = useCallback((requestId: string) => {
    removeNow(requestId);
  }, [removeNow]);

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
    for (const rid of doomedRequests) {
      requestToSessionRef.current.delete(rid);
      requestFirstSeenRef.current.delete(rid);
    }
  }, [pendingActions]);

  const addPendingQuestion = useCallback((workspaceId: string, req: InputRequest) => {
    if (resolvedTombstonesRef.current.has(req.id)) return;
    addPendingAction(workspaceId, req.sessionId ?? "", req.id);
    setPendingQuestionContent((prev) => {
      if (prev.has(req.id)) return prev;
      const next = new Map(prev);
      next.set(req.id, { ...req, receivedAt: Date.now() });
      return next;
    });
  }, [addPendingAction]);

  const addPendingPermission = useCallback((workspaceId: string, req: InputRequest) => {
    if (resolvedTombstonesRef.current.has(req.id)) return;
    addPendingAction(workspaceId, req.sessionId ?? "", req.id);
    setPendingPermissionContent((prev) => {
      if (prev.has(req.id)) return prev;
      const next = new Map(prev);
      next.set(req.id, { ...req, receivedAt: Date.now() });
      return next;
    });
  }, [addPendingAction]);

  // pendingQuestionsForSession matches both the owning session and any session
  // whose root_session_id points here, so subtask prompts bubble to the parent
  // view (same rule ChatPage previously applied at write time — now applied at
  // read time so the content is stored regardless of the currently-viewed session).
  // #1365: the stack renders newest-activity-first — a stale pill cannot
  // shadow the live ask the user needs to answer.
  const pendingQuestionsForSession = useCallback((sessionId: string): StoredInputRequest[] => {
    const out: InputRequest[] = [];
    for (const q of pendingQuestionContent.values()) {
      const root = q.rootSessionId ?? q.sessionId ?? "";
      if (root === sessionId || q.sessionId === sessionId) out.push(q);
    }
    out.sort((a, b) => (requestFirstSeenRef.current.get(b.id) ?? 0) - (requestFirstSeenRef.current.get(a.id) ?? 0));
    return out;
  }, [pendingQuestionContent]);

  const pendingPermissionsForSession = useCallback((sessionId: string): StoredInputRequest[] => {
    const out: InputRequest[] = [];
    for (const p of pendingPermissionContent.values()) {
      const root = p.rootSessionId ?? p.sessionId ?? "";
      if (root === sessionId || p.sessionId === sessionId) out.push(p);
    }
    out.sort((a, b) => (requestFirstSeenRef.current.get(b.id) ?? 0) - (requestFirstSeenRef.current.get(a.id) ?? 0));
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
    const collect = (m: Map<string, InputRequest>) => {
      for (const v of m.values()) {
        if (v.sessionId === sessionId) doomed.add(v.id);
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
    for (const rid of doomed) {
      requestToSessionRef.current.delete(rid);
      requestFirstSeenRef.current.delete(rid);
    }
  }, [pendingActions, pendingQuestionContent, pendingPermissionContent]);

  const isSessionPendingAction = useCallback(
    (sessionId: string) => pendingActions.has(sessionId),
    [pendingActions]
  );

  const isSessionAborted = useCallback(
    (sessionId: string) => abortedSessions.has(sessionId),
    [abortedSessions]
  );

  const isSessionDeleted = useCallback(
    (sessionId: string) => deletedSessions.has(sessionId),
    [deletedSessions]
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
      value={{ isSessionBusy, isSessionUnread, workspaceBusyCount, hungWorkspaces, clearPendingUnread, clearSessionBusy, isSessionPendingAction, isSessionAborted, isSessionDeleted, pendingActionSessionIds, addPendingAction, removePendingAction, dropPendingAction, clearWorkspacePendingActions, addPendingQuestion, addPendingPermission, pendingQuestionsForSession, pendingPermissionsForSession, clearSessionPendingPrompts, workspaceInputSnapshot }}
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

// useWorkspaceHung (D6 #998): true while the workspace has an active
// hung-session alert (drives the sidebar badge).
export function useWorkspaceHung(workspaceId: string): boolean {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return false;
  return ctx.hungWorkspaces.has(workspaceId);
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

// useClearSessionBusy (#1687): the contract-idle bridge — clears the busy
// badge for one session from outside the user event stream.
export function useClearSessionBusy(): (workspaceId: string, sessionId: string) => void {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return () => {};
  return ctx.clearSessionBusy;
}

export function useIsSessionPendingAction(sessionId: string): boolean {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return false;
  return ctx.isSessionPendingAction(sessionId);
}

// useIsSessionAborted (#786): true after a force-stop until the
// session's next turn — drives the "was interrupted" indicator.
export function useIsSessionAborted(sessionId: string): boolean {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return false;
  return ctx.isSessionAborted(sessionId);
}

// useIsSessionDeleted (#786): true for sessions deleted while this tab
// was open — the route-level consumer navigates away from them.
export function useIsSessionDeleted(sessionId: string): boolean {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return false;
  return ctx.isSessionDeleted(sessionId);
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

// useDropPendingAction: absence-evidence removal — no tombstone (#1365).
export function useDropPendingAction(): (requestId: string) => void {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return () => {};
  return ctx.dropPendingAction;
}

export function useSessionPendingActions(): Set<string> {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return new Set();
  return ctx.pendingActionSessionIds;
}

export function useAddPendingQuestion(): (workspaceId: string, req: InputRequest) => void {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return () => {};
  return ctx.addPendingQuestion;
}

export function useAddPendingPermission(): (workspaceId: string, req: InputRequest) => void {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return () => {};
  return ctx.addPendingPermission;
}

export function usePendingQuestionsForSession(sessionId: string): StoredInputRequest[] {
  const ctx = useContext(SessionActivityContext);
  if (!ctx) return [];
  return ctx.pendingQuestionsForSession(sessionId);
}

export function usePendingPermissionsForSession(sessionId: string): StoredInputRequest[] {
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
