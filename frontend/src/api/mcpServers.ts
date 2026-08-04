import { api } from "./client";
import type {
  McpServerResponse,
  CreateMcpServerRequest,
  UpdateMcpServerRequest,
  McpAutoApplyRule,
} from "./mcpServerTypes";

export type { McpServerResponse, CreateMcpServerRequest, UpdateMcpServerRequest, McpAutoApplyRule };

// ---------------------------------------------------------------------------
// Admin (platform) MCP servers — Epic 53 US-53.9
// Routes: /api/v1/admin/mcp-servers
// ---------------------------------------------------------------------------
// list unwraps the {servers: [...]} envelope so callers receive a bare
// array. The backend handler returns gin.H{"servers": out}; without this
// unwrap, setServers(data) stored the envelope object and servers.map threw
// "n.map is not a function" on render.
export const adminMcpServersApi = {
  list: async () => {
    const r = await api.get<{ servers?: McpServerResponse[] } | McpServerResponse[]>("/admin/mcp-servers");
    return Array.isArray(r) ? r : (r.servers ?? []);
  },
  get: (id: string) => api.get<McpServerResponse>(`/admin/mcp-servers/${id}`),
  create: (req: CreateMcpServerRequest) =>
    api.post<McpServerResponse>("/admin/mcp-servers", req),
  update: (id: string, req: UpdateMcpServerRequest) =>
    api.put<McpServerResponse>(`/admin/mcp-servers/${id}`, req),
  delete: (id: string) => api.delete<void>(`/admin/mcp-servers/${id}`),
  bind: (id: string, workspaceId: string) =>
    api.post<void>(`/admin/mcp-servers/${id}/bindings`, { workspaceId }),
  unbind: (id: string, workspaceId: string) =>
    api.delete<void>(`/admin/mcp-servers/${id}/bindings/${workspaceId}`),
  createAutoApply: (id: string, targetType: string, targetId?: string) =>
    api.post<void>(`/admin/mcp-servers/${id}/auto-apply`, { targetType, targetId }),
  listAutoApply: (id: string) =>
    api.get<McpAutoApplyRule[]>(`/admin/mcp-servers/${id}/auto-apply`),
  deleteAutoApply: (id: string, targetType: string) =>
    api.delete<void>(`/admin/mcp-servers/${id}/auto-apply/${targetType}`),
};

// ---------------------------------------------------------------------------
// Org admin MCP servers — Epic 53 US-53.10
// Routes: /api/v1/orgs/:orgId/mcp-servers
// ---------------------------------------------------------------------------
export const orgMcpServersApi = {
  list: async (orgId: string) => {
    const r = await api.get<{ servers?: McpServerResponse[] } | McpServerResponse[]>(`/orgs/${orgId}/mcp-servers`);
    return Array.isArray(r) ? r : (r.servers ?? []);
  },
  get: (orgId: string, id: string) =>
    api.get<McpServerResponse>(`/orgs/${orgId}/mcp-servers/${id}`),
  create: (orgId: string, req: CreateMcpServerRequest) =>
    api.post<McpServerResponse>(`/orgs/${orgId}/mcp-servers`, req),
  update: (orgId: string, id: string, req: UpdateMcpServerRequest) =>
    api.put<McpServerResponse>(`/orgs/${orgId}/mcp-servers/${id}`, req),
  delete: (orgId: string, id: string) =>
    api.delete<void>(`/orgs/${orgId}/mcp-servers/${id}`),
  bind: (orgId: string, id: string, workspaceId: string) =>
    api.post<void>(`/orgs/${orgId}/mcp-servers/${id}/bindings`, { workspaceId }),
  unbind: (orgId: string, id: string, workspaceId: string) =>
    api.delete<void>(`/orgs/${orgId}/mcp-servers/${id}/bindings/${workspaceId}`),
  createAutoApply: (orgId: string, id: string) =>
    api.post<void>(`/orgs/${orgId}/mcp-servers/${id}/auto-apply`, { targetType: "org", targetId: orgId }),
  listAutoApply: (orgId: string, id: string) =>
    api.get<McpAutoApplyRule[]>(`/orgs/${orgId}/mcp-servers/${id}/auto-apply`),
};

// ---------------------------------------------------------------------------
// User (personal) MCP servers — Epic 53 US-53.10b
// Routes: /api/v1/me/mcp-servers
// ---------------------------------------------------------------------------
export const userMcpServersApi = {
  list: async () => {
    const r = await api.get<{ servers?: McpServerResponse[] } | McpServerResponse[]>("/me/mcp-servers");
    return Array.isArray(r) ? r : (r.servers ?? []);
  },
  get: (id: string) => api.get<McpServerResponse>(`/me/mcp-servers/${id}`),
  create: (req: CreateMcpServerRequest) =>
    api.post<McpServerResponse>("/me/mcp-servers", req),
  update: (id: string, req: UpdateMcpServerRequest) =>
    api.put<McpServerResponse>(`/me/mcp-servers/${id}`, req),
  delete: (id: string) => api.delete<void>(`/me/mcp-servers/${id}`),
  bind: (id: string, workspaceId: string) =>
    api.post<void>(`/me/mcp-servers/${id}/bindings`, { workspaceId }),
  unbind: (id: string, workspaceId: string) =>
    api.delete<void>(`/me/mcp-servers/${id}/bindings/${workspaceId}`),
  createAutoApply: (id: string) =>
    api.post<void>(`/me/mcp-servers/${id}/auto-apply`, { targetType: "user" }),
  listAutoApply: (id: string) =>
    api.get<McpAutoApplyRule[]>(`/me/mcp-servers/${id}/auto-apply`),
};
