// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// us4b_paths.go — design 0051 US-4b: env-overridable store coordinates.
//
// The controller relocates agentd's stores by consumer in sidecar mode
// (agent-config.json + allowed-dirs.json → /agentd-config; admin-prompt
// → /agentd-secrets). Every consumer reads its coordinate through these
// helpers, so the relocation is purely a controller env change and the
// single-container defaults (the /sandbox-runtime consts) stand when the
// env is unset — byte-identical behavior.

import (
	"path/filepath"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

func agentConfigPathFromEnv() string {
	return envOrDefault("LLMSAFESPACES_AGENT_CONFIG_PATH", agentd.AgentConfigPath)
}

func adminPromptPathFromEnv() string {
	return envOrDefault("LLMSAFESPACES_ADMIN_PROMPT_PATH", agentd.AdminPromptPath)
}

func allowedDirsPathFromEnv() string {
	return envOrDefault("LLMSAFESPACES_ALLOWED_DIRS_PATH", agentd.AllowedDirsPath)
}

// secretsEnvPathFromEnv is the secrets-env coordinate (the spawn_env
// consumer, the resync apply path, and the materializer share it — one
// coordinate, so US-4b's relocation is a controller env change).
func secretsEnvPathFromEnv() string {
	return envOrDefault("LLMSAFESPACES_SECRETS_ENV_PATH", agentd.SecretsEnvPath)
}

// modelWarnPathFromEnv keeps the model-resolution-warning marker colocated
// with the agent-config file under any relocation — the materializer's
// writer derives it from filepath.Dir(agentConfigPath), so the reader
// must derive it the same way or the two diverge once the config moves.
func modelWarnPathFromEnv() string {
	return filepath.Join(filepath.Dir(agentConfigPathFromEnv()), filepath.Base(agentd.ModelResolutionWarningPath))
}

// bootAgentConfigPathsWithEnv is the (config, prompt, dirs) triple the
// boot stamp consumes in BOTH agentd modes (main and --sidecar).
func bootAgentConfigPathsWithEnv() (string, string, string) {
	return agentConfigPathFromEnv(), adminPromptPathFromEnv(), allowedDirsPathFromEnv()
}
