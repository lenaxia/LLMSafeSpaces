// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agentd

// Well-known paths shared between entrypoint scripts and agentd.
// Shell scripts reference these by convention — keep in sync.
//
// US-35.7: credential output paths point to /sandbox-runtime (tmpfs, RAM-backed)
// so no plaintext persists on the PVC at rest. $HOME-relative paths (SSH, git,
// secrets) are symlinks created by the init container pointing into /sandbox-runtime/rt/*.
//
// AdminPromptPath also lives on /sandbox-runtime — both because the merged
// platform/org/role/user system prompt is sensitive control-surface content
// (canary tokens, safety guardrails) that should die with the pod, and because
// the credential-setup init container that writes it has ReadOnlyRootFilesystem
// without a writable emptyDir at /tmp. Previously (#483) the constant pointed
// at /tmp/admin-prompt.md and the bootstrap subcommand silently failed every
// write — the admin prompt never reached opencode on any workspace, breaking
// the three-tier prompt chain end-to-end.
const (
	SecretsEnvPath  = "/sandbox-runtime/secrets-env"
	AgentConfigPath = "/sandbox-runtime/agent-config.json"
	AdminPromptPath = "/sandbox-runtime/admin-prompt.md"
	PasswordPath    = "/sandbox-cfg/password"
	SecretsBasePath = "/sandbox-runtime/rt/secrets"
	WorkspacePath   = "/workspace"
	// ReloadSecretsCachePath is where reloadSecretsHandler persists the last
	// reload-secrets batch so it can be replayed after a main-container restart
	// (#443). It lives on the /sandbox-runtime tmpfs emptyDir (Memory medium):
	// it survives a container restart (kubelet respawns the main container on
	// the same pod without touching the emptyDir) but is wiped on pod death —
	// preserving the US-35.7 "no plaintext on PVC at rest" invariant. The base
	// /sandbox-cfg/secrets.json (written by the init container) only ever
	// contains server-KEK creds; user-DEK creds (env-secrets, SSH keys, user
	// LLM providers) are live-pushed after boot and would otherwise be lost on
	// every container restart.
	ReloadSecretsCachePath = "/sandbox-runtime/last-reload-secrets.json"
	// AllowedDirsPath is where the bootstrap subcommand writes the instance's
	// allowedExternalDirectories setting (a JSON array of glob patterns). The
	// AgentConfigWriter reads it once at init and merges each pattern into
	// agent-config.json's mode.permissions.external_directory as an "allow"
	// rule, so agents stop prompting for /tmp/* on every session. Lives on
	// /sandbox-runtime tmpfs for the same reasons as ReloadSecretsCachePath —
	// survives container restart, wiped on pod death, no plaintext-on-PVC
	// concern (it's a list of public path globs, not secrets).
	AllowedDirsPath = "/sandbox-runtime/allowed-dirs.json"
	// ModelResolutionWarningPath is where the materialize subcommand records
	// that the workspace's persisted default model could not be resolved to
	// any provider in agent-config.json (e.g. a relay-only model selected
	// while the relay injector failed at boot — incident 2026-08-16). The
	// model key is deliberately omitted from agent-config.json in that case
	// (a bare ID poisons opencode's model resolution: every prompt in every
	// session fails with ProviderModelNotFoundError until the pod rebuilds).
	// agentd's healthz/statusz read this marker so the controller can relay
	// the condition into the AgentHealthy condition message the user sees.
	// Same tmpfs lifecycle as ReloadSecretsCachePath; removed by the next
	// successful resolution.
	ModelResolutionWarningPath = "/sandbox-runtime/model-resolution-warning.json"
	// SidecarRestartMarkerPath is where the restart-reason marker lives
	// in sidecar mode (design 0051 US-2): the shared /sandbox-runtime
	// tmpfs. Writers straddle uids (sidecar 2000, supervisor 1000) with
	// the pod's shared group 1000 in common, so markers there are 0640.
	// The controller stamps it into LLMSAFESPACES_RESTART_MARKER_PATH on
	// BOTH containers; env unset keeps the single-container default.
	SidecarRestartMarkerPath = "/sandbox-runtime/last-restart-reason.json"
	// UploadsPath is the Epic 67 file-ingest root on the workspace PVC:
	// every PUT /v1/files upload lands here as <uuid>-<sanitized-name>
	// (atomic tmp+rename, design epic-67 D2/D3). Override via
	// LLMSAFESPACES_UPLOADS_PATH (tests, dev relocatability).
	UploadsPath = "/workspace/uploads"
)

