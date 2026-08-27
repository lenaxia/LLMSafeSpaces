// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package prompt

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// mockPromptStore implements promptStore for testing.
type mockPromptStore struct {
	mock.Mock
}

func (m *mockPromptStore) GetPlatformSetting(ctx context.Context, key types.PlatformSettingKey) (*types.PlatformSetting, error) {
	args := m.Called(ctx, key)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.PlatformSetting), args.Error(1)
}

func (m *mockPromptStore) GetOrgPolicies(ctx context.Context, orgID string) ([]*types.OrgPolicy, error) {
	args := m.Called(ctx, orgID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*types.OrgPolicy), args.Error(1)
}

func (m *mockPromptStore) GetWorkspacePrompt(ctx context.Context, workspaceID string) (*types.WorkspacePrompt, error) {
	args := m.Called(ctx, workspaceID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.WorkspacePrompt), args.Error(1)
}

func (m *mockPromptStore) GetWorkspaceOrgID(ctx context.Context, workspaceID string) (string, error) {
	args := m.Called(ctx, workspaceID)
	return args.String(0), args.Error(1)
}

func (m *mockPromptStore) GetAgentRole(ctx context.Context, roleID string) (*types.AgentRole, error) {
	args := m.Called(ctx, roleID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.AgentRole), args.Error(1)
}

type mockCache struct {
	mock.Mock
	deletedKeys []string
}

func (m *mockCache) GetObject(ctx context.Context, key string, value interface{}) error {
	return m.Called(ctx, key, value).Error(0)
}

func (m *mockCache) SetObject(ctx context.Context, key string, value interface{}, expiration time.Duration) error {
	return m.Called(ctx, key, value, expiration).Error(0)
}

func (m *mockCache) Delete(ctx context.Context, key string) error {
	m.deletedKeys = append(m.deletedKeys, key)
	return m.Called(ctx, key).Error(0)
}

func (m *mockCache) DeleteByPrefix(ctx context.Context, prefix string) error {
	return m.Called(ctx, prefix).Error(0)
}

// --- Tests ---

func TestResolveEffective_StandaloneUser_AllowUserPrompt(t *testing.T) {
	store := new(mockPromptStore)
	store.On("GetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform).Return(&types.PlatformSetting{
		Value: []byte(`"Follow security policy at all times."`),
	}, nil)
	store.On("GetWorkspaceOrgID", mock.Anything, "ws-1").Return("", nil)
	store.On("GetWorkspacePrompt", mock.Anything, "ws-1").Return(&types.WorkspacePrompt{
		Prompt: "Focus on tests today",
	}, nil)

	svc := New(store, nil)
	result, err := svc.ResolveEffective(context.Background(), "ws-1")
	assert.NoError(t, err)
	assert.Contains(t, result.Resolved, "Follow security policy")
	assert.Contains(t, result.Resolved, "Focus on tests today")
	assert.True(t, result.AllowUserPrompt)
}

func TestResolveEffective_OrgLocked_OmitsUserPrompt(t *testing.T) {
	orgID := "org-1"
	locked := false
	store := new(mockPromptStore)
	store.On("GetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform).Return(&types.PlatformSetting{
		Value: []byte(`"Platform rules"`),
	}, nil)
	store.On("GetWorkspaceOrgID", mock.Anything, "ws-1").Return(orgID, nil)
	store.On("GetOrgPolicies", mock.Anything, orgID).Return([]*types.OrgPolicy{
		{Key: types.PolicySysPromptOrg, Value: []byte(`"Org overlay"`)},
		{Key: types.PolicyAllowUserPrompt, Value: mustMarshalBool(&locked)},
	}, nil)
	// GetWorkspacePrompt should NOT be called when locked

	svc := New(store, nil)
	result, err := svc.ResolveEffective(context.Background(), "ws-1")
	assert.NoError(t, err)
	assert.Contains(t, result.Resolved, "Platform rules")
	assert.Contains(t, result.Resolved, "Org overlay")
	assert.NotContains(t, result.Resolved, "user instructions")
	assert.False(t, result.AllowUserPrompt)
	store.AssertNotCalled(t, "GetWorkspacePrompt")
}

