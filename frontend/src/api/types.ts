// Hand-written TypeScript types matching Go pkg/types/types.go
// Contract-tested via CI (see §17 of FRONTEND.md)

export interface User {
  id: string;
  username: string;
  email: string;
  role: string;
  active: boolean;
  createdAt: string;
}

export interface AuthConfig {
  registrationEnabled: boolean;
  oidcEnabled: boolean;
  passkeyEnabled?: boolean;
  passkeyDefaultSignup?: boolean;
  ssoProviders?: string[];
  instanceName: string;
  motd?: string;
}

export interface AuthResponse {
  token: string;
  user: User;
}

export interface LoginRequest {
  email: string;
  password: string;
  rememberMe?: boolean;
}

export interface RegisterRequest {
  username: string;
  email: string;
  password: string;
}

export interface WorkspaceListItem {
  id: string;
  name: string;
  userId: string;
  orgId?: string;
  runtime: string;
  storageSize: string;
  createdAt: string;
  updatedAt: string;
  phase?: string;
  imageTag?: string;
  agentVersion?: string;
  defaultModel?: string;
  maxActiveSessions?: number;
  agentNeedsRefresh?: boolean;
  credentialsPendingSince?: string;
  devPreviewEnabled?: boolean;
}

export interface WorkspaceListResponse {
  items: WorkspaceListItem[];
  pagination: { limit: number; offset: number; total: number };
}

export interface ActivateWorkspaceResponse {
  resumed: string;
  suspended?: string;
}

export interface SessionListItem {
  id: string;
  title?: string;
  parentId?: string;
  lastMessageAt?: string;
  messageCount: number;
  status: string;
  lastSeenAt?: string;
  hasUnread: boolean;
  /** Prompt tokens from the last LLM step for this session. undefined = no step completed yet. */
  contextUsed?: number;
  /** Session origin: manual | routine | workflow | api (from session_origins enrichment). */
  origin?: string;
}

/**
 * Returned by GET /workspaces/:id/sessions/active. The `active` field lists
 * session IDs currently running on the workspace, and `maxActive` is the
 * configured concurrency ceiling for that workspace.
 */
export interface ActiveSessionsResponse {
  active: string[];
  maxActive: number;
}

// Shape returned by the opencode agent GET /session/:id (proxied through)
export interface AgentSession {
  id: string;
  title?: string;
  parentID?: string;
  share?: string;
  // Ground-truth busy/idle from the adapter's /v1/statusz sync (#792
  // Pattern 1) — the timeout-recheck in useChatStream reads it before
  // declaring an interrupted stream (2026-08-26 false-positive incident).
  status?: "idle" | "busy" | "retry";
}

export interface WorkspaceStatus {
  phase: string;
  podName?: string;
  endpoint?: string;
  credentialState?: CredentialState;
  agentHealth?: AgentHealth;
  sessions?: AgentSessionInfo[];
  imageTag?: string;
  diskUsedBytes?: number;
  diskTotalBytes?: number;
  memoryUsedBytes?: number;
  memoryTotalBytes?: number;
  contextUsed?: number;
  contextTotal?: number;
}

export interface AgentSessionInfo {
  id: string;
  title?: string;
  status: string; // "idle" | "busy"
  contextUsed?: number;
}

export interface CredentialState {
  available: boolean;
  reason?: string;
  message?: string;
}

export interface AgentHealth {
  status: string;
  providersConfigured?: number;
  agentVersion?: string;
  connected?: string[];
  message?: string;
  lastCheckedAt?: string;
  /** Boot-time degradation notices (e.g. default model unavailable). Present with status "Healthy". */
  warnings?: string[];
}

export interface MessagePart {
  type: string;
  text?: string;
  name?: string;
  input?: unknown;
  id?: string;
  // Tool-call identity from the contract (tool.callId). The reconnect
  // boundary gate matches live SSE part.updated events against history
  // part ids AND call ids — see ChatPage historyPartIds.
  toolCallId?: string;
  hash?: string;
  toolState?: string;
  toolOutput?: string;
  // ISO timestamp when the tool call started (design 0050 D5): drives the
  // elapsed badge on running tools. Absent on older API payloads.
  toolStartedAt?: string;
}

export interface Message {
  id: string;
  role: "user" | "assistant";
  parts: MessagePart[];
  createdAt?: string;
  modelID?: string;
}

export interface SendMessageRequest {
  parts: MessagePart[];
  model?: { providerID: string; modelID: string };
  messageID?: string;
  // D3 (#907): caller-supplied idempotency key. Stable across the send
  // retry loop so a retried POST cannot double-send; the API dedupes on
  // accept and returns the original acceptance for a duplicate.
  clientMessageID?: string;
  /**
   * Epic 67 (D11): workspace upload paths to attach. The BACKEND composes
   * the attachment manifest into the dispatched text — the client never
   * mutates the text itself.
   */
  files?: string[];
}

export interface ApiKey {
  id: string;
  name: string;
  prefix: string;
  createdAt: string;
  lastUsedAt?: string;
}

export interface CreateApiKeyRequest {
  name: string;
}

export interface CreateApiKeyResponse {
  key: string;
  apiKey: ApiKey;
}

