import { api } from "./client";
import type {
  ActivateWorkspaceResponse,
  SessionListItem,
  WorkspaceListResponse,
  WorkspaceStatus,
  AgentSession,
} from "./types";

export interface EnsureSessionResponse {
  workspaceId: string;
  workspacePhase: string;
  sessionId: string;
  resumed: boolean;
}

// D6 (#998): one persisted hung-session escalation record.
export interface SessionAlert {
  id: string;
  workspaceId: string;
  sessionId: string;
  alert: string;
  oldestBusySeconds: number;
  createdAt: string;
}

export interface ModelInfo {
  id: string;
  providerID: string;
  name: string;
  tier: string;
  freeTier: boolean;
  selected: boolean;
  enabled: boolean;
  details?: unknown;
}

export interface ListModelsResponse {
  models: ModelInfo[];
  currentModel: string;
  // Resolved providerID for currentModel. Empty string signals an ambiguous
  // collision (two connected providers expose the same model ID); in that case
  // the frontend falls back to find() on the models array.
  currentModelProviderID?: string;
}

export const workspacesApi = {
  list: () => api.get<WorkspaceListResponse>("/workspaces"),
  create: (params: { name: string; runtime?: string; orgId?: string; imageConfigHash?: string }) =>
    api.post<{ id: string; name: string; workspaceId?: string }>("/workspaces", {
      name: params.name,
      // hierarchy resolves at backend
      ...(params.runtime ? { runtime: params.runtime } : {}),
      ...(params.orgId ? { orgId: params.orgId } : {}),
      ...(params.imageConfigHash ? { imageConfigHash: params.imageConfigHash } : {}),
    }),
  createWorkspace: (workspaceId: string, runtime = "base") =>
    api.post<{ id: string }>("/workspaces", { runtime, workspaceRef: workspaceId }),
  ensureSession: (workspaceId: string) =>
    api.post<EnsureSessionResponse>(`/workspaces/${workspaceId}/sessions/new`),
  getStatus: (id: string) => api.get<WorkspaceStatus>(`/workspaces/${id}/status`),
  activate: (id: string) => api.post<ActivateWorkspaceResponse>(`/workspaces/${id}/activate`),
  suspend: (id: string) => api.post<void>(`/workspaces/${id}/suspend`),
  refreshCompute: (id: string) =>
    api.post<{ restartGeneration: number }>(`/workspaces/${id}/refresh-compute`),
  getSessions: (id: string) => api.get<SessionListItem[]>(`/workspaces/${id}/sessions`),
  // D6 (#998): persisted hung-session alerts (24h retention server-side).
  getAlerts: (id: string) =>
    api.get<{ alerts: SessionAlert[] }>(`/workspaces/${id}/alerts`).then((r) => r.alerts ?? []),
  getSession: (workspaceId: string, sessionId: string, opts?: { signal?: AbortSignal }) =>
    api.get<AgentSession>(`/workspaces/${workspaceId}/sessions/${sessionId}`, { signal: opts?.signal }),
  renameSession: (workspaceId: string, sessionId: string, title: string) =>
    api.put<void>(`/workspaces/${workspaceId}/sessions/${sessionId}/title`, { title }),
  markSessionSeen: (workspaceId: string, sessionId: string) =>
    api.put<void>(`/workspaces/${workspaceId}/sessions/${sessionId}/seen`),
  renameWorkspace: (workspaceId: string, name: string) =>
    api.put<void>(`/workspaces/${workspaceId}`, { name }),
  deleteWorkspace: (workspaceId: string) =>
    api.delete<void>(`/workspaces/${workspaceId}`),
  deleteSession: (workspaceId: string, sessionId: string) =>
    api.delete<void>(`/workspaces/${workspaceId}/sessions/${sessionId}`),
  abortSession: (workspaceId: string, sessionId: string) =>
    api.post<void>(`/workspaces/${workspaceId}/sessions/${sessionId}/abort`),
  reloadAgent: (workspaceId: string) =>
    api.post<{ disposed: boolean; lastDisposedAt?: string; warning?: string }>(
      `/workspaces/${workspaceId}/agent/reload`
    ),
  listModels: (workspaceId: string) =>
    api.get<ListModelsResponse>(`/workspaces/${workspaceId}/models`),
  setModel: (workspaceId: string, model: string) =>
    api.put<{ model: string; applied: boolean }>(`/workspaces/${workspaceId}/model`, { model }),
};
