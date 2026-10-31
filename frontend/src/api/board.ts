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
  /** 000050: pending_input is a FLAG riding the real stage (never a
   * stored stage) — render a badge where the work lives, not a costume
   * column. pending_origin carries the flag's provenance (also set on
   * failed/escalated rows: the re-arm queue's marker). */
  pending_input: boolean;
  pending_origin?: string | null;
  paused: boolean;
  waiting_on?: string;
  backpressure?: string;
  project_name: string;
  epic_name: string;
  tags: string[];
  acceptance_criteria: BoardAcceptanceCriterion[];
  is_blocked: boolean;
  blocked_by?: string[];
  assignee_session?: string;
  /** #1922: set when assignee_session could not be resolved to the
   * session phrase — the value is the raw harness id (design-0066 cold
   * window), flagged per the #1817/#1821 SSE precedent. */
  assignee_session_deprecated?: boolean;
  /** RC-C S2 (badge honesty): the assignee is MID-TURN right now
   * (derived busy — streaming or an in-flight tool call) on a LIVE
   * lease. Absent/false = claimed-but-quiet: idle, unknown, or past
   * the lease without renewal (the busy TTL — a lapsed claim never
   * spins). Read-edge derived; never stored. */
  assignee_busy?: boolean;
  /** #1959 implementer identity — the session that moved the row to
   * in_review (the ORIGINAL worker; the assignee during review is the
   * reviewer). Exposed on reads per the 2026-10-29 owner ruling: a
   * resolvable session ref exactly like assignee_session (the phrase
   * when the session index knows it, raw harness id + flag when cold).
   * Empty before the first forward exit and on every new episode. */
  implementer_session?: string;
  /** Cold-window degrade flag for implementer_session — see
   * assignee_session_deprecated. */
  implementer_session_deprecated?: boolean;
  claim_token?: string;
  lease_expires_at?: string | null;
  last_progress_at?: string | null;
  attempts: number;
  claim_incremented_attempts: boolean;
  /** #1813 failed-state overlay: the last worker attempt failed (lease
   * lapse → reclaim, the overlay's only writer). Never a stage; an
   * all-success exhaustion carries no marker. */
  last_attempt_failed: boolean;
  /** Consecutive failures — the doubling backoff driver. */
  failure_count: number;
  last_failure_at?: string | null;
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

/** #1832 item 4: one board_events row — the task-detail drawer's audit
 * timeline read (GET /me/tasks/:id/events). Wire shape is the row
 * VERBATIM (snake_case); chronological ascending, newest-N window. */
export interface BoardAuditEvent {
  id: string;
  project_id: string;
  item_kind?: string;
  item_id?: string;
  action: string;
  actor?: { kind?: string; user_id?: string; surface?: string; workspace_id?: string; session_id?: string; session_id_deprecated?: string } | null;
  payload?: Record<string, unknown> | null;
  created_at: string;
}

/** The structured review verdict riding a review-bounce move_stage
 * payload (PR-B's verdict; rendered by the timeline when present). */
export interface BoardVerdictFinding {
  severity: string;
  pointer?: string;
  criterion_index?: number;
  note?: string;
}

export interface BoardVerdict {
  verdict: string;
  reviewer?: string;
  timestamp?: string;
  findings?: BoardVerdictFinding[];
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
  tags?: string[];
}

export interface BoardTaskInput {
  title?: string;
  description?: string;
  tags?: string[];
  acceptanceCriteria?: BoardAcceptanceCriterion[];
  /** Prerequisite task short ids — edges added in the same write (#1747). */
  dependsOn?: string[];
  /** Check off criterion at this 0-based index (one-way on the engine;
   * toggles send the full replacement array instead). */
  setCriteriaMet?: number;
}

/** #1747: the delete result — snapshot-backed audit; orphaned dependents
 * are listed (each carries an unblocked_by_delete audit event). */
export interface BoardDeleteResult {
  item_kind: string;
  id: string;
  short_id: string;
  cascade_tasks?: string[];
  orphaned_dependents?: string[];
  reconnected?: string[];
}

