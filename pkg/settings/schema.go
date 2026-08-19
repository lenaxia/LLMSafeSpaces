// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package settings

// SchemaVersion is incremented on any schema change (add/remove/modify keys).
// Used by the seed job to detect orphaned keys and by the frontend to cache-bust.
//
// Bumped to 3 (2026-06-18): added Pattern + magnitude constraints to
// workspace.defaultResources.{cpu,memory}; tightened the existing
// pattern on workspace.defaultStorageSize to reject zero-magnitude
// values. The schema response shape exposed to the admin UI changed,
// so clients caching the schema need to refresh.
//
// Bumped to 4 (2026-06-19): tightened
// workspace.defaultResources.cpu Pattern to reject zero-magnitude
// values ("0m", "0.0", "0.00"). Closes the parallel zero-magnitude
// gap that the memory and storage tightening in v3 missed.
// Bumped to 5 (2026-06-19): added ReadOnly field to SettingDef (US-49.2
// helm-precedence model — helm-managed keys are read-only in the admin UX)
// and added four email.* instance settings (provider/sesRegion/fromAddress/
// baseUrl). The schema response shape changed (new field + new keys); clients
// caching the schema need to refresh.
// Bumped to 6 (2026-07-06): flipped user setting sendOnEnter Default from
// true to false and updated its Description (desktop Enter is now newline by
// default; Ctrl/Cmd+Enter sends; mobile is button-only). Same class of
// change as v3/v4 (modifying a property of an existing key) — admin UI and
// frontend schema caches must refresh to show the new description.
// Bumped to 7 (2026-07-30): added workspace.allowedExternalDirectories
// (TypeStrings) — directories pre-approved as opencode
// external_directory "allow" rules so agents stop prompting for /tmp/* on
// every session. New key; admin UI schema cache must refresh.
// Bumped to 10 (2026-08-05): added 8 Epic 64 instance settings —
// workflows.maxPerUser/maxPerOrg/maxRunDurationSec/workspaceActivationTimeoutSec/
// maxNodeOutputBytes + triggers.maxPerUser/cronMinIntervalSec/webhookRateLimitPerSec.
// New keys; admin UI schema cache must refresh.
// Bumped to 11 (2026-08-15): workspace.defaultImage Default changed from the
// floating "ghcr.io/lenaxia/llmsafespaces/base:latest" to "" (fall through to
// the chart-pinned base RuntimeEnvironment), its Description rewritten, and a
// new RejectMutableTags field added to SettingDef (schema response shape
// change). Context: the floating default let a registry mirror serve a
// 4-day-stale image digest to new workspaces (incident 2026-08-14); writes of
// mutable-tag image refs are now rejected. Existing deployments are cleaned
// by migration 000023.
// Bumped to 12 (2026-08-19): added 3 Epic 66 instance settings —
// devPreview.enabled/maxResponseBytes/maxConnsPerWorkspace — closing issue
// #946, where they were registered in KnownKeys only and unreachable through
// every consumer (no admin UI section, boot-time index-miss hard-disable,
// Set rejection). Same change class as v10. New keys; admin UI schema cache
// must refresh. Canary twins updated in lockstep
// (TestCanary_SchemaVersion_TwinParity).
// Bumped to 13 (2026-08-19): devPreview.enabled Label/Description rewritten
// (property modification of an existing key, same class as v6) — "Dev
// Preview"/"Kill-switch…" read as ON = feature killed; the toggle is an
// enable-flag. Now "Enable Dev Preview" with explicit ON/OFF semantics.
// Admin UI schema caches must refresh to show the new copy.
const SchemaVersion = 13

// SettingType defines the data type of a setting.
type SettingType string

const (
	TypeBool    SettingType = "bool"
	TypeInt     SettingType = "int"
	TypeString  SettingType = "string"
	TypeEnum    SettingType = "enum"
	TypeStrings SettingType = "strings"
)

