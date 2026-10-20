// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// supervisor_restart_chain_test.go — #1760 acceptance criterion: a
// simulated watchdog restart surfaces through the FULL chain against
// fakes of the existing harnesses: agentd wire JSON (exact bytes, the
// healthz contract) → controller checkAgentHealth → Workspace CRD
// status + condition → CRD wire JSON whose supervisor object keys are
// pinned to the SDK schema's property names (sdks/openapi.yaml
// SupervisorStatus/AgentRestartEvent) — the schema leg of the chain.
// The REST DTO leg is pinned by workspace_status_supervisor_test.go;
// the openapi schema itself by make sdk-check.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// TestSupervisorRestartChain_AgentdToCRDToSDKSchema: the incident's
// shape — a corroborated dead-listener kill after 78 consecutive
// failures and 12 starved suppressions — lands end-to-end.
func TestSupervisorRestartChain_AgentdToCRDToSDKSchema(t *testing.T) {
	// 1. The agentd WIRE shape: exact JSON the healthz handler emits
	// (pinned by pkg/agentd tests; re-stated here so the chain test
	// fails if the handler contract changes underneath the pipeline).
	wire := fmt.Sprintf(`{
		"healthy": true,
		"version": "1.2.27",
		"uptime_seconds": 3600,
		"supervisor": {
			"agentRestarts": 1,
			"lastAgentRestart": {
				"reason": "health_watchdog",
				"verdict": "hung",
				"lastError": "Get /global/health: context deadline exceeded",
				"at": %q
			},
			"consecutiveFailures": 78,
			"lastHealthError": "Get /global/health: context deadline exceeded",
			"suppressedRestarts": 12,
			"lastSuppressedVerdict": "starved"
		}
	}`, time.Now().UTC().Format(time.RFC3339))

	// The decode the controller performs on every scrape — this is the
	// agentd-side contract leg (wire bytes, not a Go struct literal).
	var healthResp agentd.HealthzResponse
	require.NoError(t, json.Unmarshal([]byte(wire), &healthResp))
	require.NotNil(t, healthResp.Supervisor)

	// The spawn-env harness serves the healthz body on the admin port
	// and builds the reconciler + workspace + fake client.
	r, ws, _ := setupSpawnEnvHealthTest(t, healthResp)

	// 2. The controller leg: one health check mirrors the supervisor
	// slice into the CRD within one reconcile cycle.
	r.checkAgentHealth(context.Background(), ws)

	// 3. The CRD leg: status + condition carry the restart.
	require.NotNil(t, ws.Status.AgentSupervisor)
	assert.Equal(t, int32(1), ws.Status.AgentSupervisor.AgentRestarts)
	require.NotNil(t, ws.Status.AgentSupervisor.LastAgentRestart)
	assert.Equal(t, "health_watchdog", ws.Status.AgentSupervisor.LastAgentRestart.Reason)
	assert.Equal(t, "hung", ws.Status.AgentSupervisor.LastAgentRestart.Verdict)
	assert.Equal(t, int32(12), ws.Status.AgentSupervisor.SuppressedRestarts)
	assert.Equal(t, "starved", ws.Status.AgentSupervisor.LastSuppressedVerdict)
	foundCond := false
	for _, c := range ws.Status.Conditions {
		if c.Type == v1.WorkspaceConditionAgentRestarted {
			foundCond = true
			assert.Equal(t, "True", c.Status)
			assert.Equal(t, v1.ReasonWatchdogRestarted, c.Reason)
		}
	}
	assert.True(t, foundCond, "AgentRestarted condition must be set within one reconcile cycle")

	// 4. The SDK-schema leg: the CRD's wire form (what the API reads
	// over the Kubernetes client and re-serves as the DTO) must carry
	// EXACTLY the property names sdks/openapi.yaml documents — a rename
	// on either side breaks this pin.
	crdWire, err := json.Marshal(ws.Status)
	require.NoError(t, err)
	var statusWire map[string]any
	require.NoError(t, json.Unmarshal(crdWire, &statusWire))
	sup, ok := statusWire["agentSupervisor"].(map[string]any)
	require.True(t, ok, "CRD wire must carry agentSupervisor")
	for _, key := range []string{"agentRestarts", "lastAgentRestart", "suppressedRestarts", "lastSuppressedVerdict"} {
		_, ok := sup[key]
		assert.True(t, ok, "SDK schema property %q missing from the pipeline's CRD wire", key)
	}
	last, ok := sup["lastAgentRestart"].(map[string]any)
	require.True(t, ok)
	for _, key := range []string{"reason", "verdict", "lastError", "at"} {
		_, ok := last[key]
		assert.True(t, ok, "SDK schema property %q missing from the pipeline's CRD wire", key)
	}
	assert.Equal(t, float64(1), sup["agentRestarts"])
	assert.Equal(t, "hung", last["verdict"])
}

