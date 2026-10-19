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
  /**
   * Present and true when the session is archived (read-only: chat
   * sends are rejected with 409 session_archived; history stays
   * viewable; unarchiving restores chat instantly). ABSENT means not
   * archived.
   */
  archived?: boolean;
}

/**
 * The platform-owned view of one agent session (pkg/session contract,
 * design 0049) — the getSession response shape.
 */
export interface Session {
  id: string;
  workspaceId: string;
  parentId?: string;
  title?: string;
  agentId?: string;
  model?: ModelRef;
  status: "unknown" | "idle" | "busy" | "error" | "compacting" | "archived";
  cost?: Cost;
  contextUsage?: ContextUsage;
  time?: TimeRange;
  summary?: string;
  archived?: boolean;
}

/** The session's live context occupancy (non-monotonic: compaction resets it). */
export interface ContextUsage {
  used: number;
  window?: number;
}

/** Bounds a session or message. completedAt is absent while busy. */
export interface TimeRange {
  startedAt: string;
  completedAt?: string;
}

/**
 * The delivery-outbox receipt for an async prompt: the 202 body of a
 * fresh accept, and the 200 body of a retried clientMessageID (which
 * echoes the ORIGINAL accepted entry with status "duplicate").
 */
export interface PromptAccepted {
  messageID: string;
  clientMessageID?: string;
  status: "queued" | "duplicate";
}

export interface ActiveSessionsResponse {
  active: string[];
  maxActive: number;
}

/**
 * One entry in a session transcript (pkg/session contract, design 0049).
 * Flat discriminated struct: type selects which fields are meaningful.
 */
export interface Message {
  id: string;
  sessionId?: string;
  type: "user" | "assistant" | "shell" | "agent_switch" | "model_switch" | "compaction" | "system";
  createdAt?: string;
  parts?: Part[];
  model?: ModelRef;
  cost?: Cost;
  /** Plain-text form (user/system/compaction messages). */
  text?: string;
  /** Shell-message fields. */
  command?: string;
  exitCode?: number;
  /** Agent/model-switch fields. */
  fromAgent?: string;
  toAgent?: string;
  fromModel?: ModelRef;
  toModel?: ModelRef;
  error?: { code?: string; message: string };
}

/** One renderable part of a message — the closed 5-type union. */
export interface Part {
  type: "text" | "reasoning" | "tool" | "file_change" | "custom";
  id?: string;
  text?: string;
  reasoning?: string;
  tool?: ToolPart;
  fileChange?: FileDiff;
  custom?: { kind: string; data?: unknown };
}

/** Every tool call is a ToolPart discriminated by name. */
export interface ToolPart {
  callId?: string;
  name: string;
  input?: unknown;
  output?: unknown;
  state: {
    status: "pending" | "running" | "completed" | "error";
    error?: string;
    startedAt?: string;
    completedAt?: string;
  };
}

/** Unified-diff payload of a file-change part (patch text is authoritative). */
export interface FileDiff {
  path: string;
  oldPath?: string;
  status: "added" | "modified" | "deleted" | "renamed";
  patch: string;
  additions?: number;
  deletions?: number;
}

/** The unified pending-input shape ("the agent needs a human"). */
export interface InputRequest {
  id: string;
  sessionId?: string;
  rootSessionId?: string;
  kind: "question" | "permission";
  question?: string;
  header?: string;
  options?: InputOption[];
  multiple?: boolean;
  custom?: boolean;
  permission?: string;
  patterns?: string[];
  always?: string[];
  metadata?: Record<string, unknown>;
  tool?: ToolRef;
}

export interface InputOption {
  label: string;
  description?: string;
}

export interface ToolRef {
  messageId?: string;
  callId?: string;
}

/**
 * The 202 body for a reply that landed as a late answer through the
 * delivery outbox (#1313): the ask was no longer live, so the answer
 * rides a Q&A user message instead of the live ask.
 */
