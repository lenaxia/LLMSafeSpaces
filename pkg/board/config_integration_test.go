// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package board_test

// config_integration_test.go — red-first storage pins for the #1832
// item 6 Task Board Config: the Postgres round-trip behind the service
// seam (user-scoped board-wide defaults replace-write, per-project
// override replace-write, read-time merge with unset=inherit, cascade
// on project delete, and write-side rejection of unknown/malformed
// keys). RED until pkg/board/config.go lands the store + service
// methods.

import (
	"context"
	"encoding/json"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// TestBoardConfig_UserRoundTrip: the acting user's board-wide defaults
// map persists across reads; a replace-write with a subset of keys
// reverts the absent ones to the code defaults; the empty map clears
// everything.
func (s *BoardSuite) TestBoardConfig_UserRoundTrip() {
	ctx := context.Background()

	// Fresh install: every key renders its code default (owner ruling
	// 2026-10-24: review defaults flipped to on).
	cfg, err := s.svc.GetUserBoardConfig(ctx, s.owner())
	s.Require().NoError(err)
	s.Require().NotNil(cfg)
	s.Equal("auto", cfg.ReviewColumn)
	s.True(cfg.AutoAssignReviewer)
	s.Equal(2, cfg.DefaultWorkerSlots)

	// Partial write: one key set, two revert to (stay at) defaults.
	set := board.BoardConfigOverride{ReviewColumn: strVal(board.ReviewColumnManual)}
	cfg, err = s.svc.SetUserBoardConfig(ctx, s.owner(), &set)
	s.Require().NoError(err)
	s.Equal("manual", cfg.ReviewColumn)
	s.True(cfg.AutoAssignReviewer)
	s.Equal(2, cfg.DefaultWorkerSlots)

	// Persisted across a fresh read.
	cfg, err = s.svc.GetUserBoardConfig(ctx, s.owner())
	s.Require().NoError(err)
	s.Equal("manual", cfg.ReviewColumn)

	// Replace-write: absent keys revert (reviewColumn back to the
	// flipped code default).
	set = board.BoardConfigOverride{DefaultWorkerSlots: intVal(9)}
	cfg, err = s.svc.SetUserBoardConfig(ctx, s.owner(), &set)
	s.Require().NoError(err)
	s.Equal("auto", cfg.ReviewColumn, "absent key on replace reverts to the code default")
	s.Equal(9, cfg.DefaultWorkerSlots)

	// Empty map clears every override.
	cfg, err = s.svc.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{})
	s.Require().NoError(err)
	s.Equal(board.DefaultBoardConfig(), *cfg)
}

// TestBoardConfig_UserIsolation: board-wide defaults are USER-scoped
// (R7 — "board-wide" = the user's boards, not the instance): a foreign
// user's defaults map neither leaks into the owner's effective config
// nor collides in storage.
func (s *BoardSuite) TestBoardConfig_UserIsolation() {
	ctx := context.Background()
	other := board.Actor{Kind: board.ActorUser, UserID: "board-other-user", Surface: board.SurfaceREST}
	// The FK on board_config_defaults.user_id references users(id).
	// Idempotent insert (the harness truncates board tables, not
	// users — a re-run of the suite must not collide).
	_, err := s.pool.Exec(ctx,
		`INSERT INTO users (id, username, email, password_hash, active, role)
		 VALUES ('board-other-user', 'board-other', 'other@example.test', 'x', true, 'user')
		 ON CONFLICT (id) DO NOTHING`)
	s.Require().NoError(err)

	_, err = s.svc.SetUserBoardConfig(ctx, other, &board.BoardConfigOverride{
		ReviewColumn:       strVal(board.ReviewColumnOff),
		DefaultWorkerSlots: intVal(7),
	})
	s.Require().NoError(err)

	// The owner's effective defaults are untouched by the other user's
	// map (no instance-global leak). Wire-shape comparison: the harness
	// seeds the owner's user-level stagePersonas opt-out (the legacy-
	// suite isolation under the #1939 flipped floor), which sets the
	// merge's StagePersonasSet bookkeeping flag — json:"-" — while the
	// rendered config is the pure default.
	cfg, err := s.svc.GetUserBoardConfig(ctx, s.owner())
	s.Require().NoError(err)
	// Expected = the flipped defaults overlaid with the harness's own
	// seeded opt-out (the owner's stored map) — a foreign user's rows
	// never appear in it.
	empty := map[string]board.PersonaConfig{}
	want := board.MergeBoardConfig(board.DefaultBoardConfig(), &board.BoardConfigOverride{StagePersonas: &empty})
	wantJSON, wantErr := json.Marshal(want)
	s.Require().NoError(wantErr)
	gotJSON, gotErr := json.Marshal(cfg)
	s.Require().NoError(gotErr)
	s.JSONEq(string(wantJSON), string(gotJSON), "a foreign user's defaults never leak into the owner's effective config")

	// Each user reads back their own map.
	cfg, err = s.svc.GetUserBoardConfig(ctx, other)
	s.Require().NoError(err)
	s.Equal("off", cfg.ReviewColumn)
	s.Equal(7, cfg.DefaultWorkerSlots)
}

