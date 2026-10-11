import { api } from "./client";

export interface SecretResponse {
  id: string;
  name: string;
  type: "llm-provider" | "api-key" | "ssh-key" | "git-credential" | "secret-file" | "env-secret";
  metadata: Record<string, string>;
  globalDefault: boolean;
  createdAt: string;
  updatedAt: string;
}

export interface CreateSecretRequest {
  name: string;
  type: SecretResponse["type"];
  value: string;
  metadata?: Record<string, string>;
  globalDefault?: boolean;
}

export interface UpdateSecretRequest {
  value: string;
  metadata?: Record<string, string>;
  globalDefault?: boolean;
}

export const secretsApi = {
  list: () => api.get<{ secrets: SecretResponse[] }>("/secrets"),
  get: (id: string) => api.get<SecretResponse>(`/secrets/${id}`),
  create: (req: CreateSecretRequest) => api.post<SecretResponse>("/secrets", req),
  update: (id: string, req: UpdateSecretRequest) => api.put<void>(`/secrets/${id}`, req),
  delete: (id: string) => api.delete<void>(`/secrets/${id}`),
  reveal: (id: string, password: string) => api.post<{ value: string }>(`/secrets/${id}/reveal`, { password }),
  getSecretBindings: (id: string) => api.get<{ workspaces: string[] }>(`/secrets/${id}/bindings`),
  audit: () => api.get<{ entries: { action: string; timestamp: string; metadata: Record<string, string> }[] }>("/secrets/audit"),
};