// TestSupervisorRestartChain_SidecarCrashArm (#1776): the sidecar-
// topology crash shape — reason crash, NO verdict key, NO lastError —
// lands end-to-end through the same chain (agentd wire → controller →
// CRD + condition → SDK-schema property pins). The verdict's wire
// absence is the point: uncorroborated crash attribution must not
// claim one. Acceptance pin per the issue's AC1; its red mode is any
// revert of the reason-agnostic mirror legs.
func TestSupervisorRestartChain_SidecarCrashArm(t *testing.T) {
	at := time.Now().UTC().Format(time.RFC3339)
	wire := fmt.Sprintf(`{
		"healthy": true,
		"version": "1.2.27",
		"uptime_seconds": 3600,
		"supervisor": {
			"agentRestarts": 3,
			"lastAgentRestart": {
				"reason": "crash",
				"at": %q
			},
			"consecutiveFailures": 2,
			"lastHealthError": ""
		}
	}`, at)

	var healthResp agentd.HealthzResponse
	require.NoError(t, json.Unmarshal([]byte(wire), &healthResp))
	require.NotNil(t, healthResp.Supervisor)
	require.NotNil(t, healthResp.Supervisor.LastAgentRestart)
	assert.Empty(t, healthResp.Supervisor.LastAgentRestart.Verdict, "the crash arm carries no verdict key on the wire")

	r, ws, _ := setupSpawnEnvHealthTest(t, healthResp)
	r.checkAgentHealth(context.Background(), ws)

	require.NotNil(t, ws.Status.AgentSupervisor)
	assert.Equal(t, int32(3), ws.Status.AgentSupervisor.AgentRestarts)
	require.NotNil(t, ws.Status.AgentSupervisor.LastAgentRestart)
	assert.Equal(t, "crash", ws.Status.AgentSupervisor.LastAgentRestart.Reason)
	assert.Empty(t, ws.Status.AgentSupervisor.LastAgentRestart.Verdict)
	foundCond := false
	for _, c := range ws.Status.Conditions {
		if c.Type == v1.WorkspaceConditionAgentRestarted {
			foundCond = true
			assert.Equal(t, "True", c.Status)
			assert.Equal(t, v1.ReasonWatchdogRestarted, c.Reason)
			assert.Contains(t, c.Message, "reason: crash")
		}
	}
	assert.True(t, foundCond, "a sidecar crash restart must set the AgentRestarted condition")

	crdWire, err := json.Marshal(ws.Status)
	require.NoError(t, err)
	var statusWire map[string]any
	require.NoError(t, json.Unmarshal(crdWire, &statusWire))
	sup, ok := statusWire["agentSupervisor"].(map[string]any)
	require.True(t, ok, "CRD wire must carry agentSupervisor")
	for _, key := range []string{"agentRestarts", "lastAgentRestart"} {
		_, ok := sup[key]
		assert.True(t, ok, "SDK schema property %q missing from the crash arm's CRD wire", key)
	}
	last, ok := sup["lastAgentRestart"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "crash", last["reason"])
	_, hasVerdict := last["verdict"]
	assert.False(t, hasVerdict, "an empty verdict is omitempty on the CRD wire — the crash arm claims no verdict")
}
