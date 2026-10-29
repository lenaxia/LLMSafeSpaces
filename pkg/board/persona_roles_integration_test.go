//go:build integration

package board_test

import (
	"context"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// persona_roles_integration_test.go — the role-first persona seams at
// the Service level (owner ruling 2026-10-29: personas REFERENCE the
// platform's agent roles). PG harness; the fake RoleDirectory stands
// in for the app's agent_roles-backed concrete.

// fakeRoleDirectory is the RoleDirectory seam's test double.
type fakeRoleDirectory struct {
	roles []board.AgentRoleRef
	err   error
}

func (f *fakeRoleDirectory) AgentRolesForUser(context.Context, string) ([]board.AgentRoleRef, error) {
	return f.roles, f.err
}

var reviewerRole = board.AgentRoleRef{ID: "11111111-1111-1111-1111-111111111111", Slug: "reviewer", Name: "Code Reviewer", Scope: "platform"}

// TestPersonaRoles_SetValidatesRoleRefs — board_set_config's role
// validation belt: a persona carrying an explicit role reference the
// available set cannot resolve REJECTS (GuardInput naming it); an id
// or slug reference that resolves writes. A role-bearing set with NO
// directory installed also refuses (never store a reference this
// deployment can never honor).
func (s *BoardSuite) TestPersonaRoles_SetValidatesRoleRefs() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)

	dirSvc := board.NewService(s.pool, s.cfg, board.WithRoleDirectory(&fakeRoleDirectory{roles: []board.AgentRoleRef{reviewerRole}}))

	// Unknown reference rejects, naming it.
	_, err := dirSvc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{
		StagePersonas: &map[string]board.PersonaConfig{"in_review": {RoleID: "ghost"}},
	})
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), `"ghost" does not match an available agent role`)

	// Slug reference resolves and writes.
	m := map[string]board.PersonaConfig{"in_review": {RoleID: "reviewer", Instructions: "Adversarial."}}
	_, err = dirSvc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{StagePersonas: &m})
	s.Require().NoError(err)

	// Id reference resolves too.
	idm := map[string]board.PersonaConfig{"in_progress": {RoleID: reviewerRole.ID}}
	_, err = dirSvc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{StagePersonas: &idm})
	s.Require().NoError(err)

	// A legacy name-only persona still writes (no reference, no
	// validation) — the pre-role vocabulary stays legal.
	legacy := map[string]board.PersonaConfig{"in_review": {Name: "auditor", Instructions: "Verify."}}
	_, err = dirSvc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{StagePersonas: &legacy})
	s.Require().NoError(err)

	// Role-blind deployment (no directory): a role-bearing set refuses
	// rather than storing an unresolvable reference.
	blindSvc := board.NewService(s.pool, s.cfg)
	_, err = blindSvc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{
		StagePersonas: &map[string]board.PersonaConfig{"in_review": {RoleID: "reviewer"}},
	})
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "does not match an available agent role")
}