// TestBoardConfig_ProjectOverrideMerge: the per-project override map
// merges at READ time — a set key wins over the user's board-wide
// default, an unset key inherits (and tracks later user-default
// changes), and clearing the map reverts to full inheritance.
func (s *BoardSuite) TestBoardConfig_ProjectOverrideMerge() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)

	// Establish a non-default user map so inheritance is observable.
	_, err := s.svc.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{
		ReviewColumn:       strVal(board.ReviewColumnOff),
		AutoAssignReviewer: boolVal(true),
	})
	s.Require().NoError(err)

	// No overrides yet: effective == user defaults.
	pc, err := s.svc.GetProjectBoardConfig(ctx, s.owner(), p.ID, "")
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
	s.True(pc.Effective.AutoAssignReviewer, "unset key inherits the user default")
	s.Equal(2, pc.Effective.DefaultWorkerSlots)

	pc, err = s.svc.GetProjectBoardConfig(ctx, s.owner(), p.ID, "")
	s.Require().NoError(err)
	s.Require().NotNil(pc.Overrides.ReviewColumn)
	s.Equal("auto", *pc.Overrides.ReviewColumn)
	s.Nil(pc.Overrides.AutoAssignReviewer)

	// Inheritance tracks a LATER user-default change (read-time merge,
	// not a materialized copy): flipping the user's slots default
	// re-flips the project's effective value with no project write.
	_, err = s.svc.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{
		ReviewColumn:       strVal(board.ReviewColumnOff),
		AutoAssignReviewer: boolVal(true),
		DefaultWorkerSlots: intVal(6),
	})
	s.Require().NoError(err)
	pc, err = s.svc.GetProjectBoardConfig(ctx, s.owner(), p.ID, "")
	s.Require().NoError(err)
	s.Equal(6, pc.Effective.DefaultWorkerSlots, "unset key tracks later user-default writes")
	s.Equal("auto", pc.Effective.ReviewColumn, "the set override is untouched")

	// Clearing the override map reverts to full inheritance.
	pc, err = s.svc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{})
	s.Require().NoError(err)
	s.Equal(6, pc.Effective.DefaultWorkerSlots, "cleared override re-inherits the user default")
	s.Equal("off", pc.Effective.ReviewColumn, "cleared override re-inherits the user default")
}

// TestBoardConfig_DefaultWorkerSlotsFeedsCreate: the
// defaultWorkerSlots key is CONSUMED, not decorative — a project
// created with no explicit slots takes the owner's board-wide default
// (code default when their map is empty); an explicit value wins.
func (s *BoardSuite) TestBoardConfig_DefaultWorkerSlotsFeedsCreate() {
	ctx := context.Background()
	ws := s.seedWorkspace()

	// Unset user map + NO explicit slots (CreateProject directly —
	// seedProject always passes an explicit WorkerSlots, which would
	// make this arm tautological): create falls back to the code
	// default.
	p, err := s.svc.CreateProject(ctx, s.owner(), board.ProjectInput{
		Name: "proj-slots-none", TargetWorkspaceID: ws, Tags: []string{},
	}, "")
	s.Require().NoError(err)
	s.Equal(2, p.WorkerSlots, "no user default and no explicit slots → the code default")

	// A user default flips the create fallback for the NEXT project.
	_, err = s.svc.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{
		DefaultWorkerSlots: intVal(5),
	})
	s.Require().NoError(err)
	p2, err := s.svc.CreateProject(ctx, s.owner(), board.ProjectInput{
		Name: "proj-slots-default", TargetWorkspaceID: ws, Tags: []string{},
	}, "")
	s.Require().NoError(err)
	s.Equal(5, p2.WorkerSlots, "the user's defaultWorkerSlots feeds the create fallback")

	// An explicit value still wins over the user default.
	p3, err := s.svc.CreateProject(ctx, s.owner(), board.ProjectInput{
		Name: "proj-slots-explicit", TargetWorkspaceID: ws, WorkerSlots: 3, Tags: []string{},
	}, "")
	s.Require().NoError(err)
	s.Equal(3, p3.WorkerSlots, "an explicit slots value beats the user default")

	// Clearing the user map restores the code default for creates.
	_, err = s.svc.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{})
	s.Require().NoError(err)
	p4, err := s.svc.CreateProject(ctx, s.owner(), board.ProjectInput{
		Name: "proj-slots-cleared", TargetWorkspaceID: ws, Tags: []string{},
	}, "")
	s.Require().NoError(err)
	s.Equal(2, p4.WorkerSlots)
}

