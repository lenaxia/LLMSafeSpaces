// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

import (
	"encoding/json"
	"time"
)

type Workspace struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	UserID      string            `json:"userId"`
	Runtime     string            `json:"runtime"`
	StorageSize string            `json:"storageSize"`
	Phase       string            `json:"phase"`
	PVCName     string            `json:"pvcName,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	CreatedAt   time.Time         `json:"createdAt"`
	UpdatedAt   time.Time         `json:"updatedAt"`
}

type CreateWorkspaceRequest struct {
	Name        string `json:"name,omitempty"`
	Runtime     string `json:"runtime,omitempty"`
	StorageSize string `json:"storageSize,omitempty"`
}

type WorkspaceListResult struct {
	Items      []WorkspaceListItem `json:"items"`
	Pagination *PaginationMetadata `json:"pagination,omitempty"`
}

type WorkspaceListItem struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	UserID      string    `json:"userId"`
	Runtime     string    `json:"runtime"`
	StorageSize string    `json:"storageSize"`
	Phase       string    `json:"phase,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type PaginationMetadata struct {
	Total  int `json:"total"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

type EnsureSessionResponse struct {
	WorkspaceID    string `json:"workspaceId"`
	WorkspacePhase string `json:"workspacePhase"`
	SessionID      string `json:"sessionId"`
	Resumed        bool   `json:"resumed"`
}

// Message is one entry in a session transcript (pkg/session contract).
// Flat discriminated struct: Type selects which fields are meaningful.
type Message struct {
	ID        string     `json:"id"`
	SessionID string     `json:"sessionId,omitempty"`
	Type      string     `json:"type"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	Parts     []Part     `json:"parts,omitempty"`
	Model     *ModelRef  `json:"model,omitempty"`
	Cost      *Cost      `json:"cost,omitempty"`
	Text      string     `json:"text,omitempty"`
	Command   string     `json:"command,omitempty"`
	ExitCode  *int       `json:"exitCode,omitempty"`
	FromAgent string     `json:"fromAgent,omitempty"`
	ToAgent   string     `json:"toAgent,omitempty"`
	FromModel *ModelRef  `json:"fromModel,omitempty"`
	ToModel   *ModelRef  `json:"toModel,omitempty"`
	Error     *MsgError  `json:"error,omitempty"`
}

// Part is one renderable part of a message — the closed 5-type union.
type Part struct {
	Type       string      `json:"type"`
	ID         string      `json:"id,omitempty"`
	Text       string      `json:"text,omitempty"`
	Reasoning  string      `json:"reasoning,omitempty"`
	Tool       *ToolPart   `json:"tool,omitempty"`
	FileChange *FileDiff   `json:"fileChange,omitempty"`
	Custom     *CustomPart `json:"custom,omitempty"`
}

// ToolPart discriminates tool calls by Name; input/output are raw JSON.
type ToolPart struct {
	CallID string          `json:"callId,omitempty"`
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input,omitempty"`
	Output json.RawMessage `json:"output,omitempty"`
	State  ToolState       `json:"state"`
}

// ToolState is the tool-call lifecycle.
type ToolState struct {
	Status      string     `json:"status"`
	Error       string     `json:"error,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

// FileDiff carries authoritative unified-diff text for a file change.
type FileDiff struct {
	Path      string `json:"path"`
	OldPath   string `json:"oldPath,omitempty"`
	Status    string `json:"status"`
	Patch     string `json:"patch"`
	Additions int    `json:"additions,omitempty"`
	Deletions int    `json:"deletions,omitempty"`
}

// InputRequest is the unified pending-input shape ("the agent needs a
// human", design 0049 §4.5) carried on input.request/resolved events.
type InputRequest struct {
	ID            string         `json:"id"`
	SessionID     string         `json:"sessionId,omitempty"`
	RootSessionID string         `json:"rootSessionId,omitempty"`
	Kind          string         `json:"kind"`
	Question      string         `json:"question,omitempty"`
	Header        string         `json:"header,omitempty"`
	Options       []InputOption  `json:"options,omitempty"`
	Multiple      bool           `json:"multiple,omitempty"`
	Custom        bool           `json:"custom,omitempty"`
	Permission    string         `json:"permission,omitempty"`
	Patterns      []string       `json:"patterns,omitempty"`
	Always        []string       `json:"always,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	Tool          *ToolRef       `json:"tool,omitempty"`
}

