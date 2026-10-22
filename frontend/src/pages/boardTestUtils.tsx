// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { render, type RenderResult } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { vi } from "vitest";
import { BoardPage } from "./BoardPage";
import type { BoardProject, BoardTask } from "../api/board";
import { ApiClientError } from "../api/client";
import type { ApiError } from "../api/types";

// ─── Shared board page test harness ──────────────────────────────────────────

export const mockListProjects = vi.fn();
export const mockListProjectTasks = vi.fn();
export const mockMoveStage = vi.fn();
export const mockCreateProject = vi.fn();
export const mockCreateEpic = vi.fn();
export const mockGetEpic = vi.fn();
export const mockCreateTask = vi.fn();
export const mockUpdateTask = vi.fn();
export const mockDeleteTask = vi.fn();
export const mockAddDependency = vi.fn();
export const mockRemoveDependency = vi.fn();
export const mockStartTask = vi.fn();
export const mockActivateEpic = vi.fn();
export const mockPauseEpic = vi.fn();
export const mockWorkspacesList = vi.fn();

vi.mock("../api/board", () => ({
  boardApi: {
    listProjects: () => mockListProjects(),
    listProjectTasks: (id: string, stage?: string) => mockListProjectTasks(id, stage),
    moveStage: (id: string, to: string, ev?: string) => mockMoveStage(id, to, ev),
    startTask: (id: string) => mockStartTask(id),
    createProject: (input: unknown) => mockCreateProject(input),
    createEpic: (pid: string, input: unknown) => mockCreateEpic(pid, input),
    getEpic: (id: string) => mockGetEpic(id),
    createTask: (eid: string, input: unknown) => mockCreateTask(eid, input),
    updateTask: (id: string, input: unknown) => mockUpdateTask(id, input),
    deleteTask: (id: string, reconnect?: boolean) => mockDeleteTask(id, reconnect),
    addDependency: (id: string, dep: string) => mockAddDependency(id, dep),
    removeDependency: (id: string, dep: string) => mockRemoveDependency(id, dep),
    activateEpic: (id: string) => mockActivateEpic(id),
    pauseEpic: (id: string) => mockPauseEpic(id),
  },
  newBoardRequestId: () => "req-test",
}));

vi.mock("../api/workspaces", () => ({
  workspacesApi: { list: () => mockWorkspacesList() },
}));

export const PROJECT_A: BoardProject = {
  id: "proj-uuid-a",
  short_id: "prj-aaaa1111",
  user_id: "u1",
  name: "Apollo",
  target_workspace_id: "ws-1",
  worker_slots: 2,
  tags: [],
  status: "active",
  created_at: "2026-10-01T00:00:00Z",
  updated_at: "2026-10-01T00:00:00Z",
};

export const PROJECT_B: BoardProject = {
  id: "proj-uuid-b",
  short_id: "prj-bbbb2222",
  user_id: "u1",
  name: "Borealis",
  target_workspace_id: "ws-1",
  worker_slots: 1,
  tags: [],
  status: "active",
  created_at: "2026-10-02T00:00:00Z",
  updated_at: "2026-10-02T00:00:00Z",
};

const base = {
  description: "",
  paused: false,
  project_name: "Apollo",
  epic_name: "E1",
  tags: [] as string[],
  acceptance_criteria: [] as Array<{ text: string; met: boolean }>,
  is_blocked: false,
  claim_incremented_attempts: false,
  stage_entered_at: "2026-10-10T00:00:00Z",
  created_at: "2026-10-10T00:00:00Z",
  updated_at: "2026-10-10T00:00:00Z",
  attempts: 0,
  last_attempt_failed: false,
  failure_count: 0,
};

export const TASK_FAILED: BoardTask = {
  ...base,
  id: "task-uuid-f",
  project_id: "proj-uuid-a",
  epic_id: "epic-uuid-1",
  short_id: "tsk-ffff0009",
  title: "Crash looping",
  stage: "backlog",
  attempts: 2,
  last_attempt_failed: true,
  failure_count: 2,
};