/** #1747: one plan node (dependsOn names other keys in the payload). */
export interface BoardPlanTask {
  key: string;
  title: string;
  /** REQUIRED per node (#1849): the worker's charter — seedless nodes refuse at dry-run. */
  description: string;
  tags?: string[];
  /** REQUIRED per node (#1849): Done is undefined without at least one. */
  acceptanceCriteria: BoardAcceptanceCriterion[];
  dependsOn?: string[];
}

export interface BoardPlanInput {
  epicTitle: string;
  outcome?: string;
  dryRun?: boolean;
  tasks: BoardPlanTask[];
}

export interface BoardImportResult {
  dry_run: boolean;
  plan?: { valid: boolean; error?: string; warnings: string[]; initial_ready_set: string[] };
  project_id: string;
  epic_id?: string;
  epic_short_id?: string;
  task_keys?: Record<string, string>;
  initial_ready_set?: string[];
  warnings: string[];
}

// --- requestId discipline (§4.2: one UUID per logical operation) ----------------

export function newBoardRequestId(): string {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  return `bd-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
}

// --- #1832 item 6: board config shapes (mirror pkg/board/config.go) ------------

/** One stage's configured agent persona (stage-persona
 * claim-then-spawn) — a ROLE REFERENCE, nothing else (owner ruling
 * 2026-10-29, round 2: "Board stages should ONLY ever have roles
 * configured"). The bare string is the canonical wire form (the
 * referenced agent role's id or slug — the spawned worker's
 * model/tools/system-prompt come from the role); the {roleId} object
 * decodes on the wire but carries nothing else. name/instructions do
 * not exist in this layer. */
export type BoardPersonaConfig = string | { roleId: string };

export type BoardConfig = {
  reviewColumn: string; // "off" | "manual" | "auto"
  autoAssignReviewer: boolean;
  defaultWorkerSlots: number;
  /** Stage → persona. Merges per-KEY: a set object replaces the
   * inherited map wholesale; {} is the explicit opt-out. */
  stagePersonas?: Record<string, BoardPersonaConfig>;
  /** Whether stagePersonas was EXPLICITLY set at any merged tier —
   * the REPLACE-tier carry-through rule (never materialize {} when
   * nothing was stored; never delete a stored map). */
  stagePersonasSet?: boolean;
};

/** Partial config — absent keys inherit; never null on the wire. */
export type BoardConfigOverride = {
  reviewColumn?: string;
  autoAssignReviewer?: boolean;
  defaultWorkerSlots?: number;
  stagePersonas?: Record<string, BoardPersonaConfig>;
};

/** The project tier's override input: defaultWorkerSlots is a
 * user-level key and the server 400s if it is sent (openapi:
 * BoardConfigOverrideProject omits it). */
export type ProjectBoardConfigOverrideInput = {
  reviewColumn?: string;
  autoAssignReviewer?: boolean;
  stagePersonas?: Record<string, BoardPersonaConfig>;
};

export type ProjectBoardConfig = {
  project_id: string;
  overrides: BoardConfigOverride;
  effective: BoardConfig;
};

export const BOARD_REVIEW_COLUMNS = ["off", "manual", "auto"] as const;

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
  listProjectTasks: (idOrShort: string, stage?: string, opts?: { failed?: boolean }): Promise<BoardTask[]> => {
    const params = new URLSearchParams();
    if (stage) params.set("stage", stage);
    if (opts?.failed !== undefined) params.set("failed", String(opts.failed));
    const q = params.toString();
    return api
      .get<{ tasks: BoardTask[] }>(`/me/projects/${encodeURIComponent(idOrShort)}/tasks${q ? `?${q}` : ""}`)
      .then((r) => r.tasks ?? []);
  },

  // /me/projects/:id/epics + /me/epics
  // The list seam (#1699 engine read): a project's epics with derived
  // progress — the source of existence for bands/picker (zero-task
  // epics live here, not in task rows).
  listProjectEpics: (projectIdOrShort: string): Promise<BoardEpic[]> =>
    api.get<{ epics: BoardEpic[] }>(`/me/projects/${encodeURIComponent(projectIdOrShort)}/epics`).then((r) => r.epics ?? []),
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
  deleteEpic: (idOrShort: string, force?: boolean): Promise<BoardDeleteResult> =>
    api.delete<BoardDeleteResult>(`/me/epics/${encodeURIComponent(idOrShort)}${force ? "?force=true" : ""}`),

  // /me/epics/:id/tasks + /me/tasks
  createTask: (epicIdOrShort: string, input: BoardTaskInput): Promise<BoardTask> =>
    api.post<BoardTask>(`/me/epics/${encodeURIComponent(epicIdOrShort)}/tasks`, { ...input, requestId: newBoardRequestId() }),
  getTask: (idOrShort: string): Promise<BoardTask> =>
    api.get<BoardTask>(`/me/tasks/${encodeURIComponent(idOrShort)}`),
  /** #1832 item 4: the task's FULL audit timeline (board_events rows,
   * chronological ascending; limit keeps the newest N, default 200). */
  taskEvents: (idOrShort: string, limit?: number): Promise<BoardAuditEvent[]> => {
    const q = limit ? `?limit=${limit}` : "";
    return api.get<{ events: BoardAuditEvent[] }>(`/me/tasks/${encodeURIComponent(idOrShort)}/events${q}`).then((r) => r.events ?? []);
  },
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
  deleteTask: (idOrShort: string, reconnect?: boolean): Promise<BoardDeleteResult> =>
    api.delete<BoardDeleteResult>(`/me/tasks/${encodeURIComponent(idOrShort)}${reconnect ? "?reconnect=true" : ""}`),
  pauseTask: (idOrShort: string): Promise<BoardTask> =>
    api.post<BoardTask>(`/me/tasks/${encodeURIComponent(idOrShort)}/pause`, { requestId: newBoardRequestId() }),
  resumeTask: (idOrShort: string): Promise<BoardTask> =>
    api.post<BoardTask>(`/me/tasks/${encodeURIComponent(idOrShort)}/resume`, { requestId: newBoardRequestId() }),
  pauseProject: (idOrShort: string): Promise<BoardProject> =>
    api.post<BoardProject>(`/me/projects/${encodeURIComponent(idOrShort)}/pause`, { requestId: newBoardRequestId() }),
  resumeProject: (idOrShort: string): Promise<BoardProject> =>
    api.post<BoardProject>(`/me/projects/${encodeURIComponent(idOrShort)}/resume`, { requestId: newBoardRequestId() }),
  importPlan: (projectIdOrShort: string, input: BoardPlanInput): Promise<BoardImportResult> =>
    api.post<BoardImportResult>(`/me/projects/${encodeURIComponent(projectIdOrShort)}/import-plan`, { ...input, requestId: newBoardRequestId() }),
  addDependency: (idOrShort: string, dependsOn: string): Promise<{ task: BoardTask }> =>
    api.post<{ task: BoardTask }>(`/me/tasks/${encodeURIComponent(idOrShort)}/dependencies`, {
      dependsOn,
      requestId: newBoardRequestId(),
    }),
  removeDependency: (idOrShort: string, dependsOnId: string): Promise<{ task: BoardTask }> =>
    api.delete<{ task: BoardTask }>(`/me/tasks/${encodeURIComponent(idOrShort)}/dependencies/${encodeURIComponent(dependsOnId)}`),

  // /me/board/config + /me/projects/:id/config (#1832 item 6 — the
  // config surface. Overrides are PARTIAL: absent keys inherit; on the
  // wire an unset key is ABSENT, never null. The PROJECT tier never
  // carries defaultWorkerSlots — a user-level key the server rejects.
  // NOTE: no requestId rides these PUTs — the config body is a strict
  // key-gated map (unknown keys reject 400; the config PUT has no
  // idempotency seam), unlike the entity routes above.
  getUserBoardConfig: (): Promise<BoardConfig> => api.get<BoardConfig>("/me/board/config"),
  setUserBoardConfig: (input: BoardConfigOverride): Promise<BoardConfig> =>
    api.put<BoardConfig>("/me/board/config", input),
  getProjectBoardConfig: (idOrShort: string): Promise<ProjectBoardConfig> =>
    api.get<ProjectBoardConfig>(`/me/projects/${encodeURIComponent(idOrShort)}/config`),
  setProjectBoardConfig: (idOrShort: string, input: ProjectBoardConfigOverrideInput): Promise<ProjectBoardConfig> =>
    api.put<ProjectBoardConfig>(`/me/projects/${encodeURIComponent(idOrShort)}/config`, input),
};

