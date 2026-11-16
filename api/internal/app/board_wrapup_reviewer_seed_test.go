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

// board_wrapup_reviewer_seed_test.go — the built-in wrap-up reviewer
// role's seeding (epic wrap-up review, low-level design §6): the
// platform seeds a `wrapup-reviewer` platform-scope agent role beside
// the built-in reviewer, carrying the JUDGMENT-half charter
// (board.DefaultWrapUpReviewerRoleSystemPrompt) as its system prompt —
// the default the wrapUpReviewer config key resolves to. Pins: exact
// slug + name, prompt verbatim, idempotency across restarts, the
// org-namesake non-suppression rule, and error propagation.

// TestSeedBuiltinWrapUpReviewerRole_ExactShape — an empty platform
// vocabulary seeds exactly one `wrapup-reviewer` role at platform
// scope, with the judgment charter verbatim as its system prompt.
func TestSeedBuiltinWrapUpReviewerRole_ExactShape(t *testing.T) {
	store := &fakeBuiltinRoleSeedStore{}
	require.NoError(t, seedBuiltinWrapUpReviewerRole(context.Background(), store))

	require.Len(t, store.created, 1, "exactly one role seeds — no more, no fewer")
	r := store.created[0]
	assert.Equal(t, "platform", r.Scope)
	assert.Equal(t, board.DefaultWrapUpReviewerRoleSlug, r.Slug)
	assert.Equal(t, "wrapup-reviewer", r.Slug, "the slug is pinned verbatim (the config key's default reference)")
	assert.Equal(t, "Wrap-up reviewer", r.Name, "the name is pinned verbatim (the builtin-pairs discipline)")
	assert.False(t, r.IsDefault)
	assert.NotEmpty(t, r.Description, "the role carries a picker description")

	// The seeded config carries the judgment charter verbatim — parsed
	// from the jsonb the seeding actually wrote.
	require.Len(t, store.cfgs, 1)
	parsed, err := types.UnmarshalRoleConfig(store.cfgs[0])
	require.NoError(t, err)
	require.NotNil(t, parsed.System)
	assert.Equal(t, board.DefaultWrapUpReviewerRoleSystemPrompt, *parsed.System,
		"the seeded system prompt IS the judgment-half charter verbatim")
}

// TestSeedBuiltinWrapUpReviewerRole_IdempotentAcrossRestarts — the
// restart/re-seed shape creates nothing once the vocabulary carries
// the slug; an owner-curated platform namesake is left untouched.
func TestSeedBuiltinWrapUpReviewerRole_IdempotentAcrossRestarts(t *testing.T) {
	store := &fakeBuiltinRoleSeedStore{}
	require.NoError(t, seedBuiltinWrapUpReviewerRole(context.Background(), store))
	require.Len(t, store.created, 1)

	require.NoError(t, seedBuiltinWrapUpReviewerRole(context.Background(), store))
	require.Len(t, store.created, 1, "the second boot (re-seed) creates nothing")

	require.NoError(t, seedBuiltinWrapUpReviewerRole(context.Background(), store))
	require.Len(t, store.created, 1, "a third pass is the same no-op")
}

// TestSeedBuiltinWrapUpReviewerRole_OrgNamesakeDoesNotSuppress — the
// list is platform scope only: an org-scoped namesake never suppresses
// the platform seed (org roles are invisible to boards owned by users
// outside that org — the reviewer seed's same discipline), and an
// owner-edited platform namesake IS the no-op marker.
func TestSeedBuiltinWrapUpReviewerRole_OrgNamesakeDoesNotSuppress(t *testing.T) {
	existing := []*types.AgentRole{
		{ID: "77777777-7777-7777-7777-777777777771", Scope: "org",
			Name: "curated org namesake", Slug: board.DefaultWrapUpReviewerRoleSlug},
	}
	store := &fakeBuiltinRoleSeedStore{roles: existing}
	require.NoError(t, seedBuiltinWrapUpReviewerRole(context.Background(), store))
	require.Len(t, store.created, 1, "the org namesake does not suppress the platform seed")
	assert.Equal(t, "platform", store.created[0].Scope)

	platform := &fakeBuiltinRoleSeedStore{roles: []*types.AgentRole{
		{ID: "77777777-7777-7777-7777-777777777772", Scope: "platform",
			Name: "curated by the owner", Slug: board.DefaultWrapUpReviewerRoleSlug},
	}}
	require.NoError(t, seedBuiltinWrapUpReviewerRole(context.Background(), platform))
	assert.Empty(t, platform.created, "an existing platform-scope namesake is the no-op marker — never overwritten")
}

// TestSeedBuiltinWrapUpReviewerRole_StoreErrorsPropagate — a store
// failure is never swallowed into a silent unseeded default.
func TestSeedBuiltinWrapUpReviewerRole_StoreErrorsPropagate(t *testing.T) {
	store := &fakeBuiltinRoleSeedStore{listErr: fmt.Errorf("role store unreachable")}
	err := seedBuiltinWrapUpReviewerRole(context.Background(), store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "role store unreachable")
	assert.Empty(t, store.created, "nothing seeds when the vocabulary cannot be read")
}

// TestSeedBuiltinWrapUpReviewerRole_VisibleToPersonaSelection — the
// seeded role rides the platform vocabulary into the board role
// directory: the listing wrapUpReviewer set-time validation checks
// against, so the DEFAULT reference resolves out of the box.
func TestSeedBuiltinWrapUpReviewerRole_VisibleToPersonaSelection(t *testing.T) {
	store := &fakeBuiltinRoleSeedStore{}
	require.NoError(t, seedBuiltinReviewerRole(context.Background(), store))
	require.NoError(t, seedBuiltinWrapUpReviewerRole(context.Background(), store))

	dir := newBoardRoleDirectory(store)
	refs, err := dir.AgentRolesForUser(context.Background(), "user-1")
	require.NoError(t, err)

	found := false
	for _, r := range refs {
		if r.Slug == board.DefaultWrapUpReviewerRoleSlug {
			found = true
			assert.Equal(t, "platform", r.Scope)
		}
	}
	assert.True(t, found, "wrapup-reviewer is selectable in the role vocabulary (the default resolves out of the box)")
}