// SettingDef defines a single mutable setting. Every setting has a default —
// there is no concept of a "required" setting that can be missing.
type SettingDef struct {
	Key         string      `json:"key"`
	Tier        int         `json:"tier"` // 2=instance, 3=user
	Type        SettingType `json:"type"`
	Default     any         `json:"default"`
	Min         *int        `json:"min,omitempty"`     // int range
	Max         *int        `json:"max,omitempty"`     // int range
	Pattern     string      `json:"pattern,omitempty"` // string regex
	Enum        []string    `json:"enum,omitempty"`    // enum values
	Category    string      `json:"category"`          // UI grouping
	Label       string      `json:"label"`             // UI display name
	Description string      `json:"description"`       // UI help text
	// ReadOnly is set to true when the key is managed by Helm (Tier 1
	// helm-precedence model, US-49.2). The admin UX must disable edits to
	// these keys and show a "Managed by Helm" badge. Set() rejects writes
	// to read-only keys with ErrReadOnly.
	// RejectMutableTags causes Validate to reject values shaped like
	// container image references (contain '/') unless they are pinned to
	// an explicit non-mutable tag or a digest. Un-tagged refs are rejected
	// too — registries resolve them to :latest implicitly. Values without
	// '/' are RuntimeEnvironment names and pass. Use for any setting whose
	// value becomes a workspace image.
	RejectMutableTags bool `json:"rejectMutableTags,omitempty"`
	ReadOnly          bool `json:"readOnly,omitempty"`
}

// intPtr returns a pointer to an int value.
func intPtr(v int) *int { return &v }