// TestPersonaRoles_ReadMigration — the stored-config migration: a
// legacy name-only persona (stored pre-role) renders with a resolved
// RoleID on read (both config reads AND the effective engine path);
// an unresolvable name keeps the legacy shape verbatim; the migration
// is LAZY — the stored row is byte-identical until a client writes
// the resolved shape back.
func (s *BoardSuite) TestPersonaRoles_ReadMigration() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)

	// Store legacy personas through a role-blind service (the
	// pre-role platform shape).
	blind := board.NewService(s.pool, s.cfg)
	legacy := map[string]board.PersonaConfig{
		"in_review":   {Name: "reviewer", Instructions: "Verify."},
		"in_progress": {Name: "implementer", Instructions: "Ship."},
	}
	_, err := blind.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{StagePersonas: &legacy})
	s.Require().NoError(err)

	dir := &fakeRoleDirectory{roles: []board.AgentRoleRef{reviewerRole}}
	svc := board.NewService(s.pool, s.cfg, board.WithRoleDirectory(dir))

	// The project config read renders the resolved reference for the
	// resolvable name, the verbatim legacy shape for the other.
	pc, err := svc.GetProjectBoardConfig(ctx, s.owner(), p.ID, "")
	s.Require().NoError(err)
	s.Require().NotNil(pc.Overrides.StagePersonas)
	s.Equal(board.PersonaConfig{
		RoleID: reviewerRole.ID, Name: "reviewer", Instructions: "Verify.",
	}, (*pc.Overrides.StagePersonas)["in_review"], "a resolvable stored name migrates in the rendered shape")
	s.Equal(legacy["in_progress"], (*pc.Overrides.StagePersonas)["in_progress"], "an unresolvable name keeps the legacy shape")
	s.Equal(reviewerRole.ID, pc.Effective.StagePersonas["in_review"].RoleID, "the effective row carries the reference")

	// The view read (board_get_config's shape) migrates identically.
	view, err := svc.GetBoardConfigView(ctx, s.owner(), p.ID, "")
	s.Require().NoError(err)
	s.Require().NotNil(view.Personas["in_review"].Persona)
	s.Equal(reviewerRole.ID, view.Personas["in_review"].Persona.RoleID)

	// LAZY: the stored row is unchanged (reads never mutate storage).
	// (jsonb renders `: ` separators — match on the value, not tight
	// JSON punctuation.)
	var stored string
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT value::text FROM board_project_config_overrides WHERE project_id=$1 AND key='stagePersonas'`,
		p.ID).Scan(&stored))
	s.Contains(stored, `"reviewer"`)
	s.NotContains(stored, "roleId", "the migration renders in the decoded shape only — the row persists as written")

	// The engine's effective path resolves it too: the spawn envelope
	// carries the role reference.
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "migrate", nil, false)
	spawner := &claimCapture{}
	svc2 := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithRoleDirectory(&fakeRoleDirectory{roles: []board.AgentRoleRef{
			reviewerRole,
			{ID: "44444444-4444-4444-4444-444444444444", Slug: "implementer", Name: "Implementer"},
		}}))
	c, err := svc2.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	s.Require().NotNil(c.Persona)
	s.Equal("44444444-4444-4444-4444-444444444444", c.Persona.RoleID, "the implementer persona resolves its role on the spawn envelope")
	s.Equal("Ship.", c.Persona.Instructions, "the stored instructions ride as the override layer")
}

// TestPersonaRoles_DefaultReviewerPointsAtRole — the default reviewer
// composition (reviewColumn=auto + autoAssignReviewer, no explicit
// personas) POINTS AT the reviewer agent role when the vocabulary
// carries one (owner ruling 2026-10-29): the spawned reviewer persona
// carries the role reference with the charter prose layered on as the
// override. No reviewer role → the legacy prose persona runs
// byte-identically (RoleID empty).
func (s *BoardSuite) TestPersonaRoles_DefaultReviewerPointsAtRole() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "role-reviewer", nil, false)

	// The composition fires with no config anywhere (clear the
	// harness's isolation seed), and the directory carries a reviewer
	// role (slug match).
	_, err := s.pool.Exec(ctx, `DELETE FROM board_config_defaults WHERE user_id=$1`, testUser)
	s.Require().NoError(err)
	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithRoleDirectory(&fakeRoleDirectory{roles: []board.AgentRoleRef{reviewerRole}}))

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)

	s.Require().Len(spawner.claims, 2, "grant + the composition's reviewer")
	rc := spawner.claims[1]
	require.NotNil(s.T(), rc.Persona)
	s.Equal(reviewerRole.ID, rc.Persona.RoleID, "the default reviewer composition POINTS AT the reviewer agent role")
	s.Equal(board.DefaultReviewerPersona.Name, rc.Persona.Name, "the display name survives")
	s.Equal(board.DefaultReviewerPersona.Instructions, rc.Persona.Instructions, "the charter prose rides as the override layer")
}

// TestPersonaRoles_DefaultReviewerFallsBackWithoutRole — no reviewer
// role in the vocabulary: the composition's persona keeps the legacy
// prose-only shape (RoleID empty — the fallback the ruling names).
func (s *BoardSuite) TestPersonaRoles_DefaultReviewerFallsBackWithoutRole() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "fallback-reviewer", nil, false)

	_, err := s.pool.Exec(ctx, `DELETE FROM board_config_defaults WHERE user_id=$1`, testUser)
	s.Require().NoError(err)
	spawner := &claimCapture{}
	// A directory WITHOUT a reviewer role (only unrelated roles).
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithRoleDirectory(&fakeRoleDirectory{roles: []board.AgentRoleRef{
			{ID: "33333333-3333-3333-3333-333333333333", Slug: "build", Name: "Builder"},
		}}))

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)

	s.Require().Len(spawner.claims, 2)
	rc := spawner.claims[1]
	require.NotNil(s.T(), rc.Persona)
	s.Equal("", rc.Persona.RoleID, "no reviewer role — the legacy composition stands (prose only)")
	s.Equal(board.DefaultReviewerPersona, *rc.Persona)
}
