// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package billing

import "github.com/lenaxia/llmsafespaces/pkg/types"

// PlanFeatures defines what a plan tier allows. Used for feature gating.
type PlanFeatures struct {
	MaxWorkspaces       int  `json:"maxWorkspaces"`
	MaxMembers          int  `json:"maxMembers"`
	SSOEnabled          bool `json:"ssoEnabled"`
	PoliciesEnabled     bool `json:"policiesEnabled"`
	AuditLogEnabled     bool `json:"auditLogEnabled"`
	CustomCredentials   bool `json:"customCredentials"`
	MaxPersonalMcpServers int `json:"maxPersonalMcpServers"` // 0=disabled, -1=unlimited, N=cap (Epic 53 D12)
}

// PlanTiers maps each internal plan ID to its feature set. Hardcoded —
// extend this map when adding a new plan tier.
var PlanTiers = map[types.OrgPlan]PlanFeatures{
	types.PlanFree: {
		MaxWorkspaces:       1,
		MaxMembers:          1,
		CustomCredentials:   true,
		MaxPersonalMcpServers: 5, // Epic 53 D12: free tier capped at 5 personal MCP servers
	},
	types.PlanTeam: {
		MaxWorkspaces:       10,
		MaxMembers:          25,
		CustomCredentials:   true,
		MaxPersonalMcpServers: -1, // unlimited
	},
	types.PlanBusiness: {
		MaxWorkspaces:       50,
		MaxMembers:          100,
		PoliciesEnabled:     true,
		AuditLogEnabled:     true,
		CustomCredentials:   true,
		MaxPersonalMcpServers: -1, // unlimited
	},
	types.PlanEnterprise: {
		MaxWorkspaces:       -1,
		MaxMembers:          -1,
		SSOEnabled:          true,
		PoliciesEnabled:     true,
		AuditLogEnabled:     true,
		CustomCredentials:   true,
		MaxPersonalMcpServers: -1, // unlimited
	},
}

// GetPlanFeatures returns the feature set for a plan, defaulting to Free.
func GetPlanFeatures(plan types.OrgPlan) PlanFeatures {
	if features, ok := PlanTiers[plan]; ok {
		return features
	}
	return PlanTiers[types.PlanFree]
}

// IsFeatureAllowed checks whether a plan tier permits a specific feature.
func IsFeatureAllowed(plan types.OrgPlan, feature string) bool {
	f := GetPlanFeatures(plan)
	switch feature {
	case "sso":
		return f.SSOEnabled
	case "policies":
		return f.PoliciesEnabled
	case "audit":
		return f.AuditLogEnabled
	case "custom_credentials":
		return f.CustomCredentials
	case "personal_mcp_servers":
		return f.MaxPersonalMcpServers != 0
	default:
		return true
	}
}

// TrialConfig holds configurable trial parameters per D13.
type TrialConfig struct {
	Enabled       bool `json:"enabled"`
	DurationDays  int  `json:"durationDays"`
	MaxMembers    int  `json:"maxMembers"`
	MaxWorkspaces int  `json:"maxWorkspaces"`
}

// DefaultTrialConfig is the default trial configuration (D13: off by default).
var DefaultTrialConfig = TrialConfig{
	Enabled:       false,
	DurationDays:  3,
	MaxMembers:    3,
	MaxWorkspaces: 1,
}
