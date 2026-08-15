// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// Regression for the 2026-08-14 incident: a floating-tag
// workspace.defaultImage stored in the DB (pre-validation write, not
// covered by migration 000023, which only removes the exact seeded
// default) must not be launched — registry mirrors resolve floating tags
// to stale per-node digests. The read path skips it and falls through to
// the chart-pinned base RuntimeEnvironment instead.
func TestCreateWorkspace_EmptyRuntime_StoredFloatingDefaultImage_NotLaunched(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultImage": "ghcr.io/lenaxia/llmsafespaces/base:latest",
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Runtime == "base"
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: ""}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

func TestCreateWorkspace_EmptyRuntime_StoredUntaggedDefaultImage_NotLaunched(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultImage": "ghcr.io/lenaxia/llmsafespaces/base",
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Runtime == "base"
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: ""}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

func TestCreateWorkspace_EmptyRuntime_StoredPinnedDefaultImage_StillUsed(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultImage": "ghcr.io/lenaxia/llmsafespaces/base:0.15.5",
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Runtime == "ghcr.io/lenaxia/llmsafespaces/base:0.15.5"
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: ""}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}

func TestCreateWorkspace_EmptyRuntime_StoredRTEDefaultImage_StillUsed(t *testing.T) {
	f := newDefaultsFixture(t, map[string]any{
		"workspace.defaultImage": "base",
	})
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.MatchedBy(func(ws *v1.Workspace) bool {
		return ws.Spec.Runtime == "base"
	})).Return(crdWorkspace("ws-1", "default", "user1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	req := types.CreateWorkspaceRequest{Name: "test", StorageSize: "1Gi", Runtime: ""}
	_, err := f.svc.CreateWorkspace(ctx, "user1", req)
	assert.NoError(t, err)
	f.ws.AssertExpectations(t)
}
