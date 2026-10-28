/**
 * #1966 worker-session classification, ruling 6 (2026-10-28 owner
 * batch): the pure signal that splits a workspace's live sessions
 * between the collapsed EXISTING "Workers" group and the main
 * user-facing list. Presentation-only — the classifier feeds
 * rendering, never the list query (search composes server-side over
 * the unchanged listing).
 *
 * Signal (ONE, by ruling): origin-class 'worker' — the spawn-path
 * provenance mark the board spawner records for every engine/board
 * fresh-worker spawn (implementers, reviewer personas, bounce
 * replacements), reaching the client ON the session list payload
 * (SessionListItem.origin, joined server-side from session_origins)
 * with the /session-origins merge as a compatibility fallback.
 *
 * Deliberately NOT workers (the pre-ruling-6 parentId arm is GONE —
 * "the worker subtree is for board workers"):
 *  - a parented session is a genuine Task-tool subagent of its
 *    parent; it nests beneath its parent wherever the parent renders
 *    (under a board worker INSIDE the group, under a user session in
 *    the main tree).
 *  - manual user sessions and orchestrator sessions (no origin, no
 *    parent) — the main list's audience.
 *  - routine trigger sessions: the user's own automation, badged and
 *    user-facing.
 *  - workflow (DAG agent-node) sessions: keep their existing badge
 *    treatment in the main list.
 *
 * Archiving outranks the worker class: an archived worker rows under
 * the Archived group (the caller filters archived FIRST — see the
 * Sidebar split).
 */
import type { SessionListItem } from "../api/types";

/** Origin value the board spawner stamps on engine-spawned workers (#1966). */
export const WORKER_ORIGIN = "worker";

export function isWorkerSession(s: SessionListItem): boolean {
  return s.origin === WORKER_ORIGIN;
}

/**
 * Recent-activity sort for the Workers group (ruling 6): most
 * recent of lastMessageAt / lastSeenAt first; sessions with no
 * timestamps sink to the end, falling back to alpha by title so the
 * order is deterministic. Mutates nothing — returns a sorted copy.
 */
export function sortByRecentActivity(sessions: SessionListItem[]): SessionListItem[] {
  const activity = (s: SessionListItem): number => {
    const times = [s.lastMessageAt ? Date.parse(s.lastMessageAt) : NaN, s.lastSeenAt ? Date.parse(s.lastSeenAt) : NaN];
    const valid = times.filter((t) => !Number.isNaN(t));
    return valid.length > 0 ? Math.max(...valid) : -Infinity;
  };
  return [...sessions].sort((a, b) => {
    const av = activity(a);
    const bv = activity(b);
    if (av !== bv) return bv - av;
    return (a.title ?? "").localeCompare(b.title ?? "");
  });
}