// TestBoardConfig_ValidationAndAuthz: unknown keys and malformed values
// reject at the service boundary (GuardInput); project writes require
// the owning user (R7) and the manager-surface discipline; a foreign
// project's overrides never leak (scoped 404); user writes require an
// authenticated acting user.
func (s *BoardSuite) TestBoardConfig_ValidationAndAuthz() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)

	// Service-side belt: malformed values reject even when a caller
	// bypassed the adapter's decode.
	_, err := s.svc.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{DefaultWorkerSlots: intVal(0)})
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

	// defaultWorkerSlots is a USER-level key: a per-project override
	// would be inert (the engine honors projects.worker_slots) yet
	// render as "effective" — an effective-map lie. Rejected.
	_, err = s.svc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "",
		&board.BoardConfigOverride{DefaultWorkerSlots: intVal(4)})
	s.Require().Error(err)
	guard, ok = err.(*board.GuardError)
	s.Require().True(ok, "got %T", err)
	s.Equal(board.GuardInput, guard.Code)
	s.Contains(guard.Detail, "user-level key", "the rejection names the ruling")

	// Nothing landed: effective values still the defaults.
	pc, err := s.svc.GetProjectBoardConfig(ctx, s.owner(), p.ID, "")
	s.Require().NoError(err)
	s.Equal("auto", pc.Effective.ReviewColumn)

	// User-scoped write needs an acting user (guards once, in the
	// service — a caller that bypassed the adapter still cannot write
	// an anonymous defaults map).
	_, err = s.svc.SetUserBoardConfig(ctx, board.Actor{Kind: board.ActorUser, Surface: board.SurfaceREST}, &board.BoardConfigOverride{})
	s.Require().Error(err)
	guard, ok = err.(*board.GuardError)
	s.Require().True(ok, "got %T", err)
	s.Equal(board.GuardAuthz, guard.Code)

	// The READ seam enforces the same actor discipline: an empty actor
	// must NOT silently fall back to engine-scope resolution (which
	// would read ANY project's config — the fail-open the round-2
	// review caught).
	_, err = s.svc.GetProjectBoardConfig(ctx, board.Actor{}, p.ID, "")
	s.Require().Error(err)
	guard, ok = err.(*board.GuardError)
	s.Require().True(ok)
	s.Equal(board.GuardAuthz, guard.Code, "empty actor rejects authz, never resolves unscoped")

	// The USER read seam enforces the same actor discipline (round-3
	// uniformity): an empty actor must not silently render the code
	// defaults dressed up as a resolved user's effective map.
	_, err = s.svc.GetUserBoardConfig(ctx, board.Actor{})
	s.Require().Error(err)
	guard, ok = err.(*board.GuardError)
	s.Require().True(ok)
	s.Equal(board.GuardAuthz, guard.Code, "empty actor rejects authz on the user read too")

	// R7: a foreign actor's write resolves the scoped 404 (the
	// UpdateProject discipline — foreign rows are plain 404s, never
	// disclosed, never an authz disclosure).
	foreign := board.Actor{Kind: board.ActorUser, UserID: "board-foreign-user", Surface: board.SurfaceREST}
	_, err = s.svc.SetProjectBoardConfig(ctx, foreign, p.ID, "", &board.BoardConfigOverride{})
	s.Require().Error(err)
	_, ok = err.(*board.NotFoundError)
	s.Require().True(ok, "got %T", err)

	// The READ perimeter is the service's own: a foreign actor's read
	// resolves the same scoped 404 (never disclosed).
	_, err = s.svc.GetProjectBoardConfig(ctx, foreign, p.ID, "")
	s.Require().Error(err)
	_, ok = err.(*board.NotFoundError)
	s.Require().True(ok, "got %T", err)

	// Unknown project resolves the 404 class.
	_, err = s.svc.GetProjectBoardConfig(ctx, s.owner(), "prj-zzzzzzzz", "")
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
	ov := board.BoardConfigOverride{AutoAssignReviewer: boolVal(true)}
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
		`INSERT INTO board_config_defaults (user_id, key, value) VALUES ($1, 'reviewQueue', '"off"')`, testUser)
	s.Require().Error(err, "unknown keys must not be storable")

	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)
	_, err = s.pool.Exec(ctx,
		`INSERT INTO board_project_config_overrides (project_id, key, value) VALUES ($1, 'reviewQueue', '"off"')`, p.ID)
	s.Require().Error(err, "unknown keys must not be storable per-project")

	// The project table's CHECK is NARROWER than the user table's
	// (round-3 ruling): defaultWorkerSlots is a user-level key — the
	// physical floor must match the service rejection, so an
	// out-of-band row cannot render an effective value the engine
	// does not honor.
	_, err = s.pool.Exec(ctx,
		`INSERT INTO board_project_config_overrides (project_id, key, value) VALUES ($1, 'defaultWorkerSlots', '4')`, p.ID)
	s.Require().Error(err, "defaultWorkerSlots must not be storable per-project (user-level key)")
	// The user table still admits it (the key's home).
	_, err = s.pool.Exec(ctx,
		`INSERT INTO board_config_defaults (user_id, key, value) VALUES ($1, 'defaultWorkerSlots', '4')`, testUser)
	s.Require().NoError(err, "defaultWorkerSlots remains storable on its home (user) table")
	_, err = s.pool.Exec(ctx, `DELETE FROM board_config_defaults WHERE user_id=$1 AND key='defaultWorkerSlots'`, testUser)
	s.Require().NoError(err)

	// Sanity: a known key stores fine (scoped to the user).
	_, err = s.pool.Exec(ctx,
		`INSERT INTO board_config_defaults (user_id, key, value) VALUES ($1, 'reviewColumn', '"auto"')`, testUser)
	s.Require().NoError(err)
	var v string
	err = s.pool.QueryRow(ctx, `SELECT value#>>'{}' FROM board_config_defaults WHERE user_id=$1 AND key='reviewColumn'`, testUser).Scan(&v)
	s.Require().NoError(err)
	assert.Equal(s.T(), "auto", v)
	require.NoError(s.T(), err)
}