export interface InboxLateAnswerAccepted {
  status: "queued";
  /** Ask-scoped dedupe key (`inbox-{requestID}-answer`); duplicate clicks return the original. */
  clientMessageID: string;
  /** The outbox entry ID (202) or the original accepted entry (duplicate). */
  messageID: string;
  /** Present and true when the ask-scoped key was already accepted. */
  duplicate?: boolean;
}

export interface ModelRef {
  id: string;
  provider?: string;
}

/** Display-only token/cost data (never billing). */
export interface Cost {
  inputTokens?: number;
  outputTokens?: number;
  reasoningTokens?: number;
  cacheReadTokens?: number;
  cacheWriteTokens?: number;
  totalTokens?: number;
  costUsd?: number;
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
  globalDefault: boolean;
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

/**
 * A message waiting in the session queue.
 *
 * Under the V2 session-queue model (Epic 63), this is a best-effort shadow
 * derived from SSE events. `retry_count` is vestigial (V2 has no retry —
 * opencode handles durability internally).
 */
export interface QueuedMessage {
  id: string;
  text: string;
  session_id: string;
  workspace_id: string;
  enqueued_at: string;
  retry_count: number;
}

/**
 * Result of a workspace file upload (Epic 68): the absolute path of the
 * stored file on the workspace PVC (/workspace/uploads/&lt;uuid&gt;-&lt;name&gt;),
 * its sanitized name, and the stored byte count.
 */
export interface FileUpload {
  path: string;
  name: string;
  size: number;
}

// MCP server management (Epic 53). Secrets (env/headers) are write-only —
// responses carry hasSecret only.
export interface McpServer {
  id: string;
  name: string;
  transport: "http" | "sse" | "stdio";
  url?: string;
  command?: string;
  args?: string[];
  timeoutMs?: number;
  hasSecret: boolean;
  enabled: boolean;
  createdAt?: string;
  updatedAt?: string;
}

export interface CreateMcpServerRequest {
  name: string;
  transport: "http" | "sse" | "stdio";
  url?: string;
  command?: string;
  args?: string[];
  timeoutMs?: number;
  enabled?: boolean;
  env?: Record<string, string>;
  headers?: Record<string, string>;
  autoApply?: { targetType: string; targetId?: string };
}

export interface UpdateMcpServerRequest {
  name?: string;
  url?: string;
  command?: string;
  args?: string[];
  timeoutMs?: number;
  enabled?: boolean;
  env?: Record<string, string>;
  headers?: Record<string, string>;
}

export interface McpAutoApplyRule {
  targetType: string;
  targetId?: string;
}

/** One saved prompt row (#1499) — the caller's prompt library. */
export interface UserPrompt {
  id: string;
  name: string;
  content: string;
  createdAt?: string;
  updatedAt?: string;
}

export interface CreateUserPromptRequest {
  name: string;
  content: string;
}

export interface UpdateUserPromptRequest {
  name?: string;
  content?: string;
}

// ─── Task Board (design 0067, #1699) ─────────────────────────────────────────
// Row encoding is the engine's canonical JSON (snake_case short_id —
// identical to the internal automation seam's rendering).

export interface BoardAcceptanceCriterion {
  text: string;
  met: boolean;
}

export interface BoardProject {
  id: string;
  short_id: string;
  user_id: string;
  name: string;
  target_workspace_id?: string;
  worker_slots: number;
  slot_cooldown_until?: string | null;
  tags: string[];
  status: string;
  paused: boolean;
  backpressure?: string;
  created_at: string;
  updated_at: string;
}

export interface BoardEpic {
  id: string;
  project_id: string;
  short_id: string;
  title: string;
  outcome: string;
  status: string;
  project_name?: string;
  tags: string[];
  created_at: string;
  updated_at: string;
  /** Derived at read time (ruling R2 — never stored); progress_derived false = no-progress, never 0/0. */
  progress_done: number;
  progress_total: number;
  progress_derived: boolean;
}

export interface BoardTask {
  id: string;
  project_id: string;
  epic_id: string;
  short_id: string;
  title: string;
  description: string;
  stage: string;
  paused: boolean;
  waiting_on?: string;
  backpressure?: string;
  pending_origin?: string | null;
  project_name?: string;
  epic_name?: string;
  tags: string[];
  acceptance_criteria: BoardAcceptanceCriterion[];
  is_blocked: boolean;
  blocked_by?: string[];
  assignee_session?: string;
  claim_token?: string;
  lease_expires_at?: string | null;
  last_progress_at?: string | null;
  attempts: number;
  cooldown_until?: string | null;
  stage_entered_at: string;
  created_at: string;
  updated_at: string;
}

export interface BoardClaim {
  task_id: string;
  short_id: string;
  project_id: string;
  epic_id: string;
  workspace_id?: string;
  claim_token?: string;
  assignee?: string;
  lease_expires_at?: string;
  /** true = attempts cap reached; the task waits in pending_input instead. */
  escalated: boolean;
  task?: BoardTask;
}

export interface BoardDependencyMutation {
  task: BoardTask;
}

export interface BoardProjectCreateRequest {
  name: string;
  targetWorkspaceId: string;
  workerSlots?: number;
  tags?: string[];
  requestId?: string;
}

export interface BoardProjectUpdateRequest {
  name?: string;
  targetWorkspaceId?: string;
  workerSlots?: number;
  /** REPLACES the set when provided — send the full set. */
  tags?: string[];
  requestId?: string;
}

export interface BoardEpicCreateRequest {
  title: string;
  outcome?: string;
  tags?: string[];
  requestId?: string;
}

export interface BoardEpicUpdateRequest {
  title?: string;
  outcome?: string;
  /** REPLACES the set when provided — send the full set. */
  tags?: string[];
  requestId?: string;
}

export interface BoardTaskCreateRequest {
  title: string;
  description?: string;
  /** May carry the pr:N anchor tag (owner ruling OR4: pattern ^pr:[1-9][0-9]*$). */
  tags?: string[];
  acceptanceCriteria?: BoardAcceptanceCriterion[];
  requestId?: string;
}

export interface BoardTaskUpdateRequest {
  title?: string;
  description?: string;
  tags?: string[];
  acceptanceCriteria?: BoardAcceptanceCriterion[];
  /** Checks off the criterion at this 0-based index (Done preparation). */
  setCriteriaMet?: number;
  requestId?: string;
}

export interface BoardMoveStageRequest {
  toStage: string;
  evidenceUrl?: string;
  requestId?: string;
}

export interface BoardLifecycleRequest {
  requestId?: string;
}

/** #1747: the delete result — orphaned dependents listed; every removal
 * keeps a full snapshot in the audit log. */
export interface BoardDeleteResult {
  item_kind: string;
  id: string;
  short_id: string;
  cascade_tasks?: string[];
  orphaned_dependents?: string[];
  reconnected?: string[];
}

/** #1747: one plan node (dependsOn names other keys in the payload). */
export interface BoardPlanTask {
  key: string;
  title: string;
  description?: string;
  tags?: string[];
  acceptanceCriteria?: Array<{ text: string; met: boolean }>;
  dependsOn?: string[];
}

export interface BoardPlanRequest {
  epicTitle: string;
  outcome?: string;
  dryRun?: boolean;
  tasks: BoardPlanTask[];
  requestId?: string;
}

export interface BoardImportResult {
  dry_run: boolean;
  plan?: { valid: boolean; error?: string; warnings: string[]; initial_ready_set: string[] };
  project_id: string;
  epic_id?: string;
  epic_short_id?: string;
  task_keys?: Record<string, string>;
  initial_ready_set?: string[];
  warnings: string[];
}

export interface BoardDependencyRequest {
  dependsOn: string;
  requestId?: string;
}

export interface BoardRequestIdOnly {
  requestId?: string;
}
