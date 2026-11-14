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

// RegisterAdapter installs the pi Adapter factory (S5
// harness-selection plumbing): the registry-driven construction the
// API wiring routes per-workspace harness selection through. The
// adapter's Transport is the production pod bridge client
// (podtransport.go) built from the same resolvers the opencode factory
// receives.
func RegisterAdapter() {
	agent.RegisterAdapter(agent.AgentTypePi, func(deps agent.AdapterFactoryDeps) (agent.Adapter, error) {
		return NewAdapter(NewPodTransport(deps.PasswordResolver, deps.PodResolver, deps.Logger), deps.Logger), nil
	})
}
