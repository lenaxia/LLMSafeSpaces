// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// board_builtin_roles_seed_test.go — the shipped stage-persona
// vocabulary's seeding (2026-11-13 owner ask): board.BuiltinBoardRoles
// (implementer/reviewer pairs for coding, data analysis, writing)
// become platform agent roles at startup, selectable from
// stagePersonas with zero custom-role authoring. Pins the owner's
// exact shape: (a) each role seeds exactly once and the seed is
// idempotent across restarts/re-seeds, (b) the names carry the
// stage-intent convention (exact names pinned), (c) per-role
// idempotency (a partially seeded vocabulary seeds only its gaps).

// fakeBuiltinRoleSeedStore is the seeding double that PERSISTS creates
// (the next ListAgentRoles sees them — the restart/re-seed shape).
type fakeBuiltinRoleSeedStore struct {
	roles   []*types.AgentRole // the standing vocabulary
	created []*types.AgentRole // every CreateAgentRole input, in order
	cfgs    [][]byte           // every CreateAgentRole config jsonb, in order
	listErr error              // when set, ListAgentRoles fails (the outage shape)
}

func (f *fakeBuiltinRoleSeedStore) ListAgentRoles(_ context.Context, scope, _ string) ([]*types.AgentRole, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []*types.AgentRole
	for _, r := range f.roles {
		if r != nil && r.Scope == scope {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeBuiltinRoleSeedStore) CreateAgentRole(_ context.Context, role *types.AgentRole, configJSON []byte) (*types.AgentRole, error) {
	f.created = append(f.created, role)
	f.cfgs = append(f.cfgs, configJSON)
	out := *role
	out.ID = "44444444-4444-4444-4444-444444444441"
	f.roles = append(f.roles, &out) // persist — the next list sees it
	return &out, nil
}

func (f *fakeBuiltinRoleSeedStore) GetUserOrgID(context.Context, string) (string, error) {
	return "", nil
}

// TestSeedBuiltinBoardRoles_SixExactNames — an empty platform
// vocabulary seeds exactly the six shipped roles: platform scope, the
// EXACT stage-intent names and slugs (the convention is the config
// UI's self-describing surface — pinned verbatim), and each role's
// config carries its charter as the system prompt.
func TestSeedBuiltinBoardRoles_SixExactNames(t *testing.T) {
	store := &fakeBuiltinRoleSeedStore{}
	require.NoError(t, seedBuiltinBoardRoles(context.Background(), store))

	want := []struct{ slug, name string }{
		{"implementer-coding", "implementer: coding"},
		{"reviewer-coding", "reviewer: coding"},
		{"implementer-data-analysis", "implementer: data analysis"},
		{"reviewer-data-analysis", "reviewer: data analysis"},
		{"implementer-writing", "implementer: writing"},
		{"reviewer-writing", "reviewer: writing"},
	}
	require.Len(t, store.created, len(want), "exactly the six built-ins seed — no more, no fewer")
	defs := board.BuiltinBoardRoles()
	for i, w := range want {
		r := store.created[i]
		assert.Equal(t, "platform", r.Scope, "%s seeds at platform scope", w.slug)
		assert.Equal(t, w.slug, r.Slug)
		assert.Equal(t, w.name, r.Name, "the stage-intent name convention is pinned verbatim")
		assert.False(t, r.IsDefault)
		assert.NotEmpty(t, r.Description, "%s carries a picker description", w.slug)
	}
	for i, def := range defs {
		// The definitions themselves are the six pinned shapes.
		assert.Equal(t, want[i].slug, def.Slug)
		assert.Equal(t, want[i].name, def.Name)
		assert.NotEmpty(t, def.SystemPrompt)

		// The seeded config carries the charter as the system prompt —
		// parsed from the jsonb the seeding actually wrote.
		require.NotNil(t, store.cfgs[i])
		parsed, err := types.UnmarshalRoleConfig(store.cfgs[i])
		require.NoError(t, err)
		require.NotNil(t, parsed.System)
		assert.Equal(t, def.SystemPrompt, *parsed.System,
			"%s's seeded system prompt IS its charter verbatim", def.Slug)
	}
}

// TestSeedBuiltinBoardRoles_IdempotentAcrossRestarts — a second run
// against the now-persisted vocabulary (the restart/re-seed shape)
// creates NOTHING: each of the six roles seeds exactly once.
func TestSeedBuiltinBoardRoles_IdempotentAcrossRestarts(t *testing.T) {
	store := &fakeBuiltinRoleSeedStore{}
	require.NoError(t, seedBuiltinBoardRoles(context.Background(), store))
	require.Len(t, store.created, 6, "the first boot seeds all six")

	require.NoError(t, seedBuiltinBoardRoles(context.Background(), store))
	assert.Empty(t, store.created[6:], "the second boot (re-seed) creates nothing — every role seeded exactly once")
	require.Len(t, store.created, 6)

	// A THIRD pass is the same no-op (the pinned floor: restarts and
	// re-seeds never duplicate, never overwrite).
	require.NoError(t, seedBuiltinBoardRoles(context.Background(), store))
	assert.Len(t, store.created, 6)
}

// TestSeedBuiltinBoardRoles_PartialVocabularySeedsOnlyGaps — per-role
// idempotency: a vocabulary that already carries SOME of the built-ins
// (any platform-scope id — a seeded original or an owner-curated
// successor under the same slug) seeds only the missing ones, and an
// owner-edited namesake is left untouched.
func TestSeedBuiltinBoardRoles_PartialVocabularySeedsOnlyGaps(t *testing.T) {
	existing := []*types.AgentRole{
		{ID: "55555555-5555-5555-5555-555555555551", Scope: "platform",
			Name: "curated by the owner", Slug: "implementer-coding"},
		{ID: "55555555-5555-5555-5555-555555555552", Scope: "platform",
			Name: "reviewer: writing", Slug: "reviewer-writing"},
		// An ORG-scoped namesake does NOT suppress the platform seed
		// (org roles are invisible to boards outside that org — the
		// reviewer seed's same discipline).
		{ID: "55555555-5555-5555-5555-555555555553", Scope: "org",
			Name: "reviewer: coding", Slug: "reviewer-coding"},
	}
	store := &fakeBuiltinRoleSeedStore{roles: existing}
	require.NoError(t, seedBuiltinBoardRoles(context.Background(), store))

	require.Len(t, store.created, 4, "only the four missing slugs seed")
	got := map[string]bool{}
	for _, r := range store.created {
		got[r.Slug] = true
	}
	assert.False(t, got["implementer-coding"], "the owner-curated platform namesake is left untouched")
	assert.False(t, got["reviewer-writing"])
	assert.True(t, got["reviewer-coding"], "the org-scoped namesake does not suppress the platform seed")
	assert.True(t, got["implementer-data-analysis"])
	assert.True(t, got["implementer-writing"])
	assert.True(t, got["reviewer-data-analysis"])
}

// TestSeedBuiltinBoardRoles_StoreErrorsPropagate — a store failure is
// NEVER swallowed into a silent partial vocabulary: a list outage and
// a mid-vocabulary create failure both return the error to the caller
// (app.go warns; the next boot heals the partial state — the heal
// shape itself pinned by PartialVocabularySeedsOnlyGaps above).
func TestSeedBuiltinBoardRoles_StoreErrorsPropagate(t *testing.T) {
	// The list outage: the error surfaces verbatim, nothing seeds.
	store := &fakeBuiltinRoleSeedStore{listErr: fmt.Errorf("role store unreachable")}
	err := seedBuiltinBoardRoles(context.Background(), store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "role store unreachable")
	assert.Empty(t, store.created, "nothing seeds when the vocabulary cannot be read")

	// The mid-vocabulary create failure: the error surfaces (the
	// already-created prefix stays persisted — healable, not duplicated).
	failing := &failingCreateRoleSeedStore{failAfter: 2}
	err = seedBuiltinBoardRoles(context.Background(), failing)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create blew up")
	assert.Len(t, failing.created, 2, "the first two creates persisted before the third failed")
}

// failingCreateRoleSeedStore lets CreateAgentRole succeed failAfter
// times, then fail (the mid-vocabulary outage shape).
type failingCreateRoleSeedStore struct {
	fakeBuiltinRoleSeedStore
	failAfter int
	fails     int
	created   []*types.AgentRole
}

func (f *failingCreateRoleSeedStore) CreateAgentRole(_ context.Context, role *types.AgentRole, _ []byte) (*types.AgentRole, error) {
	if f.fails >= f.failAfter {
		return nil, fmt.Errorf("create blew up")
	}
	f.fails++
	f.created = append(f.created, role)
	out := *role
	out.ID = "66666666-6666-6666-6666-666666666661"
	f.roles = append(f.roles, &out)
	return &out, nil
}

// TestSeedBuiltinBoardRoles_VisibleToPersonaSelection — the seeded
// roles ride the platform vocabulary into the board role directory:
// the exact listing stagePersonas selection validates against (and
// the persona picker renders), each marked by its platform scope
// (the built-in convention on the selection surfaces).
func TestSeedBuiltinBoardRoles_VisibleToPersonaSelection(t *testing.T) {
	store := &fakeBuiltinRoleSeedStore{}
	require.NoError(t, seedBuiltinReviewerRole(context.Background(), store))
	require.NoError(t, seedBuiltinBoardRoles(context.Background(), store))

	dir := newBoardRoleDirectory(store)
	refs, err := dir.AgentRolesForUser(context.Background(), "user-1")
	require.NoError(t, err)

	names := map[string]string{} // slug → name
	for _, r := range refs {
		names[r.Slug] = r.Name
		assert.Equal(t, "platform", r.Scope, "the built-ins render as platform scope (the built-in marker)")
	}
	for _, def := range board.BuiltinBoardRoles() {
		require.Contains(t, names, def.Slug, "%s is selectable in the stagePersonas vocabulary", def.Slug)
		assert.Equal(t, def.Name, names[def.Slug])
	}
	assert.Contains(t, names, board.DefaultReviewerRoleSlug, "the default reviewer stays in the vocabulary")
}