// TestBoardConfig_FailClosedOnDrift: the CHECK belt validates only the
// KEY, not the value — a known key with a malformed value that somehow
// drifted past the write belts (e.g. manual SQL) must surface as an
// ERROR on read, never a silently rendered zero value.
func (s *BoardSuite) TestBoardConfig_FailClosedOnDrift() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)

	// Drift: known keys, malformed values, landed via raw SQL.
	_, err := s.pool.Exec(ctx,
		`INSERT INTO board_config_defaults (user_id, key, value) VALUES ($1, 'autoAssignReviewer', '"not-a-bool"')`, testUser)
	s.Require().NoError(err)
	_, err = s.pool.Exec(ctx,
		`INSERT INTO board_project_config_overrides (project_id, key, value) VALUES ($1, 'reviewColumn', '42')`, p.ID)
	s.Require().NoError(err)

	// Reads fail closed with the DRIFT class — a server-side data
	// problem renders 5xx at the adapter, never 400 invalid_input (the
	// caller did nothing wrong).
	_, err = s.svc.GetUserBoardConfig(ctx, s.owner())
	s.Require().Error(err, "a drifted defaults row must surface as an error, not a zero value")
	drift, ok := err.(*board.StorageDriftError)
	s.Require().True(ok, "got %T", err)
	s.Contains(drift.Error(), "user defaults")

	// The project read fails closed on the SAME user-map drift (it
	// loads the user defaults beneath the overrides) — then, with the
	// user map clean, on its own override drift.
	_, err = s.svc.GetProjectBoardConfig(ctx, s.owner(), p.ID, "")
	s.Require().Error(err)
	drift, ok = err.(*board.StorageDriftError)
	s.Require().True(ok, "got %T", err)
	s.Contains(drift.Error(), "user defaults")

	_, err = s.pool.Exec(ctx, `DELETE FROM board_config_defaults WHERE user_id=$1 AND key='autoAssignReviewer'`, testUser)
	s.Require().NoError(err)

	_, err = s.svc.GetProjectBoardConfig(ctx, s.owner(), p.ID, "")
	s.Require().Error(err, "a drifted override row must surface as an error, not a zero value")
	drift, ok = err.(*board.StorageDriftError)
	s.Require().True(ok, "got %T", err)
	s.Contains(drift.Error(), "project overrides")

	// And the create fallback fails closed too (no silent 0-slot
	// projects off a drifted map) — re-drift the user map for this arm.
	_, err = s.pool.Exec(ctx,
		`INSERT INTO board_config_defaults (user_id, key, value) VALUES ($1, 'defaultWorkerSlots', '"x"')`, testUser)
	s.Require().NoError(err)
	_, err = s.svc.CreateProject(ctx, s.owner(), board.ProjectInput{
		Name: "proj-against-drift", TargetWorkspaceID: ws, Tags: []string{},
	}, "")
	s.Require().Error(err, "CreateProject's slots fallback must not silently survive a drifted defaults row")
}

