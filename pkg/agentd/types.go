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
	// StagedFilesMaxBytes is the W8 file-class delivery budget: the
	// staged manifest + bytes MUST fit under it because it is also the
	// /v1/spawn-files response cap the supervisor enforces. Binding-time
	// per-secret validation (pkg/secrets) is the user-facing gate; the
	// materializer's per-entry and whole-batch checks are the
	// defense-in-depth ceiling (size_exceeded).
	StagedFilesMaxBytes = 8 << 20
	// AllowedDirsPath is where the bootstrap subcommand writes the instance's
	// allowedExternalDirectories setting (a JSON array of glob patterns). The
	// AgentConfigWriter reads it once at init and merges each pattern into
	// agent-config.json's TOP-LEVEL permission.external_directory (the
	// LIVE key on pinned opencode — mode.permissions is inert) as an
	// "allow" rule, so agents stop prompting for /tmp/* on every
	// session. Lives on
	// /sandbox-runtime tmpfs: survives container restart, wiped on pod
	// death, no plaintext-on-PVC concern (it's a list of public path
	// globs, not secrets).
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
	// Same tmpfs lifecycle as agent-config.json; removed by the next
	// successful resolution.
	ModelResolutionWarningPath = "/sandbox-runtime/model-resolution-warning.json"
	// SidecarRestartMarkerPath is where the restart-reason marker lives
	// in sidecar mode (design 0051 US-2): the shared /sandbox-runtime
	// tmpfs. Writers straddle uids (sidecar 2000, supervisor 1000) with
	// the pod's shared group 1000 in common, so markers there are 0640.
	// The controller stamps it into LLMSAFESPACES_RESTART_MARKER_PATH on
	// BOTH containers; env unset keeps the single-container default.
	SidecarRestartMarkerPath = "/sandbox-runtime/last-restart-reason.json"
	// UploadsPath is the Epic 68 file-ingest root on the workspace PVC:
	// every PUT /v1/files upload lands here as <uuid>-<sanitized-name>
	// (atomic tmp+rename, design epic-68 D2/D3). Override via
	// LLMSAFESPACES_UPLOADS_PATH (tests, dev relocatability).
	UploadsPath = "/workspace/uploads"
)

// Ports and network constants shared between agentd and the controller.
const (
	AgentPort       = 4096 // opencode serve listens here
	AgentdPort      = 4097 // agentd user-facing HTTP API (resync-secrets, future proxy)
	AgentdAddr      = "0.0.0.0:4097"
	AgentdAdminPort = 4098 // agentd admin HTTP API (healthz, readyz, statusz) — US-22.8
	AgentdAdminAddr = "0.0.0.0:4098"
	AuthUsername    = "opencode" // Basic Auth username for opencode
)

// DeliveryCapability is this build's secret-delivery generation (US-70.5
// fleet-version evidence): "v2" marks the conditional-pull stack — the v2
// bootstrap envelope contract, revision anchoring, the resync endpoint,
// and spawn-rev terminal verification. Pre-US-70.2 runtimes omit the
// healthz delivery field entirely, so a fleet scrape can count pods still
// on the legacy stack (the W15 mixed-fleet gauge). It is a capability
// statement only — convergence is still judged from the revision signals
// (spawned_rev), never from this field.
const DeliveryCapability = "v2"

