// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"go.uber.org/zap"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// ensureBootAgentConfig constructs the agent-config writer with the
// standard agentd sources and immediately re-marshals the config with an
// empty input, stamping the platform blocks — the built-in llmsafespaces
// MCP server (pre-marshal hook), the admin system prompt, and the
// allowed external directories — onto whatever the materialize
// subcommand left on disk.
//
// Why this exists (incident 2026-08-15): those blocks are only rendered
// by the writer's marshal path, and every other write trigger is
// conditional — pre-boot relay (needs a free-models catalog), the relay
// injector (needs a successful fetch), credential reload (needs user
// action). When all skip, agent-config.json stayed at the materialize
// base {$schema, provider, model} and opencode booted with no MCP
// server, no platform system prompt, and no /tmp external-dir approval.
// A boot-time unconditional write closes every skip path at once.
//
// Called BEFORE startManagedProcess so opencode reads the completed
// config on its first start; no restart is needed for this write. The
// write is idempotent — a second call (or a boot after the pre-boot
// relay already wrote everything) reproduces the same config.
//
// A normalize failure is logged and swallowed — boot continues with a
// usable writer, because no agentd at all is strictly worse than a
// degraded config that the next write path (credential reload, relay
// injector) repairs.
func ensureBootAgentConfig(agentConfigPath, adminPromptPath, allowedDirsPath string) *opencode.ConfigWriter {
	w := opencode.NewConfigWriter(agentConfigPath,
		opencode.WithAdminPromptPath(adminPromptPath),
		opencode.WithAllowedDirsPath(allowedDirsPath),
		opencode.WithPreMarshalHook(injectAgentdMCPServer),
	)
	if _, err := w.Apply(agent.AgentConfigInput{}); err != nil {
		if log != nil {
			log.Warn("boot agent-config normalize failed; config may lack platform blocks until the next write",
				zap.String("path", agentConfigPath),
				zap.Error(err),
			)
		}
	}
	return w
}
