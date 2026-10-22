// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// config_integration_test.go — red-first storage pins for the #1832
// item 6 Task Board Config: the Postgres round-trip behind the service
// seam (global defaults replace-write, per-project override replace-
// write, read-time merge with unset=inherit, cascade on project
// delete, and write-side rejection of unknown/malformed keys). RED
// until pkg/board/config.go lands the store + service methods.

import (
	"context"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// TestBoardConfig_GlobalRoundTrip: the global defaults map persists
// across reads; a replace-write with a subset of keys reverts the
// absent ones to the code defaults; the empty map clears everything.
func (s *BoardSuite) TestBoardConfig_GlobalRoundTrip() {
	ctx := context.Background()

	// Fresh install: every key renders its code default.
	cfg, err := s.svc.GetGlobalBoardConfig(ctx)
	s.Require().NoError(err)
	s.Require().NotNil(cfg)
	s.Equal("manual", cfg.ReviewColumn)
	s.False(cfg.AutoAssignReviewer)
	s.Equal(2, cfg.DefaultWorkerSlots)

	// Partial write: one key set, two revert to (stay at) defaults.
	set := board.BoardConfigOverride{ReviewColumn: board.ReviewColumnAuto}
	cfg, err = s.svc.SetGlobalBoardConfig(ctx, &set)
	s.Require().NoError(err)
	s.Equal("auto", cfg.ReviewColumn)
	s.False(cfg.AutoAssignReviewer)
	s.Equal(2, cfg.DefaultWorkerSlots)

	// Persisted across a fresh read.
	cfg, err = s.svc.GetGlobalBoardConfig(ctx)
	s.Require().NoError(err)
	s.Equal("auto", cfg.ReviewColumn)

	// Replace-write: absent keys revert (reviewColumn back to manual).
	set = board.BoardConfigOverride{DefaultWorkerSlots: 9}
	cfg, err = s.svc.SetGlobalBoardConfig(ctx, &set)
	s.Require().NoError(err)
	s.Equal("manual", cfg.ReviewColumn, "absent key on replace reverts to the code default")
	s.Equal(9, cfg.DefaultWorkerSlots)

	// Empty map clears every override.
	cfg, err = s.svc.SetGlobalBoardConfig(ctx, &board.BoardConfigOverride{})
	s.Require().NoError(err)
	s.Equal(board.DefaultBoardConfig(), *cfg)
}

// TestBoardConfig_ProjectOverrideMerge: the per-project override map
// merges at READ time — a set key wins over the global default, an
// unset key inherits (and tracks later global changes), and clearing
// the map reverts to full inheritance.
func (s *BoardSuite) TestBoardConfig_ProjectOverrideMerge() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)

	// Establish a non-default global so inheritance is observable.
	_, err := s.svc.SetGlobalBoardConfig(ctx, &board.BoardConfigOverride{
		ReviewColumn:       board.ReviewColumnOff,
		AutoAssignReviewer: boolVal(true),
	})
	s.Require().NoError(err)

	// No overrides yet: effective == global.
	pc, err := s.svc.GetProjectBoardConfig(ctx, p.ID, "")
	s.Require().NoError(err)
	s.Equal("off", pc.Effective.ReviewColumn)
	s.True(pc.Effective.AutoAssignReviewer)
	s.Equal(2, pc.Effective.DefaultWorkerSlots)
	s.Nil(pc.Overrides.ReviewColumn, "no overrides stored yet")

	// One key set: it wins; the others inherit.
	ov := board.BoardConfigOverride{ReviewColumn: strVal(board.ReviewColumnAuto)}
	pc, err = s.svc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &ov)
	s.Require().NoError(err)
	s.Equal("auto", pc.Effective.ReviewColumn, "override wins")
	s.True(pc.Effective.AutoAssignReviewer, "unset key inherits global")
	s.Equal(2, pc.Effective.DefaultWorkerSlots)

	pc, err = s.svc.GetProjectBoardConfig(ctx, p.ID, "")
	s.Require().NoError(err)
	s.Require().NotNil(pc.Overrides.ReviewColumn)
	s.Equal("auto", *pc.Overrides.ReviewColumn)
	s.Nil(pc.Overrides.AutoAssignReviewer)

	// Inheritance tracks a LATER global change (read-time merge, not a
	// materialized copy): flipping the global slots default re-flips
	// the project's effective value with no project write.
	_, err = s.svc.SetGlobalBoardConfig(ctx, &board.BoardConfigOverride{
		ReviewColumn:       board.ReviewColumnOff,
		AutoAssignReviewer: boolVal(true),
		DefaultWorkerSlots: intVal(6),
	})
	s.Require().NoError(err)
	pc, err = s.svc.GetProjectBoardConfig(ctx, p.ID, "")
	s.Require().NoError(err)
	s.Equal(6, pc.Effective.DefaultWorkerSlots, "unset key tracks later global writes")
	s.Equal("auto", pc.Effective.ReviewColumn, "the set override is untouched")

	// Clearing the override map reverts to full inheritance.
	pc, err = s.svc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{})
	s.Require().NoError(err)
	s.Equal(6, pc.Effective.DefaultWorkerSlots, "cleared override re-inherits the global default")
	s.Equal("off", pc.Effective.ReviewColumn, "cleared override re-inherits the global default")
}

