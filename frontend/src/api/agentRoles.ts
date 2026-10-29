import { api } from "./client";

export interface PermissionRule {
  action: string;
  resource: string;
  effect: string;
}

export interface RoleConfig {
  version?: number;
  system?: string;
  description?: string;
  color?: string;
  model?: string;
  mode?: string;
  hidden?: boolean;
  permissions?: PermissionRule[];
}

export interface AgentRole {
  id: string;
  scope: string;
  orgId?: string;
  name: string;
  slug: string;
  description: string;
  extends?: string;
  isDefault: boolean;
  config: RoleConfig;
  createdAt: string;
  updatedAt: string;
}

export interface EffectiveAgentRole extends AgentRole {
  effectiveConfig: RoleConfig;
  inheritanceChain: string[];
}

export interface CreateAgentRoleRequest {
  name: string;
  slug: string;
  description?: string;
  extends?: string;
  isDefault?: boolean;
  config?: RoleConfig;
}

export interface UpdateAgentRoleRequest {
  name?: string;
  slug?: string;
  description?: string;
  extends?: string;
  isDefault?: boolean;
  config?: RoleConfig;
}

export const agentRolesApi = {
  // Member-readable vocabulary (platform + the acting user's org
  // roles): the board persona editor's dropdown and the stagePersonas
  // roleId validation set. Mounted on the auth group (/auth/me/*).
  listForMe: () => api.get<AgentRole[]>("/auth/me/agent-roles"),

  listPlatform: () => api.get<AgentRole[]>("/admin/agent-roles"),
  createPlatform: (req: CreateAgentRoleRequest) => api.post<AgentRole>("/admin/agent-roles", req),
  getPlatform: (id: string) => api.get<AgentRole>(`/admin/agent-roles/${id}`),
  updatePlatform: (id: string, req: UpdateAgentRoleRequest) => api.put<AgentRole>(`/admin/agent-roles/${id}`, req),
  deletePlatform: (id: string) => api.delete<void>(`/admin/agent-roles/${id}`),

  listOrg: (orgId: string) => api.get<AgentRole[]>(`/orgs/${orgId}/agent-roles`),
  createOrg: (orgId: string, req: CreateAgentRoleRequest) => api.post<AgentRole>(`/orgs/${orgId}/agent-roles`, req),
  getOrg: (orgId: string, roleId: string) => api.get<AgentRole>(`/orgs/${orgId}/agent-roles/${roleId}`),
  updateOrg: (orgId: string, roleId: string, req: UpdateAgentRoleRequest) =>
    api.put<AgentRole>(`/orgs/${orgId}/agent-roles/${roleId}`, req),
  deleteOrg: (orgId: string, roleId: string) => api.delete<void>(`/orgs/${orgId}/agent-roles/${roleId}`),

  getWorkspaceRole: (workspaceId: string) => api.get<AgentRole | null>(`/workspaces/${workspaceId}/agent-role`),
  setWorkspaceRole: (workspaceId: string, roleId: string) =>
    api.put<{ status: string }>(`/workspaces/${workspaceId}/agent-role`, { roleId }),
  clearWorkspaceRole: (workspaceId: string) =>
    api.delete<{ status: string }>(`/workspaces/${workspaceId}/agent-role`),
  getEffectiveWorkspaceRole: (workspaceId: string) =>
    api.get<EffectiveAgentRole>(`/workspaces/${workspaceId}/effective-agent-role`),
};
