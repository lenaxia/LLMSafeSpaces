//go:build integration

package board_test

import (
	"context"
	"fmt"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// persona_roles_integration_test.go — the ROLE-ONLY persona seams at
// the Service level (owner ruling 2026-10-29, round 2: "Board stages
// should ONLY ever have roles configured, there is no custom
// definition in the board itself."). PG harness; the fake
// RoleDirectory stands in for the app's agent_roles-backed concrete.

// fakeRoleDirectory is the RoleDirectory seam's test double.
type fakeRoleDirectory struct {
	roles []board.AgentRoleRef
	err   error
}

func (f *fakeRoleDirectory) AgentRolesForUser(context.Context, string) ([]board.AgentRoleRef, error) {
	return f.roles, f.err
}

var reviewerRole = board.AgentRoleRef{ID: "11111111-1111-1111-1111-111111111111", Slug: "reviewer", Name: "Code Reviewer", Scope: "platform"}

// buildRole is the second fixture role (a non-reviewer vocabulary
// entry for stale-ref and fallback arms).
var buildRole = board.AgentRoleRef{ID: "33333333-3333-3333-3333-333333333333", Slug: "build", Name: "Builder", Scope: "org"}

// personaRoleDirectory is the standard two-role vocabulary (the seeded
// platform reviewer plus one org role) — the shape a healthy
// deployment renders after startup seeding.
func personaRoleDirectory() *fakeRoleDirectory {
	return &fakeRoleDirectory{roles: []board.AgentRoleRef{reviewerRole, buildRole}}
}

// TestPersonaRoles_SetValidatesRoleRefs — board_set_config's role
// validation belt: a reference the available set cannot resolve
// REJECTS (GuardInput naming it); an id or slug reference that
// resolves writes (and the stored row is the canonical bare-string
// map). A role-bearing set with NO directory installed also refuses
// (never store a reference this deployment can never honor).
func (s *BoardSuite) TestPersonaRoles_SetValidatesRoleRefs() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)

	dirSvc := board.NewService(s.pool, s.cfg, board.WithRoleDirectory(personaRoleDirectory()))

	// Unknown reference rejects, naming it.
	_, err := dirSvc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{
		StagePersonas: &map[string]board.PersonaConfig{"in_review": {RoleID: "ghost"}},
	})
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), `"ghost" does not match an available agent role`)

	// Slug reference resolves and writes.
	m := map[string]board.PersonaConfig{"in_review": {RoleID: "reviewer"}}
	_, err = dirSvc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{StagePersonas: &m})
	s.Require().NoError(err)
	var stored string
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT value::text FROM board_project_config_overrides WHERE project_id=$1 AND key='stagePersonas'`,
		p.ID).Scan(&stored))
	s.Contains(stored, `"reviewer"`, "the stored row is the canonical bare-string map")

	// Id reference resolves too.
	idm := map[string]board.PersonaConfig{"in_progress": {RoleID: reviewerRole.ID}}
	_, err = dirSvc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{StagePersonas: &idm})
	s.Require().NoError(err)

	// Role-blind deployment (no directory): a role-bearing set refuses
	// rather than storing an unresolvable reference — with the
	// role-blind remedy named (review round 1 finding 2: the classes
	// stay DISTINCT — this is a deployment posture, not a payload
	// typo). Under role-only EVERY persona carries a reference, so the
	// blind deployment cannot set personas at all.
	blindSvc := board.NewService(s.pool, s.cfg)
	_, err = blindSvc.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{
		StagePersonas: &map[string]board.PersonaConfig{"in_review": {RoleID: "reviewer"}},
	})
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "no agent-role directory")
	var ge *board.GuardError
	require.ErrorAs(s.T(), err, &ge, "role-blind is still a 400-class guard (the caller can drop the reference)")
}

// TestPersonaRoles_SetDirectoryOutageIs500Class — review round 1
// finding 2: a TRANSIENT directory read failure during a set is a
// plain error (500-class, retryable — the payload may be perfectly
// valid), never GuardInput.
func (s *BoardSuite) TestPersonaRoles_SetDirectoryOutageIs500Class() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)

	outage := board.NewService(s.pool, s.cfg,
		board.WithRoleDirectory(&fakeRoleDirectory{err: fmt.Errorf("role store unreachable")}))
	_, err := outage.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{
		StagePersonas: &map[string]board.PersonaConfig{"in_review": {RoleID: "reviewer"}},
	})
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "agent-role directory read failed")
	require.Contains(s.T(), err.Error(), "retry", "the remedy is retry, not payload surgery")
	var ge *board.GuardError
	require.NotErrorAs(s.T(), err, &ge, "an outage is 500-class — never a 400 guard")
}

// TestPersonaRoles_StaleRefDegradesToPersonaLess — the role-only
// degradation (owner ruling 2026-10-29, round 2): an EXPLICIT role
// reference the vocabulary no longer resolves (the role was deleted
// after storage — the delete path has no stagePersonas cleanup)
// DEGRADES at spawn time to NO PERSONA: the claim carries none (no
// mint stamp, no agent on the dispatch — byte-identical to a
// persona-less stage; the ROLE is the behavior, and an unresolvable
// role means there is no behavior to spawn). The STORED row keeps the
// reference (the owner sees the drift in the config view and can fix
// it); the reconciler's re-arm heals once the vocabulary carries the
// role again.
func (s *BoardSuite) TestPersonaRoles_StaleRefDegradesToPersonaLess() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "stale-ref", nil, false)

	// Store a VALID reference through a directory that carries it.
	seed := board.NewService(s.pool, s.cfg, board.WithRoleDirectory(personaRoleDirectory()))
	m := map[string]board.PersonaConfig{"in_progress": {RoleID: "reviewer"}}
	_, err := seed.SetProjectBoardConfig(ctx, s.owner(), p.ID, "", &board.BoardConfigOverride{StagePersonas: &m})
	s.Require().NoError(err)

	// The role is then deleted: the vocabulary no longer resolves it.
	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithRoleDirectory(&fakeRoleDirectory{roles: []board.AgentRoleRef{buildRole}}))

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	s.Require().Nil(c.Persona, "the stale reference degrades to NO persona — nothing role-less ever spawns")

	// The STORED row keeps the reference verbatim — the config view
	// renders the drift for the owner to fix, only the spawn degrades.
	pc, err := svc.GetProjectBoardConfig(ctx, s.owner(), p.ID, "")
	s.Require().NoError(err)
	s.Equal("reviewer", (*pc.Overrides.StagePersonas)["in_progress"].RoleID, "storage keeps the stored truth")
}

// TestPersonaRoles_StoredNonRoleShapeIsDrift — the NO-migration ruling
// (owner ruling 2026-10-29: "We do not need migration, we have never
// created any personas ever."): a stored non-role persona shape (the
// pre-role {name, instructions} row — hand-inserted past the write
// belts, the only way one can exist) NEVER decodes: every config read
// fails closed with the storage-drift error class (500, not a 400 the
// caller could "fix" — the caller did nothing wrong). No lazy
// name→role resolution, no compat decode, no backfill.
func (s *BoardSuite) TestPersonaRoles_StoredNonRoleShapeIsDrift() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)

	// Hand-insert the legacy row past every write belt.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO board_project_config_overrides (project_id, key, value)
		VALUES ($1, 'stagePersonas', '{"in_review": {"name": "auditor", "instructions": "Verify."}}'::jsonb)`,
		p.ID)
	s.Require().NoError(err)

	svc := board.NewService(s.pool, s.cfg, board.WithRoleDirectory(personaRoleDirectory()))
	_, err = svc.GetProjectBoardConfig(ctx, s.owner(), p.ID, "")
	require.Error(s.T(), err, "the stored non-role shape never decodes")
	require.Contains(s.T(), err.Error(), "storage drift", "the failure class is server-side drift, not caller input")
	var de *board.StorageDriftError
	require.ErrorAs(s.T(), err, &de)
}