// TestBoardConfig_ValidationAndAuthz: unknown keys and malformed values
// reject at the service boundary (GuardInput); project writes require
// the owning user (R7) and the manager-surface discipline; a foreign
// project's overrides never leak (scoped 404).
func (s *BoardSuite) TestBoardConfig_ValidationAndAuthz() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)

	// Service-side belt: malformed values reject even when a caller
	// bypassed the adapter's decode.
	_, err := s.svc.SetGlobalBoardConfig(ctx, &board.BoardConfigOverride{DefaultWorkerSlots: intVal(0)})
	s.Require().Error(err)
	guard, ok := err.(*board.GuardError)
	s.Require().True(ok, "got %T", err)
	s.Equal(board.GuardInput, guard.Code)

	_, err = s.svc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "",
		&board.BoardConfigOverride{ReviewColumn: strVal("maybe")})
	s.Require().Error(err)
	guard, ok = err.(*board.GuardError)
	s.Require().True(ok, "got %T", err)
	s.Equal(board.GuardInput, guard.Code)

	// Nothing landed: effective values still the defaults.
	pc, err := s.svc.GetProjectBoardConfig(ctx, p.ID, "")
	s.Require().NoError(err)
	s.Equal("manual", pc.Effective.ReviewColumn)

	// R7: a foreign actor's write rejects with authz.
	foreign := board.Actor{Kind: board.ActorUser, UserID: "board-foreign-user", Surface: board.SurfaceREST}
	_, err = s.svc.SetProjectBoardConfig(ctx, foreign, p.ID, "", &board.BoardConfigOverride{})
	s.Require().Error(err)
	guard, ok = err.(*board.GuardError)
	s.Require().True(ok, "got %T", err)
	s.Equal(board.GuardAuthz, guard.Code)

	// Unknown project resolves the 404 class.
	_, err = s.svc.GetProjectBoardConfig(ctx, "prj-zzzzzzzz", "")
	s.Require().Error(err)
	_, ok = err.(*board.NotFoundError)
	s.Require().True(ok, "got %T", err)
}

// TestBoardConfig_ProjectDeleteCascades: deleting a project's overrides
// rides ON DELETE CASCADE (no orphan rows behind a deleted board).
func (s *BoardSuite) TestBoardConfig_ProjectDeleteCascades() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)
	ov := board.BoardConfigOverride{DefaultWorkerSlots: intVal(4)}
	_, err := s.svc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &ov)
	s.Require().NoError(err)

	// The row exists (direct count over the store's pool).
	var n int
	err = s.pool.QueryRow(ctx, `SELECT count(*) FROM board_project_config_overrides WHERE project_id=$1`, p.ID).Scan(&n)
	s.Require().NoError(err)
	s.Equal(1, n)

	// Delete the project (storage-level, the service cascade path):
	// the override rows vanish with it.
	_, err = s.pool.Exec(ctx, `DELETE FROM projects WHERE id=$1`, p.ID)
	s.Require().NoError(err)
	err = s.pool.QueryRow(ctx, `SELECT count(*) FROM board_project_config_overrides WHERE project_id=$1`, p.ID).Scan(&n)
	s.Require().NoError(err)
	s.Equal(0, n, "override rows cascade with the project")
}

// TestBoardConfig_StorageRejectsUnknownKeys asserts the DB-level guard:
// an unknown key cannot land in either table even via raw SQL (the
// CHECK constraint belt beneath the service validation).
func (s *BoardSuite) TestBoardConfig_StorageRejectsUnknownKeys() {
	ctx := context.Background()
	_, err := s.pool.Exec(ctx,
		`INSERT INTO board_config_defaults (key, value) VALUES ('reviewQueue', '"off"')`)
	s.Require().Error(err, "unknown keys must not be storable")

	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)
	_, err = s.pool.Exec(ctx,
		`INSERT INTO board_project_config_overrides (project_id, key, value) VALUES ($1, 'reviewQueue', '"off"')`, p.ID)
	s.Require().Error(err, "unknown keys must not be storable per-project")

	// Sanity: a known key stores fine.
	_, err = s.pool.Exec(ctx,
		`INSERT INTO board_config_defaults (key, value) VALUES ('reviewColumn', '"auto"')`)
	s.Require().NoError(err)
	var v string
	err = s.pool.QueryRow(ctx, `SELECT value#>>'{}' FROM board_config_defaults WHERE key='reviewColumn'`).Scan(&v)
	s.Require().NoError(err)
	assert.Equal(s.T(), "auto", v)
	require.NoError(s.T(), err)
}

// Local pointer helpers (external test package: unexported helpers in
// config_test.go are out of reach).
func boolVal(b bool) *bool    { return &b }
func intVal(i int) *int       { return &i }
func strVal(s string) *string { return &s }
