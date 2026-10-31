// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// agentd_version_visibility_rca_test.go — RC-A (2026-10-30 incident):
// the API's workspace status surface carries the RUNNING agentd's
// self-reported build identity (agentdVersion / agentdCommit /
// agentdBuildTime) plus the AgentdVersionStale condition, so fleet
// currency is readable without shell access to the pod — the incident
// needed a manual `curl localhost:4098/v1/healthz` to learn any of it.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func TestGetWorkspaceStatus_AgentdBuildIdentityFields(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	// The incident's exact shape: v0.44.7 (commit 6ce1859f, built
	// Oct 29 02:08) reported by the pod's healthz, mirrored by the
	// controller, flagged stale against fleet 0.44.11.
	crd.Status.AgentdVersion = "0.44.7"
	crd.Status.AgentdCommit = "6ce1859f"
	crd.Status.AgentdBuildTime = "2026-10-29T02:08:00Z"
	crd.Status.Conditions = append(crd.Status.Conditions, v1.WorkspaceCondition{
		Type: v1.WorkspaceConditionAgentdVersionStale, Status: "True",
		Reason:  v1.ReasonAgentdVersionStale,
		Message: "agentd 0.44.7 (commit 6ce1859f) is older than the fleet release 0.44.11",
	})
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)

	result, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")

	require.NoError(t, err)
	assert.Equal(t, "0.44.7", result.AgentdVersion,
		"the running agentd's reported version must surface via the API")
	assert.Equal(t, "6ce1859f", result.AgentdCommit)
	assert.Equal(t, "2026-10-29T02:08:00Z", result.AgentdBuildTime)

	found := false
	for _, c := range result.Conditions {
		if c.Type == string(v1.WorkspaceConditionAgentdVersionStale) {
			found = true
			assert.Equal(t, "True", c.Status)
			assert.Contains(t, c.Message, "0.44.7")
		}
	}
	assert.True(t, found, "the AgentdVersionStale condition must ride the conditions surface")
}

func TestGetWorkspaceStatus_AgentdBuildIdentityEmptyWhenUnreported(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)

	result, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")

	require.NoError(t, err)
	assert.Empty(t, result.AgentdVersion, "no report (pod unreachable / pre-RC-A controller): stays empty, omits from JSON")
	assert.Empty(t, result.AgentdCommit)
	assert.Empty(t, result.AgentdBuildTime)
}