export const TASK_BACKLOG: BoardTask = {
  ...base,
  id: "task-uuid-1",
  project_id: "proj-uuid-a",
  epic_id: "epic-uuid-1",
  short_id: "tsk-aaaa0001",
  title: "Write charter",
  stage: "backlog",
};

export const TASK_IN_PROGRESS: BoardTask = {
  ...base,
  id: "task-uuid-2",
  project_id: "proj-uuid-a",
  epic_id: "epic-uuid-1",
  short_id: "tsk-bbbb0002",
  title: "Implement engine",
  stage: "in_progress",
  assignee_session: "ws-1/sess-1",
  attempts: 1,
};

export const TASK_IN_REVIEW: BoardTask = {
  ...base,
  id: "task-uuid-3",
  project_id: "proj-uuid-a",
  epic_id: "epic-uuid-1",
  short_id: "tsk-cccc0003",
  title: "Review drag arm",
  stage: "in_review",
};

export const TASK_DONE: BoardTask = {
  ...base,
  id: "task-uuid-4",
  project_id: "proj-uuid-a",
  epic_id: "epic-uuid-1",
  short_id: "tsk-dddd0004",
  title: "Ship design doc",
  stage: "done",
};

export const TASK_BLOCKED: BoardTask = {
  ...base,
  id: "task-uuid-5",
  project_id: "proj-uuid-a",
  epic_id: "epic-uuid-2",
  short_id: "tsk-eeee0005",
  title: "Blocked follow-up",
  stage: "backlog",
  epic_name: "E2",
  is_blocked: true,
  blocked_by: ["tsk-cccc0003"],
  tags: ["pr:1702"],
};

export const TASK_PENDING: BoardTask = {
  ...base,
  id: "task-uuid-6",
  project_id: "proj-uuid-b",
  epic_id: "epic-uuid-1",
  short_id: "tsk-ffff0006",
  title: "Waiting on human",
  stage: "pending_input",
  pending_origin: "escalated",
  attempts: 3,
  project_name: "Borealis",
};

export function defaultTasks(): BoardTask[] {
  return [TASK_BACKLOG, TASK_IN_PROGRESS, TASK_IN_REVIEW, TASK_DONE, TASK_BLOCKED, TASK_PENDING, TASK_FAILED];
}

/** Seeds the standard two-project board. Returns the seeded task list
 * (callers derive updated states from it). */
export function seedBoard(tasks: BoardTask[] = defaultTasks(), projects: BoardProject[] = [PROJECT_A, PROJECT_B]): BoardTask[] {
  mockListProjects.mockResolvedValue(projects);
  mockListProjectTasks.mockImplementation(async (pid: string) =>
    pid === "proj-uuid-b" ? tasks.filter((t) => t.project_id === "proj-uuid-b") : tasks.filter((t) => t.project_id === "proj-uuid-a"),
  );
  mockWorkspacesList.mockResolvedValue({
    items: [{ id: "ws-1", name: "Main", userId: "u1", runtime: "python", storageSize: "1Gi", createdAt: "", updatedAt: "" }],
  });
  return tasks;
}

export function renderBoard(): RenderResult {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={["/board"]}>
        <BoardPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

/** Builds the board ApiClientError shape (404+suggestions / 409+code). */
export function boardError(status: number, body: ApiError): ApiClientError {
  return new ApiClientError(status, body);
}

// jsdom has no DataTransfer; drag arms construct one explicitly.
export function makeDataTransfer() {
  const store = new Map<string, string>();
  return {
    setData: (type: string, value: string) => store.set(type, value),
    getData: (type: string) => store.get(type) ?? "",
    effectAllowed: "move",
    dropEffect: "move",
    types: [...store.keys()],
  };
}