// TestPersonaRoles_StoredNonRoleShapeIsDriftUserTier — the USER-tier
// twin of the project-tier drift pin (review round 2, PR #1990): the
// decode path is shared, but the defaults table (board_config_defaults)
// is a separate storage belt — pinned so a future tier-specific decode
// shortcut cannot quietly admit the legacy shape on one of them.
func (s *BoardSuite) TestPersonaRoles_StoredNonRoleShapeIsDriftUserTier() {
	ctx := context.Background()

	// Hand-insert the legacy row past every write belt.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO board_config_defaults (user_id, key, value)
		VALUES ($1, 'stagePersonas', '{"in_progress": {"name": "auditor", "instructions": "Verify."}}'::jsonb)
		ON CONFLICT (user_id, key) DO UPDATE SET value = EXCLUDED.value`,
		s.owner().UserID)
	s.Require().NoError(err)
	s.T().Cleanup(func() {
		_, _ = s.pool.Exec(ctx, `DELETE FROM board_config_defaults WHERE user_id=$1 AND key='stagePersonas'`, s.owner().UserID)
	})

	svc := board.NewService(s.pool, s.cfg, board.WithRoleDirectory(personaRoleDirectory()))
	_, err = svc.GetUserBoardConfig(ctx, s.owner())
	require.Error(s.T(), err, "the stored non-role shape never decodes on the user tier either")
	require.Contains(s.T(), err.Error(), "storage drift", "the failure class is server-side drift, not caller input")
	var de *board.StorageDriftError
	require.ErrorAs(s.T(), err, &de)
}

// TestPersonaRoles_DefaultReviewerResolvesToRole — the default reviewer
// composition (reviewColumn=auto + autoAssignReviewer, no explicit
// personas) resolves to the SEEDED reviewer agent role (owner ruling
// 2026-10-29, round 2: the verdict-channel charter is the role's
// system prompt): the spawned reviewer persona carries ONLY the
// canonical role reference — no name, no instructions, nothing to
// drift.
func (s *BoardSuite) TestPersonaRoles_DefaultReviewerResolvesToRole() {
	ctx := context.Background()
	ws := s.seedWorkspace()
	p := s.seedProject(ws, 2)
	e := s.seedEpic(p, board.EpicActive)
	task := s.seedTask(e, "role-reviewer", nil, false)

	// The composition fires with no config anywhere (clear the
	// harness's isolation seed), and the directory carries a reviewer
	// role (slug match — the seeded platform role's shape).
	_, err := s.pool.Exec(ctx, `DELETE FROM board_config_defaults WHERE user_id=$1`, testUser)
	s.Require().NoError(err)
	spawner := &claimCapture{}
	svc := board.NewService(s.pool, s.cfg,
		board.WithWorkerSpawner(spawner.Spawn),
		board.WithRoleDirectory(personaRoleDirectory()))

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)

	s.Require().Len(spawner.claims, 2, "grant + the composition's reviewer")
	rc := spawner.claims[1]
	require.NotNil(s.T(), rc.Persona)
	s.Equal(reviewerRole.ID, rc.Persona.RoleID, "the default reviewer composition resolves to the reviewer agent role's canonical id")
	s.Equal(board.PersonaConfig{RoleID: reviewerRole.ID}, *rc.Persona, "the persona IS the canonicalized role reference — nothing else composes onto it")
}

// TestPersonaRoles_DefaultReviewerDegradesWithoutRole — no reviewer
// role in the vocabulary (a deployment whose seeding never landed):
// the composition's slug reference is unresolvable, so the reviewer
// persona DEGRADES to persona-less — no reviewer claim is minted, no
// reviewer spawns (the ROLE is the behavior; without it there is
// nothing to run). The transition itself lands exactly like a
// persona-less in_review entry.
func (s *BoardSuite) TestPersonaRoles_DefaultReviewerDegradesWithoutRole() {
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
		board.WithRoleDirectory(&fakeRoleDirectory{roles: []board.AgentRoleRef{buildRole}}))

	c, err := svc.ClaimNext(ctx, "w-"+ws, ws)
	s.Require().NoError(err)
	s.Require().Equal(task.ID, c.TaskID)
	_, err = svc.MoveStage(ctx, s.agent(c.Assignee, c.ClaimToken), "", c.ShortID, board.StageInReview, "", "")
	s.Require().NoError(err)

	s.Require().Len(spawner.claims, 1, "the worker grant only — the unresolvable reviewer persona spawns nothing")
}
