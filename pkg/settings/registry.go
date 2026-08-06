// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package settings

// Key is a typed reference to an instance setting. It replaces raw string
// keys ("workspace.defaultStorageSize") with compile-time-checked constants.
// A typo in a Key constant is a compile error; a typo in a string literal is
// a silent runtime default.
type Key struct {
	name    string
	section string
	def     any
}

// Name returns the dotted setting name (e.g. "workspace.defaultStorageSize").
func (k Key) Name() string { return k.name }

// Section returns the top-level section (e.g. "workspace", "auth").
func (k Key) Section() string { return k.section }

// Default returns the default value for this setting.
func (k Key) Default() any { return k.def }

// KnownKeys is the complete registry of instance settings. PUT /admin/settings
// validates against this set; unknown keys are rejected.
var KnownKeys = map[string]Key{}

func register(k Key) Key {
	KnownKeys[k.name] = k
	return k
}

// Workspace settings
var (
	KeyWorkspaceDefaultStorageSize       = register(Key{"workspace.defaultStorageSize", "workspace", "15Gi"})
	KeyWorkspaceDefaultStorageClass      = register(Key{"workspace.defaultStorageClass", "workspace", ""})
	KeyWorkspaceDefaultImage             = register(Key{"workspace.defaultImage", "workspace", ""})
	KeyWorkspaceDefaultSecurityLevel     = register(Key{"workspace.defaultSecurityLevel", "workspace", ""})
	KeyWorkspaceDefaultResourcesCPU      = register(Key{"workspace.defaultResources.cpu", "workspace", ""})
	KeyWorkspaceDefaultResourcesMemory   = register(Key{"workspace.defaultResources.memory", "workspace", ""})
	KeyWorkspaceAutoSuspendEnabled       = register(Key{"workspace.autoSuspend.enabled", "workspace", false})
	KeyWorkspaceAutoSuspendIdleTimeout   = register(Key{"workspace.autoSuspend.idleTimeoutMinutes", "workspace", 0})
	KeyWorkspaceTTLDaysAfterSuspended    = register(Key{"workspace.ttlDaysAfterSuspended", "workspace", 0})
	KeyWorkspaceDefaultNetworkIngress    = register(Key{"workspace.defaultNetworkAccess.ingress", "workspace", false})
	KeyWorkspaceDefaultNetworkEgress     = register(Key{"workspace.defaultNetworkAccess.egressDomains", "workspace", []string{}})
	KeyWorkspaceDefaultMaxActiveSessions = register(Key{"workspace.defaultMaxActiveSessions", "workspace", 0})
	KeyWorkspaceMaxActivePerUser         = register(Key{"workspace.maxActiveWorkspacesPerUser", "workspace", 0})
	KeyWorkspaceAllowedExternalDirs      = register(Key{"workspace.allowedExternalDirectories", "workspace", []string{"/tmp/*"}})
)

// Auth settings
var (
	KeyAuthLockoutEnabled         = register(Key{"auth.lockoutEnabled", "auth", false})
	KeyAuthLockoutAttempts        = register(Key{"auth.lockoutAttempts", "auth", 0})
	KeyAuthLockoutDurationMinutes = register(Key{"auth.lockoutDurationMinutes", "auth", 0})
	KeyAuthRegistrationEnabled    = register(Key{"auth.registrationEnabled", "auth", true})
)

// Instance settings
var (
	KeyInstanceName = register(Key{"instance.name", "instance", ""})
	KeyInstanceMOTD = register(Key{"instance.motd", "instance", ""})
)

// Workflow settings (Epic 64)
var (
	KeyWorkflowsMaxPerUser             = register(Key{"workflows.maxPerUser", "workflows", 50})
	KeyWorkflowsMaxPerOrg              = register(Key{"workflows.maxPerOrg", "workflows", 200})
	KeyWorkflowsMaxRunDurationSec      = register(Key{"workflows.maxRunDurationSec", "workflows", 3600})
	KeyWorkflowsWorkspaceActivationSec = register(Key{"workflows.workspaceActivationTimeoutSec", "workflows", 120})
	KeyWorkflowsMaxNodeOutputBytes     = register(Key{"workflows.maxNodeOutputBytes", "workflows", 1048576})
	KeyTriggersMaxPerUser              = register(Key{"triggers.maxPerUser", "triggers", 20})
	KeyTriggersCronMinIntervalSec      = register(Key{"triggers.cronMinIntervalSec", "triggers", 60})
	KeyTriggersWebhookRateLimitPerSec  = register(Key{"triggers.webhookRateLimitPerSec", "triggers", 10})
)

// Rate limiting settings
var (
	KeyRateLimitingEnabled       = register(Key{"rateLimiting.enabled", "rateLimiting", false})
	KeyRateLimitingDefaultLimit  = register(Key{"rateLimiting.defaultLimit", "rateLimiting", 100})
	KeyRateLimitingDefaultWindow = register(Key{"rateLimiting.defaultWindow", "rateLimiting", "1m"})
	KeyRateLimitingBurstSize     = register(Key{"rateLimiting.burstSize", "rateLimiting", 20})
	KeyRateLimitingStrategy      = register(Key{"rateLimiting.strategy", "rateLimiting", "token_bucket"})
	KeyRateLimitingWindowMinutes = register(Key{"rateLimiting.windowMinutes", "rateLimiting", 0})
)

// IsKnown reports whether keyName is a registered setting.
func IsKnown(keyName string) bool {
	_, ok := KnownKeys[keyName]
	return ok
}
