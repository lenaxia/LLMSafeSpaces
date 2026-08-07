// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// --- Design/0047 D3: allowed_image_configs policy enforcement ---

// TestImageConfigRestriction_OrgConfigBlocked verifies that when an org has
// a non-empty allowed_image_configs policy, an org-scoped config NOT in the
// list is rejected at launch.
func TestImageConfigRestriction_OrgConfigBlocked(t *testing.T) {
	f := newFixture(t)
	orgID := "org-1"
	allowed := []string{"s-allowed-hash"} // only this hash is allowed

	f.svc.SetPolicyChecker(&stubPolicyChecker{
		policy: &types.OrgPolicyValues{AllowedImageConfigs: &allowed},
	})
	f.svc.SetImageFactoryStore(&fakeImageFactoryStore{
		results: map[imagefactory.ConfigScope]struct {
			cfg      imagefactory.Config
			imageRef string
			err      error
		}{
			imagefactory.ScopeOrg: {
				cfg:      imagefactory.Config{ID: "cfg-1", Hash: "s-blocked", Scope: imagefactory.ScopeOrg},
				imageRef: "ghcr.io/test/ws:s-blocked",
				err:      nil,
			},
		},
	})

	_, err := f.svc.resolveImageFactoryConfig(context.Background(), "s-blocked", "user-1", &orgID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in your organization's allowed image list")
}

// TestImageConfigRestriction_OrgConfigAllowed verifies that a config IN the
// allowed list launches normally.
func TestImageConfigRestriction_OrgConfigAllowed(t *testing.T) {
	f := newFixture(t)
	orgID := "org-1"
	allowed := []string{"s-allowed-hash"}

	f.svc.SetPolicyChecker(&stubPolicyChecker{
		policy: &types.OrgPolicyValues{AllowedImageConfigs: &allowed},
	})
	f.svc.SetImageFactoryStore(&fakeImageFactoryStore{
		results: map[imagefactory.ConfigScope]struct {
			cfg      imagefactory.Config
			imageRef string
			err      error
		}{
			imagefactory.ScopeOrg: {
				cfg:      imagefactory.Config{ID: "cfg-1", Hash: "s-allowed-hash", Scope: imagefactory.ScopeOrg},
				imageRef: "ghcr.io/test/ws:s-allowed",
				err:      nil,
			},
		},
	})

	ref, err := f.svc.resolveImageFactoryConfig(context.Background(), "s-allowed-hash", "user-1", &orgID)
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/test/ws:s-allowed", ref)
}

// TestImageConfigRestriction_MemberConfigExempt verifies that member-scoped
// configs are NOT subject to the restriction — even when the policy is set.
func TestImageConfigRestriction_MemberConfigExempt(t *testing.T) {
	f := newFixture(t)
	orgID := "org-1"
	allowed := []string{"s-different-hash"} // member config hash not in list

	f.svc.SetPolicyChecker(&stubPolicyChecker{
		policy: &types.OrgPolicyValues{AllowedImageConfigs: &allowed},
	})
	f.svc.SetImageFactoryStore(&fakeImageFactoryStore{
		results: map[imagefactory.ConfigScope]struct {
			cfg      imagefactory.Config
			imageRef string
			err      error
		}{
			imagefactory.ScopeMember: {
				cfg:      imagefactory.Config{ID: "cfg-1", Hash: "s-member-hash", Scope: imagefactory.ScopeMember},
				imageRef: "ghcr.io/test/ws:s-member",
				err:      nil,
			},
		},
	})

	ref, err := f.svc.resolveImageFactoryConfig(context.Background(), "s-member-hash", "user-1", &orgID)
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/test/ws:s-member", ref)
}

// TestImageConfigRestriction_EmptyPolicyUnrestricted verifies that an empty
// (or nil) allowed_image_configs policy means unrestricted.
func TestImageConfigRestriction_EmptyPolicyUnrestricted(t *testing.T) {
	f := newFixture(t)
	orgID := "org-1"
	empty := []string{}

	f.svc.SetPolicyChecker(&stubPolicyChecker{
		policy: &types.OrgPolicyValues{AllowedImageConfigs: &empty},
	})
	f.svc.SetImageFactoryStore(&fakeImageFactoryStore{
		results: map[imagefactory.ConfigScope]struct {
			cfg      imagefactory.Config
			imageRef string
			err      error
		}{
			imagefactory.ScopePlatform: {
				cfg:      imagefactory.Config{ID: "cfg-1", Hash: "s-any", Scope: imagefactory.ScopePlatform},
				imageRef: "ghcr.io/test/ws:s-any",
				err:      nil,
			},
		},
	})

	ref, err := f.svc.resolveImageFactoryConfig(context.Background(), "s-any", "user-1", &orgID)
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/test/ws:s-any", ref)
}

// TestImageConfigRestriction_PlatformConfigBlocked verifies that a
// platform-scoped config is also subject to the org's restriction.
func TestImageConfigRestriction_PlatformConfigBlocked(t *testing.T) {
	f := newFixture(t)
	orgID := "org-1"
	allowed := []string{"s-different"}

	f.svc.SetPolicyChecker(&stubPolicyChecker{
		policy: &types.OrgPolicyValues{AllowedImageConfigs: &allowed},
	})
	f.svc.SetImageFactoryStore(&fakeImageFactoryStore{
		results: map[imagefactory.ConfigScope]struct {
			cfg      imagefactory.Config
			imageRef string
			err      error
		}{
			imagefactory.ScopePlatform: {
				cfg:      imagefactory.Config{ID: "cfg-1", Hash: "s-platform", Scope: imagefactory.ScopePlatform},
				imageRef: "ghcr.io/test/ws:s-platform",
				err:      nil,
			},
		},
	})

	_, err := f.svc.resolveImageFactoryConfig(context.Background(), "s-platform", "user-1", &orgID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in your organization's allowed image list")
}

// TestImageConfigRestriction_FailsOpenOnPolicyError verifies that when
// GetEffectivePolicy returns an error, the launch proceeds (fails open).
func TestImageConfigRestriction_FailsOpenOnPolicyError(t *testing.T) {
	f := newFixture(t)
	orgID := "org-1"

	f.svc.SetPolicyChecker(&stubPolicyChecker{
		err: assert.AnError,
	})
	f.svc.SetImageFactoryStore(&fakeImageFactoryStore{
		results: map[imagefactory.ConfigScope]struct {
			cfg      imagefactory.Config
			imageRef string
			err      error
		}{
			imagefactory.ScopeOrg: {
				cfg:      imagefactory.Config{ID: "cfg-1", Hash: "s-any", Scope: imagefactory.ScopeOrg},
				imageRef: "ghcr.io/test/ws:s-any",
				err:      nil,
			},
		},
	})

	ref, err := f.svc.resolveImageFactoryConfig(context.Background(), "s-any", "user-1", &orgID)
	require.NoError(t, err, "should fail open when policy read errors")
	assert.Equal(t, "ghcr.io/test/ws:s-any", ref)
}
func TestImageConfigRestriction_NoPolicyChecker(t *testing.T) {
	f := newFixture(t)
	orgID := "org-1"
	// No SetPolicyChecker — policyChecker is nil

	f.svc.SetImageFactoryStore(&fakeImageFactoryStore{
		results: map[imagefactory.ConfigScope]struct {
			cfg      imagefactory.Config
			imageRef string
			err      error
		}{
			imagefactory.ScopeOrg: {
				cfg:      imagefactory.Config{ID: "cfg-1", Hash: "s-any", Scope: imagefactory.ScopeOrg},
				imageRef: "ghcr.io/test/ws:s-any",
				err:      nil,
			},
		},
	})

	ref, err := f.svc.resolveImageFactoryConfig(context.Background(), "s-any", "user-1", &orgID)
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/test/ws:s-any", ref)
}
