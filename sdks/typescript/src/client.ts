import {
  AuthError,
  ConflictError,
  LLMSafeSpacesError,
  NotFoundError,
  RateLimitError,
  TimeoutError,
} from "./errors.js";
import type {
  ActivateWorkspaceResponse,
  ActiveSessionsResponse,
  APIKey,
  AuthResponse,
  ClientOptions,
  CreateProviderCredentialRequest,
  CreateSecretRequest,
  CreateWorkspaceRequest,
  EnsureSessionResponse,
  FetchFn,
  MessageResponse,
  ProviderCredential,
  SecretResponse,
  SessionListItem,
  TerminalTicket,
  UpdateProviderCredentialRequest,
  User,
  Workspace,
  WorkspaceListResult,
  WorkspaceStatusResult,
  RefreshWorkspaceResult,
} from "./types.js";

const DEFAULT_TIMEOUT = 120_000;

export class LLMSafeSpaces {
  private readonly baseUrl: string;
  private readonly timeout: number;
  private readonly fetchFn: FetchFn;
  private token: string | undefined;
  private apiKey: string | undefined;
  private credentials: { email: string; password: string } | undefined;
  private loggingIn = false;

  public readonly workspaces: WorkspacesAPI;
  public readonly sessions: SessionsAPI;
  public readonly auth: AuthAPI;
  public readonly secrets: SecretsAPI;
  public readonly terminal: TerminalAPI;
  public readonly userSettings: UserSettingsAPI;
  public readonly account: AccountAPI;
  public readonly providerCredentials: ProviderCredentialsAPI;
  public readonly adminProviderCredentials: AdminProviderCredentialsAPI;
  public readonly prompts: PromptsAPI;
  public readonly agentRoles: AgentRolesAPI;

  constructor(options: ClientOptions) {
    this.baseUrl = options.baseUrl.replace(/\/$/, "");
    this.timeout = options.timeout ?? DEFAULT_TIMEOUT;
    this.apiKey = options.apiKey;
    this.credentials = options.credentials;
    this.fetchFn = options.fetch ?? globalThis.fetch.bind(globalThis);

    this.workspaces = new WorkspacesAPI(this);
    this.sessions = new SessionsAPI(this);
    this.auth = new AuthAPI(this);
    this.secrets = new SecretsAPI(this);
    this.terminal = new TerminalAPI(this);
    this.userSettings = new UserSettingsAPI(this);
    this.account = new AccountAPI(this);
    this.providerCredentials = new ProviderCredentialsAPI(this);
    this.adminProviderCredentials = new AdminProviderCredentialsAPI(this);
    this.prompts = new PromptsAPI(this);
    this.agentRoles = new AgentRolesAPI(this);
  }

  /** Internal: make an authenticated request. */
  async request<T>(method: string, path: string, body?: unknown, timeout?: number): Promise<T> {
    const url = `${this.baseUrl}/api/v1${path}`;
    const headers: Record<string, string> = { "Content-Type": "application/json" };

    if (this.apiKey) {
      headers["Authorization"] = `Bearer ${this.apiKey}`;
    } else if (this.token) {
      headers["Authorization"] = `Bearer ${this.token}`;
    } else if (this.credentials && !this.loggingIn) {
      await this.login();
      headers["Authorization"] = `Bearer ${this.token}`;
    }

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeout ?? this.timeout);

    let res: Response;
    try {
      res = await this.fetchFn(url, {
        method,
        headers,
        body: body ? JSON.stringify(body) : undefined,
        signal: controller.signal,
      });
    } catch (e: unknown) {
      clearTimeout(timer);
      if (e instanceof Error && e.name === "AbortError") {
        throw new TimeoutError();
      }
      throw e;
    }
    clearTimeout(timer);

    // Handle 401 with auto-retry if credentials available (token expired)
    if (res.status === 401 && this.credentials && this.token) {
      this.token = undefined;
      return this.request<T>(method, path, body, timeout);
    }