export interface ApiError {
  error: string;
  code?: string;
  /**
   * Current workspace phase on upload 409s (the workspace must be Active
   * to accept uploads, Epic 67 D5) — surfaced in the composer chip error.
   */
  phase?: string;
  /**
   * Per-field validation details, keyed by JSON field name (e.g. "slug",
   * "ownerEmail"). Present on 400 responses generated by Gin binding
   * failures via bindingErrorResponse() in the backend. Absent for other
   * 4xx/5xx responses.
   */
  details?: Record<string, string>;
  /**
   * Seconds to wait before retrying. Sent by the proxy on 429 (active-
   * session cap / rate limit) and 503 (workspace restarting — in-place
   * opencode restart for credential reload, OOM, crash, relay injection).
   * Mirrors the HTTP `Retry-After` header value.
   */
  retryAfter?: number;
  /**
   * Structured reason for 503 responses. One of:
   * - "not_ready" — workspace is booting/resuming
   * - "agent_unreachable" — opencode hung or crashed
   * - "agent_restarting" — watchdog or credential reload in progress
   * Used by the frontend to show contextual recovery messaging.
   */
  reason?: string;
  /**
   * Human-readable explanation of the error. Always present on 503s
   * from the proxy; may be absent on other error types.
   */
  message?: string;
}

// --- Workspace SSE event types ---
// These match the WorkspaceSSEEvent struct emitted by the backend broker.

export interface WorkspacePhaseEvent {
  type: "workspace.phase";
  phase: string;
}

export interface SessionStatusEvent {
  type: "session.status";
  session_id: string;
  // The proxy synthesizes string "idle" | "busy" for this field; "retry"
  // carries the full backoff detail in data (platform shape, US-65.8).
  status: "idle" | "busy" | "retry";
  data?: RetryInfo;
}

/**
 * A pkg/session contract Event delivered over the workspace SSE stream
 * (US-65.8: clients consume contract shapes only — never agent wire
 * shapes). Mirrors pkg/session/event.go's JSON.
 */
export interface SessionContractEvent {
  type: "session.event";
  session_id?: string;
  data: ContractEvent;
}

export interface ContractEvent {
  type: string;
  timestamp?: string;
  sessionId?: string;
  messageId?: string;
  partId?: string;
  status?: string;
  session?: ContractSession;
  part?: ContractPart;
  delta?: string;
  error?: { code?: string; message: string };
}

export interface ContractSession {
  id: string;
  title?: string;
  parentId?: string;
  contextUsage?: { used: number; window?: number };
}

/** One part snapshot in a contract part.end event (5-type union). */
export interface ContractPart {
  type: "text" | "reasoning" | "tool" | "file-change" | "custom";
  id?: string;
  text?: string;
  reasoning?: string;
  tool?: {
    name: string;
    callId?: string;
    input?: unknown;
    output?: unknown;
    state?: {
      status?: "pending" | "running" | "completed" | "error";
      error?: string;
      startedAt?: string;
      completedAt?: string;
    };
  };
  custom?: { kind: string; data?: unknown };
}

export interface RetryInfo {
  attempt: number;
  message: string;
  next: number;
  action: string;
}

// --- Agent input request types (Epic 16) ---

export interface QuestionOption {
  label: string;
  description: string;
}

export interface QuestionInfo {
  question: string;
  header: string;
  options: QuestionOption[];
  multiple?: boolean;
}

export interface QuestionRequest {
  id: string;
  session_id: string;
  /**
   * Top-level session in the parent chain. Equals session_id for top-level
   * sessions; for subagent/subtask sessions (e.g. opencode `task` tool spawning
   * child sessions) it points at the user-visible ancestor session. The chat
   * UI matches incoming prompts against this so subtask prompts bubble up to
   * the parent session view.
   */
  root_session_id?: string;
  questions: QuestionInfo[];
  tool?: { message_id: string; call_id: string };
}

export interface PermissionRequest {
  id: string;
  session_id: string;
  /** See {@link QuestionRequest.root_session_id}. */
  root_session_id?: string;
  permission: string;
  patterns: string[];
  metadata?: Record<string, unknown>;
  always?: string[];
  tool?: { message_id: string; call_id: string };
}

export interface AgentQuestionEvent {
  type: "agent.question";
  data: QuestionRequest;
}

export interface AgentQuestionResolvedEvent {
  type: "agent.question.resolved";
  data: { request_id: string; session_id: string };
}

export interface AgentPermissionEvent {
  type: "agent.permission";
  data: PermissionRequest;
}

export interface AgentPermissionResolvedEvent {
  type: "agent.permission.resolved";
  data: { request_id: string; session_id: string; reply: string };
}

export interface QueueUpdateEvent {
  type: "queue.update";
  session_id: string;
  data: { event: "enqueued" | "sent" | "error"; messageID: string; error?: string };
}

export interface AgentDiedEvent {
  type: "agent_died";
  workspace_id?: string;
  data: { reason: string; message?: string };
}

// D6 (#998): notify-only escalation for hung-and-alive sessions — a
// session busy past the threshold with no progress. Policy is
// notify-only: nothing is stopped or restarted automatically.
export interface WorkspaceAlertEvent {
  type: "workspace.alert";
  workspace_id?: string;
  session_id?: string;
  status: string; // "session_hung"
  data: {
    alert: string;
    oldest_busy_seconds: number;
    busy_ages?: Record<string, number>;
    policy: "notify_only";
    guidance: string;
  };
}

/**
 * Discriminated union of all event types delivered over the workspace SSE stream.
 * Narrow on `type` to access type-specific fields.
 */
export type WorkspaceStreamEvent =
  | WorkspacePhaseEvent
  | SessionStatusEvent
  | SessionContractEvent
  | AgentQuestionEvent
  | AgentQuestionResolvedEvent
  | AgentPermissionEvent
  | AgentPermissionResolvedEvent
  | QueueUpdateEvent
  | AgentDiedEvent
  | WorkspaceAlertEvent;
