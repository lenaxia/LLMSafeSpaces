// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

import "time"

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

type MessageResponse struct {
	Raw     any    `json:"-"`
	Content string `json:"-"`
}

type TerminalTicket struct {
	Ticket    string `json:"ticket"`
	ExpiresAt string `json:"expiresAt"`
}

// SecretNamePattern is the regex for valid secret names.
// Keep in sync with pkg/validation/name.go.
const SecretNamePattern = "^[a-z0-9._-]+$"

type SecretResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
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
	ID   string `json:"id"`
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
type QueuedMessage struct {
	ID          string `json:"id"`
	Text        string `json:"text"`
	SessionID   string `json:"session_id"`
	WorkspaceID string `json:"workspace_id"`
	EnqueuedAt  string `json:"enqueued_at"`
	RetryCount  int    `json:"retry_count"`
}