    if (!res.ok) {
      const errBody = await res.json().catch(() => ({ error: res.statusText }));
      const msg = (errBody as { error?: string }).error ?? res.statusText;
      switch (res.status) {
        case 401:
        case 403:
          throw new AuthError(msg, res.status);
        case 404:
          throw new NotFoundError(msg);
        case 409:
          throw new ConflictError(msg);
        case 429:
          throw new RateLimitError(msg);
        default:
          throw new LLMSafeSpacesError(msg, res.status);
      }
    }

    // 204 No Content has no body by definition. 202 Accepted MAY carry a
    // payload describing the accepted operation's status (RFC 7231 §6.3.3),
    // so read the body and return undefined only when it is actually empty
    // (preserving the void contract for endpoints like suspend/restart).
    if (res.status === 204) return undefined as T;
    const text = await res.text();
    if (text === "") return undefined as T;
    return JSON.parse(text) as T;
  }

  private async login(): Promise<void> {
    if (!this.credentials) throw new AuthError("No credentials configured");
    this.loggingIn = true;
    try {
      const url = `${this.baseUrl}/api/v1/auth/login`;
      const res = await this.fetchFn(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(this.credentials),
      });
      if (!res.ok) throw new AuthError("Login failed", res.status);
      const data = (await res.json()) as AuthResponse;
      this.token = data.token;
    } finally {
      this.loggingIn = false;
    }
  }
}

class WorkspacesAPI {
  constructor(private client: LLMSafeSpaces) {}

  list(limit = 20, offset = 0) {
    return this.client.request<WorkspaceListResult>("GET", `/workspaces?limit=${limit}&offset=${offset}`);
  }
  create(req: CreateWorkspaceRequest) {
    return this.client.request<Workspace>("POST", "/workspaces", req);
  }
  get(id: string) {
    return this.client.request<Workspace>("GET", `/workspaces/${id}`);
  }
  rename(id: string, name: string) {
    return this.client.request<void>("PUT", `/workspaces/${id}`, { name });
  }
  delete(id: string) {
    return this.client.request<void>("DELETE", `/workspaces/${id}`);
  }
  getStatus(id: string) {
    return this.client.request<WorkspaceStatusResult>("GET", `/workspaces/${id}/status`);
  }
  activate(id: string) {
    return this.client.request<ActivateWorkspaceResponse>("POST", `/workspaces/${id}/activate`);
  }
  suspend(id: string) {
    return this.client.request<void>("POST", `/workspaces/${id}/suspend`);
  }
  restart(id: string) {
    return this.client.request<void>("POST", `/workspaces/${id}/restart`);
  }
  refreshCompute(id: string) {
    return this.client.request<RefreshWorkspaceResult>("POST", `/workspaces/${id}/refresh-compute`);
  }
  setBindings(id: string, secretIds: string[]) {
    return this.client.request<void>("PUT", `/workspaces/${id}/bindings`, { secretIds });
  }
  getBindings(id: string) {
    return this.client.request<{ bindings: Array<{ id: string; name: string; type: string }> }>(
      "GET", `/workspaces/${id}/bindings`);
  }
  reloadSecrets(id: string) {
    return this.client.request<{ reloaded: number; restarted: boolean }>("POST", `/workspaces/${id}/reload-secrets`);
  }
  setModel(id: string, model: string) {
    return this.client.request<void>("PUT", `/workspaces/${id}/model`, { model });
  }
  getModels(id: string) {
    return this.client.request<{ models: unknown[]; currentModel: string }>("GET", `/workspaces/${id}/models`);
  }
  setEnv(id: string, env: Record<string, string>) {
    return this.client.request<void>("PUT", `/workspaces/${id}/env`, { vars: env });
  }
  getEnv(id: string) {
    return this.client.request<{ vars: string[] }>("GET", `/workspaces/${id}/env`);
  }
  deleteEnv(id: string, varName: string) {
    return this.client.request<void>("DELETE", `/workspaces/${id}/env/${varName}`);
  }
}

