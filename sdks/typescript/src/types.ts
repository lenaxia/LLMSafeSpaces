/** SDK configuration options. */
export type FetchFn = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

export interface ClientOptions {
  baseUrl: string;
  apiKey?: string;
  credentials?: { email: string; password: string };
  timeout?: number;
  fetch?: FetchFn;
}

/** Workspace resource. */
export interface Workspace {
  id: string;
  name: string;
  userId: string;
  runtime: string;
  storageSize: string;
  phase: string;
  pvcName?: string;
  labels?: Record<string, string>;
  createdAt: string;
  updatedAt: string;
}

export interface CreateWorkspaceRequest {
  name?: string;
  runtime?: string;
  storageSize?: string;
  storageClass?: string;
  labels?: Record<string, string>;
}

export interface WorkspaceListResult {
  items: WorkspaceListItem[];
  pagination?: PaginationMetadata;
}

export interface WorkspaceListItem {
  id: string;
  name: string;
  userId: string;
  runtime: string;
  storageSize: string;
  phase?: string;
  maxActiveSessions?: number;
  createdAt: string;
  updatedAt: string;
}

export interface PaginationMetadata {
  total: number;
  start: number;
  end: number;
  limit: number;
  offset: number;
}

export interface WorkspaceStatusResult {
  phase: string;
  pvcName?: string;
  activeSessions: number;
  lastActivityAt?: string;
  message?: string;
  conditions?: WorkspaceCondition[];
  credentialState: { available: boolean; reason?: string; message?: string };
  agentHealth: { status: string; providersConfigured: number; agentVersion?: string };
  sessions?: { id: string; title?: string; status: string }[];
  diskUsedBytes?: number;
  diskTotalBytes?: number;
}

export interface WorkspaceCondition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
}

export interface ActivateWorkspaceResponse {
  resumed: string;
  suspended?: string;
}

export interface RefreshWorkspaceResult {
  restartGeneration: number;
}

export interface EnsureSessionResponse {
  workspaceId: string;
  workspacePhase: string;
  sessionId: string;
  resumed: boolean;
}

export interface SessionListItem {
  id: string;
  title?: string;
  lastMessageAt?: string;
  messageCount: number;
  status: string;
}

export interface ActiveSessionsResponse {
  active: string[];
  maxActive: number;
}

/** Opencode message response (proxy passthrough). */
export interface MessageResponse {
  raw: unknown;
  content: string;
}

export interface AuthResponse {
  token: string;
  user: User;
}

export interface User {
  id: string;
  username: string;
  email: string;
  createdAt: string;
  updatedAt: string;
  active: boolean;
  role: string;
}

export interface APIKey {
  id: string;
  name: string;
  key?: string;
  prefix: string;
  active: boolean;
  createdAt: string;
  expiresAt?: string;
}

export interface TerminalTicket {
  ticket: string;
  expiresAt: string;
}

export interface SecretResponse {
  id: string;
  name: string;
  type: string;
  metadata?: unknown;
  createdAt: string;
  updatedAt: string;
}

/** Regex pattern for valid secret names. Keep in sync with pkg/validation/name.go. */
export const SECRET_NAME_PATTERN = /^[a-z0-9._-]+$/;

export interface CreateSecretRequest {
  /** Lowercase alphanumeric, dots, underscores, hyphens only. Must not start with dot or hyphen. */
  name: string;
  type: "api-key" | "ssh-key" | "git-credential" | "secret-file" | "env-secret";
  value: string;
  metadata?: unknown;
}

// --- Provider credentials (US-62.4) ---

export interface ProviderCredential {
  id: string;
  name: string;
  kind: string;
  slug: string;
  baseURL?: string;
  modelAllowlist?: string[];
  modelContextLimits?: Record<string, number>;
  modelOutputLimits?: Record<string, number>;
  createdAt: string;
  updatedAt: string;
}

export interface CreateProviderCredentialRequest {
  name: string;
  kind: string;
  slug: string;
  apiKey: string;
  baseURL?: string;
}

export interface UpdateProviderCredentialRequest {
  name?: string;
  apiKey?: string;
  baseURL?: string;
  modelAllowlist?: string[];
  modelContextLimits?: Record<string, number>;
  modelOutputLimits?: Record<string, number>;
}

// --- Queued message (US-62.6) ---

export interface QueuedMessage {
  id: string;
  text: string;
  session_id: string;
  workspace_id: string;
  enqueued_at: string;
  retry_count: number;
}