// InputOption is one selectable choice within a question InputRequest.
type InputOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// ToolRef identifies the tool call that triggered an InputRequest.
type ToolRef struct {
	MessageID string `json:"messageId,omitempty"`
	CallID    string `json:"callId,omitempty"`
}

// CustomPart is the extension valve; Kind discriminates.
type CustomPart struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data,omitempty"`
}

// MsgError is the error payload on a message.
type MsgError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// ModelRef identifies a model.
type ModelRef struct {
	ID       string `json:"id"`
	Provider string `json:"provider,omitempty"`
}

// Cost is display-only token/cost data.
type Cost struct {
	InputTokens      int64   `json:"inputTokens,omitempty"`
	OutputTokens     int64   `json:"outputTokens,omitempty"`
	ReasoningTokens  int64   `json:"reasoningTokens,omitempty"`
	CacheReadTokens  int64   `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int64   `json:"cacheWriteTokens,omitempty"`
	TotalTokens      int64   `json:"totalTokens,omitempty"`
	CostUSD          float64 `json:"costUsd,omitempty"`
}

type TerminalTicket struct {
	Ticket    string `json:"ticket"`
	ExpiresAt string `json:"expiresAt"`
}

// SecretNamePattern is the regex for valid secret names.
// Keep in sync with pkg/validation/name.go.
const SecretNamePattern = "^[a-z0-9._-]+$"

type SecretResponse struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Type          string    `json:"type"`
	GlobalDefault bool      `json:"globalDefault"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// APIKey represents an API key record.
type APIKey struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Key       string     `json:"key,omitempty"` // only on creation
	Prefix    string     `json:"prefix"`
	Active    bool       `json:"active"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// WorkspaceStatus is the rich status response from GET /workspaces/:id/status.
type WorkspaceStatus struct {
	Phase            string               `json:"phase"`
	ActiveSessions   int                  `json:"activeSessions"`
	LastActivityAt   *time.Time           `json:"lastActivityAt,omitempty"`
	Message          string               `json:"message,omitempty"`
	Conditions       []WorkspaceCondition `json:"conditions,omitempty"`
	CredentialState  CredentialState      `json:"credentialState"`
	AgentHealth      AgentHealth          `json:"agentHealth"`
	Sessions         []SessionStatusItem  `json:"sessions,omitempty"`
	ImageTag         string               `json:"imageTag,omitempty"`
	DiskUsedBytes    int64                `json:"diskUsedBytes,omitempty"`
	DiskTotalBytes   int64                `json:"diskTotalBytes,omitempty"`
	MemoryUsedBytes  int64                `json:"memoryUsedBytes,omitempty"`
	MemoryTotalBytes int64                `json:"memoryTotalBytes,omitempty"`
	ContextUsed      int64                `json:"contextUsed"`
	ContextTotal     int64                `json:"contextTotal"`
}

type WorkspaceCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type CredentialState struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
}

type AgentHealth struct {
	Status              string   `json:"status"`
	ProvidersConfigured int      `json:"providersConfigured"`
	AgentVersion        string   `json:"agentVersion,omitempty"`
	Connected           []string `json:"connected,omitempty"`
	Message             string   `json:"message,omitempty"`
	LastCheckedAt       string   `json:"lastCheckedAt,omitempty"`
}

type SessionStatusItem struct {
	ID          string `json:"id"`
	Title       string `json:"title,omitempty"`
	Status      string `json:"status"`
	ContextUsed int64  `json:"contextUsed"`
}

// ActivateWorkspaceResponse is returned by POST /workspaces/:id/activate.
type ActivateWorkspaceResponse struct {
	Resumed   string `json:"resumed"`
	Suspended string `json:"suspended,omitempty"`
}

// RefreshWorkspaceResult is returned by POST /workspaces/:id/refresh-compute.
type RefreshWorkspaceResult struct {
	RestartGeneration int64 `json:"restartGeneration"`
}

// SessionListItem is sidebar metadata for a session.
type SessionListItem struct {
	ID            string     `json:"id"`
	Title         string     `json:"title,omitempty"`
	ParentID      string     `json:"parentId,omitempty"`
	LastMessageAt *time.Time `json:"lastMessageAt,omitempty"`
	MessageCount  int        `json:"messageCount"`
	Status        string     `json:"status"`
}

// ActiveSessionsResponse is returned by GET /workspaces/:id/sessions/active.
type ActiveSessionsResponse struct {
	Active    []string `json:"active"`
	MaxActive int      `json:"maxActive"`
}

// BindingsResponse is returned by GET /workspaces/:id/bindings.
type BindingsResponse struct {
	Bindings []BindingItem `json:"bindings"`
}

// BindingItem is a single binding entry.
type BindingItem struct {
	ID   string `json:"secretId"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// ReloadResult is returned by POST /workspaces/:id/reload-secrets.
type ReloadResult struct {
	Reloaded  int  `json:"reloaded"`
	Restarted bool `json:"restarted"`
}

// ModelListResponse is returned by GET /workspaces/:id/models.
type ModelListResponse struct {
	Models       []ModelItem `json:"models"`
	CurrentModel string      `json:"currentModel"`
}

// ModelItem is a single model in the catalog.
type ModelItem struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	Tier       string `json:"tier"`
	FreeTier   bool   `json:"freeTier"`
	Selected   bool   `json:"selected"`
}