class SessionsAPI {
  constructor(private client: LLMSafeSpaces) {}

  ensure(workspaceId: string) {
    return this.client.request<EnsureSessionResponse>("POST", `/workspaces/${workspaceId}/sessions/new`);
  }
  list(workspaceId: string) {
    return this.client.request<SessionListItem[]>("GET", `/workspaces/${workspaceId}/sessions`);
  }
  getActive(workspaceId: string) {
    return this.client.request<ActiveSessionsResponse>("GET", `/workspaces/${workspaceId}/sessions/active`);
  }
  rename(workspaceId: string, sessionId: string, title: string) {
    return this.client.request<void>("PUT", `/workspaces/${workspaceId}/sessions/${sessionId}/title`, { title });
  }
  async sendMessage(workspaceId: string, sessionId: string, content: string): Promise<MessageResponse> {
    const raw = await this.client.request<unknown>(
      "POST",
      `/workspaces/${workspaceId}/sessions/${sessionId}/message`,
      { content, parts: [{ type: "text", text: content }] },
    );
    return { raw, content: extractTextContent(raw) };
  }
  getHistory(workspaceId: string, sessionId: string) {
    return this.client.request<unknown[]>("GET", `/workspaces/${workspaceId}/sessions/${sessionId}/message`);
  }
  abort(workspaceId: string, sessionId: string) {
    return this.client.request<void>("POST", `/workspaces/${workspaceId}/sessions/${sessionId}/abort`);
  }
  get(workspaceId: string, sessionId: string) {
    return this.client.request<Record<string, unknown>>("GET", `/workspaces/${workspaceId}/sessions/${sessionId}`);
  }
  sendPromptAsync(workspaceId: string, sessionId: string, message: string) {
    return this.client.request<void>("POST", `/workspaces/${workspaceId}/sessions/${sessionId}/prompt`, { message });
  }
  delete(workspaceId: string, sessionId: string) {
    return this.client.request<void>("DELETE", `/workspaces/${workspaceId}/sessions/${sessionId}`);
  }
}

class AuthAPI {
  constructor(private client: LLMSafeSpaces) {}

  me() {
    return this.client.request<User>("GET", "/auth/me");
  }
  listApiKeys() {
    return this.client.request<APIKey[]>("GET", "/auth/api-keys");
  }
  createApiKey(name: string) {
    return this.client.request<APIKey>("POST", "/auth/api-keys", { name });
  }
  deleteApiKey(id: string) {
    return this.client.request<void>("DELETE", `/auth/api-keys/${id}`);
  }
}

class SecretsAPI {
  constructor(private client: LLMSafeSpaces) {}

  create(req: CreateSecretRequest) {
    return this.client.request<SecretResponse>("POST", "/secrets", req);
  }
  list() {
    // API wraps in {"secrets": [...]}
    return this.client.request<{ secrets: SecretResponse[] }>("GET", "/secrets")
      .then(r => (r as any)?.secrets ?? r as unknown as SecretResponse[]);
  }
  get(id: string) {
    return this.client.request<SecretResponse>("GET", `/secrets/${id}`);
  }
  update(id: string, value: string) {
    return this.client.request<void>("PUT", `/secrets/${id}`, { value });
  }
  delete(id: string) {
    return this.client.request<void>("DELETE", `/secrets/${id}`);
  }
  reveal(id: string, password: string) {
    return this.client.request<{ value: string }>("POST", `/secrets/${id}/reveal`, { password });
  }
  getAuditLog() {
    return this.client.request<{ entries: unknown[] }>("GET", "/secrets/audit");
  }
  getBindingsForSecret(id: string) {
    return this.client.request<{ workspaces: string[] }>("GET", `/secrets/${id}/bindings`);
  }
}

