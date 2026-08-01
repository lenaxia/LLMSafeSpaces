// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package types

import (
	"encoding/json"
	"regexp"
	"time"
)

// --- Epic 53: External MCP servers (platform/org/user scope) ---

// MCP server transports as stored in mcp_servers.transport. They map to opencode
// config types at materialization time (http/sse → "remote", stdio → "local").
const (
	MCPServerTransportHTTP  = "http"
	MCPServerTransportSSE   = "sse"
	MCPServerTransportStdio = "stdio"
)

// MCP server owner scopes (mirrors provider_credentials.owner_type).
const (
	MCPServerOwnerAdmin = "admin"
	MCPServerOwnerOrg   = "org"
	MCPServerOwnerUser  = "user"
)

// PlatformMcpOwnerID is the owner_id literal for platform-admin-scoped rows.
const PlatformMcpOwnerID = "_platform"

// mcpServerNameRe bounds mcp_servers.name — it doubles as the opencode mcp config
// key and must be a safe JSON object key. The DB CHECK enforces the same set.
var mcpServerNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$`)

// ValidMCPServerName reports whether name is an acceptable server name.
func ValidMCPServerName(name string) bool { return mcpServerNameRe.MatchString(name) }

// ValidMCPServerTransport reports whether t is a supported transport.
func ValidMCPServerTransport(t string) bool {
	switch t {
	case MCPServerTransportHTTP, MCPServerTransportSSE, MCPServerTransportStdio:
		return true
	}
	return false
}

// MCPServerSecretPayload is the JSON shape encoded into mcp_servers.ciphertext.
// Empty maps are valid (a server may have no secrets); the blob is always present
// so the NOT NULL column is satisfied.
type MCPServerSecretPayload struct {
	Env     map[string]string `json:"env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Encode returns the JSON bytes of the payload.
func (p *MCPServerSecretPayload) Encode() ([]byte, error) { return json.Marshal(p) }

// DecodeMCPServerSecretPayload parses ciphertext bytes.
func DecodeMCPServerSecretPayload(b []byte) (*MCPServerSecretPayload, error) {
	var p MCPServerSecretPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// MCPServerResponse is the API response shape. Secret bytes are never present;
// HasSecret reports whether the server carries an encrypted payload (UI eye-toggle).
type MCPServerResponse struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Transport string    `json:"transport"`
	URL       string    `json:"url,omitempty"`
	Command   string    `json:"command,omitempty"`
	Args      []string  `json:"args,omitempty"`
	TimeoutMs *int      `json:"timeoutMs,omitempty"`
	HasSecret bool      `json:"hasSecret"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// CreateMCPServerRequest is the body for POST .../mcp-servers.
type CreateMCPServerRequest struct {
	Name      string                    `json:"name" binding:"required"`
	Transport string                    `json:"transport" binding:"required"`
	URL       string                    `json:"url,omitempty"`
	Command   string                    `json:"command,omitempty"`
	Args      []string                  `json:"args,omitempty"`
	TimeoutMs *int                      `json:"timeoutMs,omitempty"`
	Enabled   *bool                     `json:"enabled,omitempty"`
	Env       map[string]string         `json:"env,omitempty"`
	Headers   map[string]string         `json:"headers,omitempty"`
	AutoApply *MCPServerAutoApplyTarget `json:"autoApply,omitempty"`
}

// UpdateMCPServerRequest supports partial update. Pointer fields: nil means "keep
// existing"; a non-nil value replaces it (an empty string clears url/command, an
// empty map clears env/headers). Mirrors the OrgSSO partial-update discipline.
type UpdateMCPServerRequest struct {
	Name      *string            `json:"name,omitempty"`
	URL       *string            `json:"url,omitempty"`
	Command   *string            `json:"command,omitempty"`
	Args      []string           `json:"args,omitempty"`
	TimeoutMs *int               `json:"timeoutMs,omitempty"`
	Enabled   *bool              `json:"enabled,omitempty"`
	Env       *map[string]string `json:"env,omitempty"`
	Headers   *map[string]string `json:"headers,omitempty"`
}

// MCPServerAutoApplyTarget describes an auto-apply rule on create.
type MCPServerAutoApplyTarget struct {
	TargetType string  `json:"targetType"`
	TargetID   *string `json:"targetId,omitempty"`
}

// MCPServerBinding is a row of mcp_server_bindings (workspace ↔ server).
type MCPServerBinding struct {
	WorkspaceID string `json:"workspaceId"`
	ServerID    string `json:"serverId"`
	SourceType  string `json:"sourceType"`
}

// MCPServerAutoApplyRule is a row of mcp_server_auto_apply.
type MCPServerAutoApplyRule struct {
	ServerID   string  `json:"-"`
	TargetType string  `json:"targetType"`
	TargetID   *string `json:"targetId,omitempty"`
}
