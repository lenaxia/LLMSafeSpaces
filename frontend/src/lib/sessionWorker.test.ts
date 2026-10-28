/**
 * #1966/ruling 6 worker-classification pins (2026-10-28): the pure
 * classifier that splits a workspace's live sessions between the
 * collapsed EXISTING Workers group and the main user-facing list.
 *
 * ONE signal (ruling 6 — "the worker subtree is for board workers"):
 * origin-class 'worker', the spawn-path provenance mark the board
 * spawner records for every engine/board fresh-worker spawn. It
 * reaches the client ON the session list payload (server-side
 * session_origins join) with the /session-origins merge as fallback.
 *
 * The pre-ruling parentId arm is GONE: a parented session is a genuine
 * Task-tool subagent of its parent — it nests beneath its parent
 * wherever the parent renders (under a board worker INSIDE the group,
 * under a user session in the main tree), and never folders itself.
 */
import { describe, expect, it } from "vitest";
import { isWorkerSession, sortByRecentActivity, WORKER_ORIGIN } from "./sessionWorker";
import type { SessionListItem } from "../api/types";

function session(overrides: Partial<SessionListItem>): SessionListItem {
  return {
    id: "ses-x",
    messageCount: 1,
    status: "idle",
    hasUnread: false,
    ...overrides,
  };
}

describe("isWorkerSession (#1966/ruling 6)", () => {
  it("classifies an origin-class worker session as a board worker", () => {
    expect(isWorkerSession(session({ id: "ses-board", origin: WORKER_ORIGIN }))).toBe(true);
  });

  it("does NOT classify a parented (Task-tool subagent) session — provenance, not parentage", () => {
    expect(isWorkerSession(session({ id: "ses-sub", parentId: "ses-parent" }))).toBe(false);
  });

  it("keeps a plain user session in the main list", () => {
    expect(isWorkerSession(session({ id: "ses-user" }))).toBe(false);
  });

  it("keeps origin-manual and origin-routine sessions in the main list", () => {
    // routine = the user's own trigger automation (badged, user-facing);
    // manual = explicitly user-created.
    expect(isWorkerSession(session({ id: "ses-manual", origin: "manual" }))).toBe(false);
    expect(isWorkerSession(session({ id: "ses-routine", origin: "routine" }))).toBe(false);
  });

  it("keeps origin-workflow sessions user-facing (DAG agent nodes keep their badge)", () => {
    expect(isWorkerSession(session({ id: "ses-dag", origin: "workflow" }))).toBe(false);
  });
});

describe("sortByRecentActivity (#1966/ruling 6)", () => {
  it("sorts workers by the most recent of lastMessageAt/lastSeenAt, newest first", () => {
    const older = session({ id: "w-old", title: "Older worker", lastMessageAt: "2026-10-26T10:00:00Z" });
    const newerSeen = session({ id: "w-seen", title: "Seen worker", lastSeenAt: "2026-10-28T09:00:00Z" });
    const newest = session({ id: "w-new", title: "Newest worker", lastMessageAt: "2026-10-28T12:00:00Z" });

    expect(sortByRecentActivity([older, newest, newerSeen]).map((s) => s.id)).toEqual([
      "w-new",
      "w-seen",
      "w-old",
    ]);
  });

  it("sinks timestamp-less workers to the end, alpha by title as the deterministic fallback", () => {
    const quiet = session({ id: "w-quiet-z", title: "Zeta task" });
    const quietA = session({ id: "w-quiet-a", title: "Alpha task" });
    const active = session({ id: "w-active", title: "Active task", lastMessageAt: "2026-10-28T12:00:00Z" });

    expect(sortByRecentActivity([quiet, quietA, active]).map((s) => s.id)).toEqual([
      "w-active",
      "w-quiet-a",
      "w-quiet-z",
    ]);
  });

  it("does not mutate the input array", () => {
    const input = [session({ id: "a", title: "A" }), session({ id: "b", title: "B", lastMessageAt: "2026-10-28T12:00:00Z" })];
    const snapshot = [...input];
    sortByRecentActivity(input);
    expect(input.map((s) => s.id)).toEqual(snapshot.map((s) => s.id));
  });
});