class TerminalAPI {
  constructor(private client: LLMSafeSpaces) {}

  getTicket(workspaceId: string) {
    return this.client.request<TerminalTicket>("POST", `/workspaces/${workspaceId}/terminal/ticket`);
  }
}

class UserSettingsAPI {
  constructor(private client: LLMSafeSpaces) {}

  get() {
    return this.client.request<{ settings: Record<string, unknown>; schemaVersion: number }>("GET", "/users/me/settings");
  }
  getSchema() {
    return this.client.request<{ settings: unknown[]; schemaVersion: number }>("GET", "/users/me/settings/schema");
  }
  set(key: string, value: unknown) {
    return this.client.request<{ key: string; value: unknown }>("PUT", `/users/me/settings/${key}`, { value });
  }
}

class AccountAPI {
  constructor(private client: LLMSafeSpaces) {}

  rotateKey(password: string) {
    return this.client.request<{ keyVersion: number; recoveryKey: string }>("POST", "/account/rotate-key", { password });
  }
  changePassword(oldPassword: string, newPassword: string) {
    return this.client.request<void>("POST", "/account/change-password", { oldPassword, newPassword });
  }
  recover(userId: string, recoveryKey: string, newPassword: string) {
    return this.client.request<{ recoveryKey: string }>("POST", "/account/recover", { userId, recoveryKey, newPassword });
  }
}

class ProviderCredentialsAPI {
  constructor(private client: LLMSafeSpaces) {}

  create(req: CreateProviderCredentialRequest) {
    return this.client.request<ProviderCredential>("POST", "/provider-credentials", req);
  }
  list() {
    return this.client.request<ProviderCredential[]>("GET", "/provider-credentials");
  }
  get(id: string) {
    return this.client.request<ProviderCredential>("GET", `/provider-credentials/${id}`);
  }
  delete(id: string) {
    return this.client.request<void>("DELETE", `/provider-credentials/${id}`);
  }
  probeModels(id: string) {
    return this.client.request<{ models: unknown[] }>("GET", `/provider-credentials/${id}/models`);
  }
  listBindings(id: string) {
    return this.client.request<string[]>("GET", `/provider-credentials/${id}/bindings`);
  }
  bind(credId: string, workspaceId: string) {
    return this.client.request<unknown>("POST", `/provider-credentials/${credId}/bind/${workspaceId}`);
  }
  unbind(credId: string, workspaceId: string) {
    return this.client.request<void>("DELETE", `/provider-credentials/${credId}/bind/${workspaceId}`);
  }
}

class AdminProviderCredentialsAPI {
  constructor(private client: LLMSafeSpaces) {}

  list() {
    return this.client.request<ProviderCredential[]>("GET", "/admin/provider-credentials");
  }
  create(req: CreateProviderCredentialRequest) {
    return this.client.request<ProviderCredential>("POST", "/admin/provider-credentials", req);
  }
  get(id: string) {
    return this.client.request<ProviderCredential>("GET", `/admin/provider-credentials/${id}`);
  }
  update(id: string, req: UpdateProviderCredentialRequest) {
    return this.client.request<ProviderCredential>("PUT", `/admin/provider-credentials/${id}`, req);
  }
  delete(id: string) {
    return this.client.request<void>("DELETE", `/admin/provider-credentials/${id}`);
  }
  probeModels(id: string) {
    return this.client.request<{ models: unknown[] }>("GET", `/admin/provider-credentials/${id}/models`);
  }
  createAutoApply(id: string, req: { targetType: string; targetId?: string }) {
    return this.client.request<unknown>("POST", `/admin/provider-credentials/${id}/auto-apply`, req);
  }
  listAutoApply(id: string) {
    return this.client.request<unknown[]>("GET", `/admin/provider-credentials/${id}/auto-apply`);
  }
  deleteAutoApply(id: string, targetType: string, targetId: string) {
    return this.client.request<void>("DELETE", `/admin/provider-credentials/${id}/auto-apply/${targetType}/${targetId}`);
  }
}