// TestBoardConfig_ConcurrentReplaceWrites: concurrent REPLACE writes
// to one scope serialize (per-scope advisory-xact-lock): no 23505
// PK-violation errors, and the final map is exactly ONE of the written
// maps — never a union of interleaved disjoint keys.
func (s *BoardSuite) TestBoardConfig_ConcurrentReplaceWrites() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)

	// mapA pins autoAssignReviewer=false explicitly: post-2026-10-24
	// the code default inherits true, so without the explicit key the
	// two written maps would agree on autoAssignReviewer and the
	// union-detection would lose a dimension.
	mapA := board.BoardConfigOverride{ReviewColumn: strVal(board.ReviewColumnOff), AutoAssignReviewer: boolVal(false), DefaultWorkerSlots: intVal(3)}
	mapB := board.BoardConfigOverride{AutoAssignReviewer: boolVal(true)}
	// The project surface rejects defaultWorkerSlots (user-level key),
	// so its competing maps differ on the other two keys.
	projA := board.BoardConfigOverride{ReviewColumn: strVal(board.ReviewColumnOff)}
	projB := board.BoardConfigOverride{AutoAssignReviewer: boolVal(true)}

	const writers = 8
	errCh := make(chan error, 2*writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			in, projIn := mapA, projA
			if i%2 == 1 {
				in, projIn = mapB, projB
			}
			_, err := s.svc.SetUserBoardConfig(ctx, s.owner(), &in)
			errCh <- err
			_, err = s.svc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &projIn)
			errCh <- err
		}(i)
	}
	for i := 0; i < 2*writers; i++ {
		s.Require().NoError(<-errCh, "concurrent REPLACE writes must not surface PK-violation 500s")
	}

	// Final USER map is exactly A or exactly B (never a union).
	cfg, err := s.svc.GetUserBoardConfig(ctx, s.owner())
	s.Require().NoError(err)
	uIsA := cfg.ReviewColumn == board.ReviewColumnOff && cfg.DefaultWorkerSlots == 3 && !cfg.AutoAssignReviewer
	uIsB := cfg.ReviewColumn == board.ReviewColumnAuto && cfg.DefaultWorkerSlots == 2 && cfg.AutoAssignReviewer
	s.True(uIsA || uIsB, "final user map is one written map, not a union: %+v", cfg)

	// Final PROJECT OVERRIDE map is exactly A or exactly B (never a
	// union). The project's EFFECTIVE row is asserted against
	// merge-of-observed, NOT "all-A or all-B": the user and project
	// scopes sit under INDEPENDENT advisory locks, so the last user
	// writer and the last project writer can land in different
	// parities — U=A/P=B and U=B/P=A are both reachable and legal
	// (pinned deterministically in TestBoardConfig_CrossScopeParity);
	// the old all-A-or-all-B effective assertion was flaky by
	// construction.
	pc, err := s.svc.GetProjectBoardConfig(ctx, s.owner(), p.ID, "")
	s.Require().NoError(err)
	pOvA := pc.Overrides.ReviewColumn != nil && *pc.Overrides.ReviewColumn == board.ReviewColumnOff &&
		pc.Overrides.AutoAssignReviewer == nil
	pOvB := pc.Overrides.AutoAssignReviewer != nil && *pc.Overrides.AutoAssignReviewer &&
		pc.Overrides.ReviewColumn == nil
	s.True(pOvA || pOvB, "final project override map is one written map, not a union: %+v", pc.Overrides)
	s.Equal(board.MergeBoardConfig(*cfg, &pc.Overrides), pc.Effective,
		"effective is exactly the observed user map merged with the observed overrides — any parity")
}

