// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// fakeImageFactoryStore is a test double for LaunchableConfigResolver.
// It records calls and returns canned results per-scope.
type fakeImageFactoryStore struct {
	// results maps scope → (config, imageRef, err)
	results map[imagefactory.ConfigScope]struct {
		cfg      imagefactory.Config
		imageRef string
		err      error
	}
	calls []struct {
		hash    string
		scope   imagefactory.ConfigScope
		ownerID string
		orgID   string
	}
}

func (f *fakeImageFactoryStore) GetLaunchableConfigByHash(_ context.Context, hash string, scope imagefactory.ConfigScope, ownerID, orgID *string) (imagefactory.Config, string, error) {
	owner := ""
	if ownerID != nil {
		owner = *ownerID
	}
	org := ""
	if orgID != nil {
		org = *orgID
	}
	f.calls = append(f.calls, struct {
		hash    string
		scope   imagefactory.ConfigScope
		ownerID string
		orgID   string
	}{hash, scope, owner, org})
	r, ok := f.results[scope]
	if !ok {
		return imagefactory.Config{}, "", ErrConfigNotLaunchable
	}
	return r.cfg, r.imageRef, r.err
}

// TestCreateWorkspace_ImageConfigHash_ResolvedToImageRef verifies the happy
// path: a user supplies ImageConfigHash pointing to their own Ready config,
// and the workspace's Runtime is set to the config's built image ref.
func TestCreateWorkspace_ImageConfigHash_ResolvedToImageRef(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.Anything).Return(crdWorkspace("ws-1", "default", "user-1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	store := &fakeImageFactoryStore{
		results: map[imagefactory.ConfigScope]struct {
			cfg      imagefactory.Config
			imageRef string
			err      error
		}{
			imagefactory.ScopeMember: {
				cfg: imagefactory.Config{
					ID:     "cfg-1",
					Hash:   "s-abc123",
					Status: imagefactory.StatusReady,
				},
				imageRef: "ghcr.io/lenaxia/ws:s-abc123-0.6.0",
			},
		},
	}
	f.svc.SetImageFactoryStore(store)

	req := types.CreateWorkspaceRequest{
		Name:            "my ws",
		StorageSize:     "1Gi",
		ImageConfigHash: "s-abc123",
	}
	_, err := f.svc.CreateWorkspace(ctx, "user-1", req)
	require.NoError(t, err)

	// The member-scope lookup should have been tried first.
	require.Len(t, store.calls, 1, "member-scope hit should short-circuit")
	assert.Equal(t, imagefactory.ScopeMember, store.calls[0].scope)
	assert.Equal(t, "user-1", store.calls[0].ownerID)

	// Verify the CRD got the image ref as its Runtime.
	f.ws.AssertCalled(t, "Create", mock.Anything, mock.MatchedBy(func(crd *v1.Workspace) bool {
		return crd.Spec.Runtime == "ghcr.io/lenaxia/ws:s-abc123-0.6.0"
	}))
}

// TestCreateWorkspace_ImageConfigHash_FallbackToOrgScope verifies that when
// a member-scope lookup misses, the resolver falls back to org-scope.
func TestCreateWorkspace_ImageConfigHash_FallbackToOrgScope(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	orgID := "org-1"

	f.ws.On("Create", mock.Anything, mock.Anything).Return(crdWorkspace("ws-1", "default", "user-1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)
	f.db.On("CountWorkspacesByUserAndOrg", mock.Anything, "user-1", orgID).Return(0, nil)
	f.db.On("CountActiveWorkspacesByUserAndOrg", mock.Anything, "user-1", orgID).Return(0, nil)

	org := newStubOrgChecker()
	org.members[orgID+":user-1"] = true
	org.userOrgID["user-1"] = orgID
	f.svc.SetOrgStore(org)

	store := &fakeImageFactoryStore{
		results: map[imagefactory.ConfigScope]struct {
			cfg      imagefactory.Config
			imageRef string
			err      error
		}{
			// Member-scope miss (no entry → ErrNotFound), org-scope hit.
			imagefactory.ScopeOrg: {
				cfg:      imagefactory.Config{ID: "cfg-org", Hash: "s-org1", Status: imagefactory.StatusReady},
				imageRef: "ghcr.io/lenaxia/ws:s-org1-0.6.0",
			},
		},
	}
	f.svc.SetImageFactoryStore(store)

	req := types.CreateWorkspaceRequest{
		Name:            "org ws",
		StorageSize:     "1Gi",
		ImageConfigHash: "s-org1",
	}
	_, err := f.svc.CreateWorkspace(ctx, "user-1", req)
	require.NoError(t, err)
	require.Len(t, store.calls, 2, "member-miss + org-hit = 2 calls")
	assert.Equal(t, imagefactory.ScopeMember, store.calls[0].scope)
	assert.Equal(t, imagefactory.ScopeOrg, store.calls[1].scope)
	assert.Equal(t, orgID, store.calls[1].orgID)
}

// TestCreateWorkspace_ImageConfigHash_NotFoundErrors verifies that a hash
// with no matching Ready config in any scope returns a validation error
// (not an internal error), so the user gets a clear message.
func TestCreateWorkspace_ImageConfigHash_NotFoundErrors(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	store := &fakeImageFactoryStore{
		results: map[imagefactory.ConfigScope]struct {
			cfg      imagefactory.Config
			imageRef string
			err      error
		}{}, // all scopes miss
	}
	f.svc.SetImageFactoryStore(store)

	req := types.CreateWorkspaceRequest{
		Name:            "ws",
		StorageSize:     "1Gi",
		ImageConfigHash: "s-nonexistent",
	}
	_, err := f.svc.CreateWorkspace(ctx, "user-1", req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not available for launch")
	// Should have tried all three scopes (member, platform; org skipped
	// because no orgStore wired → user has no org).
	assert.GreaterOrEqual(t, len(store.calls), 2, "should try member + platform")
}

// TestCreateWorkspace_ImageConfigHash_StoreNilReturnsValidation verifies
// that when image-factory is not configured, a hash is rejected cleanly.
func TestCreateWorkspace_ImageConfigHash_StoreNilReturnsValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Deliberately do NOT call SetImageFactoryStore.

	req := types.CreateWorkspaceRequest{
		Name:            "ws",
		StorageSize:     "1Gi",
		ImageConfigHash: "s-abc",
	}
	_, err := f.svc.CreateWorkspace(ctx, "user-1", req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image factory is not configured")
}

// TestCreateWorkspace_ImageConfigHash_DBErrorPropagates verifies that a
// non-ErrNotFound store error is surfaced as an internal error, not
// silently retried or swallowed.
func TestCreateWorkspace_ImageConfigHash_DBErrorPropagates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	store := &fakeImageFactoryStore{
		results: map[imagefactory.ConfigScope]struct {
			cfg      imagefactory.Config
			imageRef string
			err      error
		}{
			imagefactory.ScopeMember: {err: errors.New("connection refused")},
		},
	}
	f.svc.SetImageFactoryStore(store)

	req := types.CreateWorkspaceRequest{
		Name:            "ws",
		StorageSize:     "1Gi",
		ImageConfigHash: "s-abc",
	}
	_, err := f.svc.CreateWorkspace(ctx, "user-1", req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image_factory_lookup_failed")
}
