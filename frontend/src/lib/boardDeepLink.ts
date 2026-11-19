// boardDeepLink.ts — the /board entity deep-link contract
// (tsk-vibrant-cadet-pearls).
//
// Chat entity links into the board mint minimal query params —
//   /board?story=<tsk short id>   → open that story's drawer
//   /board?epic=<epc short id>    → open that epic's drawer
//   /board?project=<prj short id> → focus that project's lane
// — reusing the existing board route (no new route shape). This pure
// helper decides what a param set means against the current read
// model so the wiring in BoardPage stays thin and the semantics are
// unit-pinnable:
//   matched    → the open/focus action
//   not loaded → "waiting" (data still fetching; retry on arrival)
//   loaded, no match → "none" (stale/deleted id — drop silently, no
//   dead-end drawer, matching the linkify no-dead-links posture)

import type { BoardEpic, BoardProject, BoardTask } from "../api/board";

export interface BoardDeepLinkParams {
  story?: string | null;
  epic?: string | null;
  project?: string | null;
}

export interface BoardDeepLinkEntities {
  projects: BoardProject[];
  tasks: BoardTask[];
  epics: BoardEpic[];
  projectsLoaded: boolean;
  tasksLoaded: boolean;
  epicsLoaded: boolean;
}

export type BoardDeepLinkAction =
  | { kind: "none" }
  | { kind: "waiting" }
  | { kind: "focus-project"; project: BoardProject }
  | { kind: "open-story"; task: BoardTask }
  | { kind: "open-epic"; epic: BoardEpic };

export function consumeBoardDeepLink(
  params: BoardDeepLinkParams,
  entities: BoardDeepLinkEntities,
): BoardDeepLinkAction {
  if (params.story) {
    const task = entities.tasks.find(
      (t) => t && (t.short_id === params.story || t.id === params.story),
    );
    if (task) return { kind: "open-story", task };
    // Tasks are only knowable once both the project list and the task
    // fan-out settle; until then the id may still arrive.
    if (entities.projectsLoaded && entities.tasksLoaded) return { kind: "none" };
    return { kind: "waiting" };
  }
  if (params.epic) {
    const epic = entities.epics.find(
      (e) => e && (e.short_id === params.epic || e.id === params.epic),
    );
    if (epic) return { kind: "open-epic", epic };
    if (entities.projectsLoaded && entities.epicsLoaded) return { kind: "none" };
    return { kind: "waiting" };
  }
  if (params.project) {
    const project = entities.projects.find(
      (p) => p && (p.short_id === params.project || p.id === params.project),
    );
    if (project) return { kind: "focus-project", project };
    if (entities.projectsLoaded) return { kind: "none" };
    return { kind: "waiting" };
  }
  return { kind: "none" };
}
