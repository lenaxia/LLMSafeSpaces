// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package v1

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestAgentRestartedCondition pinning (#1760): the condition type and
// its reason exist on the closed enums.
func TestAgentRestartedCondition(t *testing.T) {
	assert.Equal(t, WorkspaceConditionType("AgentRestarted"), WorkspaceConditionAgentRestarted)
	assert.Equal(t, "AgentRestarted", string(WorkspaceConditionAgentRestarted))
	assert.Equal(t, "WatchdogRestarted", ReasonWatchdogRestarted)
}

// TestWorkspaceStatus_AgentSupervisor_JSON pins the CRD wire shape:
// agent-neutral names, omitempty on the optional block.
func TestWorkspaceStatus_AgentSupervisor_JSON(t *testing.T) {
	at := metav1.NewTime(time.Date(2026, 10, 19, 17, 8, 14, 0, time.UTC))
	ws := Workspace{Status: WorkspaceStatus{
		AgentSupervisor: &AgentSupervisorStatus{
			AgentRestarts: 2,
			LastAgentRestart: &AgentRestartEvent{
				Reason:    "health_watchdog",
				Verdict:   "hung",
				LastError: "context deadline exceeded",
				At:        &at,
			},
			SuppressedRestarts:    12,
			LastSuppressedVerdict: "starved",
		},
	}}
	data, err := json.Marshal(ws.Status)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"agentSupervisor"`)
	assert.Contains(t, string(data), `"agentRestarts":2`)
	assert.Contains(t, string(data), `"lastAgentRestart"`)
	assert.Contains(t, string(data), `"reason":"health_watchdog"`)
	assert.Contains(t, string(data), `"verdict":"hung"`)
	assert.Contains(t, string(data), `"suppressedRestarts":12`)
	assert.Contains(t, string(data), `"lastSuppressedVerdict":"starved"`)

	var decoded WorkspaceStatus
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.NotNil(t, decoded.AgentSupervisor)
	require.NotNil(t, decoded.AgentSupervisor.LastAgentRestart)
	assert.True(t, decoded.AgentSupervisor.LastAgentRestart.At.Equal(&at),
		"restart timestamp must survive the CRD wire round-trip")
}

// TestWorkspaceStatus_NilAgentSupervisor_Omitted: workspaces without
// restart evidence carry no block (mixed fleet / never restarted).
func TestWorkspaceStatus_NilAgentSupervisor_Omitted(t *testing.T) {
	data, err := json.Marshal(WorkspaceStatus{})
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"agentSupervisor"`)
}

// TestAgentRestartEvent_NilAtAllowed: At is optional (zero-value safe
// decode).
func TestAgentRestartEvent_NilAtAllowed(t *testing.T) {
	var ev AgentRestartEvent
	require.NoError(t, json.Unmarshal([]byte(`{"reason":"crash"}`), &ev))
	assert.Nil(t, ev.At)
	assert.Empty(t, ev.Verdict)
}
