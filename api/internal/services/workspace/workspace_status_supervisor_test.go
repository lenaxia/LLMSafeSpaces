// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// workspace_status_supervisor_test.go — #1760: GetWorkspaceStatus
// surfaces the CRD's agentSupervisor block in the REST DTO, and the
// AgentRestarted condition rides conditions[] generically.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func TestGetWorkspaceStatus_SupervisorBlock(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	at := metav1.NewTime(time.Date(2026, 10, 19, 17, 8, 14, 0, time.UTC))
	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	activeCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	activeCrd.Status.Phase = v1.WorkspacePhaseActive
	activeCrd.Status.AgentSupervisor = &v1.AgentSupervisorStatus{
		AgentRestarts: 2,
		LastAgentRestart: &v1.AgentRestartEvent{
			Reason:    "health_watchdog",
			Verdict:   "hung",
			LastError: "Get /global/health: context deadline exceeded",
			At:        &at,
		},
		SuppressedRestarts:    78,
		LastSuppressedVerdict: "starved",
	}
	activeCrd.Status.Conditions = append(activeCrd.Status.Conditions, v1.WorkspaceCondition{
		Type:    v1.WorkspaceConditionAgentRestarted,
		Status:  "True",
		Reason:  v1.ReasonWatchdogRestarted,
		Message: "agent restarted by the supervisor (reason: health_watchdog, verdict: hung)",
	})
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(activeCrd, nil)

	result, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")

	require.NoError(t, err)
	require.NotNil(t, result.Supervisor, "CRD agentSupervisor must surface in the DTO")
	assert.Equal(t, 2, result.Supervisor.AgentRestarts)
	require.NotNil(t, result.Supervisor.LastAgentRestart)
	assert.Equal(t, "health_watchdog", result.Supervisor.LastAgentRestart.Reason)
	assert.Equal(t, "hung", result.Supervisor.LastAgentRestart.Verdict)
	assert.Equal(t, "Get /global/health: context deadline exceeded", result.Supervisor.LastAgentRestart.LastError)
	assert.WithinDuration(t, at.Time, result.Supervisor.LastAgentRestart.At, time.Second)
	assert.Equal(t, 78, result.Supervisor.SuppressedRestarts)
	assert.Equal(t, "starved", result.Supervisor.LastSuppressedVerdict)

	var found bool
	for _, c := range result.Conditions {
		if c.Type == "AgentRestarted" {
			found = true
			assert.Equal(t, "True", c.Status)
			assert.Equal(t, "WatchdogRestarted", c.Reason)
		}
	}
	assert.True(t, found, "AgentRestarted condition must ride conditions[]")

	// Agent-neutral wire copy: no tool names anywhere in the supervisor
	// payload.
	raw, err := json.Marshal(result.Supervisor)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "opencode")
}

func TestGetWorkspaceStatus_SupervisorOmittedWhenNil(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	activeCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	activeCrd.Status.Phase = v1.WorkspacePhaseActive
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(activeCrd, nil)

	result, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")

	require.NoError(t, err)
	assert.Nil(t, result.Supervisor)

	raw, err := json.Marshal(result)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"supervisor"`, "nil CRD block must omit the DTO field")
}

func TestGetWorkspaceStatus_SuppressorOnlyBlock(t *testing.T) {
	// The incident shape pre-kill: suppressions climbed, no fired
	// restart yet — counts still surface, no lastAgentRestart.
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	activeCrd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	activeCrd.Status.Phase = v1.WorkspacePhaseActive
	activeCrd.Status.AgentSupervisor = &v1.AgentSupervisorStatus{
		SuppressedRestarts:    74,
		LastSuppressedVerdict: "starved",
	}
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(activeCrd, nil)

	result, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")

	require.NoError(t, err)
	require.NotNil(t, result.Supervisor)
	assert.Equal(t, 74, result.Supervisor.SuppressedRestarts)
	assert.Equal(t, "starved", result.Supervisor.LastSuppressedVerdict)
	assert.Nil(t, result.Supervisor.LastAgentRestart)
	assert.Equal(t, 0, result.Supervisor.AgentRestarts)
}
