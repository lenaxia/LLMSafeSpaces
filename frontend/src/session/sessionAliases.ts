// #1817 heal-window id translation (frontend).
//
// The platform has two session id spaces:
//   - the PHRASE (short id such as `quiet-mountain-9`) — the id the session
//     list, REST routes, and the chat URL use since the #1756/#1775/#1806
//     re-keys;
//   - the HARNESS id (such as `ses_...`) — opencode/agentd's internal id.
//
// A fixed API and a fixed agentd emit the phrase on the wire and preserve the
// harness id ADDITIVELY (`data.internal_session_id` on user-stream events,
// `internalSessionId` on contract frames). API and agentd are delivered
// independently (overlays), so during the heal window one side can still emit
// the pre-fix harness-keyed shape while the other already carries the additive
// id. Without a join the frontend keys the session twice: a harness-keyed busy
// never badges the phrase-keyed sidebar row, and harness-keyed contract frames
// never reach the phrase-keyed chat view.
//
// This module is the small, bounded translation seam: any additive pair
// (wireId = phrase, internalId = harness) teaches the per-workspace alias map;
// a later legacy harness-keyed frame resolves through it. A frame whose
// additive id EQUALS its wire id is not a pair — if that id is a known harness
// it is a stale/decoy frame masquerading as already-translated, and it is left
// alone so it cannot alias onto the phrase session.
//
// The seam never invents a mapping: absent an additive id it is the identity,
// which is exactly the pre-fix behavior (never worse).
//
// Scope limitation: learning happens wherever a translator observes the
// pair. The contract stream is mounted by ChatPage only; sidebar-only pages
// run just the user stream. With a pre-fix API (harness-keyed user events,
// no additive id) and a post-fix agentd (phrase-keyed contract rows that
// carry internalSessionId), a sidebar-only page has no source for the pair,
// so it cannot heal until a chat view has been opened once in this tab (the
// map is module-level and outlives the page). Healing sidebar-only pages
// requires hoisting the single contract-stream connection to the shell —
// not a second SSE connection — which is an architectural change outside
// this seam.

const MAX_ALIASES_PER_WORKSPACE = 512;
const MAX_WORKSPACES = 1024;
const MAX_SESSION_ID_LENGTH = 256;

/** workspaceId → (harnessId → phraseId). Insertion-ordered for bounded
 * eviction; per-workspace so one workspace's ids can never resolve in
 * another. */
const aliasesByWorkspace = new Map<string, Map<string, string>>();

/** Bumped on every learned/overwritten/evicted pair. Consumers compare it
 * against their last re-key to avoid O(all sessions) work on every frame. */
let aliasVersion = 0;
let rekeyCount = 0;
type SessionAliasListener = (workspaceId: string) => void;
const aliasListeners = new Set<SessionAliasListener>();

/** Monotonic generation of the alias map (test/observability hook). */
export function sessionAliasVersion(): number {
  return aliasVersion;
}

/** Subscribes to alias changes; receives the workspace that changed. The
 * returned function unsubscribes. Fired synchronously so a consumer can
 * re-key its own projection without waiting for another wire frame. */
export function subscribeSessionAliases(listener: SessionAliasListener): () => void {
  aliasListeners.add(listener);
  return () => {
    aliasListeners.delete(listener);
  };
}

/** Test/introspection hook: number of full re-key passes run. */
export function sessionAliasRekeyCount(): number {
  return rekeyCount;
}

/** Rejects ids that cannot be a session/workspace id: empty, overlong, or
 * whitespace/control characters (frame fields are untrusted input). */
function isBoundedId(value: string | undefined): value is string {
  if (typeof value !== "string" || value.length === 0 || value.length > MAX_SESSION_ID_LENGTH) {
    return false;
  }
  for (const ch of value) {
    const code = ch.codePointAt(0) ?? 0;
    if (/\s/.test(ch) || code < 0x20 || code === 0x7f) return false;
  }
  return true;
}

/** Teaches the seam one additive pair (wire = phrase, internal = harness).
 * Returns true when the map changed. Equal, empty, overlong, or malformed
 * ids teach nothing. The additive field is optional on every emitter, so
 * absence is normal, never an error. */