func TestResolveEffective_OrgUnlocked_IncludesUserPrompt(t *testing.T) {
	orgID := "org-1"
	unlocked := true
	store := new(mockPromptStore)
	store.On("GetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform).Return(&types.PlatformSetting{
		Value: []byte(`"Platform"`),
	}, nil)
	store.On("GetWorkspaceOrgID", mock.Anything, "ws-1").Return(orgID, nil)
	store.On("GetOrgPolicies", mock.Anything, orgID).Return([]*types.OrgPolicy{
		{Key: types.PolicyAllowUserPrompt, Value: mustMarshalBool(&unlocked)},
	}, nil)
	store.On("GetWorkspacePrompt", mock.Anything, "ws-1").Return(&types.WorkspacePrompt{
		Prompt: "My custom instructions",
	}, nil)

	svc := New(store, nil)
	result, err := svc.ResolveEffective(context.Background(), "ws-1")
	assert.NoError(t, err)
	assert.Contains(t, result.Resolved, "Platform")
	assert.Contains(t, result.Resolved, "My custom instructions")
	assert.True(t, result.AllowUserPrompt)
}

func TestResolveEffective_IncludesRoleSystemPrompt(t *testing.T) {
	orgID := "org-1"
	unlocked := true
	roleID := "role-1"
	roleSys := "You are a code reviewer."
	store := new(mockPromptStore)
	store.On("GetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform).Return(&types.PlatformSetting{
		Value: []byte(`"Platform"`),
	}, nil)
	store.On("GetWorkspaceOrgID", mock.Anything, "ws-1").Return(orgID, nil)
	store.On("GetOrgPolicies", mock.Anything, orgID).Return([]*types.OrgPolicy{
		{Key: types.PolicyAllowUserPrompt, Value: mustMarshalBool(&unlocked)},
	}, nil)
	store.On("GetWorkspacePrompt", mock.Anything, "ws-1").Return(&types.WorkspacePrompt{
		Prompt:      "Custom",
		AgentRoleID: &roleID,
	}, nil)
	store.On("GetAgentRole", mock.Anything, roleID).Return(&types.AgentRole{
		Config: types.RoleConfig{System: &roleSys},
	}, nil)

	svc := New(store, nil)
	result, err := svc.ResolveEffective(context.Background(), "ws-1")
	assert.NoError(t, err)
	assert.Contains(t, result.Resolved, "You are a code reviewer.")
	assert.Equal(t, roleSys, result.RolePrompt)
}

func TestInvalidateOrgWorkspacesCache_CallsDeleteByPrefix(t *testing.T) {
	cache := new(mockCache)
	cache.On("DeleteByPrefix", mock.Anything, "ws:prompt:").Return(nil)

	svc := New(new(mockPromptStore), cache)
	svc.InvalidateOrgWorkspacesCache(context.Background(), "org-1")
	cache.AssertCalled(t, "DeleteByPrefix", mock.Anything, "ws:prompt:")
}

func mustMarshalBool(b *bool) []byte {
	if *b {
		return []byte("true")
	}
	return []byte("false")
}

// missCache faithfully reproduces the real cache.Service miss behavior:
// GetObject returns a nil error (redis.Nil is swallowed internally) and does
// NOT touch the caller's value pointer — so it stays nil. Before the fix,
// ResolveEffective treated nil-error as a hit and returned the zero-value
// prompt without ever consulting the store.
type missCache struct{}

func (missCache) GetObject(context.Context, string, interface{}) error                { return nil }
func (missCache) SetObject(context.Context, string, interface{}, time.Duration) error { return nil }
func (missCache) Delete(context.Context, string) error                                { return nil }
func (missCache) DeleteByPrefix(context.Context, string) error                        { return nil }

// hitCache simulates a cache hit: GetObject populates the caller's value
// pointer with the stored object.
type hitCache struct{ hit *types.EffectivePrompt }

func (h hitCache) GetObject(_ context.Context, _ string, value interface{}) error {
	*value.(**types.EffectivePrompt) = h.hit
	return nil
}
func (hitCache) SetObject(context.Context, string, interface{}, time.Duration) error { return nil }
func (hitCache) Delete(context.Context, string) error                                { return nil }
func (hitCache) DeleteByPrefix(context.Context, string) error                        { return nil }

