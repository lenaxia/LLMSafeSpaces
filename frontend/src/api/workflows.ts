import { api } from './client';

export interface Workflow {
  id: string;
  ownerType: string;
  name: string;
  slug: string;
  description: string;
  specYaml: string;
  status: string;
  createdAt: string;
  updatedAt: string;
}

export interface WorkflowRun {
  id: string;
  workflowId: string;
  status: string;
  errorCode?: string;
  error?: any;
  input?: any;
  output?: any;
  startedAt?: string;
  finishedAt?: string;
  createdAt: string;
}

export interface NodeRun {
  id: string;
  nodeId: string;
  nodeType: string;
  status: string;
  attempt: number;
  output?: any;
  errorCode?: string;
  startedAt: string;
  finishedAt?: string;
}

export interface Trigger {
  id: string;
  name: string;
  enabled: boolean;
  sourceType: string;
  sourceConfig: any;
  targetType: string;
  targetConfig: any;
  consecutiveFailures: number;
  autoDisableAfter: number;
  lastFiredAt?: string;
  nextFireAt?: string;
}

export const workflowApi = {
  list: () => api.get<{ workflows?: Workflow[] }>('/me/workflows').then(r => r.workflows || []),
  get: (id: string) => api.get(`/me/workflows/${id}`),
  create: (data: { name: string; specYaml: string; status?: string }) =>
    api.post('/me/workflows', data),
  update: (id: string, data: Partial<{ name: string; status: string; specYaml: string }>) =>
    api.put(`/me/workflows/${id}`, data),
  delete: (id: string) => api.delete(`/me/workflows/${id}`),
  run: (id: string, input?: unknown, workspaceId?: string) =>
    api.post(`/me/workflows/${id}/runs`, { input, workspaceId }),
};

export const triggerApi = {
  list: () => api.get<{ triggers?: unknown[] }>('/me/triggers').then(r => r.triggers || []),
  get: (id: string) => api.get(`/me/triggers/${id}`),
  create: (data: { name: string; sourceType: string; targetType: string; sourceConfig: unknown; targetConfig: unknown }) =>
    api.post('/me/triggers', data),
  update: (id: string, data: Partial<{ enabled: boolean; autoDisableAfter: number }>) =>
    api.put(`/me/triggers/${id}`, data),
  delete: (id: string) => api.delete(`/me/triggers/${id}`),
};

export const runApi = {
  get: (id: string) => api.get(`/me/runs/${id}`),
  cancel: (id: string) => api.post(`/me/runs/${id}/cancel`),
  nodes: (id: string) => api.get(`/me/runs/${id}/nodes`).then((r: any) => r.nodes || []),
  listForWorkflow: (workflowId: string) => api.get(`/me/workflows/${workflowId}/runs`).then((r: any) => r.runs || []),
};
