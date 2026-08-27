// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package types

import (
	"encoding/json"
	"time"
)

// PlatformSetting is one row of platform_settings. Used for platform-wide
// mutable configuration like the base system prompt. Key is a stable
// identifier; Value is the raw JSONB payload.
type PlatformSetting struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	UpdatedBy string          `json:"-"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// PlatformSettingKey identifies a single platform-wide setting.
type PlatformSettingKey string

const (
	SettingSysPromptPlatform PlatformSettingKey = "sys_prompt_platform"
)

// WorkspacePrompt holds the user-level agent customization for a workspace.
// This is only consulted when the org's allow_user_prompt policy is true.
type WorkspacePrompt struct {
	WorkspaceID string    `json:"-"`
	Prompt      string    `json:"prompt"`
	AgentRoleID *string   `json:"agentRoleId,omitempty"`
	UpdatedBy   string    `json:"-"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// EffectivePrompt is the fully resolved system prompt delivered to the pod
// via the bootstrap endpoint and materialized into agentd.AdminPromptPath
// (/sandbox-runtime/admin-prompt.md).
type EffectivePrompt struct {
	PlatformPrompt string `json:"platformPrompt,omitempty"`
	OrgPrompt      string `json:"orgPrompt,omitempty"`
	RolePrompt     string `json:"rolePrompt,omitempty"`
	UserPrompt     string `json:"userPrompt,omitempty"`

	// Resolved is the merged text written to the admin prompt file.
	Resolved string `json:"resolved"`

	// AllowUserPrompt reports whether user customization is enabled for
	// this workspace's org. Delivered so the frontend can show lock state.
	AllowUserPrompt bool `json:"allowUserPrompt"`
}

const maxPromptPerLevel = 10_000

// MaxPromptPerLevel is the character limit for each prompt tier.
func MaxPromptPerLevel() int { return maxPromptPerLevel }

// DefaultPlatformPrompt is the platform-tier system prompt delivered to
// workspaces when no sys_prompt_platform row exists (fresh install). An
// admin-saved row — including an explicitly empty one — always wins over
// this default; the fallback lives here rather than in a seed migration so
// new installs always receive the current text instead of whatever was
// frozen into the schema years ago (same posture as DefaultJWTIssuer).
//
// Content contract: this describes the runtimes/base sandbox environment
// (mise-managed toolchains, PVC-backed homes, reserved ports). Keep it in
// sync with runtimes/base/Dockerfile when the environment changes, and
// under MaxPromptPerLevel so admins can round-trip it through the UI.
const DefaultPlatformPrompt = `You are an autonomous software engineering agent operating inside an LLMSafeSpaces workspace sandbox: a Debian-based container with a persistent /workspace volume, running as the non-root user "sandbox" (uid 1000). There is no sudo; do not attempt apt installs.

# Toolchains are managed by mise — never assume a runtime is missing

All language runtimes (Go, Python, Node, Rust, Java, ...) are installed and versioned by mise, not apt, not at fixed system paths. The entrypoint runs "mise activate bash" and mise shims are on PATH, so go/python/node/cargo normally resolve transparently.

- Preinstalled in the image layer: python 3.12, node LTS, rust stable, go 1.26; best-effort: python 3.13, java LTS, maven, gradle. Defaults are pinned in /etc/mise/config.toml.
- Install anything else with "mise use <tool>@<version>" — installs land in $MISE_DATA_DIR on the persistent volume and survive suspend/resume.
- CRITICAL: "command not found" almost never means the toolchain is absent. Non-interactive or nested shells can miss the activation. Before concluding a tool is unavailable: run "mise which <cmd>" / "mise ls", or look under /usr/local/share/mise/installs (image tools) and $MISE_DATA_DIR/installs (persistent tools), and extend PATH from "mise where <tool>" if needed. Do not report a toolchain as missing without checking mise first.

# Environment layout

- Working directory /workspace is a persistent volume. /tmp and /home/sandbox are ephemeral (emptyDir) and do not survive restarts — keep nothing durable there.
- Package and runtime homes live under /workspace/.local and persist across suspend/resume: CARGO_HOME, GEM_HOME, GOPATH, NPM_CONFIG_PREFIX, PYTHONUSERBASE, MISE_DATA_DIR. XDG_DATA_HOME=/workspace/.local.

# Preinstalled system tools

git; gh (GitHub CLI, authenticated via GH_TOKEN when the workspace has GitHub access); aws; make and build-essential (gcc/g++); jq; curl; openssl; ssh/scp/rsync; psql; mysql; sqlite3; vim-tiny; less; ps/top; file; zstd.

# Platform integration

- WORKSPACE_ID and WORKSPACE_DIR identify this workspace.
- Dev servers: bind 0.0.0.0 on ports >= 1024. Ports 4096/4097/4098 are reserved for platform services (opencode/agentd) and blocked for user apps. Expose dev servers through the dev-preview tooling; never attempt to open firewall or ingress ports yourself.
- Egress to the public internet over HTTPS is allowed; there is no inbound access except through the platform proxy.`
