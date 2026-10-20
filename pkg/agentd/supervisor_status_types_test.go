// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agentd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSupervisorStatus_JSON pins the wire shape of the #1760 supervisor
// slice: agent-neutral field names, the full five-value verdict taxonomy
// (hung/starved/flat/respawn/unknown — the real enum in
// cmd/workspace-agentd/watchdog_vitals.go, #892), and omitempty on the
// optional nested record.
func TestSupervisorStatus_JSON(t *testing.T) {
	at := time.Date(2026, 10, 19, 17, 8, 14, 0, time.UTC)
	resp := HealthzResponse{Healthy: true, Supervisor: &SupervisorStatus{
		AgentRestarts:         2,
		LastAgentRestart:      &AgentRestartEvent{Reason: "health_watchdog", Verdict: "hung", LastError: "Get /global/health: context deadline exceeded", At: at},
		ConsecutiveFailures:   78,
		LastHealthError:       "Get /global/health: context deadline exceeded",
		SuppressedRestarts:    12,
		LastSuppressedVerdict: "starved",
	}}
	data, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"supervisor"`)
	assert.Contains(t, string(data), `"agentRestarts":2`)
	assert.Contains(t, string(data), `"lastAgentRestart"`)
	assert.Contains(t, string(data), `"reason":"health_watchdog"`)
	assert.Contains(t, string(data), `"verdict":"hung"`)
	assert.Contains(t, string(data), `"lastError":"Get /global/health: context deadline exceeded"`)
	assert.Contains(t, string(data), `"consecutiveFailures":78`)
	assert.Contains(t, string(data), `"suppressedRestarts":12`)
	assert.Contains(t, string(data), `"lastSuppressedVerdict":"starved"`)

	var decoded HealthzResponse
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.NotNil(t, decoded.Supervisor)
	require.NotNil(t, decoded.Supervisor.LastAgentRestart)
	assert.Equal(t, at.UTC(), decoded.Supervisor.LastAgentRestart.At.UTC())
	assert.Equal(t, "starved", decoded.Supervisor.LastSuppressedVerdict)
}

// TestSupervisorStatus_NilOmitted: mixed-fleet safety — a pre-#1760
// agentd omits the field entirely (no supervisor key on the wire).
func TestSupervisorStatus_NilOmitted(t *testing.T) {
	for name, resp := range map[string]any{
		"healthz": HealthzResponse{Healthy: true},
		"statusz": StatuszResponse{Healthy: true},
	} {
		data, err := json.Marshal(resp)
		require.NoError(t, err, name)
		assert.NotContains(t, string(data), `"supervisor"`, name)
	}
}

// TestSupervisorStatus_StatuszRoundTrip: the same snapshot shape rides
// /v1/statusz.
func TestSupervisorStatus_StatuszRoundTrip(t *testing.T) {
	resp := StatuszResponse{Healthy: true, Supervisor: &SupervisorStatus{
		AgentRestarts:    1,
		LastAgentRestart: &AgentRestartEvent{Reason: "health_watchdog_unknown_episode", Verdict: "unknown"},
	}}
	data, err := json.Marshal(resp)
	require.NoError(t, err)

	var decoded StatuszResponse
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.NotNil(t, decoded.Supervisor)
	require.NotNil(t, decoded.Supervisor.LastAgentRestart)
	assert.Equal(t, "health_watchdog_unknown_episode", decoded.Supervisor.LastAgentRestart.Reason)
	assert.Equal(t, "unknown", decoded.Supervisor.LastAgentRestart.Verdict)
	assert.Empty(t, decoded.Supervisor.LastAgentRestart.LastError)
	assert.True(t, decoded.Supervisor.LastAgentRestart.At.IsZero(), "zero At marshals as the zero time, never panics")
}

// TestSupervisorStatus_VerdictTaxonomy pins the exact closed set of
// verdict values the schema documents (the real #892 enum, not the
// two-value example in the issue text).
func TestSupervisorStatus_VerdictTaxonomy(t *testing.T) {
	want := map[string]bool{"hung": true, "starved": true, "flat": true, "respawn": true, "unknown": true}
	for v := range want {
		_, err := json.Marshal(AgentRestartEvent{Reason: "health_watchdog", Verdict: v})
		assert.NoError(t, err, v)
	}
}
