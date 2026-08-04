// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// fakeUserSettings is a test double for UserDefaultReader.
type fakeUserSettings struct {
	preferredRuntime string
	err              error
}

func (f *fakeUserSettings) GetString(_ context.Context, _, _ string) (string, error) {
	return f.preferredRuntime, f.err
}

// fakeOrgPolicyChecker is a test double for PolicyChecker that returns
// a policy with DefaultRuntime set (for the org-default tier of the hierarchy).
type fakeOrgPolicyChecker struct {
	defaultRuntime string
	err            error
}

func (f *fakeOrgPolicyChecker) GetEffectivePolicy(_ context.Context, _ string) (*types.OrgPolicyValues, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.defaultRuntime == "" {
		return &types.OrgPolicyValues{}, nil
	}
	rt := f.defaultRuntime
	return &types.OrgPolicyValues{DefaultRuntime: &rt}, nil
}

// TestResolveDefaultRuntime_UserPreferenceWins verifies that when a user has
// a preferredRuntime set and it resolves, it takes priority over org/platform.
func TestResolveDefaultRuntime_UserPreferenceWins(t *testing.T) {
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
				cfg:      imagefactory.Config{ID: "cfg-1", Hash: "s-user", Status: imagefactory.StatusReady},
				imageRef: "ghcr.io/ws:s-user-0.6.0",
			},
		},
	}
	f.svc.SetImageFactoryStore(store)
	f.svc.SetUserSettings(&fakeUserSettings{preferredRuntime: "s-user"})

	req := types.CreateWorkspaceRequest{Name: "ws", StorageSize: "1Gi"}
	_, err := f.svc.CreateWorkspace(ctx, "user-1", req)
	require.NoError(t, err)

	f.ws.AssertCalled(t, "Create", mock.Anything, mock.MatchedBy(func(crd *v1.Workspace) bool {
		return crd.Spec.Runtime == "ghcr.io/ws:s-user-0.6.0"
	}))
}

// TestResolveDefaultRuntime_OrgDefaultWhenNoUserPref verifies org default
// is used when the user has no preference set.
func TestResolveDefaultRuntime_OrgDefaultWhenNoUserPref(t *testing.T) {
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
			imagefactory.ScopeOrg: {
				cfg:      imagefactory.Config{ID: "cfg-org", Hash: "s-org", Status: imagefactory.StatusReady},
				imageRef: "ghcr.io/ws:s-org-0.6.0",
			},
		},
	}
	f.svc.SetImageFactoryStore(store)
	f.svc.SetUserSettings(&fakeUserSettings{preferredRuntime: ""}) // no user pref
	f.svc.SetPolicyChecker(&fakeOrgPolicyChecker{defaultRuntime: "s-org"})

	req := types.CreateWorkspaceRequest{Name: "ws", StorageSize: "1Gi"}
	_, err := f.svc.CreateWorkspace(ctx, "user-1", req)
	require.NoError(t, err)

	f.ws.AssertCalled(t, "Create", mock.Anything, mock.MatchedBy(func(crd *v1.Workspace) bool {
		return crd.Spec.Runtime == "ghcr.io/ws:s-org-0.6.0"
	}))
}

// TestResolveDefaultRuntime_FallbackToBase verifies that when nothing is
// configured (no user, org, or platform default), runtime falls back to "base".
func TestResolveDefaultRuntime_FallbackToBase(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.Anything).Return(crdWorkspace("ws-1", "default", "user-1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	// No user/org settings wired → should fall through to "base".
	req := types.CreateWorkspaceRequest{Name: "ws", StorageSize: "1Gi"}
	_, err := f.svc.CreateWorkspace(ctx, "user-1", req)
	require.NoError(t, err)

	f.ws.AssertCalled(t, "Create", mock.Anything, mock.MatchedBy(func(crd *v1.Workspace) bool {
		return crd.Spec.Runtime == "base"
	}))
}

// TestResolveDefaultRuntime_UserPrefNotLaunchableFallsThrough verifies that
// if the user's preferred config can't be resolved (deleted, not Ready),
// the hierarchy falls through to the next tier rather than erroring.
func TestResolveDefaultRuntime_UserPrefNotLaunchableFallsThrough(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.Anything).Return(crdWorkspace("ws-1", "default", "user-1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	// User pref points to a non-existent config → falls through to "base".
	f.svc.SetImageFactoryStore(&fakeImageFactoryStore{}) // all scopes miss
	f.svc.SetUserSettings(&fakeUserSettings{preferredRuntime: "s-deleted"})

	req := types.CreateWorkspaceRequest{Name: "ws", StorageSize: "1Gi"}
	_, err := f.svc.CreateWorkspace(ctx, "user-1", req)
	require.NoError(t, err)

	f.ws.AssertCalled(t, "Create", mock.Anything, mock.MatchedBy(func(crd *v1.Workspace) bool {
		return crd.Spec.Runtime == "base"
	}))
}

// TestResolveDefaultRuntime_DBErrorFallsThrough verifies that a DB error
// when reading user settings doesn't block creation — it logs and falls through.
func TestResolveDefaultRuntime_DBErrorFallsThrough(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.ws.On("Create", mock.Anything, mock.Anything).Return(crdWorkspace("ws-1", "default", "user-1", "1Gi"), nil)
	f.db.On("CreateWorkspace", ctx, mock.Anything).Return(nil)

	f.svc.SetUserSettings(&fakeUserSettings{err: errors.New("connection refused")})

	req := types.CreateWorkspaceRequest{Name: "ws", StorageSize: "1Gi"}
	_, err := f.svc.CreateWorkspace(ctx, "user-1", req)
	require.NoError(t, err)

	// Should have fallen through to "base".
	f.ws.AssertCalled(t, "Create", mock.Anything, mock.MatchedBy(func(crd *v1.Workspace) bool {
		return crd.Spec.Runtime == "base"
	}))
}

// TestResolveDefaultRuntime_UserMissOrgHit verifies the most important
// cross-tier behavior: when the user has NO preference (miss), the org
// default is used. This validates the hierarchy waterfall actually falls
// through correctly from tier 1 to tier 2.
func TestResolveDefaultRuntime_UserMissOrgHit(t *testing.T) {
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

	// User has no preference (empty string). Org has a default.
	store := &fakeImageFactoryStore{
		results: map[imagefactory.ConfigScope]struct {
			cfg      imagefactory.Config
			imageRef string
			err      error
		}{
			imagefactory.ScopeOrg: {
				cfg:      imagefactory.Config{ID: "cfg-org", Hash: "s-org", Status: imagefactory.StatusReady},
				imageRef: "ghcr.io/ws:s-org-0.6.0",
			},
		},
	}
	f.svc.SetImageFactoryStore(store)
	f.svc.SetUserSettings(&fakeUserSettings{preferredRuntime: ""})         // user miss
	f.svc.SetPolicyChecker(&fakeOrgPolicyChecker{defaultRuntime: "s-org"}) // org hit

	req := types.CreateWorkspaceRequest{Name: "ws", StorageSize: "1Gi"}
	_, err := f.svc.CreateWorkspace(ctx, "user-1", req)
	require.NoError(t, err)

	f.ws.AssertCalled(t, "Create", mock.Anything, mock.MatchedBy(func(crd *v1.Workspace) bool {
		return crd.Spec.Runtime == "ghcr.io/ws:s-org-0.6.0"
	}))
}