// Ports and network constants shared between agentd and the controller.
const (
	AgentPort       = 4096 // opencode serve listens here
	AgentdPort      = 4097 // agentd user-facing HTTP API (reload-secrets, future proxy)
	AgentdAddr      = "0.0.0.0:4097"
	AgentdAdminPort = 4098 // agentd admin HTTP API (healthz, readyz, statusz) — US-22.8
	AgentdAdminAddr = "0.0.0.0:4098"
	AuthUsername    = "opencode" // Basic Auth username for opencode
)

// HealthzResponse is the response for GET /v1/healthz.
//
// UserCredsPresent (worklog 0591) is TRUE when agentd's
// last-reload-secrets.json cache exists AND parses AND contains at
// least one entry — i.e., a prior successful reload push delivered
// user-DEK content that is currently materialized on disk. FALSE on
// fresh pod boot (no prior push), on empty batch (user unbound all
// secrets), on cache-read failure, or on corrupt cache. The API's
// workspace watcher reads this field via the controller's
// scrape-and-mirror pattern to decide whether to fire a
// background auto-push after pod recreation.
//
// This field is observability data, not a liveness gate. A hasUserCreds
// failure does NOT block the healthz response — healthy stays true so
// kubelet's liveness probe doesn't cascade to pod-kill from an unrelated
// cache-read fault.
type HealthzResponse struct {
	Healthy          bool   `json:"healthy"`
	Version          string `json:"version"`
	UptimeSeconds    int    `json:"uptime_seconds"`
	UserCredsPresent bool   `json:"userCredsPresent"`
	// CommitSHA is the git commit this agentd binary was built from
	// (pkg/version, -ldflags injected; buildinfo vcs fallback). Surfaced
	// so a deployed agentd can be traced to source without binary
	// forensics — a devel hash build reporting a clean release Version
	// (incident 2026-08-15) must be distinguishable from the real tag.
	// "unknown" means neither ldflags nor vcs buildinfo could supply it.
	CommitSHA string `json:"commit_sha,omitempty"`
	// BuildTime is the UTC build timestamp (pkg/version). Same purpose
	// as CommitSHA; "unknown" when not stamped via ldflags (unstamped
	// builds never emit an empty string, so the omitempty tags on both
	// fields are inert by design — kept for forward-compatibility if the
	// defaults ever change).
	BuildTime string `json:"build_time,omitempty"`
	// Warnings are boot-time degradation notices (e.g. the persisted
	// default model could not be resolved — ModelResolutionWarningPath).
	// Observability only: never affects Healthy, never gates liveness.
	// The controller relays them into the AgentHealthy condition message
	// so users see why their selected model is not in use.
	Warnings []string `json:"warnings,omitempty"`
}

// ReadyzResponse is the response for GET /v1/readyz.
type ReadyzResponse struct {
	Ready               bool     `json:"ready"`
	ProvidersConnected  []string `json:"providers_connected"`
	ProvidersConfigured int      `json:"providers_configured"`
	AgentVersion        string   `json:"agent_version"`
	AgentType           string   `json:"agent_type"`
	// RelayInjected is true when the relay injector successfully completed
	// (wrote relay config and restarted opencode). False before the injector
	// has run, if it was skipped (personal opencode key), or if it failed.
	// Included here (readyz) rather than statusz because: relay injection is
	// a one-time boot event with the same semantics as pod readiness, readyz
	// is cache-based and lightweight (no synchronous opencode calls), and the
	// API server needs this flag on every ListModels cache miss — using statusz
	// (which has no latency upper bound) would be unsafe.
	RelayInjected bool `json:"relay_injected"`
}

