import { api } from "./client";

// OrgPolicy mirrors types.OrgPolicy. Value is the raw JSONB payload — a bool
// for allow_* keys, a number for max_* keys, a string for sys_prompt_org,
// or []string for allowed_models/providers.
export interface OrgPolicy {
  key: string;
  value: unknown;
  updatedAt: string;
}

export const policiesApi = {
  // GET /api/v1/orgs/:id/policies — returns all configured policies.
  listOrg: (orgId: string) => api.get<OrgPolicy[]>(`/orgs/${orgId}/policies`),

  // PUT /api/v1/orgs/:id/policies/:key — body is the raw JSON value
  // (e.g. true / 5 / "..." / [...]). The backend binds it as json.RawMessage.
  setOrg: (orgId: string, key: string, value: unknown) =>
    api.put<{ status: string }>(`/orgs/${orgId}/policies/${key}`, value),
};