// TestBoardConfig_CrossScopeParity: the user-defaults map and the
// project-overrides map are INDEPENDENTLY locked scopes — a final
// U=A/P=B or U=B/P=A state is legal and renders exactly the merge of
// the two observed maps. The deterministic twin of the concurrency
// test's racing path: this pins the semantics with no race at all
// (the round-3 review's cross-parity arms).
func (s *BoardSuite) TestBoardConfig_CrossScopeParity() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)

	// U=A / P=B: user sets reviewColumn=off + slots=3; project sets
	// only autoAssignReviewer=true. Effective = {off, true, 3}.
	userA := board.BoardConfigOverride{ReviewColumn: strVal(board.ReviewColumnOff), DefaultWorkerSlots: intVal(3)}
	projB := board.BoardConfigOverride{AutoAssignReviewer: boolVal(true)}
	_, err := s.svc.SetUserBoardConfig(ctx, s.owner(), &userA)
	s.Require().NoError(err)
	pc, err := s.svc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &projB)
	s.Require().NoError(err)
	s.Equal(board.ReviewColumnOff, pc.Effective.ReviewColumn, "U=A/P=B: review column from the user map")
	s.True(pc.Effective.AutoAssignReviewer, "U=A/P=B: auto-assign from the project override")
	s.Equal(3, pc.Effective.DefaultWorkerSlots, "U=A/P=B: slots from the user map")

	// U=B / P=A: user sets only autoAssignReviewer=true; project sets
	// reviewColumn=off. Effective = {off, true, 2} — slots re-inherit
	// the CODE default: the exact mixed row the old all-A-or-all-B
	// effective assertion wrongly ruled impossible.
	userB := board.BoardConfigOverride{AutoAssignReviewer: boolVal(true)}
	projA := board.BoardConfigOverride{ReviewColumn: strVal(board.ReviewColumnOff)}
	_, err = s.svc.SetUserBoardConfig(ctx, s.owner(), &userB)
	s.Require().NoError(err)
	pc, err = s.svc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &projA)
	s.Require().NoError(err)
	s.Equal(board.ReviewColumnOff, pc.Effective.ReviewColumn, "U=B/P=A: review column from the project override")
	s.True(pc.Effective.AutoAssignReviewer, "U=B/P=A: auto-assign from the user map")
	s.Equal(2, pc.Effective.DefaultWorkerSlots, "U=B/P=A: slots re-inherit the code default — a legal mixed row, not a union")
}

