// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// wrapup_config_integration_test.go — PG pins for the wrapUpReviewer
// config slot (epic wrap-up review, low-level design §6 / D8): set-time
// role-ref validation refusing the WHOLE write on BOTH tiers, tier
// inheritance through the real storage merge, the source-annotated
// config view at both tiers, and the physical floor (the grown CHECK
// constraints admit the key on both tables — proven by the writes
// themselves landing).

import (
	"context"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// TestWrapUpConfig_SetValidatesRoleRefsBothTiers — an unresolved
// wrapUpReviewer reference refuses the ENTIRE write on the user tier
// AND the project tier (D8: identically to stagePersonas — the
// stored map survives a refused write untouched); a resolvable
// reference (slug or id) writes and the stored row is the canonical
// bare string; a role-bearing set with no directory installed refuses.
func (s *BoardSuite) TestWrapUpConfig_SetValidatesRoleRefsBothTiers() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)

	// --- USER TIER: the refusal leaves the stored map untouched.
	_, err := s.svc.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{
		ReviewColumn: strVal(board.ReviewColumnManual),
	})
	require.NoError(s.T(), err)
	_, err = s.svc.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{
		WrapUpReviewer: strVal("ghost"),
	})
	require.Error(s.T(), err, "an unresolved wrapUpReviewer ref refuses the user-tier write")
	assert.Contains(s.T(), err.Error(), "wrapUpReviewer")
	assert.Contains(s.T(), err.Error(), "ghost")
	cfg, err := s.svc.GetUserBoardConfig(ctx, s.owner())
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.ReviewColumnManual, cfg.ReviewColumn, "the refused write stored nothing — the prior map survives")
	assert.Equal(s.T(), board.DefaultWrapUpReviewerRoleSlug, cfg.WrapUpReviewer, "the refused write stored nothing — the floor stands")

	// --- PROJECT TIER: the same refusal shape.
	_, err = s.svc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{
		AutoAssignReviewer: boolVal(false),
	})
	require.NoError(s.T(), err)
	_, err = s.svc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{
		WrapUpReviewer: strVal("ghost"),
	})
	require.Error(s.T(), err, "an unresolved wrapUpReviewer ref refuses the project-tier write")
	assert.Contains(s.T(), err.Error(), "wrapUpReviewer")
	pc, err := s.svc.GetProjectBoardConfig(ctx, s.owner(), p.ID, "")
	require.NoError(s.T(), err)
	require.NotNil(s.T(), pc.Overrides.AutoAssignReviewer)
	assert.False(s.T(), *pc.Overrides.AutoAssignReviewer, "the refused write stored nothing — the prior map survives")
	assert.Nil(s.T(), pc.Overrides.WrapUpReviewer)

	// --- Resolvable refs write, by slug and by id (the standard
	// directory carries reviewer + build).
	_, err = s.svc.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{
		WrapUpReviewer: strVal("build"),
	})
	require.NoError(s.T(), err)
	cfg, err = s.svc.GetUserBoardConfig(ctx, s.owner())
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "build", cfg.WrapUpReviewer)

	_, err = s.svc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{
		WrapUpReviewer: strVal("11111111-1111-1111-1111-111111111111"),
	})
	require.NoError(s.T(), err, "an id reference resolves on the project tier too")
	pc, err = s.svc.GetProjectBoardConfig(ctx, s.owner(), p.ID, "")
	require.NoError(s.T(), err)
	require.NotNil(s.T(), pc.Overrides.WrapUpReviewer)
	assert.Equal(s.T(), "11111111-1111-1111-1111-111111111111", *pc.Overrides.WrapUpReviewer)

	// The stored row is the canonical bare string (the physical floor —
	// the grown CHECK constraint admits the key on BOTH tables, proven
	// by these writes landing at all).
	var stored string
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT value::text FROM board_config_defaults WHERE user_id=$1 AND key='wrapUpReviewer'`, testUser).Scan(&stored))
	assert.Equal(s.T(), `"build"`, stored)
	require.NoError(s.T(), s.pool.QueryRow(ctx,
		`SELECT value::text FROM board_project_config_overrides WHERE project_id=$1 AND key='wrapUpReviewer'`, p.ID).Scan(&stored))
	assert.Equal(s.T(), `"11111111-1111-1111-1111-111111111111"`, stored)

	// --- Role-blind deployment: a wrapUpReviewer-bearing set refuses
	// on both tiers (never store a reference this deployment can never
	// honor).
	blind := board.NewService(s.pool, s.cfg)
	_, err = blind.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{WrapUpReviewer: strVal("reviewer")})
	require.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "no agent-role directory")
	_, err = blind.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{WrapUpReviewer: strVal("reviewer")})
	require.Error(s.T(), err)
	assert.Contains(s.T(), err.Error(), "no agent-role directory")
}

// TestWrapUpConfig_TierInheritance — the slot inherits user→project
// exactly like reviewColumn: nothing stored renders the seeded floor;
// a user default flows into every project without a project write and
// tracks later user changes; a project override wins at its tier; key
// ABSENCE (the REPLACE discipline) is the un-set — the effective floor
// returns when the key stops being sent.
func (s *BoardSuite) TestWrapUpConfig_TierInheritance() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)

	// Nothing stored: the seeded floor at both tiers.
	cfg, err := s.svc.GetUserBoardConfig(ctx, s.owner())
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.DefaultWrapUpReviewerRoleSlug, cfg.WrapUpReviewer)
	pc, err := s.svc.GetProjectBoardConfig(ctx, s.owner(), p.ID, "")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.DefaultWrapUpReviewerRoleSlug, pc.Effective.WrapUpReviewer)

	// A user default flows into the project with no project write.
	_, err = s.svc.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{
		WrapUpReviewer: strVal("build"),
	})
	require.NoError(s.T(), err)
	pc, err = s.svc.GetProjectBoardConfig(ctx, s.owner(), p.ID, "")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "build", pc.Effective.WrapUpReviewer, "the user default inherits into the project")

	// A project override wins at its tier.
	_, err = s.svc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{
		WrapUpReviewer: strVal("reviewer"),
	})
	require.NoError(s.T(), err)
	pc, err = s.svc.GetProjectBoardConfig(ctx, s.owner(), p.ID, "")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "reviewer", pc.Effective.WrapUpReviewer, "the project override wins")

	// Dropping the key from the project's REPLACE write re-inherits
	// the user default.
	_, err = s.svc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{})
	require.NoError(s.T(), err)
	pc, err = s.svc.GetProjectBoardConfig(ctx, s.owner(), p.ID, "")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "build", pc.Effective.WrapUpReviewer, "key absence re-inherits the user default")

	// Dropping the user key too returns to the seeded floor.
	_, err = s.svc.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{})
	require.NoError(s.T(), err)
	pc, err = s.svc.GetProjectBoardConfig(ctx, s.owner(), p.ID, "")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.DefaultWrapUpReviewerRoleSlug, pc.Effective.WrapUpReviewer, "full inheritance returns the code floor")
}

// TestWrapUpConfig_ViewSourcesBothTiers — board_get_config renders the
// slot with ConfigSource* annotations at BOTH tiers: code_default
// (the seeded floor), user_default, project_override — the sibling
// keys' precedence.
func (s *BoardSuite) TestWrapUpConfig_ViewSourcesBothTiers() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)

	// Nothing stored: code_default at both tiers.
	v, err := s.svc.GetBoardConfigView(ctx, s.owner(), "", "")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "user", v.Scope)
	assert.Equal(s.T(), board.ConfigSourceCodeDefault, v.Sources[board.ConfigKeyWrapUpReviewer])
	assert.Equal(s.T(), board.DefaultWrapUpReviewerRoleSlug, v.Effective.WrapUpReviewer)

	v, err = s.svc.GetBoardConfigView(ctx, s.owner(), p.ID, "")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), "project", v.Scope)
	assert.Equal(s.T(), board.ConfigSourceCodeDefault, v.Sources[board.ConfigKeyWrapUpReviewer])

	// A user default: user_default on BOTH tiers (the project tier
	// inherits it).
	_, err = s.svc.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{
		WrapUpReviewer: strVal("build"),
	})
	require.NoError(s.T(), err)
	v, err = s.svc.GetBoardConfigView(ctx, s.owner(), "", "")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.ConfigSourceUserDefault, v.Sources[board.ConfigKeyWrapUpReviewer])
	require.NotNil(s.T(), v.Overrides.WrapUpReviewer)
	assert.Equal(s.T(), "build", *v.Overrides.WrapUpReviewer)

	v, err = s.svc.GetBoardConfigView(ctx, s.owner(), p.ID, "")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.ConfigSourceUserDefault, v.Sources[board.ConfigKeyWrapUpReviewer])
	assert.Nil(s.T(), v.Overrides.WrapUpReviewer, "the project tier stores nothing until its own write")

	// A project override: project_override at the project tier only.
	_, err = s.svc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{
		WrapUpReviewer: strVal("reviewer"),
	})
	require.NoError(s.T(), err)
	v, err = s.svc.GetBoardConfigView(ctx, s.owner(), p.ID, "")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.ConfigSourceProjectOverride, v.Sources[board.ConfigKeyWrapUpReviewer])
	assert.Equal(s.T(), "reviewer", v.Effective.WrapUpReviewer)

	v, err = s.svc.GetBoardConfigView(ctx, s.owner(), "", "")
	require.NoError(s.T(), err)
	assert.Equal(s.T(), board.ConfigSourceUserDefault, v.Sources[board.ConfigKeyWrapUpReviewer],
		"the user tier is untouched by the project override")
}