export function learnSessionAlias(
  workspaceId: string | undefined,
  wireSessionId: string | undefined,
  internalSessionId: string | undefined,
): boolean {
  if (
    !isBoundedId(workspaceId) ||
    !isBoundedId(wireSessionId) ||
    !isBoundedId(internalSessionId)
  ) {
    return false;
  }
  if (wireSessionId === internalSessionId) return false;

  let table = aliasesByWorkspace.get(workspaceId);
  if (!table) {
    if (aliasesByWorkspace.size >= MAX_WORKSPACES) {
      const oldestWorkspace = aliasesByWorkspace.keys().next().value;
      if (oldestWorkspace !== undefined) aliasesByWorkspace.delete(oldestWorkspace);
    }
    table = new Map();
    aliasesByWorkspace.set(workspaceId, table);
  }
  if (table.get(internalSessionId) === wireSessionId) return false;

  table.delete(internalSessionId);
  table.set(internalSessionId, wireSessionId);
  if (table.size > MAX_ALIASES_PER_WORKSPACE) {
    const oldest = table.keys().next().value;
    if (oldest !== undefined) table.delete(oldest);
  }

  aliasVersion++;
  for (const listener of Array.from(aliasListeners)) listener(workspaceId);
  return true;
}

/** Resolves a harness id to its phrase; unknown ids pass through. */
export function resolveSessionAlias(
  workspaceId: string | undefined,
  sessionId: string | undefined,
): string | undefined {
  if (!workspaceId || !sessionId) return sessionId;
  return aliasesByWorkspace.get(workspaceId)?.get(sessionId) ?? sessionId;
}

export interface SessionIdPair {
  sessionId: string | undefined;
  internalSessionId: string | undefined;
}

/**
 * Normalizes one observed id pair to the phrase-keyed wire shape.
 * - pair (wire != internal): already phrase-keyed; learns the alias.
 * - equal ids: not a pair; identity (never aliases a decoy onto the phrase).
 * - legacy (no internal): resolves the wire id through the learned aliases and
 *   carries the harness id additively, so downstream consumers never lose it.
 */
export function translateSessionIds(
  workspaceId: string | undefined,
  wireSessionId: string | undefined,
  internalSessionId: string | undefined,
): SessionIdPair {
  if (!wireSessionId) return { sessionId: wireSessionId, internalSessionId };

  if (internalSessionId) {
    learnSessionAlias(workspaceId, wireSessionId, internalSessionId);
    return { sessionId: wireSessionId, internalSessionId };
  }

  const phrase = resolveSessionAlias(workspaceId, wireSessionId);
  if (phrase !== wireSessionId) {
    return { sessionId: phrase, internalSessionId: wireSessionId };
  }
  return { sessionId: wireSessionId, internalSessionId };
}

/** Test hook: clears the process-wide alias map and its observers. The
 * version/counter stay monotonic (they only move forward for the process). */
export function resetSessionAliases(): void {
  aliasesByWorkspace.clear();
  aliasListeners.clear();
}

/** Reads the additive harness id off a user-stream event payload (both the
 * `data.internal_session_id` map shape and a defensive top-level field). */
export function eventInternalSessionId(evt: unknown): string | undefined {
  if (!evt || typeof evt !== "object") return undefined;
  const top = (evt as { internal_session_id?: unknown }).internal_session_id;
  if (typeof top === "string" && top) return top;
  const data = (evt as { data?: unknown }).data;
  if (data && typeof data === "object") {
    const nested = (data as { internal_session_id?: unknown }).internal_session_id;
    if (typeof nested === "string" && nested) return nested;
  }
  return undefined;
}

/** Test/introspection helper: whether a workspace has any learned aliases. */
export function hasSessionAliases(workspaceId: string): boolean {
  return (aliasesByWorkspace.get(workspaceId)?.size ?? 0) > 0;
}

/** Shape the fold's session map needs to participate in a late re-key. */
export interface AliasableSession {
  sessionId?: string;
  internalSessionId?: string;
}

/**
 * Re-keys fold entries learned before the pair was known. A legacy row can
 * land before the additive pair arrives (contract stream connects first);
 * without this it would stay stranded under the harness key even though
 * later events translate. Only legacy rows move: a row whose additive id
 * equals its key is the decoy/stale shape and is never aliased (the same
 * guard as translateSessionIds). An existing phrase entry wins — it is the
 * newer projection and events will update it.
 *
 * Returns true when the map changed (callers publish on true).
 */
export function rekeySessionMap(
  workspaceId: string | undefined,
  sessions: Map<string, AliasableSession>,
): boolean {
  rekeyCount++;
  let changed = false;
  for (const [key, snap] of Array.from(sessions.entries())) {
    if (!key) continue;
    if (snap.internalSessionId === key) continue;
    const phrase = resolveSessionAlias(workspaceId, key);
    if (!phrase || phrase === key) continue;
    if (sessions.has(phrase)) continue;
    sessions.delete(key);
    snap.sessionId = phrase;
    if (!snap.internalSessionId) snap.internalSessionId = key;
    sessions.set(phrase, snap);
    changed = true;
  }
  return changed;
}
