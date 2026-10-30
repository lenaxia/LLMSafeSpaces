// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// board_role_seed_test.go — the built-in reviewer agent role's seeding
// (owner ruling 2026-10-29, round 2): the default reviewer's
// verdict-channel charter lives in the SEEDED platform role's system
// prompt (board.DefaultReviewerRoleSystemPrompt), so the default
// reviewer resolves to a role deterministically. ROLE SEEDING, never a
// persona migration — no persona data is read or written.

// fakeRoleSeedStore is the boardRoleStore seam's seeding double.
type fakeRoleSeedStore struct {
	roles     []*types.AgentRole
	create    *types.AgentRole // the last CreateAgentRole input, if any
	createCfg []byte           // the last CreateAgentRole config jsonb
	err       error
}

func (f *fakeRoleSeedStore) ListAgentRoles(_ context.Context, scope, _ string) ([]*types.AgentRole, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []*types.AgentRole
	for _, r := range f.roles {
		if r != nil && r.Scope == scope {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeRoleSeedStore) CreateAgentRole(_ context.Context, role *types.AgentRole, configJSON []byte) (*types.AgentRole, error) {
	f.create = role
	f.createCfg = configJSON
	out := *role
	out.ID = "11111111-1111-1111-1111-111111111111"
	return &out, nil
}

func (f *fakeRoleSeedStore) GetUserOrgID(context.Context, string) (string, error) {
	return "", nil
}

// TestSeedBuiltinReviewerRole_SeedsWhenAbsent — an empty platform
// vocabulary seeds the reviewer role: platform scope, the
// DefaultReviewerRoleSlug slug, and the verdict-channel charter
// (DefaultReviewerRoleSystemPrompt) as the config's system prompt.
func TestSeedBuiltinReviewerRole_SeedsWhenAbsent(t *testing.T) {
	store := &fakeRoleSeedStore{}
	require.NoError(t, seedBuiltinReviewerRole(context.Background(), store))

	require.NotNil(t, store.create, "the reviewer role was seeded")
	assert.Equal(t, "platform", store.create.Scope)
	assert.Equal(t, board.DefaultReviewerRoleSlug, store.create.Slug)
	assert.NotEmpty(t, store.create.Name)

	// The seeded config carries the charter as the system prompt —
	// parsed from the jsonb the seeding actually wrote (the same
	// UnmarshalRoleConfig seam every read uses).
	require.NotNil(t, store.createCfg, "the seeded role carries a config jsonb")
	parsed, err := types.UnmarshalRoleConfig(store.createCfg)
	require.NoError(t, err)
	require.NotNil(t, parsed.System)
	assert.Equal(t, board.DefaultReviewerRoleSystemPrompt, *parsed.System,
		"the seeded system prompt IS the default reviewer's verdict-channel charter")
}

// TestSeedBuiltinReviewerRole_Idempotent — a vocabulary that already
// carries a platform reviewer role (any id — the seeded one or an
// owner-curated successor) makes the seed a NO-OP: never a duplicate,
// never an overwrite of an existing definition.
func TestSeedBuiltinReviewerRole_Idempotent(t *testing.T) {
	existing := &types.AgentRole{
		ID: "22222222-2222-2222-2222-222222222222", Scope: "platform",
		Name: "Reviewer", Slug: board.DefaultReviewerRoleSlug,
	}
	store := &fakeRoleSeedStore{roles: []*types.AgentRole{existing}}
	require.NoError(t, seedBuiltinReviewerRole(context.Background(), store))
	assert.Nil(t, store.create, "an existing reviewer role is left untouched")

	// An ORG-scoped reviewer role does NOT count as the platform seed
	// (the platform vocabulary is its own list) — the seed still runs.
	store = &fakeRoleSeedStore{roles: []*types.AgentRole{{
		ID: "33333333-3333-3333-3333-333333333333", Scope: "org",
		Name: "Reviewer", Slug: board.DefaultReviewerRoleSlug,
	}}}
	require.NoError(t, seedBuiltinReviewerRole(context.Background(), store))
	assert.NotNil(t, store.create, "the platform reviewer is seeded regardless of org-scoped namesakes")
}