// TestBoardConfig_ViewRealWiring: GetBoardConfigView against the REAL
// store (the round-2 review finding: the handler tests fake the
// manager and the pkg tests pinned only the pure builder — this is
// the missing service-seam wiring). The user tier renders the RAW
// stored map with per-key sources; the project tier resolves through
// the scoped read, overlays both RAW maps, and annotates every config
// key and persona-eligible stage.
func (s *BoardSuite) TestBoardConfig_ViewRealWiring() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)

	// The harness seeds the owner's user-tier stagePersonas opt-out
	// ({}); this REPLACE write (one key) reverts it — absent keys
	// revert, so the raw user map below is EXACTLY {defaultWorkerSlots}.
	// The slots key (not reviewColumn) keeps the flipped composition
	// (reviewColumn=auto + autoAssignReviewer=true) live for the
	// persona-provenance assertions below.
	_, err := s.svc.SetUserBoardConfig(ctx, s.owner(), &board.BoardConfigOverride{
		DefaultWorkerSlots: intVal(5),
	})
	s.Require().NoError(err)

	// --- user tier: no project ref ------------------------------------------------
	v, err := s.svc.GetBoardConfigView(ctx, s.owner(), "", "")
	s.Require().NoError(err)
	s.Equal("user", v.Scope)
	s.Empty(v.ProjectID)
	// The RAW stored map rides (not just the merge): exactly the one
	// key written; the seeded opt-out is gone (REPLACE semantics).
	s.Require().NotNil(v.Overrides.DefaultWorkerSlots)
	s.Equal(5, *v.Overrides.DefaultWorkerSlots)
	s.Nil(v.Overrides.StagePersonas, "the REPLACE write reverted the harness-seeded {} opt-out")
	s.Nil(v.Overrides.ReviewColumn)
	// Effective = code defaults ⊕ the raw map.
	s.Equal(board.ReviewColumnAuto, v.Effective.ReviewColumn)
	s.True(v.Effective.AutoAssignReviewer)
	s.Equal(5, v.Effective.DefaultWorkerSlots)
	// Per-key sources, ordered by the merge: user default, else code.
	s.Equal(board.ConfigSourceCodeDefault, v.Sources[board.ConfigKeyReviewColumn])
	s.Equal(board.ConfigSourceCodeDefault, v.Sources[board.ConfigKeyAutoAssignReviewer])
	s.Equal(board.ConfigSourceUserDefault, v.Sources[board.ConfigKeyDefaultWorkerSlots])
	s.Equal(board.ConfigSourceCodeDefault, v.Sources[board.ConfigKeyStagePersonas])
	// Personas with NO explicit map anywhere: the flipped composition
	// (reviewColumn=auto + autoAssignReviewer=true) renders in_review
	// as the BUILT-IN default reviewer; in_progress has no composition.
	s.Equal(board.PersonaSourceDefaultReviewer, v.Personas[board.StageInReview].Source)
	s.Require().NotNil(v.Personas[board.StageInReview].Persona)
	s.Equal(board.DefaultReviewerPersona.Name, v.Personas[board.StageInReview].Persona.Name)
	s.Equal(board.PersonaSourceNone, v.Personas[board.StageInProgress].Source)
	s.Nil(v.Personas[board.StageInProgress].Persona)

	// --- project tier: one config key + one persona stage overridden --------------
	pers := map[string]board.PersonaConfig{
		board.StageInReview: {Name: "proj reviewer", Instructions: "Check the pins."},
	}
	_, err = s.svc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{
		ReviewColumn:  strVal(board.ReviewColumnAuto),
		StagePersonas: &pers,
	})
	s.Require().NoError(err)

	v, err = s.svc.GetBoardConfigView(ctx, s.owner(), p.ID, "")
	s.Require().NoError(err)
	s.Equal("project", v.Scope)
	s.Equal(p.ID, v.ProjectID)
	// The project tier's overrides are ITS OWN raw map (what THIS tier
	// stores — not the merge, not the user's rows).
	s.Require().NotNil(v.Overrides.ReviewColumn)
	s.Equal(board.ReviewColumnAuto, *v.Overrides.ReviewColumn)
	s.Nil(v.Overrides.AutoAssignReviewer, "unset at the project tier")
	s.Nil(v.Overrides.DefaultWorkerSlots, "a user-level key — never stored at the project tier")
	s.Require().NotNil(v.Overrides.StagePersonas)
	s.Len(*v.Overrides.StagePersonas, 1)
	// Effective: the project overlay wins; the rest inherit (the
	// user's slots default included).
	s.Equal(board.ReviewColumnAuto, v.Effective.ReviewColumn)
	s.True(v.Effective.AutoAssignReviewer, "unset key inherits the (code) default")
	s.Equal(5, v.Effective.DefaultWorkerSlots, "unset key inherits the USER default")
	// Sources: project override > user default > code, per key.
	s.Equal(board.ConfigSourceProjectOverride, v.Sources[board.ConfigKeyReviewColumn])
	s.Equal(board.ConfigSourceCodeDefault, v.Sources[board.ConfigKeyAutoAssignReviewer])
	s.Equal(board.ConfigSourceUserDefault, v.Sources[board.ConfigKeyDefaultWorkerSlots])
	s.Equal(board.ConfigSourceProjectOverride, v.Sources[board.ConfigKeyStagePersonas])
	// Persona provenance: in_review from the project map (payload
	// included); in_progress UNLISTED in the explicit map → none.
	s.Equal(board.PersonaSourceProjectOverride, v.Personas[board.StageInReview].Source)
	s.Require().NotNil(v.Personas[board.StageInReview].Persona)
	s.Equal("proj reviewer", v.Personas[board.StageInReview].Persona.Name)
	s.Equal(board.PersonaSourceNoneExplicit, v.Personas[board.StageInProgress].Source)
	s.Nil(v.Personas[board.StageInProgress].Persona)
}

