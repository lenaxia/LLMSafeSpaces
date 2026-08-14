import {
  AuthError,
  ConflictError,
  LLMSafeSpacesError,
  NotFoundError,
  RateLimitError,
  ServiceUnavailableError,
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
  QueuedMessage,
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
  readonly baseUrl: string;
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
  public readonly usage: UsageAPI;
  public readonly inputRequests: InputRequestsAPI;
  public readonly probe: ProbeAPI;
  public readonly prompts: PromptsAPI;
  public readonly agentRoles: AgentRolesAPI;
  public readonly workflows: WorkflowsAPI;
  public readonly triggers: TriggersAPI;

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
    this.usage = new UsageAPI(this);
    this.inputRequests = new InputRequestsAPI(this);
    this.probe = new ProbeAPI(this);
    this.prompts = new PromptsAPI(this);
    this.agentRoles = new AgentRolesAPI(this);
    this.workflows = new WorkflowsAPI(this);
    this.triggers = new TriggersAPI(this);
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
        case 503: {
          const reason = (errBody as { reason?: string }).reason;
          const retryAfter = (errBody as { retryAfter?: number }).retryAfter;
          const apiMessage = (errBody as { message?: string }).message ?? msg;
          throw new ServiceUnavailableError(apiMessage, reason, retryAfter);
        }
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
  setDevPreview(id: string, enabled: boolean) {
    return this.client.request<void>("PUT", `/workspaces/${id}/dev-preview`, { enabled });
  }
  /**
   * Returns the URL to open the dev-preview proxy in a browser.
   * The URL is authenticated via session cookie; no token in the URL.
   * The dev server must be running on `port` inside the workspace.
   */
  devPreviewUrl(id: string, port: number, path = "/"): string {
    const p = path.startsWith("/") ? path : `/${path}`;
    return `${this.client.baseUrl}/api/v1/workspaces/${id}/dev-preview/${port}${p}`;
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
  enqueue(workspaceId: string, sessionId: string, text: string) {
    return this.client.request<{ messageID: string }>("POST", `/workspaces/${workspaceId}/sessions/${sessionId}/queue`, { text });
  }
  /**
   * @deprecated Under the V2 session-queue model (Epic 63), the queue is
   * inboard in opencode and this endpoint returns a best-effort shadow
   * derived from SSE events. Subscribe to the workspace SSE stream and
   * track `queue.update` events instead. Removed in next major.
   */
  listQueue(workspaceId: string, sessionId: string) {
    return this.client.request<{ messages: QueuedMessage[] }>("GET", `/workspaces/${workspaceId}/sessions/${sessionId}/queue`);
  }
  /**
   * @deprecated Under the V2 session-queue model (Epic 63), abort is
   * non-destructive and queued messages survive. This removes from the
   * best-effort shadow only; it does not revoke the durable input.
   * Removed in next major.
   */
  dismissQueued(workspaceId: string, sessionId: string, messageId: string) {
    return this.client.request<void>("DELETE", `/workspaces/${workspaceId}/sessions/${sessionId}/queue/${messageId}`);
  }
  markSeen(workspaceId: string, sessionId: string) {
    return this.client.request<void>("PUT", `/workspaces/${workspaceId}/sessions/${sessionId}/seen`);
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

}

class ProviderCredentialsAPI {
  constructor(private client: LLMSafeSpaces) {}

  create(req: CreateProviderCredentialRequest) {
    return this.client.request<ProviderCredential | { credential: ProviderCredential; bindWarning: string }>("POST", "/provider-credentials", req).then((data) => {
      if (data && typeof data === "object" && "credential" in data) {
        return (data as { credential: ProviderCredential }).credential;
      }
      return data as ProviderCredential;
    });
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
    return this.client.request<{ workspaceIds: string[]; bindings: unknown[] }>("GET", `/provider-credentials/${id}/bindings`).then((data) => data.workspaceIds);
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
  createAutoApply(id: string, req: { targetType: string; targetId?: string; withinPriority?: number }) {
    return this.client.request<unknown>("POST", `/admin/provider-credentials/${id}/auto-apply`, req);
  }
  listAutoApply(id: string) {
    return this.client.request<unknown[]>("GET", `/admin/provider-credentials/${id}/auto-apply`);
  }
  deleteAutoApply(id: string, targetType: string, targetId: string) {
    return this.client.request<void>("DELETE", `/admin/provider-credentials/${id}/auto-apply/${targetType}/${targetId}`);
  }
}

class UsageAPI {
  constructor(private client: LLMSafeSpaces) {}
  get() { return this.client.request<Record<string, unknown>>("GET", "/usage"); }
  getWorkspace(workspaceId: string) { return this.client.request<Record<string, unknown>>("GET", `/usage/workspaces/${workspaceId}`); }
  getQuota() { return this.client.request<Record<string, unknown>>("GET", "/usage/quota"); }
}

class InputRequestsAPI {
  constructor(private client: LLMSafeSpaces) {}
  listQuestions(workspaceId: string) { return this.client.request<unknown[]>("GET", `/workspaces/${workspaceId}/question`); }
  replyQuestion(workspaceId: string, requestId: string, body: Record<string, unknown>) { return this.client.request<void>("POST", `/workspaces/${workspaceId}/question/${requestId}/reply`, body); }
  rejectQuestion(workspaceId: string, requestId: string) { return this.client.request<void>("POST", `/workspaces/${workspaceId}/question/${requestId}/reject`); }
  listPermissions(workspaceId: string) { return this.client.request<unknown[]>("GET", `/workspaces/${workspaceId}/permission`); }
  replyPermission(workspaceId: string, requestId: string, body: Record<string, unknown>) { return this.client.request<void>("POST", `/workspaces/${workspaceId}/permission/${requestId}/reply`, body); }
}

class ProbeAPI {
  constructor(private client: LLMSafeSpaces) {}
  probeModels(apiKey: string, baseURL: string) { return this.client.request<{ models: unknown[] }>("POST", "/probe-models", { apiKey, baseURL }); }
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

// ─── Epic 64: Workflows + Triggers ────────────────────────────────────────────

export interface WorkflowResponse {
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

export interface WorkflowRunResponse {
  id: string;
  workflowId: string;
  status: string;
  errorCode?: string;
  input?: unknown;
  output?: unknown;
  startedAt?: string;
  finishedAt?: string;
  createdAt: string;
}

export interface TriggerResponse {
  id: string;
  name: string;
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
  captureMode?: string;
  preserveSession?: string;
  consecutiveFailures: number;
  autoDisableAfter: number;
  lastFiredAt?: string;
  nextFireAt?: string;
}

class WorkflowsAPI {
  constructor(private readonly client: LLMSafeSpaces) {}

  list() {
    return this.client.request<{ workflows: WorkflowResponse[] }>("GET", "/me/workflows");
  }

  get(id: string) {
    return this.client.request<WorkflowResponse>("GET", `/me/workflows/${id}`);
  }

  create(req: { name: string; specYaml: string; status?: string }) {
    return this.client.request<WorkflowResponse>("POST", "/me/workflows", req);
  }

  update(id: string, req: { name?: string; status?: string; specYaml?: string }) {
    return this.client.request<WorkflowResponse>("PUT", `/me/workflows/${id}`, req);
  }

  delete(id: string) {
    return this.client.request<void>("DELETE", `/me/workflows/${id}`);
  }

  run(id: string, input?: unknown, workspaceId?: string) {
    return this.client.request<WorkflowRunResponse>("POST", `/me/workflows/${id}/runs`, { input, workspaceId });
  }

  getRun(runId: string) {
    return this.client.request<WorkflowRunResponse>("GET", `/me/runs/${runId}`);
  }

  cancelRun(runId: string) {
    return this.client.request<void>("POST", `/me/runs/${runId}/cancel`);
  }
}

class TriggersAPI {
  constructor(private readonly client: LLMSafeSpaces) {}

  list() {
    return this.client.request<{ triggers: TriggerResponse[] }>("GET", "/me/triggers");
  }

  create(req: {
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
  }) {
    return this.client.request<TriggerResponse>("POST", "/me/triggers", req);
  }

  update(id: string, req: { enabled?: boolean; autoDisableAfter?: number }) {
    return this.client.request<TriggerResponse>("PUT", `/me/triggers/${id}`, req);
  }

  delete(id: string) {
    return this.client.request<void>("DELETE", `/me/triggers/${id}`);
  }
}
