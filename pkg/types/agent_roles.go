// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package types

import (
	"encoding/json"
	"time"
)

// RoleConfigVersion is the current config schema version.
const RoleConfigVersion = 1

// AgentRole is the database row representation.
type AgentRole struct {
	ID          string     `json:"id"`
	Scope       string     `json:"scope"`
	OrgID       *string    `json:"orgId,omitempty"`
	Name        string     `json:"name"`
	Slug        string     `json:"slug"`
	Description string     `json:"description"`
	Extends     *string    `json:"extends,omitempty"`
	IsDefault   bool       `json:"isDefault"`
	Config      RoleConfig `json:"config"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// RoleConfig is the strongly-typed view of a role's JSONB config.
// Fields are pointers so nil = inherit from parent (during merge).
type RoleConfig struct {
	Version     int              `json:"version"`
	System      *string          `json:"system,omitempty"`
	Description *string          `json:"description,omitempty"`
	Color       *string          `json:"color,omitempty"`
	Model       *string          `json:"model,omitempty"`
	Mode        *string          `json:"mode,omitempty"`
	Hidden      *bool            `json:"hidden,omitempty"`
	Permissions []PermissionRule `json:"permissions,omitempty"`
	Tools       json.RawMessage  `json:"tools,omitempty"`
	MCP         json.RawMessage  `json:"mcp,omitempty"`
	Raw         map[string]any   `json:"-"`
}

// PermissionRule is one tool-permission rule in a role config.
type PermissionRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// MaterializedAgentRole is the pod-delivery shape of one addressable
// agent role: the bootstrap response carries the workspace's vocabulary
// (platform scope + the workspace's org scope — the same composition
// the board role directory serves the board owner), the agentd
// bootstrap subcommand writes it verbatim to agentd.AgentRolesPath
// (tmpfs), and the opencode ConfigWriter materializes each entry as a
// NAMED opencode agent (agent.<ID>) so a stage-persona dispatch
// (spec["agent"] = <roleId>) resolves to the role's own system prompt
// (NODE-EXECUTE-CONTRACT §A8: opencode silently falls back to the
// build agent for an unknown agentID — the role MUST exist in the
// config). ID is the opencode agent name (a platform UUID — it cannot
// collide with opencode's built-in agent names); Slug/Name ride for
// diagnostics only, never as agent keys. Freshness contract: the file
// is written at every pod bootstrap, so role edits land on the next
// boot — the same contract as the admin prompt.
type MaterializedAgentRole struct {
	ID          string          `json:"id"`
	Slug        string          `json:"slug,omitempty"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	System      string          `json:"system,omitempty"`
	Model       string          `json:"model,omitempty"`
	Mode        string          `json:"mode,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
}

// UnmarshalRoleConfig decodes a JSONB config blob into a RoleConfig,
// preserving unknown keys in the Raw map for forward compatibility.
// (Stress test 3.4: Go's default unmarshaler would silently drop unknown keys.)
func UnmarshalRoleConfig(data []byte) (*RoleConfig, error) {
	var rawMap map[string]any
	if err := json.Unmarshal(data, &rawMap); err != nil {
		return nil, err
	}

	cfg := &RoleConfig{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	if cfg.Version == 0 {
		cfg.Version = RoleConfigVersion
	}

	known := map[string]bool{
		"version": true, "system": true, "description": true,
		"color": true, "model": true, "mode": true, "hidden": true,
		"permissions": true, "tools": true, "mcp": true,
	}
	cfg.Raw = make(map[string]any)
	for k, v := range rawMap {
		if !known[k] {
			cfg.Raw[k] = v
		}
	}

	return cfg, nil
}

// MarshalRoleConfig encodes a RoleConfig back to JSONB, including Raw keys.
func MarshalRoleConfig(cfg *RoleConfig) ([]byte, error) {
	if cfg == nil {
		return []byte(`{"version":1}`), nil
	}
	base, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if len(cfg.Raw) == 0 {
		return base, nil
	}
	var merged map[string]any
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	for k, v := range cfg.Raw {
		if _, exists := merged[k]; !exists {
			merged[k] = v
		}
	}
	return json.Marshal(merged)
}

// MergeRoleConfigs merges a parent config with a child config. Child values
// override parent values for scalar fields; permissions are concatenated
// (child appended after parent); Raw keys from both are merged (child wins).
func MergeRoleConfigs(parent, child *RoleConfig) *RoleConfig {
	merged := &RoleConfig{
		Version: child.Version,
	}
	if child.Version == 0 {
		merged.Version = parent.Version
	}
	if child.System != nil {
		merged.System = child.System
	} else {
		merged.System = parent.System
	}
	if child.Description != nil {
		merged.Description = child.Description
	} else {
		merged.Description = parent.Description
	}
	if child.Color != nil {
		merged.Color = child.Color
	} else {
		merged.Color = parent.Color
	}
	if child.Model != nil {
		merged.Model = child.Model
	} else {
		merged.Model = parent.Model
	}
	if child.Mode != nil {
		merged.Mode = child.Mode
	} else {
		merged.Mode = parent.Mode
	}
	if child.Hidden != nil {
		merged.Hidden = child.Hidden
	} else {
		merged.Hidden = parent.Hidden
	}

	merged.Permissions = append(append([]PermissionRule{}, parent.Permissions...), child.Permissions...)

	if len(child.Tools) > 0 {
		merged.Tools = child.Tools
	} else {
		merged.Tools = parent.Tools
	}
	if len(child.MCP) > 0 {
		merged.MCP = child.MCP
	} else {
		merged.MCP = parent.MCP
	}

	merged.Raw = make(map[string]any)
	for k, v := range parent.Raw {
		merged.Raw[k] = v
	}
	for k, v := range child.Raw {
		merged.Raw[k] = v
	}

	return merged
}

// EffectiveAgentRole is the fully resolved role after walking the inheritance
// chain and merging all configs from root to leaf.
type EffectiveAgentRole struct {
	AgentRole
	EffectiveConfig  RoleConfig `json:"effectiveConfig"`
	InheritanceChain []string   `json:"inheritanceChain"`
}
