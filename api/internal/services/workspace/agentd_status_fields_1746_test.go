// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// agentd_status_fields_1746_test.go — #1746: the API's workspace
// status surface carries the agentd delivery drift fields the
// controller records on the CRD (agentdEffectiveDigest /
// agentdPendingDigest), so the dashboard can render "agentd X pending
// — applies on next refresh" without cluster access.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func TestGetWorkspaceStatus_AgentdDigestFields(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	crd.Status.AgentdEffectiveDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	crd.Status.AgentdPendingDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)

	result, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")

	require.NoError(t, err)
	assert.Equal(t, "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", result.AgentdEffectiveDigest,
		"the effective digest surfaces — what the running pod was built with")
	assert.Equal(t, "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", result.AgentdPendingDigest,
		"the pending digest surfaces — 'applies on next refresh'")
}

func TestGetWorkspaceStatus_AgentdDigestFieldsConverged(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.db.On("GetWorkspace", ctx, "ws-1").Return(dbWorkspace("ws-1", "user1", "my-ws", "10Gi"), nil)
	crd := crdWorkspace("ws-1", "default", "user1", "10Gi")
	crd.Status.Phase = v1.WorkspacePhaseActive
	crd.Status.AgentdEffectiveDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// Pending empty (converged) — omits from the JSON surface.
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(crd, nil)

	result, err := f.svc.GetWorkspaceStatus(ctx, "user1", "ws-1")

	require.NoError(t, err)
	assert.Equal(t, "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", result.AgentdEffectiveDigest)
	assert.Empty(t, result.AgentdPendingDigest, "converged: no pending digest")
}