// TestResolveEffective_CacheMiss_ConsultsStore is the regression test for the
// critical cache bug: a miss (nil error, value untouched) MUST fall through to
// the store and return the resolved prompt, not an empty EffectivePrompt.
func TestResolveEffective_CacheMiss_ConsultsStore(t *testing.T) {
	store := new(mockPromptStore)
	store.On("GetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform).Return(&types.PlatformSetting{
		Value: []byte(`"Platform rules"`),
	}, nil)
	store.On("GetWorkspaceOrgID", mock.Anything, "ws-1").Return("", nil)
	store.On("GetWorkspacePrompt", mock.Anything, "ws-1").Return(&types.WorkspacePrompt{
		Prompt: "User instructions",
	}, nil)

	svc := New(store, missCache{})
	result, err := svc.ResolveEffective(context.Background(), "ws-1")
	assert.NoError(t, err)
	assert.Equal(t, "Platform rules", result.PlatformPrompt)
	assert.Contains(t, result.Resolved, "Platform rules")
	assert.Contains(t, result.Resolved, "User instructions")
	store.AssertCalled(t, "GetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform)
}

// TestResolveEffective_CacheHit_SkipsStore verifies a hit returns the cached
// value without touching the store.
func TestResolveEffective_CacheHit_SkipsStore(t *testing.T) {
	store := new(mockPromptStore)
	cached := &types.EffectivePrompt{
		PlatformPrompt: "Cached platform",
		Resolved:       "Cached platform",
	}
	svc := New(store, hitCache{hit: cached})
	result, err := svc.ResolveEffective(context.Background(), "ws-1")
	assert.NoError(t, err)
	assert.Equal(t, cached, result)
	store.AssertNotCalled(t, "GetPlatformSetting")
}

// TestGetPlatformPrompt_CacheMiss_ConsultsStore guards the same bug in the
// platform-prompt path, where an empty platform prompt is a legitimate value
// and must not be confused with a miss.
func TestGetPlatformPrompt_CacheMiss_ConsultsStore(t *testing.T) {
	store := new(mockPromptStore)
	store.On("GetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform).Return(&types.PlatformSetting{
		Value: []byte(`"Platform rules"`),
	}, nil)

	svc := New(store, missCache{})
	prompt, err := svc.getPlatformPrompt(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, "Platform rules", prompt)
	store.AssertCalled(t, "GetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform)
}

// Fresh install: the platform_settings table has no sys_prompt_platform row
// (migration 000002 creates the schema, no seed). The platform tier must
// fall back to the compiled-in default — mirrors the DefaultJWTIssuer
// posture (defaults keep out-of-the-box deploys working).
func TestGetPlatformPrompt_SettingAbsent_ReturnsDefault(t *testing.T) {
	store := new(mockPromptStore)
	store.On("GetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform).Return((*types.PlatformSetting)(nil), nil)

	svc := New(store, missCache{})
	prompt, err := svc.getPlatformPrompt(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, types.DefaultPlatformPrompt, prompt)
}

// A saved row holding an empty string is an admin's explicit "cleared"
// choice — the default must NOT be re-applied over it.
func TestGetPlatformPrompt_SettingEmptyString_RespectsExplicitClear(t *testing.T) {
	store := new(mockPromptStore)
	store.On("GetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform).Return(&types.PlatformSetting{
		Value: []byte(`""`),
	}, nil)

	svc := New(store, missCache{})
	prompt, err := svc.getPlatformPrompt(context.Background())
	assert.NoError(t, err)
	assert.Equal(t, "", prompt)
}

// End-to-end through ResolveEffective on a fresh install: Resolved must
// carry the default platform text (and the default must respect the
// 10k per-tier cap so an admin can always round-trip it through the UI).
func TestResolveEffective_FreshInstall_PlatformTierCarriesDefault(t *testing.T) {
	store := new(mockPromptStore)
	store.On("GetPlatformSetting", mock.Anything, types.SettingSysPromptPlatform).Return((*types.PlatformSetting)(nil), nil)
	store.On("GetWorkspaceOrgID", mock.Anything, "ws-1").Return("org-1", nil)
	store.On("GetOrgPolicies", mock.Anything, "org-1").Return([]*types.OrgPolicy{}, nil)
	store.On("GetWorkspacePrompt", mock.Anything, "ws-1").Return((*types.WorkspacePrompt)(nil), nil)

	svc := New(store, missCache{})
	eff, err := svc.ResolveEffective(context.Background(), "ws-1")
	assert.NoError(t, err)
	assert.Equal(t, types.DefaultPlatformPrompt, eff.PlatformPrompt)
	assert.Equal(t, types.DefaultPlatformPrompt, eff.Resolved)
	assert.LessOrEqual(t, len(types.DefaultPlatformPrompt), types.MaxPromptPerLevel(),
		"default platform prompt must fit the per-tier cap")
}