// InstanceSettings returns all Tier 2 (admin-mutable) setting definitions.
func InstanceSettings() []SettingDef {
	return []SettingDef{
		// Auth
		{Key: "auth.registrationEnabled", Tier: 2, Type: TypeBool, Default: true, Category: "Auth", Label: "Registration Enabled", Description: "Allow new user sign-ups"},
		{Key: "auth.lockoutEnabled", Tier: 2, Type: TypeBool, Default: false, Category: "Auth", Label: "Account Lockout", Description: "Account lockout on failed attempts"},
		{Key: "auth.lockoutAttempts", Tier: 2, Type: TypeInt, Default: 5, Min: intPtr(1), Max: intPtr(100), Category: "Auth", Label: "Lockout Attempts", Description: "Failed attempts before lockout"},
		{Key: "auth.lockoutDurationMinutes", Tier: 2, Type: TypeInt, Default: 15, Min: intPtr(1), Max: intPtr(1440), Category: "Auth", Label: "Lockout Duration (min)", Description: "Lockout duration in minutes"},

		// Rate Limiting
		{Key: "rateLimiting.enabled", Tier: 2, Type: TypeBool, Default: true, Category: "Rate Limiting", Label: "Rate Limiting", Description: "Global rate limiting"},
		{Key: "rateLimiting.defaultLimit", Tier: 2, Type: TypeInt, Default: 100, Min: intPtr(1), Max: intPtr(100000), Category: "Rate Limiting", Label: "Default Limit", Description: "Requests per window"},
		{Key: "rateLimiting.windowMinutes", Tier: 2, Type: TypeInt, Default: 1, Min: intPtr(1), Max: intPtr(1440), Category: "Rate Limiting", Label: "Window (min)", Description: "Window duration in minutes"},
		{Key: "rateLimiting.burstSize", Tier: 2, Type: TypeInt, Default: 20, Min: intPtr(1), Max: intPtr(1000), Category: "Rate Limiting", Label: "Burst Size", Description: "Burst size"},
		{Key: "rateLimiting.strategy", Tier: 2, Type: TypeEnum, Default: "token_bucket", Enum: []string{"token_bucket", "fixed_window", "sliding_window"}, Category: "Rate Limiting", Label: "Strategy", Description: "Rate limiting algorithm"},

		// Workspace
		{Key: "workspace.defaultImage", Tier: 2, Type: TypeString, Default: "", RejectMutableTags: true, Category: "Workspace", Label: "Default Image", Description: "Image for new workspaces (pinned tag or digest). Empty = the chart-pinned 'base' RuntimeEnvironment. Floating tags like :latest are rejected — they resolve differently per pull and per registry mirror."},
		{Key: "workspace.defaultStorageSize", Tier: 2, Type: TypeString, Default: "15Gi", Pattern: StorageQuantityPattern, Category: "Workspace", Label: "Default Storage", Description: "Default PVC size"},
		{Key: "workspace.defaultStorageClass", Tier: 2, Type: TypeString, Default: "", Category: "Workspace", Label: "Storage Class", Description: "K8s StorageClass (empty = cluster default)"},
		{Key: "workspace.maxActiveWorkspacesPerUser", Tier: 2, Type: TypeInt, Default: 10, Min: intPtr(1), Max: intPtr(50), Category: "Workspace", Label: "Max Active Workspaces", Description: "Max running pods per user; oldest auto-suspended"},
		{Key: "workspace.defaultMaxActiveSessions", Tier: 2, Type: TypeInt, Default: 5, Min: intPtr(1), Max: intPtr(20), Category: "Workspace", Label: "Max Sessions", Description: "Concurrent sessions per workspace"},
		// workspace.allowedExternalDirectories: glob patterns pre-approved as
		// opencode external_directory "allow" rules, injected into every
		// workspace's agent-config.json at boot. Agents prompt for any path
		// outside the /workspace project root; /tmp/* is a PVC subPath with no
		// credentials (US-35.7 moved them to tmpfs), so auto-approving it is
		// safe. DO NOT add /sandbox-cfg/* here — that emptyDir holds plaintext
		// credential files the agent must never silently read.
		{Key: "workspace.allowedExternalDirectories", Tier: 2, Type: TypeStrings, Default: []string{"/tmp/*"}, Category: "Workspace", Label: "Allowed External Dirs", Description: "Glob patterns auto-approved as agent external_directory allow-rules (e.g. /tmp/*). Avoid credential paths like /sandbox-cfg/*."},
		{Key: "workspace.defaultResources.cpu", Tier: 2, Type: TypeString, Default: "500m", Pattern: CPUQuantityPattern, Category: "Workspace", Label: "Default CPU", Description: "Default CPU limit (e.g. 500m, 1.0)"},
		{Key: "workspace.defaultResources.memory", Tier: 2, Type: TypeString, Default: "1Gi", Pattern: MemoryQuantityPattern, Category: "Workspace", Label: "Default Memory", Description: "Default memory limit (e.g. 512Mi, 1Gi). Suffix is case-sensitive; must be > 0."},

		// Auto-Suspend
		{Key: "workspace.autoSuspend.enabled", Tier: 2, Type: TypeBool, Default: true, Category: "Auto-Suspend", Label: "Auto-Suspend", Description: "Global auto-suspend"},
		{Key: "workspace.autoSuspend.idleTimeoutMinutes", Tier: 2, Type: TypeInt, Default: 60, Min: intPtr(5), Max: intPtr(10080), Category: "Auto-Suspend", Label: "Idle Timeout (min)", Description: "Idle timeout before auto-suspend"},
		{Key: "workspace.ttlDaysAfterSuspended", Tier: 2, Type: TypeInt, Default: 0, Min: intPtr(0), Max: intPtr(365), Category: "Auto-Suspend", Label: "TTL After Suspend (days)", Description: "Auto-delete after suspend (0 = never)"},

		// Credentials
		{Key: "credentials.autoProvision", Tier: 2, Type: TypeBool, Default: false, Category: "Credentials", Label: "Auto-Provision", Description: "Auto-copy default credential set to new workspaces"},

		// Network
		{Key: "workspace.defaultNetworkAccess.ingress", Tier: 2, Type: TypeBool, Default: false, Category: "Network", Label: "Default Ingress", Description: "Allow inbound by default"},
		{Key: "workspace.defaultNetworkAccess.egressDomains", Tier: 2, Type: TypeStrings, Default: []string{}, Category: "Network", Label: "Default Egress Domains", Description: "Default allowed egress domains"},

		// Security
		{Key: "workspace.defaultSecurityLevel", Tier: 2, Type: TypeEnum, Default: "standard", Enum: []string{"standard", "high"}, Category: "Security", Label: "Default Security Level", Description: "Pod security posture"},

		// Branding
		{Key: "instance.name", Tier: 2, Type: TypeString, Default: "LLMSafeSpaces", Pattern: `^.{1,64}$`, Category: "Branding", Label: "Instance Name", Description: "Instance display name"},
		{Key: "instance.motd", Tier: 2, Type: TypeString, Default: "", Pattern: `^.{0,500}$`, Category: "Branding", Label: "Message of the Day", Description: "Login page message"},

		// Email (US-49.2). When email.enabled=true in helm, these keys are
		// marked ReadOnly via SetHelmOverrides and their values come from
		// the helm-rendered config.yaml. When email.enabled=false, they are
		// admin-mutable and stored in PostgreSQL; the admin must then
		// restart the API for the new provider to take effect (the provider
		// is constructed once at boot).
		{Key: "email.provider", Tier: 2, Type: TypeString, Default: "", Category: "Email", Label: "Provider", Description: "Email provider (empty=noop/dev, ses=AWS SES)"},
		{Key: "email.sesRegion", Tier: 2, Type: TypeString, Default: "", Category: "Email", Label: "SES Region", Description: "AWS region for SES (e.g. us-east-1)"},
		{Key: "email.fromAddress", Tier: 2, Type: TypeString, Default: "", Pattern: `^.{0,254}$`, Category: "Email", Label: "From Address", Description: "Verified SES sender address"},
		{Key: "email.baseUrl", Tier: 2, Type: TypeString, Default: "", Pattern: `^.{0,500}$`, Category: "Email", Label: "Base URL", Description: "Public origin for links in email bodies (e.g. https://app.example.com)"},

		// MCP (Epic 53)
		{Key: "mcp.allowOrgAdminServers", Tier: 2, Type: TypeBool, Default: true, Category: "MCP", Label: "Allow Org-Admin MCP Servers", Description: "When false, org admins cannot register MCP servers (platform-wide kill-switch)"},

		// Workflows (Epic 64)
		{Key: "workflows.maxPerUser", Tier: 2, Type: TypeInt, Default: 50, Min: intPtr(0), Max: intPtr(10000), Category: "Workflows", Label: "Max Workflows Per User", Description: "Maximum workflow definitions a single user can own (0 = unlimited)"},
		{Key: "workflows.maxPerOrg", Tier: 2, Type: TypeInt, Default: 200, Min: intPtr(0), Max: intPtr(100000), Category: "Workflows", Label: "Max Workflows Per Org", Description: "Maximum workflow definitions per org (0 = unlimited)"},
		{Key: "workflows.maxRunDurationSec", Tier: 2, Type: TypeInt, Default: 3600, Min: intPtr(60), Max: intPtr(86400), Category: "Workflows", Label: "Max Run Duration (sec)", Description: "Hard timeout per workflow run"},
		{Key: "workflows.workspaceActivationTimeoutSec", Tier: 2, Type: TypeInt, Default: 120, Min: intPtr(10), Max: intPtr(600), Category: "Workflows", Label: "Workspace Activation Timeout (sec)", Description: "Deadline for waking a suspended workspace before failing the run fast"},
		{Key: "workflows.maxNodeOutputBytes", Tier: 2, Type: TypeInt, Default: 1048576, Min: intPtr(1024), Max: intPtr(10485760), Category: "Workflows", Label: "Max Node Output (bytes)", Description: "Per-node output cap; oversize output fails the node (no spill in v1)"},

		// Triggers (Epic 64)
		{Key: "triggers.maxPerUser", Tier: 2, Type: TypeInt, Default: 20, Min: intPtr(0), Max: intPtr(1000), Category: "Triggers", Label: "Max Triggers Per User", Description: "Maximum triggers a single user can own (0 = unlimited)"},
		{Key: "triggers.cronMinIntervalSec", Tier: 2, Type: TypeInt, Default: 60, Min: intPtr(10), Max: intPtr(86400), Category: "Triggers", Label: "Cron Min Interval (sec)", Description: "Floor on cron interval (anti-abuse)"},
		{Key: "triggers.webhookRateLimitPerSec", Tier: 2, Type: TypeInt, Default: 10, Min: intPtr(1), Max: intPtr(1000), Category: "Triggers", Label: "Webhook Rate Limit (per sec)", Description: "Default per-webhook token-bucket rate"},

		// Dev Preview (Epic 66). Registered in KnownKeys by US-66.5 but
		// missing here, which made every consumer unreachable: no admin UI
		// section (schema-driven), a boot-time GetBool index-miss error that
		// app.go swallowed (hard-disable), and Set rejecting the key (no
		// remediation). Defaults mirror registry.go (issue #946).
		{Key: "devPreview.enabled", Tier: 2, Type: TypeBool, Default: true, Category: "Dev Preview", Label: "Enable Dev Preview", Description: "ON: workspaces that also enable Dev Preview in their own settings can serve live previews of in-workspace dev servers. OFF: every preview URL returns 503 platform-wide. Takes effect after an API restart."},
		{Key: "devPreview.maxResponseBytes", Tier: 2, Type: TypeInt, Default: 52428800, Min: intPtr(1024), Max: intPtr(1073741824), Category: "Dev Preview", Label: "Max Response (bytes)", Description: "Per-response body cap on the dev-preview proxy (default 50 MiB)"},
		{Key: "devPreview.maxConnsPerWorkspace", Tier: 2, Type: TypeInt, Default: 50, Min: intPtr(1), Max: intPtr(1000), Category: "Dev Preview", Label: "Max Conns Per Workspace", Description: "Concurrent dev-preview connections per workspace (separate from the agent-session budget)"},
	}
}

