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

const MAX_ALIASES_PER_WORKSPACE = 512;

/** workspaceId → (harnessId → phraseId). Insertion-ordered for bounded
 * eviction; per-workspace so one workspace's ids can never resolve in
 * another. */
const aliasesByWorkspace = new Map<string, Map<string, string>>();

/** Teaches the seam one additive pair (wire = phrase, internal = harness).
 * Equal or empty ids teach nothing. The additive field is optional on every
 * emitter, so absence is normal, never an error. */
export function learnSessionAlias(
  workspaceId: string | undefined,
  wireSessionId: string | undefined,
  internalSessionId: string | undefined,
): void {
  if (!workspaceId || !wireSessionId || !internalSessionId) return;
  if (wireSessionId === internalSessionId) return;

  let table = aliasesByWorkspace.get(workspaceId);
  if (!table) {
    table = new Map();
    aliasesByWorkspace.set(workspaceId, table);
  }
  if (table.get(internalSessionId) === wireSessionId) return;

  table.delete(internalSessionId);
  table.set(internalSessionId, wireSessionId);
  if (table.size > MAX_ALIASES_PER_WORKSPACE) {
    const oldest = table.keys().next().value;
    if (oldest !== undefined) table.delete(oldest);
  }
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

/** Test hook: clears the process-wide alias map. */
export function resetSessionAliases(): void {
  aliasesByWorkspace.clear();
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
