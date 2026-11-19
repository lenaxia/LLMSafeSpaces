// boardDeepLink.test.ts — born-red pins for the /board entity
// deep-link semantics (tsk-vibrant-cadet-pearls): ?story= opens the
// story drawer, ?epic= the epic drawer, ?project= focuses the lane;
// waiting while the read model loads; silent drop for stale ids.

import { describe, expect, it } from "vitest";
import { consumeBoardDeepLink } from "./boardDeepLink";
import type { BoardEpic, BoardProject, BoardTask } from "../api/board";

function project(short_id: string, id = "p-uuid"): BoardProject {
  return {
    id,
    short_id,
    user_id: "u",
    name: `Project ${short_id}`,
    worker_slots: 2,
    tags: [],
    status: "active",
    created_at: "",
    updated_at: "",
  };
}

function task(short_id: string, id = "t-uuid"): BoardTask {
  return {
    id,
    project_id: "p-uuid",
    epic_id: "e-uuid",
    short_id,
    title: `Story ${short_id}`,
    description: "",
    stage: "backlog",
    pending_input: false,
    paused: false,
    project_name: "Project",
    epic_name: "Epic",
    tags: [],
    acceptance_criteria: [],
    is_blocked: false,
    attempts: 0,
    claim_incremented_attempts: false,
    last_attempt_failed: false,
    failure_count: 0,
    stage_entered_at: "",
    created_at: "",
    updated_at: "",
  } as BoardTask;
}

function epic(short_id: string, id = "e-uuid"): BoardEpic {
  return {
    id,
    project_id: "p-uuid",
    short_id,
    title: `Epic ${short_id}`,
    outcome: "",
    status: "active",
    tags: [],
    created_at: "",
    updated_at: "",
    progress_done: 0,
    progress_total: 0,
    progress_derived: false,
  };
}

const loaded = {
  projects: [project("prj-alpha")],
  tasks: [task("tsk-yeasty-magnet-scores")],
  epics: [epic("epc-quiet-harbor-9")],
  projectsLoaded: true,
  tasksLoaded: true,
  epicsLoaded: true,
};

describe("consumeBoardDeepLink", () => {
  it("no params → none", () => {
    expect(consumeBoardDeepLink({}, loaded)).toEqual({ kind: "none" });
  });

  it("?story=<short> opens the story drawer (match by short id or uuid)", () => {
    const byShort = consumeBoardDeepLink({ story: "tsk-yeasty-magnet-scores" }, loaded);
    expect(byShort).toEqual({ kind: "open-story", task: loaded.tasks[0] });
    const byUuid = consumeBoardDeepLink({ story: "t-uuid" }, loaded);
    expect(byUuid).toEqual({ kind: "open-story", task: loaded.tasks[0] });
  });

  it("?epic=<short> opens the epic drawer; ?project=<short> focuses the lane", () => {
    expect(consumeBoardDeepLink({ epic: "epc-quiet-harbor-9" }, loaded)).toEqual({
      kind: "open-epic",
      epic: loaded.epics[0],
    });
    expect(consumeBoardDeepLink({ project: "prj-alpha" }, loaded)).toEqual({
      kind: "focus-project",
      project: loaded.projects[0],
    });
  });

  it("waits while the relevant read model is still loading", () => {
    // Ids not (yet) present: with the read model unsettled the param
    // may still arrive — waiting. (A row that already matched opens
    // immediately regardless of the settled flag.)
    expect(
      consumeBoardDeepLink({ story: "tsk-still-loading" }, { ...loaded, tasksLoaded: false }),
    ).toEqual({ kind: "waiting" });
    expect(
      consumeBoardDeepLink({ epic: "epc-still-loading" }, { ...loaded, epicsLoaded: false }),
    ).toEqual({ kind: "waiting" });
    expect(
      consumeBoardDeepLink({ project: "prj-still-loading" }, { ...loaded, projectsLoaded: false }),
    ).toEqual({ kind: "waiting" });
  });

  it("drops stale ids once loaded — none, never a dead drawer", () => {
    expect(consumeBoardDeepLink({ story: "tsk-gone-gone-gone" }, loaded)).toEqual({ kind: "none" });
    expect(consumeBoardDeepLink({ epic: "epc-gone-gone-gone" }, loaded)).toEqual({ kind: "none" });
    expect(consumeBoardDeepLink({ project: "prj-gone" }, loaded)).toEqual({ kind: "none" });
  });
});
