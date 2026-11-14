// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import agent "github.com/lenaxia/llmsafespaces/pkg/agent"

// Register installs the opencode AgentRuntime (credentials seam).
func Register() {
	agent.Register(agent.AgentTypeOpenCode, &OpenCodeAgent{})
}

// RegisterAdapter installs the opencode Adapter factory (S5
// harness-selection plumbing): the registry-driven construction the
// API wiring routes per-workspace harness selection through. Builds
// exactly what app.go hard-constructed pre-S5 (NewAdapter → the V1
// delivery split, #1314) — the golden parity is pinned by the app
// wiring test.
func RegisterAdapter() {
	agent.RegisterAdapter(agent.AgentTypeOpenCode, func(deps agent.AdapterFactoryDeps) (agent.Adapter, error) {
		base := NewAdapter(
			PasswordResolver(deps.PasswordResolver),
			deps.PodResolver,
			deps.Logger,
		)
		return NewAdapterV1(base), nil
	})
}
