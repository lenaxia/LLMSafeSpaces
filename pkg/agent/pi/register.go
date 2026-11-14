// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package pi

// register.go — explicit registration at boot (no init() side effects,
// the US-65.6-followup posture; the wiring layer calls Register()).
// The Adapter itself is constructed per workspace through a Transport
// (S5 harness-selection plumbing); this registers the credentials
// runtime only, exactly as pkg/agent/opencode/register.go does.

import (
	"github.com/lenaxia/llmsafespaces/pkg/agent"
)

// Register installs the pi AgentRuntime (credentials seam).
func Register() {
	agent.Register(agent.AgentTypePi, &piAgentRuntime{})
}