// TestResolveRoleSystemPrompt_InheritsFromParent: when the leaf role has no
// system prompt but a parent it extends does, the parent's prompt is delivered
// (the extends chain is walked, not just the leaf).
func TestResolveRoleSystemPrompt_InheritsFromParent(t *testing.T) {
	store := new(mockPromptStore)
	parentSys := "Parent system prompt."
	store.On("GetAgentRole", mock.Anything, "leaf").Return(&types.AgentRole{
		ID: "leaf", Extends: roleStrPtr("parent"),
	}, nil)
	store.On("GetAgentRole", mock.Anything, "parent").Return(&types.AgentRole{
		ID: "parent", Config: types.RoleConfig{System: &parentSys},
	}, nil)

	svc := New(store, nil)
	got, ok := svc.resolveRoleSystemPrompt(context.Background(), "leaf")
	assert.True(t, ok)
	assert.Equal(t, parentSys, got)
}

func roleStrPtr(s string) *string { return &s }

// TestResolveRoleSystemPrompt_CycleTerminates: a circular extends chain
// (A → B → A) must not loop forever — the visited map breaks the cycle and
// returns false (no system prompt found).
func TestResolveRoleSystemPrompt_CycleTerminates(t *testing.T) {
	store := new(mockPromptStore)
	store.On("GetAgentRole", mock.Anything, "a").Return(&types.AgentRole{
		ID: "a", Extends: roleStrPtr("b"),
	}, nil)
	store.On("GetAgentRole", mock.Anything, "b").Return(&types.AgentRole{
		ID: "b", Extends: roleStrPtr("a"),
	}, nil)

	svc := New(store, nil)

	done := make(chan struct{})
	var got string
	var ok bool
	go func() {
		got, ok = svc.resolveRoleSystemPrompt(context.Background(), "a")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("resolveRoleSystemPrompt did not terminate on cycle — infinite loop")
	}

	assert.False(t, ok, "no system prompt in a cycle → false")
	assert.Empty(t, got)
}

// TestResolveRoleSystemPrompt_DeepChainRespectsDepthLimit: a chain longer
// than maxChainDepth (10) with no system prompt at any node must terminate
// via the depth guard, not by walking forever.
func TestResolveRoleSystemPrompt_DeepChainRespectsDepthLimit(t *testing.T) {
	store := new(mockPromptStore)
	// Build a 15-deep chain: r0 → r1 → ... → r14, none with a system prompt.
	for i := 0; i < 15; i++ {
		id := "r" + strconv.Itoa(i)
		next := "r" + strconv.Itoa(i+1)
		if i == 14 {
			next = ""
		}
		store.On("GetAgentRole", mock.Anything, id).Return(&types.AgentRole{
			ID: id, Extends: roleStrPtr(next),
		}, nil).Once()
	}

	svc := New(store, nil)

	done := make(chan struct{})
	var ok bool
	go func() {
		_, ok = svc.resolveRoleSystemPrompt(context.Background(), "r0")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("resolveRoleSystemPrompt did not terminate on deep chain")
	}

	assert.False(t, ok, "no system prompt in chain → false")
	// maxChainDepth is 10, so at most 10 GetAgentRole calls should fire.
	store.AssertNumberOfCalls(t, "GetAgentRole", 10)
}

// TestResolveRoleSystemPrompt_LeafWithoutExtends_ReturnsFalse: a role with
// no system prompt and no extends is a terminal leaf — must return false
// without error.
func TestResolveRoleSystemPrompt_LeafWithoutExtends_ReturnsFalse(t *testing.T) {
	store := new(mockPromptStore)
	store.On("GetAgentRole", mock.Anything, "leaf").Return(&types.AgentRole{
		ID: "leaf", Extends: nil,
	}, nil)

	svc := New(store, nil)
	got, ok := svc.resolveRoleSystemPrompt(context.Background(), "leaf")

	assert.False(t, ok)
	assert.Empty(t, got)
}

// TestResolveRoleSystemPrompt_StoreError_ReturnsFalse: a DB error when
// loading a role must cause the walk to abort with false (not panic).
func TestResolveRoleSystemPrompt_StoreError_ReturnsFalse(t *testing.T) {
	store := new(mockPromptStore)
	store.On("GetAgentRole", mock.Anything, "r1").Return((*types.AgentRole)(nil), assert.AnError)

	svc := New(store, nil)
	_, ok := svc.resolveRoleSystemPrompt(context.Background(), "r1")

	assert.False(t, ok)
}
