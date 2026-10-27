/**
 * Builds a hierarchical tree from a flat list of SessionListItem records,
 * grouping children under their parent (subagent / `task` tool sessions).
 *
 * Sorting: top-level sessions and orphans are kept in input order (the API
 * already sorts by lastMessageAt DESC). Children are sorted independently
 * within each parent — also by input order. We deliberately do NOT re-sort
 * by lastMessageAt within children because the parent's lastMessageAt
 * already implicitly reflects the most recent activity in its subtree.
 *
 * Cycle protection: a malformed parent chain (A→B, B→A) cannot crash the
 * UI. {@link buildSessionTree} bounds the parent walk to {@link MAX_DEPTH}
 * and treats anything beyond as an orphan. In practice opencode never
 * produces cycles — this is purely a defensive guard.
 *
 * Orphans: a session whose parentId points at an ID not present in the
 * input list (e.g. parent was deleted) lands in the `orphans` bucket.
 * NOTE (#1966): the sidebar no longer renders a synthetic "Orphaned
 * subtasks" group — every parented session (the only orphan
 * candidates) is worker-classified into the collapsed Workers folder
 * before tree building, so `orphans` is structurally empty at that
 * call site; the bucket remains for other consumers of this lib.
 */
import type { SessionListItem } from "../api/types";

export interface SessionTreeNode {
  session: SessionListItem;
  children: SessionTreeNode[];
}

export interface SessionTree {
  /** Top-level sessions (no parentId) in input order. */
  roots: SessionTreeNode[];
  /**
   * Sessions whose parentId references a session not present in the input.
   * Empty when there are no orphans — caller can hide the group entirely.
   */
  orphans: SessionTreeNode[];
}

/** Max walk depth when expanding the ancestor chain — generous; opencode
 *  task nesting in practice never exceeds 2-3 levels. */
const MAX_DEPTH = 16;

export function buildSessionTree(sessions: SessionListItem[]): SessionTree {
  const byId = new Map<string, SessionListItem>();
  for (const s of sessions) {
    byId.set(s.id, s);
  }

  // Build child lookup in a single pass keyed on parentId.
  // Sessions without parentId go directly into `roots`.
  const childrenByParent = new Map<string, SessionListItem[]>();
  const roots: SessionListItem[] = [];
  const orphans: SessionListItem[] = [];

  for (const s of sessions) {
    if (!s.parentId) {
      roots.push(s);
      continue;
    }
    if (!byId.has(s.parentId)) {
      // Parent doesn't exist in the input — treat as orphan.
      orphans.push(s);
      continue;
    }
    const list = childrenByParent.get(s.parentId);
    if (list) {
      list.push(s);
    } else {
      childrenByParent.set(s.parentId, [s]);
    }
  }

  const buildNode = (s: SessionListItem, depth: number): SessionTreeNode => {
    if (depth >= MAX_DEPTH) {
      // Truncate to prevent runaway recursion on a cyclic parent chain.
      return { session: s, children: [] };
    }
    const childList = childrenByParent.get(s.id) ?? [];
    return {
      session: s,
      children: childList.map((c) => buildNode(c, depth + 1)),
    };
  };

  return {
    roots: roots.map((s) => buildNode(s, 0)),
    orphans: orphans.map((s) => buildNode(s, 0)),
  };
}

/**
 * Returns the full ancestor chain for a session, root first, including the
 * session itself. Useful for auto-expanding the path to the active session
 * when the sidebar mounts.
 *
 * Returns [] if the session is not in the input or if cycle protection
 * triggers.
 */
export function ancestorChain(
  sessions: SessionListItem[],
  sessionId: string,
): string[] {
  const byId = new Map<string, SessionListItem>();
  for (const s of sessions) byId.set(s.id, s);

  const chain: string[] = [];
  const seen = new Set<string>();
  let current: string | undefined = sessionId;
  while (current && !seen.has(current)) {
    if (chain.length >= MAX_DEPTH) break;
    seen.add(current);
    const s = byId.get(current);
    if (!s) break;
    chain.push(current);
    current = s.parentId;
  }
  return chain.reverse();
}
