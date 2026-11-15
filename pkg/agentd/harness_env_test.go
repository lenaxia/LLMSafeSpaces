// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agentd

import "testing"

// TestHarnessEnvNames pins the literal env names of the harness
// contract: the controller wires these into pod specs and agentd reads
// them at boot — a rename on either side breaks a mixed-version fleet
// in a way no compile error catches. The values are API now.
func TestHarnessEnvNames(t *testing.T) {
	if EnvAgentdHarness != "AGENTD_HARNESS" {
		t.Fatalf("EnvAgentdHarness = %q; want AGENTD_HARNESS (wired by controller/internal/workspace/agentd_sidecar.go, read by cmd/workspace-agentd/harness_driver.go)", EnvAgentdHarness)
	}
	if EnvPiAgentDir != "LLMSAFESPACES_PI_AGENT_DIR" {
		t.Fatalf("EnvPiAgentDir = %q; want LLMSAFESPACES_PI_AGENT_DIR (wired by controller/internal/workspace/agentd_sidecar.go, read by cmd/workspace-agentd/{sidecar_pi,pi_supervisor}.go)", EnvPiAgentDir)
	}
}