// FileUploadResponse is the response for PUT /v1/files (Epic 67 US-67.1).
// Path is the absolute final location (/workspace/uploads/<uuid>-<name> in
// production); Name is the sanitized filename; Size the byte count on disk.
// Error responses never echo .tmp or internal paths (design U1.1.20).
type FileUploadResponse struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// SessionTokens describes token usage for a session.
type SessionTokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// SessionInfo describes a single opencode session.
type SessionInfo struct {
	ID          string         `json:"id"`
	Title       string         `json:"title,omitempty"`
	Status      string         `json:"status"` // "idle" | "busy"
	Tokens      *SessionTokens `json:"tokens,omitempty"`
	Model       string         `json:"model,omitempty"` // model ID, e.g. "claude-sonnet-4-20250514"
	ContextUsed int64          `json:"contextUsed"`
}

// CPUUsage reports cumulative CPU consumption from cgroup v2 cpu.stat.
type CPUUsage struct {
	// UsageMicros is cumulative CPU microseconds consumed (cpu.stat usage_usec).
	// Monotonically increasing — callers compute delta between polls for rate.
	UsageMicros int64 `json:"usage_micros"`
	// LimitMicrosPerSec is the CPU quota in µs/s from cpu.max (quota/period×1e6).
	// 0 means no quota (unlimited).
	LimitMicrosPerSec int64 `json:"limit_micros_per_sec,omitempty"`
}

// DiskUsage reports workspace filesystem usage.
type DiskUsage struct {
	UsedBytes  int64 `json:"used_bytes"`
	TotalBytes int64 `json:"total_bytes"`
}

// MemoryUsage reports workspace memory usage.
type MemoryUsage struct {
	UsedBytes  int64 `json:"used_bytes"`
	TotalBytes int64 `json:"total_bytes"`
}

// ContextUsage reports LLM context window usage across active sessions.
type ContextUsage struct {
	UsedTokens  int64 `json:"used_tokens"`
	TotalTokens int64 `json:"total_tokens"`
}

// StatuszResponse is the response for GET /v1/statusz.
type StatuszResponse struct {
	Healthy             bool          `json:"healthy"`
	Ready               bool          `json:"ready"`
	Connected           []string      `json:"connected"`
	ProvidersConfigured int           `json:"providers_configured"`
	Sessions            []SessionInfo `json:"sessions"`
	SessionsActive      int           `json:"sessions_active"`
	SessionsError       int           `json:"sessions_error"`
	LastError           string        `json:"last_error"`
	AgentType           string        `json:"agent_type"`
	AgentVersion        string        `json:"agent_version"`
	UptimeSeconds       int           `json:"uptime_seconds"`
	Disk                *DiskUsage    `json:"disk,omitempty"`
	Memory              *MemoryUsage  `json:"memory,omitempty"`
	CPU                 *CPUUsage     `json:"cpu,omitempty"`
	// RelayFreeModels: 0 unknown, 1 ok, 2 degraded (injector deadline
	// exhausted — free-tier routing unavailable until the next agent
	// restart; #901 G8).
	RelayFreeModels int32 `json:"relay_free_models"`
	// OldestBusySeconds: age of the longest-busy session, 0 when idle
	// (D6/#998: unattended-escalation detection input — a session busy
	// this long with sustained watchdog flat-suppressions is likely
	// hung-and-alive; the API escalates to the owner, never executes).
	OldestBusySeconds int            `json:"oldest_busy_seconds"`
	BusyAges          map[string]int `json:"busy_ages,omitempty"`
	Context           *ContextUsage  `json:"context,omitempty"`
	// MemoryPressure is true when memory usage exceeds the warning
	// threshold (85% of cgroup limit). Set by agentd's periodic memory
	// check (US-44.5). The controller reads this to set the
	// WorkspaceConditionMemoryPressure condition.
	MemoryPressure bool `json:"memory_pressure,omitempty"`
	// Warnings mirrors HealthzResponse.Warnings: boot-time degradation
	// notices (ModelResolutionWarningPath). The deep-status scrape feeds
	// the controller's AgentHealthy condition message, so the warning
	// must appear here too or it would be erased between scrapes.
	Warnings []string `json:"warnings,omitempty"`
}
