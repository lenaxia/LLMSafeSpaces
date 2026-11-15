// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agentd

// Harness-selection env contract (S5 harness-selection plumbing,
// pi-scoping §6): the single source for the env names the CONTROLLER
// wires into the pod and the AGENTD processes read. One definition,
// pinned by test — never re-spelled on either side of the boundary
// (the review finding: duplicated literals drift silently and a
// mixed-version controller/agentd fleet breaks).

const (
	// EnvAgentdHarness carries the workspace's spec.harness into
	// agentd (absent = opencode — the mixed-fleet encoding).
	EnvAgentdHarness = "AGENTD_HARNESS"

	// EnvPiAgentDir names the shared pi agent-dir mount: the sidecar's
	// ConfigWriter renders into it, the supervisor's children consume
	// it (PI_CODING_AGENT_DIR is derived from this coordinate).
	EnvPiAgentDir = "LLMSAFESPACES_PI_AGENT_DIR"
)
