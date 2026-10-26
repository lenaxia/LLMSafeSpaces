/**
 * #1966 worker-session classification: the pure signal that splits a
 * workspace's live sessions between the collapsed "Workers" folder and
 * the main user-facing list. Presentation-only — the classifier feeds
 * rendering, never the list query (search composes server-side over
 * the unchanged listing).
 *
 * Signals (issue #1966):
 *  - origin-class 'worker': the #1915 origin marks extended with a
 *    worker class, recorded by the board spawner for every
 *    engine/board fresh-worker spawn (implementers, reviewer personas,
 *    bounce replacements). Reaches the client through the same
 *    session-origins merge the sidebar already performs.
 *  - parentage: a session with a parentId is a Task-tool subagent /
 *    one-shot lane — the same bucket (ChatPage already gives these the
 *    read-only permission-surface treatment).
 *
 * Deliberately NOT workers:
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
  return s.origin === WORKER_ORIGIN || !!s.parentId;
}
