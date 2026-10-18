// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * board.ts — the Task Board REST surface's frontend client (design 0067
 * §4.3a, #1699 Lane 2, consumed by the Board UI per #1702). Wire shapes
 * are the engine's canonical JSON VERBATIM (snake_case short_id etc.) —
 * the same encoding both server projections ship; no second encoding is
 * invented here. Error bodies carry the board's richer shape (code on
 * 409s, suggestions on 404s) — see ApiError.suggestions and
 * lib/boardRules.describeBoardError.
 */

import { api } from "./client";
import type { ApiError } from "./types";

// --- Row shapes (pkg/board/rows.go, verbatim snake_case) ------------------------

export interface BoardProject {
  id: string;
  short_id: string;
  user_id: string;
  name: string;
  target_workspace_id?: string;
  worker_slots: number;
  slot_cooldown_until?: string | null;
  tags: string[];
  status: string;
  created_at: string;
  updated_at: string;
}

export interface BoardEpic {
  id: string;
  project_id: string;
  short_id: string;
  title: string;
  outcome: string;
  status: string;
  priority: string;
  project_name?: string;
  tags: string[];
  created_at: string;
  updated_at: string;
  progress_done: number;
  progress_total: number;
  progress_derived: boolean;
}

export interface BoardAcceptanceCriterion {
  text: string;
  met: boolean;
}

export interface BoardTask {
  id: string;
  project_id: string;
  epic_id: string;
  short_id: string;
  title: string;
  description: string;
  stage: string;
  pending_origin?: string | null;
  priority: string;
  project_name: string;
  epic_name: string;
  tags: string[];
  acceptance_criteria: BoardAcceptanceCriterion[];
  is_blocked: boolean;
  blocked_by?: string[];
  assignee_session?: string;
  claim_token?: string;
  lease_expires_at?: string | null;
  last_progress_at?: string | null;
  attempts: number;
  claim_incremented_attempts: boolean;
  cooldown_until?: string | null;
  stage_entered_at: string;
  created_at: string;
  updated_at: string;
}

/** task_start's outcome: the engine's Claim (escalated=true when the
 * attempts cap withheld the grant — no token, no lease). */
export interface BoardClaim {
  task_id: string;
  short_id: string;
  project_id: string;
  epic_id: string;
  workspace_id: string;
  claim_token?: string;
  assignee?: string;
  lease_expires_at?: string;
  escalated: boolean;
  task?: BoardTask;
}

/** The §4.6 SSE payload: a board_events row fanned out on the user
 * stream as type "board.event". resulting row-version fields ride the
 * payload when the action mutated a task row. */
export interface BoardEventPayload {
  type: "board.event";
  project_id: string;
  id: string;
  item_kind?: string;
  item_id?: string;
  action: string;
  actor?: { kind?: string; user_id?: string; surface?: string };
  payload?: Record<string, unknown>;
  created_at?: string;
  task?: Partial<BoardTask> & { stage?: string; short_id?: string };
}

// --- Inputs (camelCase request bodies, per the REST projection) -----------------

export interface BoardProjectInput {
  name?: string;
  targetWorkspaceId?: string;
  workerSlots?: number;
  tags?: string[];
}

export interface BoardEpicInput {
  title?: string;
  outcome?: string;
  priority?: string;
  tags?: string[];
}

export interface BoardTaskInput {
  title?: string;
  description?: string;
  priority?: string;
  tags?: string[];
  acceptanceCriteria?: BoardAcceptanceCriterion[];
  /** Check off criterion at this 0-based index (one-way on the engine;
   * toggles send the full replacement array instead). */
  setCriteriaMet?: number;
}

// --- requestId discipline (§4.2: one UUID per logical operation) ----------------