/** Extract text content from opencode response parts. */
function extractTextContent(raw: unknown): string {
  if (!raw || typeof raw !== "object") return "";
  const obj = raw as { parts?: Array<{ type?: string; text?: string }> };
  if (!Array.isArray(obj.parts)) return "";
  return obj.parts
    .filter((p) => p.type === "text" && p.text)
    .map((p) => p.text!)
    .join("");
}

class PromptsAPI {
  constructor(private client: LLMSafeSpaces) {}

  getPlatform() {
    return this.client.request<{ prompt: string }>("GET", "/admin/prompt");
  }

  setPlatform(prompt: string) {
    return this.client.request<void>("PUT", "/admin/prompt", { prompt });
  }

  getOrg(orgId: string) {
    return this.client.request<{ prompt: string; allowUserPrompt: boolean }>("GET", `/orgs/${orgId}/prompt`);
  }

  setOrg(orgId: string, body: { prompt?: string; allowUserPrompt?: boolean }) {
    return this.client.request<void>("PUT", `/orgs/${orgId}/prompt`, body);
  }

  getWorkspace(workspaceId: string) {
    return this.client.request<{ prompt: string }>("GET", `/workspaces/${workspaceId}/prompt`);
  }

  setWorkspace(workspaceId: string, prompt: string) {
    return this.client.request<void>("PUT", `/workspaces/${workspaceId}/prompt`, { prompt });
  }
}

class AgentRolesAPI {
  constructor(private client: LLMSafeSpaces) {}

  listPlatform() {
    return this.client.request<unknown[]>("GET", "/admin/agent-roles");
  }

  createPlatform(body: Record<string, unknown>) {
    return this.client.request<unknown>("POST", "/admin/agent-roles", body);
  }

  getPlatform(roleId: string) {
    return this.client.request<unknown>("GET", `/admin/agent-roles/${roleId}`);
  }

  updatePlatform(roleId: string, body: Record<string, unknown>) {
    return this.client.request<unknown>("PUT", `/admin/agent-roles/${roleId}`, body);
  }

  deletePlatform(roleId: string) {
    return this.client.request<void>("DELETE", `/admin/agent-roles/${roleId}`);
  }

  listOrg(orgId: string) {
    return this.client.request<unknown[]>("GET", `/orgs/${orgId}/agent-roles`);
  }

  createOrg(orgId: string, body: Record<string, unknown>) {
    return this.client.request<unknown>("POST", `/orgs/${orgId}/agent-roles`, body);
  }

  getOrg(orgId: string, roleId: string) {
    return this.client.request<unknown>("GET", `/orgs/${orgId}/agent-roles/${roleId}`);
  }

  updateOrg(orgId: string, roleId: string, body: Record<string, unknown>) {
    return this.client.request<unknown>("PUT", `/orgs/${orgId}/agent-roles/${roleId}`, body);
  }

  deleteOrg(orgId: string, roleId: string) {
    return this.client.request<void>("DELETE", `/orgs/${orgId}/agent-roles/${roleId}`);
  }

  getWorkspaceRole(workspaceId: string) {
    return this.client.request<unknown | null>("GET", `/workspaces/${workspaceId}/agent-role`);
  }

  setWorkspaceRole(workspaceId: string, roleId: string) {
    return this.client.request<void>("PUT", `/workspaces/${workspaceId}/agent-role`, { roleId });
  }

  clearWorkspaceRole(workspaceId: string) {
    return this.client.request<void>("DELETE", `/workspaces/${workspaceId}/agent-role`);
  }

  getEffectiveWorkspaceRole(workspaceId: string) {
    return this.client.request<unknown>("GET", `/workspaces/${workspaceId}/effective-agent-role`);
  }
}
