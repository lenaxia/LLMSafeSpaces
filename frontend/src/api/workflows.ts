import { api } from './client';

export interface Workflow {
  id: string;
  ownerType: string;
  name: string;
  slug: string;
  description: string;
  specYaml: string;
  inputSchema?: unknown;
  targetWorkspaceId?: string;
  onMissingWorkspace?: string;
  status: string;
  defaults?: unknown;
  createdAt: string;
  updatedAt: string;
}

// error/input/output mirror the backend's json.RawMessage wire fields —
// arbitrary JSON payloads, typed as unknown and rendered via JSON.stringify.
export interface WorkflowRun {
  id: string;
  workflowId: string;
  status: string;
  errorCode?: string;
  error?: unknown;
  input?: unknown;
  output?: unknown;
  triggerId?: string;
  workspaceId?: string;
  startedAt?: string;
  finishedAt?: string;
  createdAt: string;
  updatedAt: string;
}

export interface NodeRun {
  id: string;
  nodeId: string;
  nodeType: string;
  status: string;
  attempt: number;
  branch?: string;
  output?: unknown;
  input?: unknown;
  errorCode?: string;
  error?: unknown;
  startedAt: string;
  finishedAt?: string;
}

export interface Trigger {
  id: string;
  name: string;
  description?: string;
  enabled: boolean;
  sourceType: string;
  sourceConfig: unknown;
  workspaceId?: string;
  workflowId?: string;
  prompt?: string;
  agent?: string;
  scriptPath?: string;
  scriptArgs?: string[];
  scriptEnv?: unknown;
  memoryMode?: string;
  memoryMaxRuns?: number;
  captureMode?: string;
  preserveSession?: string;
  consecutiveFailures: number;
  autoDisableAfter: number;
  lastFiredAt?: string;
  nextFireAt?: string;
  createdAt: string;
  updatedAt: string;
}

export interface TriggerFire {
  id: string;
  triggerId: string;
  sourceType: string;
  inputEnvelope?: unknown;
  actionType: string;
  actionResult?: unknown;
  status: string;
  firedAt: string;
  completedAt?: string;
}

export interface WebhookCreateResult {
  trigger: Trigger;
  webhookUrl?: string;
  webhookSecret?: string;
}

export interface SessionOrigin {
  sessionId: string;
  workspaceId?: string;
  origin: string;
  triggerId?: string;
  title?: string;
}

export const workflowApi = {
  list: () => api.get<{ workflows?: Workflow[] }>('/me/workflows').then(r => r.workflows || []),
  get: (id: string) => api.get<Workflow>(`/me/workflows/${id}`),
  create: (data: {
    name: string; specYaml: string; status?: string; description?: string;
    targetWorkspaceId?: string; onMissingWorkspace?: string;
    inputSchema?: unknown; defaults?: unknown;
  }) => api.post<Workflow>('/me/workflows', data),
  update: (id: string, data: Partial<{
    name: string; status: string; specYaml: string; description: string;
    targetWorkspaceId: string; onMissingWorkspace: string;
    inputSchema?: unknown; defaults?: unknown;
  }>) => api.put<Workflow>(`/me/workflows/${id}`, data),
  delete: (id: string) => api.delete(`/me/workflows/${id}`),
  run: (id: string, input?: unknown, workspaceId?: string) =>
    api.post<WorkflowRun>(`/me/workflows/${id}/runs`, { input, workspaceId }),
};

export interface TriggerCreateInput {
  name: string;
  sourceType: string;
  sourceConfig: unknown;
  workspaceId?: string;
  workflowId?: string;
  prompt?: string;
  agent?: string;
  scriptPath?: string;
  scriptArgs?: string[];
  scriptEnv?: unknown;
  memoryMode?: string;
  captureMode?: string;
  preserveSession?: string;
  description?: string;
  enabled?: boolean;
  autoDisableAfter?: number;
  webhookAllowedIps?: string[];
  webhookIdempotencyMode?: string;
  webhookIdempotencyHeader?: string;
}

export interface TriggerUpdateInput {
  name?: string;
  description?: string;
  enabled?: boolean;
  autoDisableAfter?: number;
  sourceConfig?: unknown;
  workspaceId?: string;
  workflowId?: string;
  prompt?: string;
  agent?: string;
  scriptPath?: string;
  scriptArgs?: string[];
  scriptEnv?: unknown;
  memoryMode?: string;
  captureMode?: string;
  preserveSession?: string;
}

export const triggerApi = {
  list: () => api.get<{ triggers?: Trigger[] }>('/me/triggers').then(r => r.triggers || []),
  get: (id: string) => api.get<Trigger>(`/me/triggers/${id}`),
  create: (data: TriggerCreateInput) => api.post<WebhookCreateResult>('/me/triggers', data),
  update: (id: string, data: TriggerUpdateInput) => api.put<Trigger>(`/me/triggers/${id}`, data),
  delete: (id: string) => api.delete(`/me/triggers/${id}`),
  fires: (id: string) =>
    api.get<{ fires?: TriggerFire[] }>(`/me/triggers/${id}/fires`).then((r) => r.fires || []),
  rotateSecret: (id: string) =>
    api.post<{ webhookSecret: string; webhookUrl: string }>(`/me/triggers/${id}/rotate-secret`),
};

export const workspaceWorkflowApi = {
  activeRuns: (workspaceId: string) =>
    api.get<{ runs?: WorkflowRun[] }>(`/workspaces/${workspaceId}/runs/active`).then(r => r.runs || []),
  sessionOrigins: (workspaceId: string) =>
    api.get<{ origins?: SessionOrigin[] }>(`/workspaces/${workspaceId}/session-origins`).then(r => r.origins || []),
};

export const runApi = {
  get: (id: string) => api.get<WorkflowRun>(`/me/runs/${id}`),
  cancel: (id: string) => api.post(`/me/runs/${id}/cancel`),
  nodes: (id: string) => api.get<{ nodes?: NodeRun[] }>(`/me/runs/${id}/nodes`).then((r) => r.nodes || []),
  listForWorkflow: (workflowId: string) =>
    api.get<{ runs?: WorkflowRun[] }>(`/me/workflows/${workflowId}/runs`).then((r) => r.runs || []),
};