export function newBoardRequestId(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  return `bd-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
}

// --- The 23 operations (router.go board groups, all AuthMiddleware) -------------

export const boardApi = {
  // /me/projects
  listProjects: (): Promise<BoardProject[]> =>
    api.get<{ projects: BoardProject[] }>("/me/projects").then((r) => r.projects ?? []),
  createProject: (input: BoardProjectInput): Promise<BoardProject> =>
    api.post<BoardProject>("/me/projects", { ...input, requestId: newBoardRequestId() }),
  getProject: (idOrShort: string): Promise<BoardProject> =>
    api.get<BoardProject>(`/me/projects/${encodeURIComponent(idOrShort)}`),
  updateProject: (idOrShort: string, input: BoardProjectInput): Promise<BoardProject> =>
    api.patch<BoardProject>(`/me/projects/${encodeURIComponent(idOrShort)}`, { ...input, requestId: newBoardRequestId() }),
  archiveProject: (idOrShort: string): Promise<BoardProject> =>
    api.post<BoardProject>(`/me/projects/${encodeURIComponent(idOrShort)}/archive`),
  unarchiveProject: (idOrShort: string): Promise<BoardProject> =>
    api.post<BoardProject>(`/me/projects/${encodeURIComponent(idOrShort)}/unarchive`),
  listProjectTasks: (idOrShort: string, stage?: string): Promise<BoardTask[]> => {
    const q = stage ? `?stage=${encodeURIComponent(stage)}` : "";
    return api.get<{ tasks: BoardTask[] }>(`/me/projects/${encodeURIComponent(idOrShort)}/tasks${q}`).then((r) => r.tasks ?? []);
  },

  // /me/projects/:id/epics + /me/epics
  createEpic: (projectIdOrShort: string, input: BoardEpicInput): Promise<BoardEpic> =>
    api.post<BoardEpic>(`/me/projects/${encodeURIComponent(projectIdOrShort)}/epics`, { ...input, requestId: newBoardRequestId() }),
  getEpic: (idOrShort: string): Promise<BoardEpic> =>
    api.get<BoardEpic>(`/me/epics/${encodeURIComponent(idOrShort)}`),
  updateEpic: (idOrShort: string, input: BoardEpicInput): Promise<BoardEpic> =>
    api.patch<BoardEpic>(`/me/epics/${encodeURIComponent(idOrShort)}`, { ...input, requestId: newBoardRequestId() }),
  activateEpic: (idOrShort: string): Promise<BoardEpic> =>
    api.post<BoardEpic>(`/me/epics/${encodeURIComponent(idOrShort)}/activate`),
  pauseEpic: (idOrShort: string): Promise<BoardEpic> =>
    api.post<BoardEpic>(`/me/epics/${encodeURIComponent(idOrShort)}/pause`),
  archiveEpic: (idOrShort: string): Promise<BoardEpic> =>
    api.post<BoardEpic>(`/me/epics/${encodeURIComponent(idOrShort)}/archive`),
  unarchiveEpic: (idOrShort: string): Promise<BoardEpic> =>
    api.post<BoardEpic>(`/me/epics/${encodeURIComponent(idOrShort)}/unarchive`),
  cancelEpic: (idOrShort: string, reason?: string): Promise<BoardEpic> =>
    api.post<BoardEpic>(`/me/epics/${encodeURIComponent(idOrShort)}/cancel`, { reason: reason ?? "", requestId: newBoardRequestId() }),

  // /me/epics/:id/tasks + /me/tasks
  createTask: (epicIdOrShort: string, input: BoardTaskInput): Promise<BoardTask> =>
    api.post<BoardTask>(`/me/epics/${encodeURIComponent(epicIdOrShort)}/tasks`, { ...input, requestId: newBoardRequestId() }),
  getTask: (idOrShort: string): Promise<BoardTask> =>
    api.get<BoardTask>(`/me/tasks/${encodeURIComponent(idOrShort)}`),
  updateTask: (idOrShort: string, input: BoardTaskInput): Promise<BoardTask> =>
    api.patch<BoardTask>(`/me/tasks/${encodeURIComponent(idOrShort)}`, { ...input, requestId: newBoardRequestId() }),
  moveStage: (idOrShort: string, toStage: string, evidenceUrl?: string): Promise<BoardTask> =>
    api.post<BoardTask>(`/me/tasks/${encodeURIComponent(idOrShort)}/move-stage`, {
      toStage,
      evidenceUrl: evidenceUrl ?? "",
      requestId: newBoardRequestId(),
    }),
  startTask: (idOrShort: string): Promise<BoardClaim> =>
    api.post<BoardClaim>(`/me/tasks/${encodeURIComponent(idOrShort)}/start`, { requestId: newBoardRequestId() }),
  cancelTask: (idOrShort: string, reason?: string): Promise<BoardTask> =>
    api.post<BoardTask>(`/me/tasks/${encodeURIComponent(idOrShort)}/cancel`, { reason: reason ?? "", requestId: newBoardRequestId() }),
  addDependency: (idOrShort: string, dependsOn: string): Promise<{ task: BoardTask }> =>
    api.post<{ task: BoardTask }>(`/me/tasks/${encodeURIComponent(idOrShort)}/dependencies`, {
      dependsOn,
      requestId: newBoardRequestId(),
    }),
  removeDependency: (idOrShort: string, dependsOnId: string): Promise<{ task: BoardTask }> =>
    api.delete<{ task: BoardTask }>(`/me/tasks/${encodeURIComponent(idOrShort)}/dependencies/${encodeURIComponent(dependsOnId)}`),
};

/** Narrows an ApiClientError carrying a board error body (404+suggestions
 * / 409+code) for describeBoardError. */
export function asBoardApiError(err: unknown): { message?: string; status?: number; body?: ApiError & { suggestions?: string[] } } | null {
  if (err && typeof err === "object" && "status" in err && "body" in err) {
    return err as { status: number; body: ApiError & { suggestions?: string[] } };
  }
  return null;
}