// AuditEntry is a single secret audit log entry.
type AuditEntry struct {
	Action      string    `json:"action"`
	SecretID    string    `json:"secretId"`
	UserID      string    `json:"userId"`
	WorkspaceID string    `json:"workspaceId,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
}

// UserSettings is the response from GET /users/me/settings.
type UserSettings struct {
	Settings      map[string]any `json:"settings"`
	SchemaVersion int            `json:"schemaVersion"`
}

// QueuedMessage is a message waiting in the session queue.
//
// Under the V2 session-queue model (Epic 63), this is a best-effort
// shadow derived from SSE events. RetryCount is vestigial (V2 has no
// retry — opencode handles durability internally).
type QueuedMessage struct {
	ID          string `json:"id"`
	Text        string `json:"text"`
	SessionID   string `json:"session_id"`
	WorkspaceID string `json:"workspace_id"`
	EnqueuedAt  string `json:"enqueued_at"`
	RetryCount  int    `json:"retry_count"`
}

// McpServer is an external MCP server registration (Epic 53). Secrets
// (env/headers) are write-only — the response carries HasSecret only.
type McpServer struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Transport string   `json:"transport"`
	URL       string   `json:"url,omitempty"`
	Command   string   `json:"command,omitempty"`
	Args      []string `json:"args,omitempty"`
	TimeoutMs *int     `json:"timeoutMs,omitempty"`
	HasSecret bool     `json:"hasSecret"`
	Enabled   bool     `json:"enabled"`
	CreatedAt string   `json:"createdAt,omitempty"`
	UpdatedAt string   `json:"updatedAt,omitempty"`
}

// CreateMcpServerRequest registers an MCP server. Exactly one of URL
// (http/sse transports) or Command (stdio) describes the endpoint.
type CreateMcpServerRequest struct {
	Name      string              `json:"name"`
	Transport string              `json:"transport"`
	URL       string              `json:"url,omitempty"`
	Command   string              `json:"command,omitempty"`
	Args      []string            `json:"args,omitempty"`
	TimeoutMs *int                `json:"timeoutMs,omitempty"`
	Enabled   *bool               `json:"enabled,omitempty"`
	Env       map[string]string   `json:"env,omitempty"`
	Headers   map[string]string   `json:"headers,omitempty"`
	AutoApply *McpAutoApplyTarget `json:"autoApply,omitempty"`
}

// UpdateMcpServerRequest is a partial update — nil fields keep existing
// values. Transport is immutable after create.
type UpdateMcpServerRequest struct {
	Name      *string           `json:"name,omitempty"`
	URL       *string           `json:"url,omitempty"`
	Command   *string           `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	TimeoutMs *int              `json:"timeoutMs,omitempty"`
	Enabled   *bool             `json:"enabled,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// McpAutoApplyTarget selects who receives the server automatically.
type McpAutoApplyTarget struct {
	TargetType string  `json:"targetType"`
	TargetID   *string `json:"targetId,omitempty"`
}

// McpAutoApplyRule is one persisted auto-apply rule.
type McpAutoApplyRule struct {
	TargetType string  `json:"targetType"`
	TargetID   *string `json:"targetId,omitempty"`
}
