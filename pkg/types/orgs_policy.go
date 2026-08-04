// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package types

import (
	"encoding/json"
	"time"
)

// --- US-43.7: Org policies ---

// OrgPolicyKey identifies a single org-scoped policy. Per D15, Phase 2 ships
// exactly these four; the migration CHECK constraint enforces the same set.
type OrgPolicyKey string

const (
	PolicyAllowedModels             OrgPolicyKey = "allowed_models"
	PolicyAllowedProviders          OrgPolicyKey = "allowed_providers"
	PolicyMaxWorkspacesPerMember    OrgPolicyKey = "max_workspaces_per_member"
	PolicyMaxActiveWorkspacesPerMem OrgPolicyKey = "max_active_workspaces_per_member"

	// Agent customization policies
	PolicySysPromptOrg    OrgPolicyKey = "sys_prompt_org"
	PolicyAllowUserPrompt OrgPolicyKey = "allow_user_prompt"

	// Epic 53 — MCP server governance policies
	PolicyAllowUserMcpServers       OrgPolicyKey = "allow_user_mcp_servers"
	PolicyMaxMcpServersPerWorkspace OrgPolicyKey = "max_mcp_servers_per_workspace"

	// Image factory — org default workspace image (design/0046 launch hierarchy)
	PolicyDefaultRuntime OrgPolicyKey = "default_runtime"
)

// OrgPolicy is one row of org_policies. The Value is the raw JSONB payload; the
// interpretation depends on the Key (see OrgPolicyValues).
type OrgPolicy struct {
	OrgID     string          `json:"-"`
	Key       OrgPolicyKey    `json:"key"`
	Value     json.RawMessage `json:"value"`
	UpdatedBy string          `json:"-"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// OrgPolicyValues is the typed view of all four Phase 2 policies for one org.
// Fields are pointers so nil means "not set / unrestricted"; the zero value of
// the dereferenced type is never confused with "unset".
type OrgPolicyValues struct {
	AllowedModels             *[]string `json:"allowedModels,omitempty"`
	AllowedProviders          *[]string `json:"allowedProviders,omitempty"`
	MaxWorkspacesPerMember    *int      `json:"maxWorkspacesPerMember,omitempty"`
	MaxActiveWorkspacesPerMem *int      `json:"maxActiveWorkspacesPerMember,omitempty"`

	// Agent customization
	SysPromptOrg    *string `json:"sysPromptOrg,omitempty"`
	AllowUserPrompt *bool   `json:"allowUserPrompt,omitempty"`

	// Epic 53 — MCP server governance
	AllowUserMcpServers       *bool `json:"allowUserMcpServers,omitempty"`
	MaxMcpServersPerWorkspace *int  `json:"maxMcpServersPerWorkspace,omitempty"`

	// Image factory — org default workspace image (design/0046)
	DefaultRuntime *string `json:"defaultRuntime,omitempty"`
}

// IsModelAllowed reports whether modelID is permitted under the allowed-models
// policy. Returns true when no policy is set (unrestricted).
func (p *OrgPolicyValues) IsModelAllowed(modelID string) bool {
	if p == nil || p.AllowedModels == nil || len(*p.AllowedModels) == 0 {
		return true
	}
	for _, m := range *p.AllowedModels {
		if m == modelID {
			return true
		}
	}
	return false
}

// IsProviderAllowed reports whether providerID is permitted.
func (p *OrgPolicyValues) IsProviderAllowed(providerID string) bool {
	if p == nil || p.AllowedProviders == nil || len(*p.AllowedProviders) == 0 {
		return true
	}
	for _, id := range *p.AllowedProviders {
		if id == providerID {
			return true
		}
	}
	return false
}

// MaxWorkspaces returns the per-member workspace creation limit, or -1 (unlimited) when unset.
func (p *OrgPolicyValues) MaxWorkspaces() int {
	if p == nil || p.MaxWorkspacesPerMember == nil {
		return -1
	}
	return *p.MaxWorkspacesPerMember
}

// MaxActive returns the per-member concurrent active workspace limit, or -1.
func (p *OrgPolicyValues) MaxActive() int {
	if p == nil || p.MaxActiveWorkspacesPerMem == nil {
		return -1
	}
	return *p.MaxActiveWorkspacesPerMem
}

// OrgPrompt returns the org-level system prompt overlay, or "" when unset.
func (p *OrgPolicyValues) OrgPrompt() string {
	if p == nil || p.SysPromptOrg == nil {
		return ""
	}
	return *p.SysPromptOrg
}

// IsUserPromptAllowed reports whether org members can customize their agent
// prompts. Defaults to false (locked) when no policy is set.
func (p *OrgPolicyValues) IsUserPromptAllowed() bool {
	if p == nil || p.AllowUserPrompt == nil {
		return false
	}
	return *p.AllowUserPrompt
}

// DefaultRuntimeImage returns the org's default workspace image config hash,
// or "" when unset. Used by the workspace service's default-image hierarchy.
func (p *OrgPolicyValues) DefaultRuntimeImage() string {
	if p == nil || p.DefaultRuntime == nil {
		return ""
	}
	return *p.DefaultRuntime
}

// DefaultMaxMcpServersPerWorkspace is the per-workspace MCP server quota when an
// org has not set PolicyMaxMcpServersPerWorkspace. It bounds agent-startup cost
// and blast radius (each server is a startup connection).
const DefaultMaxMcpServersPerWorkspace = 5

// IsUserMcpAllowed reports whether org members can register their own MCP
// servers (user-scope). Defaults to false (locked) when no policy is set —
// mirroring IsUserPromptAllowed so a value the org admin must opt into is never
// silently granted by omission.
func (p *OrgPolicyValues) IsUserMcpAllowed() bool {
	if p == nil || p.AllowUserMcpServers == nil {
		return false
	}
	return *p.AllowUserMcpServers
}

// MaxMcpServers returns the per-workspace MCP server quota, or the default
// (DefaultMaxMcpServersPerWorkspace) when unset.
func (p *OrgPolicyValues) MaxMcpServers() int {
	if p == nil || p.MaxMcpServersPerWorkspace == nil {
		return DefaultMaxMcpServersPerWorkspace
	}
	if *p.MaxMcpServersPerWorkspace < 0 {
		return 0
	}
	return *p.MaxMcpServersPerWorkspace
}

// --- US-43.13: Org-scoped audit log ---

// AuditEntry is one row of the audit_log, scoped to an org when OrgID is non-empty.
type AuditEntry struct {
	ID        int64          `json:"id"`
	ActorID   string         `json:"actorId"`
	Domain    string         `json:"domain"`
	Action    string         `json:"action"`
	TargetID  string         `json:"targetId,omitempty"`
	OrgID     string         `json:"orgId,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
}

// AuditFilters holds optional filters for cross-org audit queries.
type AuditFilters struct {
	OrgID   *string
	ActorID *string
	Domain  *string
	Limit   int
	Offset  int
}