// HealthzResponse is the response for GET /v1/healthz.
type HealthzResponse struct {
	Healthy       bool   `json:"healthy"`
	Version       string `json:"version"`
	UptimeSeconds int    `json:"uptime_seconds"`
	// Delivery is the build's delivery capability marker
	// (DeliveryCapability; absent on pre-US-70.2 runtimes).
	Delivery string `json:"delivery,omitempty"`
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
	// SpawnEnv (US-70.1, design 0057) is the supervisor's
	// terminal-verified spawn-env state: the revision of the secrets
	// delta the agent process actually spawned with, plus a
	// machine-readable degrade reason when delivery is incomplete.
	// Nil when the reporter has no supervisor evidence yet
	// (single-container mode, or before the first status poll) — no
	// evidence is not a degrade. Observability only, like Warnings.
	SpawnEnv *SpawnEnvHealth `json:"spawnEnv,omitempty"`
	// PendingApply (#1342, L11) is the deferred-credential-apply state:
	// a restart-worthy credential change staged on the pod but its
	// applying restart is riding a maintenance window behind busy
	// sessions. Nil when nothing is pending (the credential applied, or
	// no restart-worthy change arrived). The controller mirrors it into
	// the Workspace CRD's CredentialsApplyPending condition.
	PendingApply *PendingApplyHealth `json:"pendingApply,omitempty"`
	// Relay (US-72.4, design 0058 §4.5) is the relay-only liveness
	// slice: cached monitor state over the batch's relay-fronted
	// providers. Nil when the monitor is not wired (tests); a non-nil
	// RelayHealth with Present=false is a flag-off pod. Observability
	// only — a relay degrade never flips Healthy (it must not cascade to
	// a liveness-probe kill; the §4.8 remedy is bounded re-arm).
	Relay *RelayHealth `json:"relay,omitempty"`
	// LegacyScrub (US-72.6, design 0058 §8) is the one-time legacy-key
	// migration scrub's outcome: run at boot the first time the batch
	// observes relay-fronted providers (a post-flip pod). Nil when the
	// scrub has not run (flag-off pods). Observability only.
	LegacyScrub *LegacyScrubHealth `json:"legacyScrub,omitempty"`
}

// LegacyScrubHealth is the US-72.6 scrub slice of HealthzResponse: the
// one-time removal counts and any error. The controller mirrors it into
// the LegacyKeysScrubbed condition and emits the event on first
// observation (idempotent thereafter — the report is static).
type LegacyScrubHealth struct {
	RanAt             int64  `json:"ranAt"`
	AuthKeysRemoved   int    `json:"authKeysRemoved"`
	ConfigKeysRemoved int    `json:"configKeysRemoved"`
	Error             string `json:"error,omitempty"`
}

// PendingApplyHealth is the deferred-apply slice of HealthzResponse.
// Reason is the closed reason set's first value ("credential_change");
// WaitingSeconds/BusySessions tell the operator how long and why.
type PendingApplyHealth struct {
	Reason         string `json:"reason,omitempty"`
	WaitingSeconds int    `json:"waitingSeconds"`
	BusySessions   int    `json:"busySessions"`
}

// SpawnEnvHealth is the spawn-env delivery slice of HealthzResponse.
// Degraded=true carries a non-empty Reason from the closed reason set
// (spawn_env_unavailable, spawn_env_unauthorized, spawn_env_no_credential,
// spawn_env_bad_response); the controller mirrors it into the Workspace
// CRD's secretsDelivery status.
type SpawnEnvHealth struct {
	SpawnedRev string `json:"spawnedRev,omitempty"`
	Degraded   bool   `json:"degraded"`
	Reason     string `json:"reason,omitempty"`
	// FilesRev / FilesDegraded / FilesReason (R2b, #1165): the file-class
	// delivery slice — the terminal revision over the files the uid-1000
	// supervisor actually wrote, and its machine-readable degrade reason
	// ("" healthy).
	FilesRev      string `json:"filesRev,omitempty"`
	FilesDegraded bool   `json:"filesDegraded,omitempty"`
	FilesReason   string `json:"filesReason,omitempty"`
}