// UserSettings returns all Tier 3 (per-user) setting definitions.
func UserSettings() []SettingDef {
	return []SettingDef{
		// Appearance
		{Key: "theme", Tier: 3, Type: TypeEnum, Default: "system", Enum: []string{"light", "dark", "system"}, Category: "Appearance", Label: "Theme", Description: "Color theme"},
		{Key: "fontSize", Tier: 3, Type: TypeInt, Default: 14, Min: intPtr(10), Max: intPtr(24), Category: "Appearance", Label: "Font Size", Description: "Base font size"},
		{Key: "compactMode", Tier: 3, Type: TypeBool, Default: false, Category: "Appearance", Label: "Compact Mode", Description: "Reduce spacing"},

		// Chat
		{Key: "codeBlockWordWrap", Tier: 3, Type: TypeBool, Default: false, Category: "Chat", Label: "Code Word Wrap", Description: "Wrap long lines in code blocks"},
		{Key: "sendOnEnter", Tier: 3, Type: TypeBool, Default: false, Category: "Chat", Label: "Send on Enter", Description: "Enter sends message on desktop (off: Ctrl+Enter sends; mobile is always button-only)"},
		{Key: "preferredModel", Tier: 3, Type: TypeString, Default: "", Category: "Chat", Label: "Preferred Model", Description: "Default model ID"},

		// Workspace
		{Key: "preferredRuntime", Tier: 3, Type: TypeString, Default: "", Category: "Workspace", Label: "Default Image", Description: "Image-factory config hash to use for new workspaces (empty = use org/platform default)"},

		// Notifications
		{Key: "notifyOnSessionComplete", Tier: 3, Type: TypeBool, Default: true, Category: "Notifications", Label: "Session Complete", Description: "Notify when session completes"},
		{Key: "notifyOnWorkspaceReady", Tier: 3, Type: TypeBool, Default: true, Category: "Notifications", Label: "Workspace Ready", Description: "Notify when workspace is ready"},
	}
}

// AllSettings returns all setting definitions across all tiers.
func AllSettings() []SettingDef {
	all := make([]SettingDef, 0, len(InstanceSettings())+len(UserSettings()))
	all = append(all, InstanceSettings()...)
	all = append(all, UserSettings()...)
	return all
}

// settingIndex builds a lookup map from key to SettingDef.
func settingIndex(defs []SettingDef) map[string]SettingDef {
	m := make(map[string]SettingDef, len(defs))
	for _, d := range defs {
		m[d.Key] = d
	}
	return m
}

// InstanceSettingIndex returns a key→SettingDef map for Tier 2 settings.
func InstanceSettingIndex() map[string]SettingDef {
	return settingIndex(InstanceSettings())
}

// UserSettingIndex returns a key→SettingDef map for Tier 3 settings.
func UserSettingIndex() map[string]SettingDef {
	return settingIndex(UserSettings())
}
