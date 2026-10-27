/**
 * #1966 worker-classification pins: the pure classifier that splits a
 * workspace's live sessions into the collapsed Workers folder vs the
 * main user-facing list. Born RED — isWorkerSession did not exist.
 *
 * Classification signals (issue #1966):
 *  - origin-class worker: the #1915 origin marks extended with the
 *    'worker' class, recorded by the board spawner for every
 *    engine/board fresh-worker spawn (board implementers, reviewer
 *    personas, bounce replacements).
 *  - parentage: a session with a parentId is a Task-tool subagent /
 *    one-shot lane — the same bucket (they already get the read-only
 *    permission-surface treatment in ChatPage).
 *  - everything else (manual user sessions, orchestrator sessions,
 *    routine trigger sessions — the user's own automation, badged) —
 *    stays in the main list.
 */
import { describe, expect, it } from "vitest";
import { isWorkerSession } from "./sessionWorker";
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

describe("isWorkerSession (#1966)", () => {
  it("classifies an origin-class worker session as a worker", () => {
    expect(isWorkerSession(session({ id: "ses-board", origin: "worker" }))).toBe(true);
  });

  it("classifies a parented (Task-tool subagent) session as a worker", () => {
    expect(isWorkerSession(session({ id: "ses-sub", parentId: "ses-parent" }))).toBe(true);
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