// RelayHealth is the relay-only liveness slice of healthz/readyz/statusz
// (US-72.4, design 0058 §4.5/§4.6): cached monitor state only — the
// handlers perform no I/O. The controller mirrors DegradedReason into
// the Workspace CRD's SecretsDelivery surface the US-72.3 staging
// classification reads; AppliedRevision feeds the lineage conjunct.
type RelayHealth struct {
	// Present reports whether relay-fronted providers were observed in
	// the applied batch (the entry metadata the US-72.4 builder emits).
	// False on flag-off pods: nothing to watch, never a degrade.
	Present bool `json:"present"`
	// Reachable reports the last credential-level probe outcome (GET
	// {baseURL}/models with the scoped token — served from the router's
	// staged catalog, zero upstream).
	Reachable bool `json:"reachable"`
	// DegradedReason is the active degrade code ("" healthy):
	// relay_unreachable | token_expired — plus the router's
	// machine-readable reject reasons when a probe was rejected
	// (credential_stale, scope_violation, sanitization_refused,
	// quota_exceeded) — the CredentialRejected/CredentialStale feeds.
	DegradedReason string `json:"degradedReason,omitempty"`
	// RouterURL is the router origin the monitor probes (no path, no
	// token material).
	RouterURL string `json:"routerUrl,omitempty"`
	// TokenExpiresAt is the earliest RFC3339 expiry across the batch's
	// relay tokens ("" when unknown).
	TokenExpiresAt string `json:"tokenExpiresAt,omitempty"`
	// AppliedRevision is the staged relay revision of the applied batch
	// (the entry metadata's relayRevision) — the controller compares it
	// against the workspace's staged-revision annotation (the lineage
	// conjunct of the §4.2 terminator).
	AppliedRevision string `json:"appliedRevision,omitempty"`
	// LastProbeAt is the last probe's unix seconds (0 before the first).
	LastProbeAt int64 `json:"lastProbeAt,omitempty"`
}

// ReadyzResponse is the response for GET /v1/readyz.
type ReadyzResponse struct {
	Ready               bool     `json:"ready"`
	ProvidersConnected  []string `json:"providers_connected"`
	ProvidersConfigured int      `json:"providers_configured"`
	AgentVersion        string   `json:"agent_version"`
	AgentType           string   `json:"agent_type"`
	// RelayInjected is true when the writer holds relay state — i.e. the
	// relay config block has been applied (via pre-boot injection, a
	// successful injector run, or a successful #910 re-arm cycle
	// mid-pod-life; evaluated live per request, not latched at boot).
	// The opencode restart that LOADS the config may still be pending:
	// session-aware deferral holds it until sessions idle, and the
	// re-arm path skips its own kill entirely when another deferred
	// restart is outstanding. False before any apply, when skipped
	// (personal opencode key), or while every attempt keeps failing.
	// Known corner (pre-existing, documented): a terminal auth.json
	// write failure after a successful config apply leaves this true
	// with the opencode-relay auth entry missing. Included here (readyz)
	// rather than statusz: readyz is cache-based and lightweight (no
	// synchronous opencode calls), and the API server needs this flag
	// on every ListModels cache miss — using statusz (which has no
	// latency upper bound) would be unsafe.
	RelayInjected bool `json:"relay_injected"`
	// Relay (US-72.4): the relay-only liveness degrade codes
	// (relay_unreachable / token_expired / router reject reasons).
	// Observability only — NEVER gates Ready: a relay outage must not
	// drop the pod from Service endpoints (that would compound the
	// outage); the remedy is the bounded re-arm + conditions.
	Relay *RelayHealth `json:"relay,omitempty"`
}

// FileUploadResponse is the response for PUT /v1/files (Epic 68 US-68.1).
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
	// RelayFreeModels: 0 unknown, 1 ok, 2 degraded (terminal fetch
	// failure this attempt — free-tier routing degraded until a re-arm
	// cycle applies, bounded by the 5m→30m backoff; #901 G8, #910).
	RelayFreeModels int32 `json:"relay_free_models"`
	// InFlightDeliveries: the ledger's unresolved delivery count pod-wide
	// (ledgered + admitted + stalled — the flip gate's drain signal,
	// design 0055 M4; 0 when the authority is not mounted).
	InFlightDeliveries int64 `json:"ledger_in_flight"`
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
	// SpawnedRev (US-70.1 / design 0057 I4): the env revision the agent
	// child ACTUALLY spawned with — terminal verification at the point
	// of consumption, relayed from the supervisor's control socket.
	SpawnedRev string `json:"spawned_rev,omitempty"`
	// Degraded (US-70.1 / design 0057 I10): active machine-readable
	// degrade reason ("" healthy). First value of the family:
	// spawn_env_unavailable — the child spawned platform-env-only from
	// the last-good cache because the pull missed its bound.
	Degraded string `json:"degraded,omitempty"`
	// Relay (US-72.4): the relay-only liveness slice (cached monitor
	// state; nil when not wired). Mirrors healthz's field on the deep
	// status endpoint so a relay degrade is visible between scrapes.
	Relay *RelayHealth `json:"relay,omitempty"`
}