// TestBoardConfig_ViewGuards: the view read's refusal shapes on the
// real seam — the empty-actor authz guard (both tiers), the #1747
// workspace binding on the agent surface's project arm, the scoped
// 404 for a foreign project, and short-id resolution parity.
func (s *BoardSuite) TestBoardConfig_ViewGuards() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 0)

	// Empty actor: both tiers refuse the user-scoped guard.
	_, err := s.svc.GetBoardConfigView(ctx, board.Actor{Kind: board.ActorUser, Surface: board.SurfaceREST}, "", "")
	var guard *board.GuardError
	s.Require().ErrorAs(err, &guard)
	s.Equal(board.GuardAuthz, guard.Code)

	_, err = s.svc.GetBoardConfigView(ctx, board.Actor{Kind: board.ActorUser, Surface: board.SurfaceREST}, p.ID, "")
	s.Require().ErrorAs(err, &guard)
	s.Equal(board.GuardAuthz, guard.Code)

	// Agent manager surface, foreign workspace: the #1747 binding
	// refuses the project arm (the read does not disclose foreign
	// rows); the user tier (no project ref) is unaffected by the
	// binding — it is the caller's own row.
	agent := board.Actor{Kind: board.ActorUser, UserID: testUser, Surface: board.SurfaceAgentManager, WorkspaceID: "ws-not-mine"}
	_, err = s.svc.GetBoardConfigView(ctx, agent, p.ID, "")
	s.Require().ErrorAs(err, &guard)
	s.Equal(board.GuardWorkspaceBinding, guard.Code)

	v, err := s.svc.GetBoardConfigView(ctx, agent, "", "")
	s.Require().NoError(err, "the user tier carries no project ref — no binding to violate")
	s.Equal("user", v.Scope)

	// The WRITE perimeter mirrors the read's: board_set_config's
	// project arm (SetProjectBoardConfig) refuses the same
	// foreign-workspace binding at the guard — BEFORE any write lands
	// (the override rows never appear; the guard runs ahead of the
	// replace, structural in SetProjectBoardConfig's ordering).
	_, err = s.svc.SetProjectBoardConfig(ctx, agent, p.ID, "",
		&board.BoardConfigOverride{AutoAssignReviewer: boolVal(true)})
	s.Require().ErrorAs(err, &guard)
	s.Equal(board.GuardWorkspaceBinding, guard.Code)
	var rows int
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT count(*) FROM board_project_config_overrides WHERE project_id=$1`, p.ID).Scan(&rows))
	s.Zero(rows, "the refused set writes nothing — the binding guard precedes the replace")

	// Foreign project: the scoped 404 (never disclosed).
	_, err = s.svc.GetBoardConfigView(ctx, s.owner(), "0ea0c65c-0000-4000-8000-000000000001", "")
	s.Require().Error(err)
	var nf *board.NotFoundError
	s.Require().ErrorAs(err, &nf)

	// Short-id resolution parity: the same project renders by
	// short_id and by UUID.
	byShort, err := s.svc.GetBoardConfigView(ctx, s.owner(), "", p.ShortID)
	s.Require().NoError(err)
	byID, err := s.svc.GetBoardConfigView(ctx, s.owner(), p.ID, "")
	s.Require().NoError(err)
	s.Equal(byID, byShort, "short-id and UUID resolve the same view")
}

// Local pointer helpers (external test package: unexported helpers in
// config_test.go are out of reach).
func boolVal(b bool) *bool    { return &b }
func intVal(i int) *int       { return &i }
func strVal(s string) *string { return &s }
